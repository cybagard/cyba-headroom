// Package ollama collects Ollama's loaded models and what they cost the host
// (R1).
//
// Ollama reports each loaded model's size in memory. The real cost is the
// footprint of the server's process tree, where each loaded model has a
// runner. headroom only reads GET /api/ps, and only once an `ollama serve`
// process is running, so it never starts, pulls or loads anything.
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// API reads Ollama's /api/ps.
type API interface {
	PS(ctx context.Context) ([]byte, error)
}

// Procs is the process access the source needs; vmproc.Host implements it.
type Procs interface {
	ProcessesNamed(comm string) ([]vmproc.Process, error)
	Tree(root int) ([]vmproc.Process, error)
	Footprints(ctx context.Context, pids ...int) (uint64, error)
}

// Source is the Ollama source.
type Source struct {
	api       API
	procs     Procs
	installed bool
}

// New returns a source. A nil api means the configured endpoint is not on
// this Mac, so the model list stays unknown.
func New(api API, procs Procs, installed bool) *Source {
	return &Source{api: api, procs: procs, installed: installed}
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "ollama" }

// Collect implements daemon.Source.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	o := protocol.Ollama{Installed: s.installed, Models: []protocol.OllamaModel{}}
	tree, err := s.server()
	if err != nil {
		return nil, fmt.Errorf("ollama processes: %w", err)
	}
	if tree == nil {
		return reading{o}, nil
	}
	o.Installed, o.Running = true, true

	// The whole tree under the server: one runner per loaded model.
	pids := make([]int, len(tree))
	for i, p := range tree {
		pids[i] = p.PID
	}
	if fp, err := s.procs.Footprints(ctx, pids...); err != nil {
		o.FootprintError = err.Error()
	} else {
		o.FootprintBytes = &fp
	}

	if s.api == nil {
		o.Models = nil
		o.ModelsError = "the Ollama endpoint is not on this Mac"
		return reading{o}, nil
	}
	out, err := s.api.PS(ctx)
	if err != nil {
		return nil, fmt.Errorf("ollama /api/ps: %w", err)
	}
	var ps struct{ Models []apiModel }
	if err := json.Unmarshal(out, &ps); err != nil {
		return nil, fmt.Errorf("ollama /api/ps: %w", err)
	}
	now := time.Now()
	for _, m := range ps.Models {
		o.Models = append(o.Models, m.toProtocol(now))
	}
	return reading{o}, nil
}

// server returns the process tree of the running `ollama serve`, or nil.
func (s *Source) server() ([]vmproc.Process, error) {
	procs, err := s.procs.ProcessesNamed("ollama")
	if err != nil {
		return nil, err
	}
	for _, p := range procs {
		if len(p.Args) > 1 && p.Args[1] == "serve" {
			return s.procs.Tree(p.PID)
		}
	}
	return nil, nil
}

// apiModel is the part of an /api/ps entry headroom reads.
type apiModel struct {
	Name          string    `json:"name"`
	Size          uint64    `json:"size"`
	SizeVRAM      uint64    `json:"size_vram"`
	ContextLength int       `json:"context_length"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// forever is how far ahead an expiry means "never": for keep_alive < 0
// Ollama reports now plus the largest duration, about 292 years.
const forever = 100 * 365 * 24 * time.Hour

func (m apiModel) toProtocol(now time.Time) protocol.OllamaModel {
	out := protocol.OllamaModel{
		Name:          m.Name,
		SizeBytes:     m.Size,
		VRAMBytes:     m.SizeVRAM,
		ContextLength: m.ContextLength,
	}
	if !m.ExpiresAt.IsZero() && m.ExpiresAt.Sub(now) < forever {
		t := m.ExpiresAt.UTC()
		out.ExpiresAt = &t
	}
	return out
}

type reading struct{ o protocol.Ollama }

func (r reading) Apply(s *protocol.Snapshot) {
	o := r.o
	s.Ollama = &o
}
