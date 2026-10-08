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
	cfg, err := config.Load(getenv)
	switch {
	case err != nil:
		fmt.Fprintf(e.Stderr, "headroom: %s; running `%s` without the gate\n", oneLine(err), argv[0])
	case !hasShims(cfg.ShimDir):
		fmt.Fprintf(e.Stderr, "headroom: no shims in %s (run `headroom install`); running `%s` without the gate\n", cfg.ShimDir, argv[0])
	default:
		env = setEnv(env, "PATH", withFirst(getenv("PATH"), cfg.ShimDir))
	}
	path := argv[0]
	if !strings.Contains(path, "/") {
		path = binpath.Search(path, getenv, nil, nil)
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
