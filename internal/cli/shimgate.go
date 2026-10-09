package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/shim"
)

const (
	// shimCheckedVar marks a checked call with the shim's PID, which the
	// real binary keeps. A binary that replaces itself with another shimmed
	// one (podman-docker's `exec podman`) has the same PID and is not
	// checked, and leased, twice; a child it starts has another PID and is.
	shimCheckedVar = "HEADROOM_SHIM_CHECKED"
	// waitPoll is how often a waiting call (BUDGET_WAIT=1) asks again.
	waitPoll = 2 * time.Second
)

// errCannotCheck means the daemon answered but gave no decision; a
// daemonError carries what it said.
var errCannotCheck = errors.New("the daemon cannot check calls")

// daemonError is a daemon's answer without a decision.
type daemonError struct{ said string }

func (e daemonError) Error() string { return errCannotCheck.Error() + " (" + e.said + ")" }
func (e daemonError) Unwrap() error { return errCannotCheck }

// olderDaemon is the answer of a daemon built before the check (#24).
const olderDaemon = `unknown op "check"`

// gated is how gate decided.
type gated struct {
	proceed bool
	code    int       // the exit code when not proceeding
	signal  os.Signal // the signal that ended a wait, to die of
	checked bool      // a daemon allowed the call: mark it for later shims
	lease   string    // the allow's lease, to release if the exec fails
	cfg     config.Config
}

