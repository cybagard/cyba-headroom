// Package docker collects the containers and VM cost (R1) of each engine
// that speaks the Docker Engine API on a Unix socket: Docker Desktop's, and
// those DOCKER_HOST and the Docker contexts name (#227). It uses net/http directly rather
// than the Docker SDK, which would bloat the binary the shim also runs from.
package docker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// statsWorkers bounds concurrent stats requests: sequential round trips for
// a large compose project would overrun the source timeout under pressure.
const statsWorkers = 8

// Source is the Docker source: every engine that speaks the Docker API on
// a Unix socket (#227).
type Source struct {
	// socket is the engine Inspect, Clock and Events ask, through one:
	// New's. They may run concurrently with each other and with Collect.
	socket     string
	one        *http.Client
	candidates func() []Candidate
	vms        vmproc.Lister
	clients    map[string]*http.Client

	// prevCPU holds each engine's containers' CPU counters from the
	// previous tick, by engine ID. Collect is never called concurrently
	// (daemon.Tick), so no lock.
	prevCPU map[string]map[string]cpuCounters
}

type cpuCounters struct{ container, system uint64 }

// New returns a source for the Engine API at socket alone.
func New(socket string, vms vmproc.Lister) *Source {
	s := Discover(func() []Candidate {
		return Candidates(socket, func(string) string { return "" }, slog.New(slog.DiscardHandler))
	}, vms)
	s.socket, s.one = socket, dial(socket)
	return s
}

// Discover returns a source for the engines on the sockets candidates
// returns, asked afresh each tick: Candidates, for the daemon.
func Discover(candidates func() []Candidate, vms vmproc.Lister) *Source {
	return &Source{candidates: candidates, vms: vms, clients: map[string]*http.Client{}, prevCPU: map[string]map[string]cpuCounters{}}
}

// untrustedBudget bounds the reading of every untrusted candidate, all
// together (#227, amendment 4): however many there are, and however slow,
// they must not hold up the reading.
const untrustedBudget = time.Second

// client returns Collect's HTTP client for socket, made once.
func (s *Source) client(socket string) *http.Client {
	h, ok := s.clients[socket]
	if !ok {
		h = dial(socket)
		s.clients[socket] = h
	}
	return h
}

