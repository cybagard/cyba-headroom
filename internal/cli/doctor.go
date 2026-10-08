package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/attribution"
	"github.com/cybagard/cyba-headroom/internal/binpath"
	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/shim"
	"github.com/cybagard/cyba-headroom/internal/view"
)

// A finding is one line of `headroom doctor`.
type finding struct {
	mark   string // ✓ passes, ! warns, ✗ fails
	name   string
	detail string
	more   []string // indented lines under it
}

const (
	pass = "✓"
	warn = "!"
	fail = "✗"
)

// runDoctor checks, from the shell it runs in, what decides whether this
// shell's docker, podman and tart calls are gated and charged to the right
// worktree (R9, #32): the config, the shims, PATH order, the daemon and the
// caller's identity. It exits 1 if a check fails; warnings do not.
func runDoctor(e Env) int {
	if len(e.Args) > 2 {
		fmt.Fprintf(e.Stderr, "headroom: doctor: unknown argument %q\n", e.Args[2])
		return 2
	}
	code := 0
	fmt.Fprintln(e.Stdout, "headroom doctor")
	for _, f := range doctor(e) {
		fmt.Fprintf(e.Stdout, "  %s %-11s %s\n", f.mark, f.name, view.Clean(f.detail))
		for _, m := range f.more {
			fmt.Fprintf(e.Stdout, "                %s\n", view.Clean(m))
		}
		if f.mark == fail {
			code = 1
		}
	}
	return code
}

func doctor(e Env) []finding {
	h := e.withDefaults()
	if h.status == nil {
		h.status = daemonStatus
	}
	if h.loginShell == nil {
		h.loginShell = askLoginShell
	}
	var out []finding

	cfg, err := config.Load(e.Getenv)
	if err != nil {
		out = append(out, finding{mark: fail, name: "config", detail: oneLine(err)})
		// The shims fail open on a bad config (R7), from the default dir.
		dir, derr := config.Dir(e.Getenv)
		if derr != nil {
			return out
		}
		cfg = config.Defaults(dir)
	} else {
		out = append(out, finding{mark: pass, name: "config", detail: filepath.Join(cfg.Dir, config.FileName)})
	}

	out = append(out, shimsFinding(cfg.ShimDir))
	fallbacks := h.fallbacks
	if fallbacks == nil {
		fallbacks = shim.Fallbacks
	}
	out = append(out, pathFinding(e.Getenv, cfg.ShimDir, fallbacks))

	snap, serr := h.status(cfg)
	if serr != nil {
		snap = nil
		out = append(out, finding{mark: warn, name: "daemon",
			detail: fmt.Sprintf("not reachable at %s (%s): calls run ungated until it is back; start it with `headroom install`", cfg.Socket, oneLine(serr))})
	} else {
		out = append(out, finding{mark: pass, name: "daemon", detail: "running at " + cfg.Socket})
	}

	out = append(out, identityFinding(e.Getenv, h, snap))
	out = append(out, loginFinding(e.Getenv, h.loginShell))
	return out
}

// daemonStatus asks the daemon for its snapshot.
func daemonStatus(cfg config.Config) (*protocol.Snapshot, error) {
	return client.Status(context.Background(), cfg.Socket, cfg.Policy.DaemonTimeout.Duration)
}

// shimsFinding checks that each shim in dir links to a headroom binary
// that exists.
func shimsFinding(dir string) finding {
	self, _ := os.Executable()
	var bad []string
	target := ""
	for _, n := range shimList() {
		p := filepath.Join(dir, n)
		t, err := os.Readlink(p)
		if err != nil {
			bad = append(bad, n+" missing")
			continue
		}
		if _, err := os.Stat(p); err != nil || !isHeadroom(p, t, self) {
			bad = append(bad, n+" → "+t+" (not a headroom binary that exists)")
			continue
		}
		target = t
	}
	if len(bad) > 0 {
		return finding{mark: fail, name: "shims", detail: fmt.Sprintf("in %s: %s; run `headroom install`", dir, strings.Join(bad, ", "))}
	}
	return finding{mark: pass, name: "shims", detail: fmt.Sprintf("%s in %s → %s", strings.Join(shimList(), ", "), dir, target)}
}

// pathFinding checks that each tool resolves to its shim on this shell's
// PATH, as `command -v` finds it, and names the real binary behind it.
func pathFinding(getenv func(string) string, shimDir string, fallbacks map[string][]string) finding {
	self, _ := os.Executable()
	var wrong, more []string
	for _, n := range shimList() {
		first := binpath.Search(n, getenv, nil, nil)
		bin, rerr := shim.Resolve(n, []string{self}, getenv, fallbacks[n])
		switch {
		case first == "" && rerr != nil:
			more = append(more, n+": not installed")
		case first == "":
			// Not on PATH, so no call by name reaches it, gated or not.
			more = append(more, fmt.Sprintf("%s: not on this PATH (installed at %s)", n, bin))
		case !shim.LeadsToHeadroom(first) && !isShim(first, shimDir, n):
			wrong = append(wrong, fmt.Sprintf("%s resolves to %s, not the shim", n, first))
		case rerr != nil:
			more = append(more, n+": shim first; not installed (calls say command not found)")
		default:
			more = append(more, fmt.Sprintf("%s: shim first, runs %s", n, bin))
		}
	}
	if len(wrong) > 0 {
		f := finding{mark: fail, name: "PATH", detail: strings.Join(wrong, "; ") +
			". PATH is fixed when an agent starts: restart it with `headroom run -- <agent>` (after `headroom install`)", more: more}
		return f
	}
	return finding{mark: pass, name: "PATH", detail: strings.Join(shimList(), ", ") + " go through the shims in " + shimDir, more: more}
}

