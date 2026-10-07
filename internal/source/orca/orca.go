// Package orca collects Orca's worktrees, agent states and the agents' own
// memory and CPU (R1) from the orca CLI.
//
// The CLI output carries user content (prompts, tool input, assistant
// messages, terminal previews). Only the typed fields below are decoded, so
// that content never reaches the snapshot or logs.
package orca

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// CLI runs the orca binary.
type CLI interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Source is the Orca source.
type Source struct{ cli CLI }

// New returns a source. A nil cli means Orca is not installed.
func New(cli CLI) *Source { return &Source{cli: cli} }

// Name implements daemon.Source.
func (s *Source) Name() string { return "orca" }

// Collect implements daemon.Source.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	o := protocol.Orca{Worktrees: []protocol.Worktree{}}
	if s.cli == nil {
		return reading{o}, nil
	}
	o.Installed = true

	var ps struct {
		Worktrees []apiWorktree `json:"worktrees"`
	}
	running, err := s.call(ctx, &ps, "worktree", "ps", "--json")
	if err != nil || !running {
		return reading{o}, err
	}
	o.Running = true

	// Memory is optional: on failure keep the worktrees and say why.
	var mem apiMemory
	if _, err := s.call(ctx, &mem, "diagnostics", "memory", "--json"); err != nil {
		o.MemoryError = err.Error()
	}
	o.AppMemoryBytes = mem.App.Memory
	byID := map[string]apiMemoryWorktree{}
	for _, m := range mem.Worktrees {
		byID[m.WorktreeID] = m
	}

	for _, w := range ps.Worktrees {
		if w.IsArchived || w.HostID != "local" {
			continue // remote runtimes are #43
		}
		pw := w.toProtocol()
		if m, ok := byID[w.WorktreeID]; ok {
			pw.MemoryBytes, pw.CPUPercent = m.Memory, m.CPU
			for _, sess := range m.Sessions {
				pw.SessionPIDs = append(pw.SessionPIDs, sess.PID)
			}
		}
		o.Worktrees = append(o.Worktrees, pw)
	}
	return reading{o}, nil
}

// apiMemory is the part of diagnostics memory headroom reads.
type apiMemory struct {
	App struct {
		Memory uint64 `json:"memory"`
	} `json:"app"`
	Worktrees []apiMemoryWorktree `json:"worktrees"`
}

type apiMemoryWorktree struct {
	WorktreeID string  `json:"worktreeId"`
	Memory     uint64  `json:"memory"`
	CPU        float64 `json:"cpu"`
	Sessions   []struct {
		PID int `json:"pid"`
	} `json:"sessions"`
}

// apiWorktree is the part of a worktree ps row headroom reads. Fields with
// user content (prompts, tool input, previews) are deliberately absent.
type apiWorktree struct {
	WorktreeID        string     `json:"worktreeId"`
	Path              string     `json:"path"`
	DisplayName       string     `json:"displayName"`
	Branch            string     `json:"branch"`
	Status            string     `json:"status"`
	HostID            string     `json:"hostId"`
	IsArchived        bool       `json:"isArchived"`
	LiveTerminalCount int        `json:"liveTerminalCount"`
	LastActivityAt    int64      `json:"lastActivityAt"` // epoch ms
	Agents            []apiAgent `json:"agents"`
}

type apiAgent struct {
	PaneKey        string `json:"paneKey"`
	AgentType      string `json:"agentType"`
	State          string `json:"state"`
	StateStartedAt int64  `json:"stateStartedAt"` // epoch ms
}

func (w apiWorktree) toProtocol() protocol.Worktree {
	out := protocol.Worktree{
		ID:             w.WorktreeID,
		Path:           w.Path,
		Name:           w.DisplayName,
		Branch:         strings.TrimPrefix(w.Branch, "refs/heads/"),
		Status:         w.Status,
		LiveTerminals:  w.LiveTerminalCount,
		LastActivityAt: millis(w.LastActivityAt),
		Agents:         []protocol.Agent{},
	}
	for _, a := range w.Agents {
		out.Agents = append(out.Agents, protocol.Agent{
			PaneKey:    a.PaneKey,
			Type:       a.AgentType,
			State:      a.State,
			StateSince: millis(a.StateStartedAt),
		})
	}
	return out
}

func millis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// envelope is the CLI's reply wrapper.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call runs an orca command and decodes its result into v. It reports
// running=false, without error, when the Orca app is not running.
func (s *Source) call(ctx context.Context, v any, args ...string) (running bool, err error) {
	out, runErr := s.cli.Run(ctx, args...)
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		if runErr != nil {
			return false, fmt.Errorf("orca %s %s: %w", args[0], args[1], runErr)
		}
		return false, fmt.Errorf("orca %s %s: %w", args[0], args[1], err)
	}
	if !env.OK {
		if env.Error != nil && env.Error.Code == "runtime_unavailable" {
			return false, nil
		}
		return false, fmt.Errorf("orca %s %s: %s", args[0], args[1], errMessage(env))
	}
	if err := json.Unmarshal(env.Result, v); err != nil {
		return false, fmt.Errorf("orca %s %s: %w", args[0], args[1], err)
	}
	return true, nil
}

func errMessage(e envelope) string {
	if e.Error == nil {
		return "not ok"
	}
	return e.Error.Code + ": " + e.Error.Message
}

type reading struct{ o protocol.Orca }

func (r reading) Apply(s *protocol.Snapshot) {
	o := r.o
	s.Orca = &o
}