// withDefaults is e with each unset test hook set to the real thing.
func (e Env) withDefaults() Env {
	if e.exec == nil {
		e.exec = syscall.Exec
	}
	if e.ask == nil {
		e.ask = askDaemon
	}
	if e.release == nil {
		e.release = releaseLease
	}
	if e.ancestors == nil {
		e.ancestors = shim.Ancestors
	}
	if e.getwd == nil {
		e.getwd = os.Getwd
	}
	if e.stat == nil {
		e.stat = os.Stat
	}
	if e.composeAsk == nil {
		env, _ := e.envOf()
		e.composeAsk = func(bin string, args []string) ([]byte, error) { return askCompose(bin, args, env, "") }
	}
	if e.composeDry == nil {
		env, _ := e.envOf()
		e.composeDry = func(bin string, args []string) ([]byte, error) { return askComposeDry(bin, args, env) }
	}

	if e.fallbacks == nil {
		e.fallbacks = shim.Fallbacks
	}
	if e.status == nil {
		e.status = daemonStatus
	}
	if e.loginShell == nil {
		e.loginShell = askLoginShell
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.raise == nil {
		e.raise = reraise
	}
	return e // wait stays nil until a wait starts: see gate
}

// gate asks the daemon whether call c may start (R5, #28). Calls that start
// nothing, calls already checked and calls to a remote engine are not asked
// about; nor, failing open (R7), is anything when the config or the daemon
// cannot answer.
func gate(e Env, name, bin string, c shim.Call, getenv func(string) string) gated {
	if c.Kind == "" || getenv(shimCheckedVar) == shim.Self() {
		return gated{proceed: true}
	}
	// The engine, once and first: a remote one proceeds before the config
	// or the call's labels matter. Docker's is its CLI's (#90); an unknown
	// one ("") is this Mac's, never DOCKER_HOST, which the CLI ignores when
	// the call names a context.
	engine, remote := "", false
	switch {
	case c.Kind == "tart":
	case name == "docker":
		engine = dockerEndpointIn(getenv, c)
		remote = engine != "" && shim.Remote(name, engine, getenv)
	default:
		// Podman's is the last engine flag, as before #90 (its own order
		// is #71).
		endpoint := c.Context
		if c.HostLast {
			endpoint = c.Host
		}
		remote = shim.Remote(name, endpoint, getenv)
	}
	if remote {
		return gated{proceed: true}
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		notGated(e, "config: "+oneLine(err), c.Command)
		return gated{proceed: true}
	}
	if c.Kind == "container" && (c.Op == "run" || c.Op == "create") && shim.SetsLabel(e.Args[1:], protocol.LeaseLabel) {
		// The shim's label must be the one Docker keeps: past an option it
		// does not know, it goes first, and a later one would win and name
		// another worktree's lease.
		fmt.Fprintf(e.Stderr, "headroom: refused `%s`: the label %s is headroom's own\n", c.Command, protocol.LeaseLabel)
		return gated{code: exitDenied}
	}
	h := e.withDefaults()
	req := callerRequest(getenv, h.ancestors, h.getwd)
	req.Kind, req.Command, req.CostBytes = c.Kind, c.Command, c.MemoryBytes
	req.Target, req.Name, req.Op = c.Target, c.Name, c.Op
	if name == "docker" {
		req.Engine = engine
	}
	req.MultiTarget, req.Targets = c.MultiTarget, c.Targets
	var idle func() bool
	if c.Kind == "compose" && name == "docker" {
		// Compose names the project (-p needs no asking; podman compose is
		// not asked), and says whether a detached up starts anything.
		// Bounded by composeTimeout, also when the daemon turns out to be
		// down (R7).
		env, _ := e.envOf()
		lookupEnv := lookupIn(env)
		if slices.Contains(c.ComposeFiles, "-") {
			// Its file is on stdin, which the call needs: not asked.
			req.Target, req.Guessed = composeStdinProject(c, lookupEnv, h.getwd)
		} else {
			req.Target, req.Guessed = composeKey(bin, e.Args[1:], c, lookupEnv, h.getwd, h.composeAsk, h.now, h.stat)
			// Only a detached up: an attached up's dry run stops before
			// Compose would start anything ("interactive run is not
			// supported"), and a restart's lists each container as
			// restarting, running or not.
			if c.Op == "up" && c.ComposeDetached && composePlain(bin, e.Args[1:], h.composeAsk) {
				idle = func() bool { return composeIdle(bin, e.Args[1:], h.composeDry) }
			}
		}
	}
	// A run or create carries its lease as a label: runShim adds it the
	// same way, so the two agree.
	_, req.Labelled = shim.Labelled(name, e.Args[1:], protocol.LeaseLabel, "")
	if c.Kind == "tart" {
		// The VM's own memory, and whether it takes a macOS slot (R6).
		if macOS, mem, ok := shim.TartVM(c.Target, getenv); ok {
			req.MacOS, req.CostBytes = macOS, mem
		} else {
			// Unknown: count it as macOS rather than let a third one by.
			req.MacOS, req.VMUnknown = true, true
			if getenv("HEADROOM_SHIM_DEBUG") != "" {
				fmt.Fprintf(e.Stderr, "headroom: no config for the VM in `%s` under TART_HOME: counted as a macOS VM\n", c.Command)
			}
		}
		// This process becomes tart run: when it exits before its VM
		// appears, the daemon ends the lease (#29).
		req.PID = os.Getpid()
	}
	wait := shim.IsTrue(getenv("BUDGET_WAIT"))
	var deadline time.Time
	for {
		if idle != nil {
			// Asked afresh each time: a wait may outlast what it said.
			req.Idle = idle()
		}
		d, err := h.ask(cfg, req)
		if h.wait != nil {
			// A Ctrl-C during the ask cancels the call, even one now allowed.
			if sig := h.wait(0); sig != nil {
				if err == nil && d.Allow && d.LeaseID != "" {
					_ = h.release(cfg, d.LeaseID)
				}
				return interrupted(sig)
			}
		}
		switch {
		case err != nil:
			// Also while waiting: a daemon that went away must not hold
			// the call (R7).
			notGated(e, "daemon "+daemonCause(err, cfg.Policy.DaemonTimeout.Duration, cfg.Socket), c.Command)
			return gated{proceed: true}
		case d.Allow:
			if getenv("HEADROOM_SHIM_DEBUG") != "" {
				if d.Worktree == "" {
					fmt.Fprintln(e.Stderr, "headroom: allowed as a manual call")
				} else {
					fmt.Fprintf(e.Stderr, "headroom: allowed for %s (by %s)\n", d.Worktree, d.IdentifiedBy)
				}
			}
			return gated{proceed: true, checked: true, lease: d.LeaseID, cfg: cfg}
		case !wait || !d.Retry:
			fmt.Fprintln(e.Stderr, d.Message)
			fmt.Fprintln(e.Stderr, denyHint(d))
			return gated{code: exitDenied}
		case deadline.IsZero():
			deadline = h.now().Add(cfg.Policy.WaitTimeout.Duration)
			if h.wait == nil {
				// One registration for the whole wait, asks included.
				w := newSignalWait()
				defer w.stop()
				h.wait = w.sleep
			}
			fmt.Fprintf(e.Stderr, "headroom: waiting for room for `%s`%s, up to %s (BUDGET_WAIT)\n",
				c.Command, why(d), cfg.Policy.WaitTimeout.Duration)
		case !h.now().Before(deadline):
			fmt.Fprintln(e.Stderr, d.Message)
			fmt.Fprintf(e.Stderr, "headroom: gave up after waiting %s (policy.wait_timeout)\n", cfg.Policy.WaitTimeout.Duration)
			return gated{code: exitDenied}
		}
		if sig := h.wait(waitPoll); sig != nil {
			return interrupted(sig)
		}
	}
}

// interrupted is a wait ended by sig: exit as the shell reports it.
func interrupted(sig os.Signal) gated {
	code := 128
	if n, ok := sig.(syscall.Signal); ok {
		code += int(n)
	}
	return gated{code: code, signal: sig}
}

// callerRequest is a check request carrying who is calling: the worktree
// from the environment (HEADROOM_WORKTREE, else ORCA_WORKTREE_ID), else
// the working directory and the parent PIDs for the daemon to match.
func callerRequest(getenv func(string) string, ancestors func() []int, getwd func() (string, error)) protocol.CheckRequest {
	var r protocol.CheckRequest
	if r.Worktree = getenv("HEADROOM_WORKTREE"); r.Worktree == "" {
		r.Worktree = getenv("ORCA_WORKTREE_ID")
	}
	if r.Worktree != "" {
		return r
	}
	r.Cwd, _ = getwd()
	if resolved, err := filepath.EvalSymlinks(r.Cwd); err == nil && resolved != r.Cwd {
		r.RealCwd = resolved
	}
	r.Ancestors = ancestors()
	return r
}

// composeProject is a compose call's project, as Compose itself names it
// (#33): -p (target), else the name docker compose config gives, run with
// the call's own global options (args: -f, --project-directory,
// --env-file, --config, ...) in its own environment and working directory.
// Compose labels each container with the project, so the name is the
// lease's key. Asking Compose, rather than reading .env, override files and
// name: the way it does, keeps the two from parting. When Compose cannot
// say (its config fails or times out), the name it gives by default
// (composeDefaultProject, #84).
func composeProject(bin string, args []string, c shim.Call, lookupEnv func(string) (string, bool), getwd func() (string, error), ask func(bin string, args []string) ([]byte, error)) string {
	project, _ := composeKey(bin, args, c, lookupEnv, getwd, ask, time.Now, os.Stat)
	return project
}

// composeKey is composeProject's name, and whether it is a guess: the name
// Compose gives by default, its config having failed, which misses a name
// Compose finds elsewhere (#85). The lease book trusts a guess less (#84).
// now and stat are the clock and os.Stat.
func composeKey(bin string, args []string, c shim.Call, lookupEnv func(string) (string, bool), getwd func() (string, error),
	ask func(bin string, args []string) ([]byte, error), now func() time.Time, stat func(string) (fs.FileInfo, error)) (project string, guessed bool) {
	if c.Target != "" {
		return c.Target, false
	}
	cargs, ok := shim.ComposeConfig(args, false)
	if !ok {
		return "", false
	}
	asked := now()
	out, err := ask(bin, cargs)
	var cfg struct {
		Name string `json:"name"`
	}
	if err != nil || json.Unmarshal(out, &cfg) != nil {
		project = composeDefaultProject(c, lookupEnv, getwd, asked.Add(composeTimeout), now, stat)
		return project, project != ""
	}
	return cfg.Name, false
}

// composeDefaultProject is the project Compose names when it is not asked
// (#84): COMPOSE_PROJECT_NAME from the call's environment, else from the
// env files (composeEnvFileProject, #85), else the name of the project
// directory: --project-directory, else the directory of the first file (-f,
// else COMPOSE_FILE split by COMPOSE_PATH_SEPARATOR; not stdin's -), else of
// the compose file found in the working directory or the nearest of its
// parents, else the working directory. Normalised as Compose does. As
// compose-go's cli/options.go has it (GetWorkingDir, WithConfigFileEnv,
// WithDefaultConfigPath, withNamePrecedenceLoad) and
// loader.NormalizeProjectName; Compose's docs, "Specify a project name".
// Compose reads .env in the directory it has before COMPOSE_FILE and the
// search (--project-directory, -f's, else the working directory), then in
// the project directory it ends with, the first winning (docker/compose's
// cmd/compose/compose.go, toProjectOptions). The files are read and the
// parents searched until deadline, by the clock now: config has already
// waited.
//
// It cannot see a name: in the compose file, nor a COMPOSE_FILE in .env, nor
// an interpolated COMPOSE_PROJECT_NAME: there the name differs from
// Compose's, and the lease, a guess, binds nothing (entry.guessed).
func composeDefaultProject(c shim.Call, lookupEnv func(string) (string, bool), getwd func() (string, error), deadline time.Time, now func() time.Time,
	stat func(string) (fs.FileInfo, error)) string {
	getenv := getenvOf(lookupEnv)
	if name := getenv("COMPOSE_PROJECT_NAME"); name != "" {
		return normalProject(name)
	}
	cwd, err := getwd()
	if err != nil {
		return ""
	}
	first := func(files []string) int { return slices.IndexFunc(files, func(f string) bool { return f != "-" }) }
	dir := cwd
	if c.ComposeProjectDir != "" {
		dir = absIn(cwd, c.ComposeProjectDir)
	} else if i := first(c.ComposeFiles); i >= 0 {
		dir = filepath.Dir(absIn(cwd, c.ComposeFiles[i]))
	}
	if name, ok := composeEnvFileProject(c, lookupEnv, cwd, []string{dir}, deadline.Sub(now())); ok {
		return normalProject(name)
	}
	if dir == cwd && c.ComposeProjectDir == "" {
		files := c.ComposeFiles
		if f := getenv("COMPOSE_FILE"); len(files) == 0 && f != "" {
			sep := getenv("COMPOSE_PATH_SEPARATOR")
			if sep == "" {
				sep = string(filepath.ListSeparator)
			}
			files = strings.Split(f, sep)
		}
		if i := first(files); i >= 0 {
			dir = filepath.Dir(absIn(cwd, files[i]))
		} else if found, ok := composeFileDirWithin(cwd, deadline.Sub(now()), stat); ok {
			dir = found
		}
		if dir != cwd {
			// The working directory's .env again, first: one that sets it
			// to a name the shim cannot know wins too.
			if name, ok := composeEnvFileProject(c, lookupEnv, cwd, []string{cwd, dir}, deadline.Sub(now())); ok {
				return normalProject(name)
			}
		}
	}
	return normalProject(filepath.Base(dir))
}

// composeFileDir is the nearest of dir and its parents that holds a compose
// file under one of the names Compose looks for (compose-go's
// DefaultFileNames).
func composeFileDir(dir string, stat func(string) (fs.FileInfo, error)) (string, bool) {
	for {
		for _, n := range []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
			if _, err := stat(filepath.Join(dir, n)); err == nil {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// composeFileDirWithin is composeFileDir given up after left, what is left
// of config's budget: a parent may be a mount that hangs (autofs's /net),
// and the call has already waited for config (#84). Given up, none is
// found. The search left behind ends with the process, which the real
// binary replaces.
func composeFileDirWithin(dir string, left time.Duration, stat func(string) (fs.FileInfo, error)) (string, bool) {
	if left <= 0 {
		return "", false
	}
	type found struct {
		dir string
		ok  bool
	}
	done := make(chan found, 1)
	go func() {
		d, ok := composeFileDir(dir, stat)
		done <- found{d, ok}
	}()
	t := time.NewTimer(left)
	defer t.Stop()
	select {
	case f := <-done:
		return f.dir, f.ok
	case <-t.C:
		return "", false
	}
}

// absIn is path p made absolute in directory dir.
func absIn(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// composePlain reports whether a compose call's dry run tells what it
// starts. Not when Compose's dry run cannot see it: a model provider (a
// service's provider or models, in any profile) runs for real even in a
// dry run; a pull or build (--pull always, --build, a pull_policy other
// than missing or never) is not done, so a newer image's recreate is not
// shown. Config, with the call's own options, runs none of them.
func composePlain(bin string, args []string, ask func(bin string, args []string) ([]byte, error)) bool {
	for i, a := range args {
		if a == "--build" || a == "--pull=always" || a == "--pull" && i+1 < len(args) && args[i+1] == "always" {
			return false
		}
		// --build=true, =1, =T: a build. Only a false one builds nothing.
		if v, ok := strings.CutPrefix(a, "--build="); ok {
			if build, err := strconv.ParseBool(v); err != nil || build {
				return false
			}
		}
	}
	cargs, ok := shim.ComposeConfig(args, true)
	if !ok {
		return false
	}
	out, err := ask(bin, cargs)
	var cfg struct {
		Models   json.RawMessage `json:"models"`
		Services map[string]struct {
			Provider   json.RawMessage `json:"provider"`
			Models     json.RawMessage `json:"models"`
			PullPolicy string          `json:"pull_policy"`
		} `json:"services"`
	}
	if err != nil || json.Unmarshal(out, &cfg) != nil {
		return false
	}
	plain := noJSON(cfg.Models)
	for _, sv := range cfg.Services {
		switch sv.PullPolicy {
		case "", "missing", "if_not_present", "never":
		default:
			plain = false
		}
		plain = plain && noJSON(sv.Provider) && noJSON(sv.Models)
	}
	return plain
}

// noJSON reports whether a JSON value is absent, null or empty.
func noJSON(v json.RawMessage) bool {
	switch strings.TrimSpace(string(v)) {
	case "", "null", "{}", "[]":
		return true
	}
	return false
}

// ansi matches a terminal's colour codes.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// composeIdle reports whether Compose's own dry run of the call (args,
// with --dry-run added) says it creates, recreates and starts nothing:
// compose up -d of a stack that runs as configured. Asking Compose, rather
// than reading its config against what runs, leaves what it does (profiles,
// replicas, --scale, pull policies, a changed file or image) to Compose.
// false when it cannot say: the call holds its estimate.
func composeIdle(bin string, args []string, dry func(bin string, args []string) ([]byte, error)) bool {
	dryArgs, ok := shim.DryRun(args)
	if !ok {
		return false
	}
	out, err := dry(bin, dryArgs)
	if err != nil {
		return false
	}
	running := false
	for line := range strings.Lines(ansi.ReplaceAllString(string(out), "")) {
		// " Container app-db-1 Running", also after an older Compose's
		// "DRY-RUN MODE -" or a tty's tick.
		f := strings.Fields(line)
		i := slices.Index(f, "Container")
		if i < 0 || len(f) < i+3 {
			continue
		}
		switch status := f[len(f)-1]; {
		case status == "Running":
			running = true
		case strings.HasPrefix(status, "Creat"), strings.HasPrefix(status, "Recreat"),
			strings.HasPrefix(status, "Start"), strings.HasPrefix(status, "Restart"):
			return false
		}
		// Waiting, Healthy: a healthcheck it waits for starts nothing.
	}
	return running
}

// composeStdinProject names the project of compose -f - as Compose does
// when its file is on stdin: -p, else COMPOSE_PROJECT_NAME from the
// environment, else from the env files (composeEnvFileProject), else the
// project directory's name (--project-directory, else the working
// directory). Only that last one is a guess (#84): a name: in the piped
// file, which the shim cannot read, beats it but none of the others
// (compose-go's cli/options.go, withNamePrecedenceLoad). A guess that
// misses binds nothing and ends quietly (#85).
func composeStdinProject(c shim.Call, lookupEnv func(string) (string, bool), getwd func() (string, error)) (project string, guessed bool) {
	getenv := getenvOf(lookupEnv)
	switch {
	case c.Target != "":
		return c.Target, false
	case getenv("COMPOSE_PROJECT_NAME") != "":
		return normalProject(getenv("COMPOSE_PROJECT_NAME")), false
	}
	cwd, err := getwd()
	if err != nil {
		if filepath.IsAbs(c.ComposeProjectDir) {
			project = normalProject(filepath.Base(c.ComposeProjectDir))
		}
		return project, project != ""
	}
	dir := cwd
	if c.ComposeProjectDir != "" {
		dir = absIn(cwd, c.ComposeProjectDir)
	}
	if name, ok := composeEnvFileProject(c, lookupEnv, cwd, []string{dir}, composeTimeout); ok {
		return normalProject(name), false
	}
	project = normalProject(filepath.Base(dir))
	return project, project != ""
}

// composeEnvFileProject is COMPOSE_PROJECT_NAME as the env files Compose
// loads set it (#85), read within left; ok is false when none sets it to a
// value the shim can know, or the time ran out. The files: the call's
// --env-files, else COMPOSE_ENV_FILES (comma-separated), relative to the
// working directory cwd, a later one winning; else .env in each of dirs,
// the first that sets it winning, unless COMPOSE_DISABLE_ENV_FILE is true.
// A file that cannot be read is skipped (R7). As docker/compose's
// cmd/compose/compose.go has it (the --env-file flag's default,
// toProjectOptions: WithEnvFiles and WithDotEnv once before the compose file
// is found and once after, so the working directory's .env and then the
// found project directory's) and compose-go's cli/options.go (WithEnvFiles,
// WithDotEnv), dotenv/env.go (GetEnvFromFile: one map, a later file
// overwriting) and types/mapping.go (Mapping.Merge keeps what is set: the
// process environment, then the first .env, beat what follows). A
// COMPOSE_PROJECT_NAME set in the process environment, even empty, is kept
// (WithOsEnv copies it so), and none is read: an empty one leaves the
// directory's name (withNamePrecedenceLoad).
func composeEnvFileProject(c shim.Call, lookupEnv func(string) (string, bool), cwd string, dirs []string, left time.Duration) (string, bool) {
	if _, set := lookupEnv("COMPOSE_PROJECT_NAME"); set {
		return "", false
	}
	getenv := getenvOf(lookupEnv)
	files := c.ComposeEnvFiles
	if len(files) == 0 {
		files = strings.FieldsFunc(getenv("COMPOSE_ENV_FILES"), func(r rune) bool { return r == ',' })
	}
	if len(files) > 0 {
		dirs = nil
		files = slices.Clone(files)
		for i, f := range files {
			files[i] = absIn(cwd, f)
		}
	} else if off, _ := strconv.ParseBool(getenv("COMPOSE_DISABLE_ENV_FILE")); off {
		return "", false
	}
	found, done := within(left, func() (name string) {
		// The --env-files: the last that sets it.
		for _, f := range files {
			if v, set := envFileValue(f, "COMPOSE_PROJECT_NAME"); set {
				name = v
			}
		}
		// The .env files: the first that sets it.
		for _, d := range dirs {
			if v, set := envFileValue(filepath.Join(d, ".env"), "COMPOSE_PROJECT_NAME"); set {
				return v
			}
		}
		return name
	})
	return found, done && found != ""
}

// envFileValue is key's value in env file path, and whether the file sets
// it: "" for a value the shim cannot know (envValue). A file that cannot be
// read or parsed sets nothing.
func envFileValue(path, key string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return envValue(string(b), key)
}

// envValue is key's value in env file src, and whether src sets it, read a
// statement at a time as compose-go's dotenv parser does (dotenv/parser.go,
// parse; godotenv.go drops a UTF-8 BOM): blank lines and # comments
// skipped; an optional export; KEY=VALUE or KEY: VALUE, spaces around
// both; a value in single or double quotes, which may span lines and
// escape its quote with \; an unquoted one up to the line's end or " #".
// A bare KEY looks itself up, which leaves key as it was. A value Compose
// would interpolate ($, unquoted or in double quotes) or unescape (\ in
// double quotes) is unknown: "". A file the parser rejects sets nothing,
// as Compose then fails.
func envValue(src, key string) (value string, set bool) {
	src = strings.TrimPrefix(src, "\uFEFF")
	for {
		src = strings.TrimLeftFunc(src, unicode.IsSpace)
		if src == "" {
			return value, set
		}
		if src[0] == '#' {
			_, src, _ = strings.Cut(src, "\n")
			continue
		}
		if exportPrefix.MatchString(src) {
			src = strings.TrimLeftFunc(strings.TrimPrefix(src, "export"), envSpace)
		}
		// The key, up to =, : or a newline (a bare key); with none, the
		// key is "" and the statement its value.
		k, bare := "", false
		for i, r := range src {
			if envSpace(r) || r == '_' || r == '.' || r == '-' || r == '[' || r == ']' || unicode.IsLetter(r) || unicode.IsNumber(r) {
				continue
			}
			if r != '=' && r != ':' && r != '\n' {
				return "", false
			}
			k, bare, src = src[:i], r == '\n', src[i+1:]
			break
		}
		if k = strings.TrimRightFunc(k, unicode.IsSpace); strings.Contains(k, " ") {
			return "", false
		}
		src = strings.TrimLeftFunc(src, envSpace)
		if bare {
			continue
		}
		var v string
		var known bool
		if src != "" && (src[0] == '"' || src[0] == '\'') {
			quote, closed, escaped := src[0], false, false
			var b []byte
			i := 1
			for ; i < len(src) && !closed; i++ {
				switch ch := src[i]; {
				case ch != quote && !escaped && ch == '\\':
					escaped = true
				case ch != quote && escaped:
					escaped = false
					b = append(b, '\\', ch)
				case ch != quote, escaped: // an escaped quote is kept
					escaped = false
					b = append(b, ch)
				default:
					closed = true
				}
			}
			if !closed {
				return "", false
			}
			v, src = string(b), src[i:]
			known = quote == '\'' || !strings.ContainsAny(v, `$\`)
		} else {
			v, src, _ = strings.Cut(src, "\n")
			v, _, _ = strings.Cut(v, " #")
			v = strings.TrimRightFunc(v, unicode.IsSpace)
			known = !strings.Contains(v, "$")
		}
		if k == key {
			value, set = v, true
			if !known {
				value = ""
			}
		}
	}
}

// exportPrefix is the export an env file's statement may start with
// (compose-go's dotenv exportRegex).
var exportPrefix = regexp.MustCompile(`^export\s+`)

// envSpace is a space within an env file's line (compose-go's dotenv
// isSpace): not a newline.
func envSpace(r rune) bool {
	switch r {
	case '\t', '\v', '\f', '\r', ' ', 0x85, 0xA0:
		return true
	}
	return false
}

// getenvOf is lookupEnv as getenv: "" when unset.
func getenvOf(lookupEnv func(string) (string, bool)) func(string) string {
	return func(k string) string {
		v, _ := lookupEnv(k)
		return v
	}
}

// within is f's result, unless it takes longer than left: then false. What
// f is left doing ends with the process, which the real binary replaces.
func within[T any](left time.Duration, f func() T) (T, bool) {
	var zero T
	if left <= 0 {
		return zero, false
	}
	done := make(chan T, 1)
	go func() { done <- f() }()
	t := time.NewTimer(left)
	defer t.Stop()
	select {
	case v := <-done:
		return v, true
	case <-t.C:
		return zero, false
	}
}

// normalProject is a project name as Compose normalises it (compose-go's
// loader.NormalizeProjectName): lower case, then only a-z, 0-9, - and _,
// and no leading - or _.
func normalProject(s string) string {
	return strings.TrimLeft(strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			return c
		}
		return -1
	}, strings.ToLower(s)), "_-")
}

// composeTimeout bounds docker compose config (30-40 ms on a plain stack):
// it reads the compose files, as the call itself is about to, and delays a
// call that fails open by at most this much.
const composeTimeout = 2 * time.Second

// askCompose runs docker compose config (args) at bin in env and dir ("":
// this process's) and returns its JSON.
func askCompose(bin string, args, env []string, dir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env, cmd.Dir, cmd.WaitDelay = env, dir, 500*time.Millisecond
	return cmd.Output()
}

// askComposeDry runs a compose call's dry run (args) at bin in env: its
// plan is on stderr, with stdout.
func askComposeDry(bin string, args, env []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	// Its plan as plain lines, whatever the call's environment asks for.
	cmd.Env, cmd.WaitDelay = append(slices.Clone(env), "COMPOSE_PROGRESS=plain", "COMPOSE_ANSI=never"), 500*time.Millisecond
	return cmd.CombinedOutput()
}

// dockerEndpointIn is the endpoint the docker CLI talks to for call c: its
// own -H (trimmed; "" is the default socket, and one with no scheme is TCP,
// on port 2375 if it names none), or its --context's; else DOCKER_HOST,
// else the context's (DOCKER_CONTEXT, else currentContext in the config at
// c.ConfigDir, from --config, else DOCKER_CONFIG, else ~/.docker), else
// Docker's default socket. That is the CLI's order: resolveContextName in
// docker/cli's cli/command/cli.go, and ParseHost, parseDockerDaemonHost and
// ParseTCPAddr in its opts/hosts.go for -H. "" (unknown) when the context
// cannot be read, or the config directory is relative.
func dockerEndpointIn(getenv func(string) string, c shim.Call) string {
	if c.HasHost {
		h := strings.TrimSpace(c.Host)
		switch {
		case h == "":
			return "unix:///var/run/docker.sock"
		case strings.Contains(h, "://"):
			return h
		case !strings.Contains(h, ":"):
			h += ":2375"
		}
		return "tcp://" + h
	}
	if h := getenv("DOCKER_HOST"); h != "" && c.Context == "" {
		return h
	}
	dir := c.ConfigDir
	if dir == "" {
		dir = getenv("DOCKER_CONFIG")
	}
	if dir == "" {
		dir = filepath.Join(getenv("HOME"), ".docker")
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	name := c.Context
	if name == "" {
		name = getenv("DOCKER_CONTEXT")
	}
	if name == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		// A config that cannot be read or parsed is the default context:
		// the docker CLI warns and goes on with defaults too.
		if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
			_ = json.Unmarshal(b, &cfg)
		}
		name = cfg.CurrentContext
	}
	if name == "default" && getenv("DOCKER_HOST") != "" {
		return getenv("DOCKER_HOST") // --context default: DOCKER_HOST's
	}
	if name == "" || name == "default" {
		return "unix:///var/run/docker.sock"
	}
	sum := sha256.Sum256([]byte(name))
	b, err := os.ReadFile(filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Endpoints map[string]struct{ Host string }
	}
	if json.Unmarshal(b, &meta) != nil {
		return ""
	}
	return meta.Endpoints["docker"].Host
}

// sameSocket reports whether a Docker endpoint (unix://path) is socket.
func sameSocket(endpoint, socket string) bool {
	p, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok || socket == "" {
		return false
	}
	if filepath.Clean(p) == filepath.Clean(socket) {
		return true
	}
	a, err1 := filepath.EvalSymlinks(p)
	b, err2 := filepath.EvalSymlinks(socket)
	return err1 == nil && err2 == nil && a == b
}

// notGated warns, in one line, that a call runs without a check (R7).
func notGated(e Env, cause, command string) {
	fmt.Fprintf(e.Stderr, "headroom: not gated (%s); running `%s` anyway. See: headroom status\n", cause, command)
}

// daemonCause says briefly why the daemon at socket gave no decision.
func daemonCause(err error, timeout time.Duration, socket string) string {
	var ne net.Error
	var de daemonError
	switch {
	case errors.Is(err, client.ErrVersion), errors.As(err, &de) && de.said == olderDaemon:
		return "is another version: restart it with this build: headroom install"
	case errors.As(err, &de) && de.said == "":
		return "gave no decision; restart it with this build: headroom install"
	case errors.As(err, &de):
		return "gave no decision (" + strings.Join(strings.Fields(de.said), " ") + "); restart it with this build: headroom install"
	case errors.Is(err, client.ErrBadReply):
		return "gave a bad reply"
	case errors.As(err, &ne) && ne.Timeout(): // deadlines of conn and context alike
		return fmt.Sprintf("gave no answer in %s", timeout)
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "socket not accessible"
	}
	if fi, statErr := os.Lstat(socket); statErr == nil && fi.Mode()&os.ModeSocket == 0 {
		return "socket path " + socket + " is not a socket"
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK) {
		return "not running"
	}
	return "unreachable: " + oneLine(err)
}

// oneLine is err's text on one line.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// askDaemon sends one check to the daemon. errCannotCheck means it
// answered without a decision.
func askDaemon(cfg config.Config, req protocol.CheckRequest) (*protocol.Decision, error) {
	rep, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpCheck, Check: &req})
	switch {
	case err != nil && rep.Error != "":
		return nil, daemonError{said: rep.Error}
	case err != nil:
		return nil, err
	case rep.Decision == nil:
		return nil, daemonError{}
	}
	return rep.Decision, nil
}

// releaseLease hands back the lease of a call that did not start.
func releaseLease(cfg config.Config, id string) error {
	_, err := client.Do(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration,
		protocol.Request{Op: protocol.OpRelease, Release: id})
	return err
}

// signalWait sleeps between asks unless SIGINT or SIGTERM comes. A signal
// the shim was started ignoring (a background job's SIGINT) stays ignored.
type signalWait struct{ ch chan os.Signal }

func newSignalWait() *signalWait {
	w := &signalWait{ch: make(chan os.Signal, 1)}
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if !signal.Ignored(s) {
			signal.Notify(w.ch, s)
		}
	}
	return w
}

func (w *signalWait) stop() { signal.Stop(w.ch) }

// sleep waits d; it returns the signal that cut it short, or nil. With d
// 0 it only reports a signal already pending.
func (w *signalWait) sleep(d time.Duration) os.Signal {
	if d == 0 {
		select {
		case s := <-w.ch:
			return s
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case s := <-w.ch:
		return s
	}
}

// reraise dies of sig with its default action, so the parent sees the
// signal and not just an exit code.
func reraise(sig os.Signal) {
	if n, ok := sig.(syscall.Signal); ok {
		signal.Reset(n)
		_ = syscall.Kill(os.Getpid(), n)
		time.Sleep(100 * time.Millisecond) // delivery is asynchronous
	}
}

// denyHint tells an agent what to do about a deny.
func denyHint(d *protocol.Decision) string {
	if d.Retry {
		if len(d.Reasons) > 0 && d.Reasons[0].Code == policy.VMSlots {
			return "headroom: retry later, or run it with BUDGET_WAIT=1 to wait for a slot"
		}
		return "headroom: retry later, or run it with BUDGET_WAIT=1 to wait for room"
	}
	// Speak to the first reason waiting cannot fix.
	for _, r := range d.Reasons {
		if !r.Retry {
			if r.Code == policy.VMSlots {
				return "headroom: waiting will not help: set budget.max_macos_vms in headroom's config to allow macOS VMs"
			}
			break
		}
	}
	return "headroom: waiting will not help: reuse or stop what this worktree holds, or ask for less memory"
}

// why is the first reason for a deny, as " (reason)"; "" if none is given.
func why(d *protocol.Decision) string {
	if len(d.Reasons) == 0 {
		return ""
	}
	return " (" + d.Reasons[0].Text + ")"
}
