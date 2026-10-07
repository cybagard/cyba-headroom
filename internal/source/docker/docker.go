// Package docker collects Docker Desktop's containers and VM cost (R1) from
// the Docker Engine API on its Unix socket. It uses net/http directly rather
// than the Docker SDK, which would bloat the binary the shim also runs from.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// statsWorkers bounds concurrent stats requests: sequential round trips for
// a large compose project would overrun the source timeout under pressure.
const statsWorkers = 8

// Source is the Docker source.
type Source struct {
	socket string
	http   *http.Client
	vms    vmproc.Lister

	// prevCPU holds each container's CPU counters from the previous tick.
	// Collect is never called concurrently (daemon.Tick), so no lock.
	prevCPU map[string]cpuCounters
}

type cpuCounters struct{ container, system uint64 }

// New returns a source for the Engine API at socket.
func New(socket string, vms vmproc.Lister) *Source {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Source{socket: socket, http: &http.Client{Transport: tr}, vms: vms, prevCPU: map[string]cpuCounters{}}
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "docker" }

// Collect implements daemon.Source.
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	if s.socket == "" {
		return nil, errors.New("docker: socket unknown (no DOCKER_HOST or HOME); set [docker] socket")
	}
	var info struct{ MemTotal uint64 }
	if err := s.get(ctx, "/info", &info); err != nil {
		if notRunning(err) {
			return reading{protocol.Docker{Containers: []protocol.Container{}}}, nil
		}
		return nil, err
	}
	d := protocol.Docker{Running: true, VMLimitBytes: info.MemTotal, Containers: []protocol.Container{}}

	var list []apiContainer
	if err := s.get(ctx, "/containers/json", &list); err != nil {
		return nil, err
	}
	stats, err := s.allStats(ctx, list)
	if err != nil {
		return nil, err
	}
	cpu := make(map[string]cpuCounters, len(list))
	for i, c := range list {
		st := stats[i]
		if st == nil {
			continue // exited or being removed since the list call
		}
		cur := cpuCounters{st.CPUStats.CPUUsage.TotalUsage, st.CPUStats.SystemCPUUsage}
		cpu[c.ID] = cur
		var pct *float64
		if prev, ok := s.prevCPU[c.ID]; ok {
			pct = cpuPercent(prev, cur, st.CPUStats.OnlineCPUs)
		}
		d.Containers = append(d.Containers, protocol.Container{
			ID:          c.ID,
			Name:        strings.TrimPrefix(firstOr(c.Names, ""), "/"),
			Image:       c.Image,
			Labels:      c.Labels,
			Mounts:      c.bindSources(),
			MemoryBytes: st.memory(),
			CPUPercent:  pct,
		})
	}
	s.prevCPU = cpu // containers that are gone drop out

	// A VM read failure leaves the containers valid; report it alongside.
	vms, err := s.vms.List(ctx)
	if err != nil {
		d.VMError = err.Error()
	}
	for _, vm := range vms {
		if vm.Kind == vmproc.Docker {
			d.VMRunning = true
			d.VMFootprintBytes = vm.FootprintBytes
		}
	}
	return reading{d}, nil
}

// allStats fetches each container's one-shot stats concurrently. A container
// whose stats fail (exited, being removed, restarting) gets nil and is left
// out; only when every one fails is that an error, since the engine itself is
// then likely broken.
func (s *Source) allStats(ctx context.Context, list []apiContainer) ([]*apiStats, error) {
	out := make([]*apiStats, len(list))
	errs := make([]error, len(list))
	sem := make(chan struct{}, statsWorkers)
	var wg sync.WaitGroup
	for i, c := range list {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var st apiStats
			if errs[i] = s.get(ctx, "/containers/"+c.ID+"/stats?stream=false&one-shot=true", &st); errs[i] == nil {
				out[i] = &st
			}
		})
	}
	wg.Wait()
	if len(list) > 0 && !slices.ContainsFunc(out, func(st *apiStats) bool { return st != nil }) {
		return nil, fmt.Errorf("docker: stats for all %d containers failed: %w", len(list), errs[0])
	}
	return out, nil
}

// apiContainer is the part of GET /containers/json headroom reads.
type apiContainer struct {
	ID     string `json:"Id"`
	Names  []string
	Image  string
	Labels map[string]string
	Mounts []struct {
		Type   string
		Source string
	}
}

// bindSources returns the host paths of bind mounts. The API reports Docker
// Desktop binds as paths inside its VM (/host_mnt/...); Desktop records the
// path the user gave in desktop.docker.io/binds/<n>/Source labels, so those
// win, and /host_mnt is stripped otherwise.
func (c apiContainer) bindSources() []string {
	var out []string
	for i := 0; ; i++ {
		src, ok := c.Labels[fmt.Sprintf("desktop.docker.io/binds/%d/Source", i)]
		if !ok {
			break
		}
		out = append(out, src)
	}
	if len(out) > 0 {
		return out
	}
	for _, m := range c.Mounts {
		if m.Type == "bind" {
			out = append(out, strings.TrimPrefix(m.Source, "/host_mnt"))
		}
	}
	return out
}

// apiStats is the part of a one-shot stats response headroom reads.
type apiStats struct {
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Stats struct {
			InactiveFile uint64 `json:"inactive_file"`
		} `json:"stats"`
	} `json:"memory_stats"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
}

// cpuPercent is docker stats' formula over two samples: container CPU time
// as a share of host CPU time, times the core count (100 = one core).
func cpuPercent(prev, cur cpuCounters, cpus uint32) *float64 {
	p := 0.0
	if cur.container >= prev.container && cur.system > prev.system {
		p = float64(cur.container-prev.container) / float64(cur.system-prev.system) * float64(cpus) * 100
	}
	return &p
}

// memory is usage minus inactive file cache: what docker stats shows.
func (s apiStats) memory() uint64 {
	m := s.MemoryStats
	if m.Stats.InactiveFile > m.Usage {
		return 0
	}
	return m.Usage - m.Stats.InactiveFile
}

func firstOr(xs []string, def string) string {
	if len(xs) == 0 {
		return def
	}
	return xs[0]
}

// notRunning reports a socket that is missing or not accepting: Docker
// Desktop is quit, which is a state to report, not a failure.
func notRunning(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func (s *Source) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker: GET %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("docker: GET %s: %w", path, err)
	}
	return nil
}

type reading struct{ d protocol.Docker }

func (r reading) Apply(s *protocol.Snapshot) {
	d := r.d
	s.Docker = &d
}
