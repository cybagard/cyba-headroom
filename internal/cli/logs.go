package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// runLogs prints the tail of the daemon's log and crash log; -f follows the
// log, across rotation (#22).
func runLogs(e Env) int {
	follow := false
	for _, a := range e.Args[2:] {
		if a != "-f" {
			fmt.Fprintf(e.Stderr, "headroom: logs: unknown argument %q\n", a)
			return 2
		}
		follow = true
	}
	home := e.Getenv("HOME")
	if home == "" {
		fmt.Fprintln(e.Stderr, "headroom: HOME is not set")
		return 1
	}
	dir, logPath, crashPath := logPaths(home)
	if empty(logPath) && empty(crashPath) && !follow {
		fmt.Fprintf(e.Stdout, "no logs yet in %s (is the daemon installed? `headroom install`)\n", dir)
		return 0
	}
	// The follower carries on from the open file where the tail stopped, so a
	// line logged in between is shown once (#149).
	f := &follower{path: logPath, out: e.Stdout}
	if lf, err := os.Open(logPath); err == nil {
		_, _ = lf.Seek(tailFile(e.Stdout, lf, 50), io.SeekStart)
		f.f = lf
	}
	defer f.close()
	tail(e.Stdout, crashPath, 20)
	if !follow {
		return 0
	}
	if e.tailed != nil {
		e.tailed()
	}
	ctx, stop := signalContext(e, os.Interrupt, syscall.SIGTERM)
	defer stop()
	every := e.watchEvery
	if every == 0 {
		every = 500 * time.Millisecond
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-tick.C:
			f.poll()
		}
	}
}

// follower copies what is appended to a log file, and starts over at the
// top of a new file when the old one is rotated away.
type follower struct {
	path string
	out  io.Writer
	f    *os.File
}

func (fl *follower) poll() {
	if fl.f == nil {
		f, err := os.Open(fl.path)
		if err != nil {
			return
		}
		fl.f = f
	}
	// Copy what the open file has, then check whether the path now names a
	// different (rotated-in) file; if so, read the rest of the old one and
	// switch to the new one from the start.
	_, _ = io.Copy(fl.out, fl.f)
	cur, err := os.Stat(fl.path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	open, err2 := fl.f.Stat()
	if err != nil || err2 != nil || os.SameFile(cur, open) {
		return
	}
	fl.close()
	if f, err := os.Open(fl.path); err == nil {
		fl.f = f
		_, _ = io.Copy(fl.out, fl.f)
	}
}

func (fl *follower) close() {
	if fl.f != nil {
		_ = fl.f.Close()
		fl.f = nil
	}
}

// empty reports whether path is missing or has nothing in it.
func empty(path string) bool {
	fi, err := os.Stat(path)
	return err != nil || fi.Size() == 0
}