// dial returns an HTTP client for the engine at socket.
func dial(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

// Name implements daemon.Source.
func (s *Source) Name() string { return "docker" }

// errSeen is a candidate whose engine an earlier candidate reached.
var errSeen = errors.New("docker: engine seen already")

// Collect implements daemon.Source. Each candidate socket answers /info;
// those that answer with one ID are one engine, by the first of them, a
// trusted one before any other. A socket that is missing or refuses is an
// engine that is not running. Any other failure of a trusted one fails the
// reading: the last good one stands, rather than one missing an engine's
// containers. The untrusted ones (a context's: a file anything may write)
// are read after, all at once, within untrustedBudget; one that fails in
// any way, or is not done by then, is an engine not running, with its
// error, so it cannot hide or hold up the others (amendments 3 and 4).
func (s *Source) Collect(ctx context.Context) (daemon.Reading, error) {
	cands := s.candidates()
	type result struct {
		e        protocol.Engine
		counters map[string]cpuCounters
		err      error
	}
	res := make([]result, len(cands))
	seen := map[string]bool{}
	for i, cand := range cands {
		if !cand.Trusted {
			continue
		}
		r := &res[i]
		r.e, r.counters, r.err = s.read(ctx, s.client(cand.Socket), cand, seen)
		switch {
		case r.err == nil:
			seen[cmp.Or(r.e.ID, r.e.Socket)] = true
		case errors.Is(r.err, errSeen):
		case notRunning(r.err):
			r.e, r.err = protocol.Engine{Socket: cand.Socket, Containers: []protocol.Container{}}, nil
		default:
			return nil, r.err
		}
	}
	// seen is only read from here on.
	uctx, cancel := context.WithTimeout(ctx, untrustedBudget)
	var wg sync.WaitGroup
	for i, cand := range cands {
		if cand.Trusted {
			continue
		}
		c := s.client(cand.Socket)
		wg.Go(func() {
			r := &res[i]
			r.e, r.counters, r.err = s.read(uctx, c, cand, seen)
			if r.err != nil && !errors.Is(r.err, errSeen) {
				r.e = protocol.Engine{Socket: cand.Socket, Error: r.err.Error(), Containers: []protocol.Container{}}
			}
		})
	}
	wg.Wait()
	cancel()

	engines := []protocol.Engine{}
	cpu := map[string]map[string]cpuCounters{}
	for i, cand := range cands {
		r := res[i]
		if errors.Is(r.err, errSeen) {
			continue
		}
		if r.e.Running {
			id := cmp.Or(r.e.ID, r.e.Socket)
			if !cand.Trusted && cpu[id] != nil {
				continue // an earlier untrusted one reached it
			}
			cpu[id] = r.counters
		}
		engines = append(engines, r.e)
	}
	s.prevCPU = cpu // engines and containers that are gone drop out
	s.vmCost(ctx, engines)
	return reading{engines}, nil
}

// read reads the engine c reaches at cand's socket, unless an engine in
// seen answers there.
func (s *Source) read(ctx context.Context, c *http.Client, cand Candidate, seen map[string]bool) (protocol.Engine, map[string]cpuCounters, error) {
	socket := cand.Socket
	var info struct {
		ID       string
		MemTotal uint64
	}
	if err := get(ctx, c, "/info", &info); err != nil {
		return protocol.Engine{}, nil, err
	}
	var version apiVersion
	if err := get(ctx, c, "/version", &version); err != nil {
		return protocol.Engine{}, nil, err
	}
	kind := version.kind()
	id := info.ID
	if kind == protocol.EnginePodman {
		id = podmanID(cand)
	}
	key := cmp.Or(id, socket)
	if seen[key] {
		return protocol.Engine{}, nil, errSeen
	}
	e := protocol.Engine{Kind: kind, Socket: socket, ID: id, Machine: cand.Machine, Running: true, VMLimitBytes: info.MemTotal}
	var counters map[string]cpuCounters
	var err error
	e.Containers, counters, err = containers(ctx, c, s.prevCPU[key])
	return e, counters, err
}

// podmanID is a Podman engine's ID: Podman's /info answers with a new
// random one on every request (#228, plan amendment 2). It is
// podman:<machine> for a machine's socket, from podman machine inspect,
// else its socket's resolved path.
func podmanID(cand Candidate) string {
	if cand.Machine != "" {
		return "podman:" + cand.Machine
	}
	if p, err := filepath.EvalSymlinks(cand.Socket); err == nil {
		return p
	}
	return cand.Socket
}

// vmCost fills in each running engine's VM. Docker Desktop's VM process
// is known (vmproc), and so is a Podman machine's, by its name (#228);
// another engine's is not, so its cost is unknown, not zero. A VM read
// failure leaves the containers valid; it is reported alongside.
func (s *Source) vmCost(ctx context.Context, engines []protocol.Engine) {
	var known []*protocol.Engine
	desktop := false
	for i := range engines {
		e := &engines[i]
		switch {
		case !e.Running:
		case e.Kind == protocol.EngineDockerDesktop && !desktop:
			desktop = true
			known = append(known, e)
		case e.Machine != "":
			known = append(known, e)
		default:
			e.VMError = "the VM of a " + e.Kind + " engine is not known"
		}
	}
	if len(known) == 0 {
		return
	}
	vms, err := s.vms.List(ctx)
	for _, e := range known {
		if err != nil {
			e.VMError = err.Error()
		}
		for _, vm := range vms {
			if e.Machine == "" && vm.Kind == vmproc.Docker || e.Machine != "" && vm.Kind == vmproc.Podman && vm.Name == e.Machine {
				e.VMRunning = true
				e.VMFootprintBytes = vm.FootprintBytes
			}
		}
	}
}

// apiVersion is the part of GET /version headroom reads.
type apiVersion struct {
	Platform   struct{ Name string }
	Components []struct{ Name string }
}

// kind tells the engine apart (spike #49, P3): Podman names itself among
// the components, Docker Desktop in the platform's name.
func (v apiVersion) kind() string {
	for _, c := range v.Components {
		if c.Name == "Podman Engine" {
			return protocol.EnginePodman
		}
	}
	if strings.HasPrefix(v.Platform.Name, "Docker Desktop") {
		return protocol.EngineDockerDesktop
	}
	return protocol.EngineDocker
}

// containers lists an engine's running containers with their memory, and
// CPU since prev; it returns their counters for the next tick.
func containers(ctx context.Context, c *http.Client, prev map[string]cpuCounters) ([]protocol.Container, map[string]cpuCounters, error) {
	var list []apiContainer
	if err := get(ctx, c, "/containers/json", &list); err != nil {
		return nil, nil, err
	}
	stats, err := allStats(ctx, c, list)
	if err != nil {
		return nil, nil, err
	}
	out := []protocol.Container{}
	cpu := make(map[string]cpuCounters, len(list))
	for i, ct := range list {
		st := stats[i]
		if st == nil {
			continue // exited or being removed since the list call
		}
		cur := cpuCounters{st.CPUStats.CPUUsage.TotalUsage, st.CPUStats.SystemCPUUsage}
		cpu[ct.ID] = cur
		var pct *float64
		if p, ok := prev[ct.ID]; ok {
			pct = cpuPercent(p, cur, st.CPUStats.OnlineCPUs)
		}
		out = append(out, protocol.Container{
			ID:          ct.ID,
			Name:        strings.TrimPrefix(firstOr(ct.Names, ""), "/"),
			Image:       ct.Image,
			Labels:      ct.Labels,
			Mounts:      ct.bindSources(),
			MemoryBytes: st.memory(),
			CPUPercent:  pct,
		})
	}
	return out, cpu, nil
}

// allStats fetches each container's one-shot stats concurrently. A container
// whose stats fail (exited, being removed, restarting) gets nil and is left
// out; only when every one fails is that an error, since the engine itself is
// then likely broken.
func allStats(ctx context.Context, c *http.Client, list []apiContainer) ([]*apiStats, error) {
	out := make([]*apiStats, len(list))
	errs := make([]error, len(list))
	sem := make(chan struct{}, statsWorkers)
	var wg sync.WaitGroup
	for i, ct := range list {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var st apiStats
			if errs[i] = get(ctx, c, "/containers/"+ct.ID+"/stats?stream=false&one-shot=true", &st); errs[i] == nil {
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

// get decodes the answer to GET path on the engine c reaches into v.
func get(ctx context.Context, c *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(path, "/containers/") {
		// Docker's own answer, not any proxy's 404.
		var e struct{ Message string }
		if json.NewDecoder(resp.Body).Decode(&e) == nil && strings.Contains(strings.ToLower(e.Message), "no such container") {
			return fmt.Errorf("docker: GET %s: %w", path, ErrNoSuchContainer)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker: GET %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("docker: GET %s: %w", path, err)
	}
	return nil
}

type reading struct{ engines []protocol.Engine }

func (r reading) Apply(s *protocol.Snapshot) { s.Engines = r.engines }

// ErrNoSuchContainer is Docker's word that a container does not exist: a
// start of it starts nothing.
var ErrNoSuchContainer = errors.New("no such container")

// containerRef is a container name or ID as Docker forms them: only such a
// reference that Docker does not find is known missing.
var containerRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Inspect resolves a container name, ID or ID prefix to its full ID, its
// labels and whether it runs, as Docker resolves the target of a docker
// start (#33). ref is taken as given: the caller trims it as the docker CLI
// does, and anything but a plain name or ID is an error that is not
// ErrNoSuchContainer.
func (s *Source) Inspect(ctx context.Context, ref string) (string, map[string]string, bool, error) {
	if !containerRef.MatchString(ref) {
		// Unknown, not missing: Docker may read it otherwise, and a start
		// of it costs.
		return "", nil, false, fmt.Errorf("docker: not a container reference: %q", ref)
	}
	var c struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string
		}
		State struct {
			Running bool
		}
	}
	if s.socket == "" {
		return "", nil, false, errors.New("docker: socket unknown")
	}
	if err := get(ctx, s.one, "/containers/"+ref+"/json", &c); err != nil {
		return "", nil, false, err
	}
	return c.ID, c.Config.Labels, c.State.Running, nil
}

// Clock is the time on the Docker VM's clock, in Unix nanoseconds: /info's
// SystemTime, the clock Docker dates its events by (#170).
func (s *Source) Clock(ctx context.Context) (int64, error) {
	if s.socket == "" {
		return 0, errors.New("docker: socket unknown")
	}
	var info struct{ SystemTime string }
	if err := get(ctx, s.one, "/info", &info); err != nil {
		return 0, err
	}
	t, err := time.Parse(time.RFC3339Nano, info.SystemTime)
	if err != nil {
		return 0, fmt.Errorf("docker: /info SystemTime: %w", err)
	}
	return t.UnixNano(), nil
}

// EventsPath streams container starts, stops and exits (#67).
var EventsPath = "/events?" + url.Values{"filters": {`{"event":["start","stop","kill","die"],"type":["container"]}`}}.Encode()

// ErrBadSince is Docker refusing an events stream's since: the caller asks
// again without it (#146).
var ErrBadSince = errors.New("docker: events: since refused")

// Events streams Docker's container start and die events to fn, with the
// container's ID, the event's time in Unix nanoseconds (0 if Docker gave
// none) and the container's attributes (its labels, and name), until ctx
// ends or the stream drops: it always returns an error, and the caller
// reconnects. Events are what a 5 s reading misses: a container that lives
// between two readings. With since > 0, Docker first replays the events
// from since on, that one included, from the few it keeps (#146). With
// until > 0 as well, the stream ends after the events up to until, that
// one included, and a stream that ends so returns nil (#170).
func (s *Source) Events(ctx context.Context, since, until int64, fn func(action, id string, timeNano int64, attrs map[string]string)) error {
	if s.socket == "" {
		return errors.New("docker: socket unknown")
	}
	path := EventsPath
	if since > 0 {
		path += "&since=" + unixNano(since)
	}
	if until > 0 {
		path += "&until=" + unixNano(until)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.one.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadRequest && since > 0 {
		return fmt.Errorf("%w: %s", ErrBadSince, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker: GET /events: %s", resp.Status)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Type   string
			Action string
			Actor  struct {
				ID         string
				Attributes map[string]string
			}
			Time     int64 `json:"time"`
			TimeNano int64 `json:"timeNano"`
		}
		if err := dec.Decode(&ev); err != nil {
			if until > 0 && errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker: events: %w", err)
		}
		if ev.Type == "container" && ev.Actor.ID != "" && (ev.Action == "start" || ev.Action == "stop" || ev.Action == "kill" || ev.Action == "die") {
			fn(ev.Action, ev.Actor.ID, cmp.Or(ev.TimeNano, ev.Time*1e9), ev.Actor.Attributes)
		}
	}
}

// unixNano is a Unix time in ns as Docker reads one: seconds, and nine
// digits of nanoseconds.
func unixNano(ns int64) string { return fmt.Sprintf("%d.%09d", ns/1e9, ns%1e9) }
