package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/view"
)

// watchInterval is how often --watch polls the daemon. A snapshot changes
// once per daemon tick; polling faster keeps its age current.
var watchInterval = time.Second

// Terminal escapes for --watch: the alternate screen keeps the user's
// scrollback, and the cursor is hidden while frames redraw.
const (
	enterScreen = "\x1b[?1049h\x1b[?25l"
	leaveScreen = "\x1b[?25h\x1b[?1049l"
	clearScreen = "\x1b[H\x1b[2J"
)

// runView prints the observe view once, or keeps redrawing it with --watch
// (R4).
func runView(e Env) int {
	watch, all := false, false
	for _, a := range e.Args[1:] {
		switch a {
		case "--watch":
			watch = true
		case "--all":
			all = true
		default:
			fmt.Fprintf(e.Stderr, "headroom: unknown argument %q\n", a)
			return 2
		}
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	terminal := e.Terminal
	if terminal == nil {
		terminal = func() (bool, int) { return detectTerminal(e.Stdout) }
	}
	opts := func() view.Options {
		tty, width := terminal()
		if !tty {
			width = 0 // piped or logged: keep whole lines
		}
		return view.Options{Width: width, Color: tty && e.Getenv("NO_COLOR") == "", All: all,
			Now: time.Now(), TrendWindow: cfg.Daemon.TrendWindow.Duration}
	}
	fetch := func(ctx context.Context) (*protocol.Snapshot, error) {
		return client.Status(ctx, cfg.Socket, cfg.Policy.DaemonTimeout.Duration)
	}
	if !watch {
		snap, err := fetch(context.Background())
		if err != nil {
			fmt.Fprintf(e.Stderr, "headroom: daemon not reachable at %s: %v (start it with `headroom daemon`)\n", cfg.Socket, err)
			return 1
		}
		view.Render(e.Stdout, snap, opts())
		return 0
	}

	base := e.Context
	if base == nil {
		base = context.Background()
	}
	ctx, stop := signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if tty, _ := terminal(); tty {
		watchTerminal(ctx, e.Stdout, fetch, opts, cfg.Socket)
	} else {
		watchPlain(ctx, e.Stdout, fetch, opts, cfg.Socket)
	}
	return 0
}

type fetchFunc func(context.Context) (*protocol.Snapshot, error)

// watchTerminal redraws the whole view every interval on the alternate
// screen, and restores the terminal when ctx ends.
func watchTerminal(ctx context.Context, w io.Writer, fetch fetchFunc, opts func() view.Options, socket string) {
	fmt.Fprint(w, enterScreen)
	defer fmt.Fprint(w, leaveScreen)
	tick := time.NewTicker(watchInterval)
	defer tick.Stop()
	for {
		var frame bytes.Buffer
		frame.WriteString(clearScreen)
		if snap, err := fetch(ctx); err != nil {
			fmt.Fprintf(&frame, "headroom: daemon not reachable at %s, retrying (%v)\n", socket, err)
		} else {
			view.Render(&frame, snap, opts())
		}
		_, _ = w.Write(frame.Bytes()) // one write, so a frame never shows half drawn
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// watchPlain prints a frame only when there is a new snapshot, or when the
// daemon becomes unreachable, so a pipe or log gets no repeats.
func watchPlain(ctx context.Context, w io.Writer, fetch fetchFunc, opts func() view.Options, socket string) {
	tick := time.NewTicker(watchInterval)
	defer tick.Stop()
	var lastSeq uint64
	down, first := false, true
	for {
		snap, err := fetch(ctx)
		switch {
		case err != nil && !down && ctx.Err() == nil:
			fmt.Fprintf(w, "headroom: daemon not reachable at %s, retrying (%v)\n", socket, err)
			down = true
		case err == nil && (down || first || snap.Seq != lastSeq):
			if !first {
				fmt.Fprintln(w)
			}
			view.Render(w, snap, opts())
			lastSeq, down, first = snap.Seq, false, false
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// detectTerminal reports whether w is a terminal, and its width.
func detectTerminal(w io.Writer) (bool, int) {
	f, ok := w.(*os.File)
	if !ok {
		return false, 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return false, 0
	}
	return true, terminalWidth(ws.Col)
}

// terminalWidth is a terminal's width, or 100 when it reports none (a pty
// that was never sized, such as script(1)'s).
func terminalWidth(cols uint16) int {
	if cols == 0 {
		return 100
	}
	return int(cols)
}
