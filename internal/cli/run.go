package cli

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/cybagard/cyba-headroom/internal/binpath"
	"github.com/cybagard/cyba-headroom/internal/config"
)

// runRun launches a command, usually a coding agent, with the shim dir first
// on PATH, so every docker, podman and tart it runs is gated (R9, #31). Its
// tool shells keep that PATH (#8). Launching never fails because of
// headroom: without shims it runs the command as it is.
func runRun(e Env) int {
	args := e.Args[2:]
	if len(args) < 2 || args[0] != "--" {
		fmt.Fprintln(e.Stderr, "headroom: usage: headroom run -- <command> [args...]")
		return 2
	}
	argv := args[1:]
	env, getenv := e.envOf()
	shimDir := ""
	cfg, err := config.Load(getenv)
	if err == nil {
		shimDir = cfg.ShimDir
	} else if dir, derr := config.Dir(getenv); derr == nil {
		// The shims fail open on a bad config themselves (R7): keep the
		// gate, from the default shim dir.
		shimDir = config.Defaults(dir).ShimDir
		fmt.Fprintf(e.Stderr, "headroom: %s; using the shims in %s\n", oneLine(err), shimDir)
	}
	switch {
	case shimDir != "" && hasShims(shimDir):
		env = setEnv(env, "PATH", withFirst(getenv("PATH"), shimDir))
	case err == nil:
		fmt.Fprintf(e.Stderr, "headroom: no shims in %s (run `headroom install`); running `%s` without the gate\n", shimDir, argv[0])
	}
	// The agent is found on the PATH it gets; none at all is the system's.
	_, childGetenv := Env{Environ: func() []string { return env }}.envOf()
	path := argv[0]
	if !strings.Contains(path, "/") {
		lookup := childGetenv
		if childGetenv("PATH") == "" {
			lookup = func(k string) string {
				if k == "PATH" {
					return defaultPath
				}
				return childGetenv(k)
			}
		}
		path = binpath.Search(path, lookup, nil, nil)
		if path == "" {
			fmt.Fprintf(e.Stderr, "headroom: %s: command not found\n", argv[0])
			return 127
		}
	}
	h := e.withDefaults()
	if err := h.exec(path, argv, env); err != nil {
		fmt.Fprintf(e.Stderr, "headroom: running %s: %v\n", path, err)
		if errors.Is(err, syscall.ENOENT) {
			return 127
		}
		return 126
	}
	return 0
}
