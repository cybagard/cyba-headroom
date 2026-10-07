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
const watchInterval = time.Second

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
			unreachable(e, cfg.Socket, err)
			return 1
		}
		view.Render(e.Stdout, snap, opts())
		return 0
	}

	// SIGHUP too: a closed terminal window should still restore and exit.
	ctx, stop := signalContext(e, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	w := watcher{out: e.Stdout, fetch: fetch, opts: opts, socket: cfg.Socket,
		every: e.watchEvery, suspend: e.suspend, stopSelf: e.stopSelf}
	if w.every == 0 {
		w.every = watchInterval
	}
	var reached bool
	if tty, _ := terminal(); tty {
		if w.suspend == nil {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGTSTP)
			defer signal.Stop(ch)
			w.suspend = ch
		}
		if w.stopSelf == nil {
			w.stopSelf = func() { _ = syscall.Kill(syscall.Getpid(), syscall.SIGSTOP) }
		}
		reached = w.terminal(ctx)
	} else {
		reached = w.plain(ctx)
	}
	if !reached {
		return 1 // never got a snapshot, as the one-shot view would say
	}
	return 0
}

// watcher runs --watch.
type watcher struct {
	out      io.Writer
	fetch    fetchFunc
	opts     func() view.Options
	socket   string
	every    time.Duration
	suspend  <-chan os.Signal
	stopSelf func()
}

type fetchFunc func(context.Context) (*protocol.Snapshot, error)

// terminal redraws the whole view every interval on the alternate screen,
// and restores the terminal when ctx ends or before Ctrl-Z suspends the
// process. It reports whether any snapshot arrived.
func (w watcher) terminal(ctx context.Context) (reached bool) {
	fmt.Fprint(w.out, enterScreen)
	defer fmt.Fprint(w.out, leaveScreen)
	tick := time.NewTicker(w.every)
	defer tick.Stop()
	for {
		var frame bytes.Buffer
		frame.WriteString(clearScreen)
		if snap, err := w.fetch(ctx); err != nil {
			fmt.Fprintf(&frame, "headroom: daemon not reachable at %s, retrying (%v)\n", w.socket, err)
		} else {
			reached = true
			view.Render(&frame, snap, w.opts())
		}
		_, _ = w.out.Write(frame.Bytes()) // one write, so a frame never shows half drawn
		select {
		case <-ctx.Done():
			return reached
		case <-w.suspend:
			// Give the shell its screen back, stop, and take it again on
			// resume (SIGCONT).
			fmt.Fprint(w.out, leaveScreen)
			w.stopSelf()
			fmt.Fprint(w.out, enterScreen)
		case <-tick.C:
		}
	}
}

// plain prints a frame only when there is a new snapshot, or when the
// daemon becomes unreachable, so a pipe or log gets no repeats. It reports
// whether any snapshot arrived.
func (w watcher) plain(ctx context.Context) (reached bool) {
	out, fetch, opts, socket := w.out, w.fetch, w.opts, w.socket
	tick := time.NewTicker(w.every)
	defer tick.Stop()
	var lastSeq uint64
	down, first := false, true
	for {
		snap, err := fetch(ctx)
		switch {
		case err != nil && !down && ctx.Err() == nil:
			fmt.Fprintf(out, "headroom: daemon not reachable at %s, retrying (%v)\n", socket, err)
			down = true
		case err == nil && (down || first || snap.Seq != lastSeq):
			if !first {
				fmt.Fprintln(out)
			}
			view.Render(out, snap, opts())
			lastSeq, down, first, reached = snap.Seq, false, false, true
		}
		select {
		case <-ctx.Done():
			return reached
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