// isShim reports whether p is the shim for n in dir.
func isShim(p, dir, n string) bool {
	return filepath.Clean(p) == filepath.Join(filepath.Clean(dir), n)
}

// identityFinding resolves this shell's worktree as the shim would (#28).
func identityFinding(getenv func(string) string, h Env, snap *protocol.Snapshot) finding {
	req := callerRequest(getenv, h.ancestors, h.getwd)
	id, by := attribution.Identify(snap, attribution.Caller{Worktree: req.Worktree, Cwd: req.Cwd, RealCwd: req.RealCwd, Ancestors: req.Ancestors})
	source := map[string]string{
		protocol.IdentifiedByCwd:     "the working directory",
		protocol.IdentifiedByProcess: "the Orca terminal it runs in",
	}[by]
	if by == protocol.IdentifiedByCaller {
		source = "HEADROOM_WORKTREE"
		if getenv("HEADROOM_WORKTREE") == "" {
			source = "ORCA_WORKTREE_ID"
		}
	}
	switch {
	case id == "" && snap == nil:
		return finding{mark: warn, name: "identity", detail: "unknown: no worktree in the environment, and the daemon is needed to match the working directory or terminal"}
	case id == "":
		return finding{mark: warn, name: "identity", detail: "manual: no worktree matches this shell, so its calls are charged to no worktree. Agents launched by Orca get ORCA_WORKTREE_ID"}
	case snap == nil || snap.Orca == nil:
		return finding{mark: pass, name: "identity", detail: fmt.Sprintf("worktree %s, from %s (not checked against Orca: no worktree list)", id, source)}
	}
	name := ""
	for _, w := range snap.Orca.Worktrees {
		if w.ID == id {
			name = w.Name
		}
	}
	if name == "" {
		return finding{mark: warn, name: "identity", detail: fmt.Sprintf("worktree %s, from %s, is not one of Orca's worktrees: its calls are charged to an unknown worktree", id, source)}
	}
	f := finding{mark: pass, name: "identity", detail: fmt.Sprintf("worktree %q (%s), from %s", name, id, source)}
	if by == protocol.IdentifiedByCaller {
		// The environment wins; say so when the directory says otherwise.
		// callerRequest leaves the directory out once the env names one.
		cwd, _ := h.getwd()
		resolved, _ := filepath.EvalSymlinks(cwd)
		if cwdID, _ := attribution.Identify(snap, attribution.Caller{Cwd: cwd, RealCwd: resolved}); cwdID != "" && cwdID != id {
			f.mark = warn
			f.more = append(f.more, fmt.Sprintf("the working directory is in worktree %s; calls are charged to %s, from %s", cwdID, id, source))
		}
	}
	return f
}

// loginScript prints where a login shell finds each tool.
const loginScript = `for c in docker podman tart; do printf '%s=%s\n' "$c" "$(command -v "$c")"; done`

// askLoginShell runs loginScript in a login shell, which runs path_helper
// and the user's profile, with a short timeout.
func askLoginShell(sh string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, sh, "-lc", loginScript).Output()
	return string(out), err
}

// loginFinding warns when a login shell started from here puts a real
// tool first: scripts that start one (`zsh -l`, `bash -l`) run ungated.
func loginFinding(getenv func(string) string, ask func(string) (string, error)) finding {
	sh := getenv("SHELL")
	if !filepath.IsAbs(sh) {
		sh = "/bin/zsh"
	}
	out, err := ask(sh)
	if err != nil {
		return finding{mark: warn, name: "login", detail: fmt.Sprintf("could not run `%s -l`: %s", sh, oneLine(err))}
	}
	var ungated []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		n, p, ok := strings.Cut(sc.Text(), "=")
		if !ok || p == "" || !ShimNames[n] {
			continue
		}
		if !shim.LeadsToHeadroom(p) {
			ungated = append(ungated, n+" → "+p)
		}
	}
	if len(ungated) > 0 {
		return finding{mark: warn, name: "login", detail: fmt.Sprintf("`%s -l` puts the real tools first (%s): scripts that start a login shell run ungated (#33)", sh, strings.Join(ungated, ", "))}
	}
	return finding{mark: pass, name: "login", detail: fmt.Sprintf("`%s -l` keeps the shims first", sh)}
}
