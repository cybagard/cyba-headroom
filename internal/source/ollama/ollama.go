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
	installed func() bool
}

// New returns a source. installed is asked on each tick with no server
// running, so an install while the daemon runs shows up.
func New(api API, procs Procs, installed func() bool) *Source {
	return &Source{api: api, procs: procs, installed: installed}
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "ollama" }

// Collect implements daemon.Source.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	o := protocol.Ollama{Models: []protocol.OllamaModel{}}
	pids, err := s.servers()
	if err != nil {
		return nil, fmt.Errorf("ollama processes: %w", err)
	}
	if len(pids) == 0 {
		o.Installed = s.installed()
		return reading{o}, nil
	}
	o.Installed, o.Running = true, true

	// The footprint run and the API read are independent: run them side by
	// side, so together they fit the source timeout. Without the model list
	// the footprint still counts: the budget reserves it, so a server on a
	// port headroom was not told about is not free.
	type ps struct {
		models []protocol.OllamaModel
		err    error
	}
	read := make(chan ps, 1)
	go func() {
		m, err := s.models(ctx)
		read <- ps{m, err}
	}()
	if fp, err := s.procs.Footprints(ctx, pids...); err != nil {
		o.FootprintError = err.Error()
	} else {
		o.FootprintBytes = &fp
	}
	r := <-read
	models, err := r.models, r.err
	if err != nil {
		o.Models, o.ModelsError = nil, err.Error()
	} else {
		o.Models = models
	}
	return reading{o}, nil
}

func (s *Source) models(ctx context.Context) ([]protocol.OllamaModel, error) {
	out, err := s.api.PS(ctx)
	if err != nil {
		return nil, fmt.Errorf("/api/ps: %w", err)
	}
	var ps struct{ Models []apiModel }
	if err := json.Unmarshal(out, &ps); err != nil {
		return nil, fmt.Errorf("/api/ps: %w", err)
	}
	now := time.Now()
	models := []protocol.OllamaModel{}
	for _, m := range ps.Models {
		models = append(models, m.toProtocol(now))
	}
	return models, nil
}

// servers returns the pids of every running `ollama serve` and the
// processes under it: one runner per loaded model. A server that exits
// before its tree is read is not running.
func (s *Source) servers() ([]int, error) {
	procs, err := s.procs.ProcessesNamed("ollama")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		if len(p.Args) < 2 || p.Args[1] != "serve" {
			continue
		}
		tree, err := s.procs.Tree(p.PID)
		if err != nil {
			continue
		}
		for _, q := range tree {
			pids = append(pids, q.PID)
		}
	}
	return pids, nil
}

// Unusable is the API of an Ollama host headroom does not ask (see
// Endpoint): every read fails with Err.
type Unusable struct{ Err error }

// PS implements API.
func (u Unusable) PS(context.Context) ([]byte, error) { return nil, u.Err }

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
