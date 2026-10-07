// Package lmstudio collects LM Studio's loaded models and what they cost the
// host (R1).
//
// LM Studio reports only a model's file size. The real cost is the
// footprint of its per-model worker processes, which is read from the
// process table. The lms CLI wakes LM Studio when it is not running, so it is
// only called once the backend is known to be running: the pid in
// ~/.lmstudio/.internal/llmster-pid.lock must be alive and be the app's
// executable from app-install-location.json.
package lmstudio

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// CLI runs the lms binary.
type CLI interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Procs is the process access the source needs; vmproc.Host implements it.
type Procs interface {
	ProcessesNamed(comm string) ([]vmproc.Process, error)
	Footprint(ctx context.Context, pid int) (uint64, error)
}

// Source is the LM Studio source.
type Source struct {
	cli      CLI
	procs    Procs
	internal string // ~/.lmstudio/.internal
}

// New returns a source. A nil cli means lms was not found.
func New(cli CLI, procs Procs, home string) *Source {
	return &Source{cli: cli, procs: procs, internal: filepath.Join(home, ".lmstudio", ".internal")}
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "lmstudio" }

// Collect implements daemon.Source.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	l := protocol.LMStudio{Models: []protocol.LoadedModel{}}
	app, installed := s.appExec()
	if s.cli == nil || !installed {
		return reading{l}, nil
	}
	l.Installed = true
	backend, ok := s.backend(app)
	if !ok {
		return reading{l}, nil
	}
	l.Running = true

	out, err := s.cli.Run(ctx, "ps", "--json")
	if err != nil {
		return nil, fmt.Errorf("lms ps: %w", err)
	}
	var models []apiModel
	if err := json.Unmarshal(out, &models); err != nil {
		return nil, fmt.Errorf("lms ps: %w", err)
	}
	for _, m := range models {
		l.Models = append(l.Models, m.toProtocol())
	}
	l.FootprintBytes = s.footprint(ctx, backend)
	return reading{l}, nil
}

// footprint sums the backend and its worker processes: node children
// running from ~/.lmstudio/.internal. A process that exits meanwhile is
// left out.
func (s *Source) footprint(ctx context.Context, backend vmproc.Process) uint64 {
	total, _ := s.procs.Footprint(ctx, backend.PID)
	workers, err := s.procs.ProcessesNamed("node")
	if err != nil {
		return total
	}
	for _, w := range workers {
		if w.PPID != backend.PID || len(w.Args) == 0 || !strings.HasPrefix(w.Args[0], s.internal+"/") {
			continue
		}
		if fp, err := s.procs.Footprint(ctx, w.PID); err == nil {
			total += fp
		}
	}
	return total
}

// apiModel is the part of an lms ps entry headroom reads.
type apiModel struct {
	Type          string `json:"type"`
	ModelKey      string `json:"modelKey"`
	Format        string `json:"format"`
	SizeBytes     uint64 `json:"sizeBytes"`
	ContextLength int    `json:"contextLength"`
	Status        string `json:"status"`
	LastUsedTime  int64  `json:"lastUsedTime"` // epoch ms
	TTLMs         *int64 `json:"ttlMs"`
}

func (m apiModel) toProtocol() protocol.LoadedModel {
	out := protocol.LoadedModel{
		Key:           m.ModelKey,
		Type:          m.Type,
		Format:        m.Format,
		SizeBytes:     m.SizeBytes,
		ContextLength: m.ContextLength,
		Status:        m.Status,
	}
	if m.LastUsedTime > 0 {
		out.LastUsedAt = time.UnixMilli(m.LastUsedTime)
	}
	if m.TTLMs != nil {
		ttl := time.Duration(*m.TTLMs) * time.Millisecond
		out.TTL = &ttl
	}
	return out
}

// appExec reads the app's executable path from app-install-location.json.
func (s *Source) appExec() (string, bool) {
	b, err := os.ReadFile(filepath.Join(s.internal, "app-install-location.json"))
	if err != nil {
		return "", false
	}
	var loc struct{ Path string }
	if json.Unmarshal(b, &loc) != nil || loc.Path == "" {
		return "", false
	}
	return loc.Path, true
}

// backend finds the running backend: the pid in llmster-pid.lock, alive and
// running the app's executable (not a reused pid).
func (s *Source) backend(app string) (vmproc.Process, bool) {
	b, err := os.ReadFile(filepath.Join(s.internal, "llmster-pid.lock"))
	if err != nil {
		return vmproc.Process{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return vmproc.Process{}, false
	}
	procs, err := s.procs.ProcessesNamed(comm(app))
	if err != nil {
		return vmproc.Process{}, false
	}
	for _, p := range procs {
		if p.PID == pid && len(p.Args) > 0 && p.Args[0] == app {
			return p, true
		}
	}
	return vmproc.Process{}, false
}

// comm is the kernel's short process name for an executable: its base name,
// cut to MAXCOMLEN (16) bytes.
func comm(exec string) string {
	c := filepath.Base(exec)
	if len(c) > 16 {
		c = c[:16]
	}
	return c
}

type reading struct{ l protocol.LMStudio }

func (r reading) Apply(s *protocol.Snapshot) {
	l := r.l
	s.LMStudio = &l
}
