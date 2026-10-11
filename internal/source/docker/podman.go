package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// Podman's engine socket is known only from podman machine inspect, in a
// per-user temp dir (spike #49, P2). A stopped machine's socket is gone, so
// inspect is asked again when the socket stops answering, but no more often
// than podmanRetry; each run of the CLI has podmanTimeout.
const (
	podmanRetry   = 30 * time.Second
	podmanTimeout = 2 * time.Second
)

// PodmanCLI runs the podman CLI: binpath.Exec.
type PodmanCLI interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Podman finds the socket of Podman's engine (#228): that of the default
// machine, else of the first running one. It is cached, and inspect is run
// again only when that socket stops answering, at most once every
// podmanRetry. Candidates is called only from Collect, never concurrently.
type Podman struct {
	cli PodmanCLI // nil: no podman CLI
	now func() time.Time
	log *slog.Logger

	cand   Candidate // Socket empty: none
	asked  time.Time // when inspect last ran
	ran    bool
	logged string // why there is no candidate, as last logged
}

// NewPodman returns a finder that runs cli, nil when there is no podman
// CLI.
func NewPodman(cli PodmanCLI, now func() time.Time, log *slog.Logger) *Podman {
	return &Podman{cli: cli, now: now, log: log}
}

// Candidates returns Podman's socket, a trusted candidate of its machine,
// or none. A socket that is known but dead is still returned: Candidates
// leaves it out if it is missing, and Collect reads one that refuses as an
// engine not running.
func (p *Podman) Candidates() []Candidate {
	if p.cli == nil {
		p.note("no podman CLI: Podman's engine is not looked for")
		return nil
	}
	if (p.cand.Socket == "" || !answers(p.cand.Socket)) && (!p.ran || p.now().Sub(p.asked) >= podmanRetry) {
		p.ran, p.asked = true, p.now()
		c, err := p.inspect()
		switch {
		case err == nil:
			p.cand = c
			p.note("")
		case errors.Is(err, errNoMachine):
			p.cand = Candidate{}
			p.note("no Podman machine: Podman's engine is not looked for")
		default:
			// The CLI failing says nothing of the socket: keep the last.
			p.note(err.Error())
		}
	}
	if p.cand.Socket == "" {
		return nil
	}
	return []Candidate{p.cand}
}

// note logs why there is no candidate when that changes, once, and nothing
// when there is one again.
func (p *Podman) note(why string) {
	if why != "" && why != p.logged {
		p.log.Info(why)
	}
	p.logged = why
}

// errNoMachine is no machine to read: none, or none default and none
// running.
var errNoMachine = errors.New("podman: no machine")

// inspect reads the socket from podman machine inspect, of the machine
// podman machine list names default, else the first running one.
func (p *Podman) inspect() (Candidate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), podmanTimeout)
	defer cancel()
	out, err := p.cli.Run(ctx, "machine", "list", "--format", "json")
	if err != nil {
		return Candidate{}, fmt.Errorf("podman machine list: %w", err)
	}
	var list []struct {
		Name             string
		Default, Running bool
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return Candidate{}, fmt.Errorf("podman machine list: %w", err)
	}
	name := ""
	for _, m := range list {
		if m.Default {
			name = m.Name
			break
		}
		if name == "" && m.Running {
			name = m.Name
		}
	}
	if name == "" {
		return Candidate{}, errNoMachine
	}
	out, err = p.cli.Run(ctx, "machine", "inspect", name)
	if err != nil {
		return Candidate{}, fmt.Errorf("podman machine inspect %s: %w", name, err)
	}
	var machines []struct {
		Name           string
		ConnectionInfo struct {
			PodmanSocket *struct{ Path string }
		}
	}
	if err := json.Unmarshal(out, &machines); err != nil {
		return Candidate{}, fmt.Errorf("podman machine inspect %s: %w", name, err)
	}
	if len(machines) != 1 || machines[0].ConnectionInfo.PodmanSocket == nil || machines[0].ConnectionInfo.PodmanSocket.Path == "" {
		return Candidate{}, fmt.Errorf("podman machine inspect %s: no socket", name)
	}
	return Candidate{Socket: machines[0].ConnectionInfo.PodmanSocket.Path, Trusted: true, Machine: name}, nil
}

// answers reports whether something accepts on socket: not missing, not
// refused.
func answers(socket string) bool {
	c, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return !notRunning(err)
	}
	_ = c.Close()
	return true
}
