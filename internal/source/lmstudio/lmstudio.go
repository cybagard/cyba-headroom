// Package lmstudio collects LM Studio's loaded models and what they cost the
// host (R1).
//
// LM Studio reports only a model's file size. The real cost is the
// footprint of its process tree, where each loaded model has a worker. The
// lms CLI wakes LM Studio when it is not running, so it is only called once
// the backend is known to be running: the pid in
// ~/.lmstudio/.internal/llmster-pid.lock must be alive and run the app's
// executable from app-install-location.json. LM Studio quitting between that
// check and the lms call (milliseconds) can still wake it.
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
	Tree(root int) ([]vmproc.Process, error)
	Footprints(ctx context.Context, pids ...int) (uint64, error)
}

// Source is the LM Studio source.
type Source struct {
	cli   CLI
	procs Procs
	// dir is ~/.lmstudio; empty when HOME is unset or relative, which would
	// resolve against the daemon's cwd.
	dir string
}

// New returns a source. A nil cli means lms was not found.
func New(cli CLI, procs Procs, home string) *Source {
	s := &Source{cli: cli, procs: procs}
	if filepath.IsAbs(home) {
		s.dir = filepath.Join(home, ".lmstudio")
	}
	return s
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "lmstudio" }

// Collect implements daemon.Source.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	l := protocol.LMStudio{Models: []protocol.LoadedModel{}}
	app, installed := s.appExec()
	if s.cli == nil || s.dir == "" || !installed {
		return reading{l}, nil
	}
	l.Installed = true
	tree, ok := s.backend(app)
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
	// The whole tree under the backend: Electron helpers, model workers and
	// any engine processes they start, in one footprint run.
	pids := make([]int, len(tree))
	for i, p := range tree {
		pids[i] = p.PID
	}
	if fp, err := s.procs.Footprints(ctx, pids...); err != nil {
		l.FootprintError = err.Error()
	} else {
		l.FootprintBytes = &fp
	}
	return reading{l}, nil
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
		t := time.UnixMilli(m.LastUsedTime).UTC()
		out.LastUsedAt = &t
	}
	// A TTL of 0 or less unloads nothing: the model stays loaded, as with none.
	if m.TTLMs != nil && *m.TTLMs > 0 {
		ttl := time.Duration(*m.TTLMs) * time.Millisecond
		out.TTL = &ttl
	}
	return out
}

// appExec reads the app's executable path from app-install-location.json.
func (s *Source) appExec() (string, bool) {
	if s.dir == "" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(s.dir, ".internal", "app-install-location.json"))
	if err != nil {
		return "", false
	}
	var loc struct{ Path string }
	if json.Unmarshal(b, &loc) != nil || loc.Path == "" {
		return "", false
	}
	return loc.Path, true
}

// backend finds the running backend and its process tree: the pid in
// llmster-pid.lock must be alive and run the app's executable, or one under
// ~/.lmstudio (the headless llmster daemon); a reused pid fails this.
func (s *Source) backend(app string) ([]vmproc.Process, bool) {
	b, err := os.ReadFile(filepath.Join(s.dir, ".internal", "llmster-pid.lock"))
	if err != nil {
		return nil, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, false
	}
	tree, err := s.procs.Tree(pid)
	if err != nil || len(tree) == 0 {
		return nil, false
	}
	// The kernel keeps the path as invoked, so compare resolved paths too.
	exec := tree[0].Exec
	isApp := exec == app || resolve(exec) == resolve(app)
	underHome := strings.HasPrefix(exec, s.dir+"/") || strings.HasPrefix(resolve(exec), resolve(s.dir)+"/")
	if !isApp && !underHome {
		return nil, false
	}
	return tree, true
}

// resolve returns p with symlinks resolved, or p if that fails.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

type reading struct{ l protocol.LMStudio }

func (r reading) Apply(s *protocol.Snapshot) {
	l := r.l
	s.LMStudio = &l
}
