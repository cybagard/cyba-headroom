// Package logfile is a size-capped log file for the daemon (#22): at the cap
// it moves the file to <path>.1 and starts a new one, so the log never takes
// more than about twice the cap.
package logfile

import (
	"os"
	"path/filepath"
	"sync"
)

// File is an append-only log file that rotates at a size cap. It is safe for
// concurrent writers.
type File struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens path for appending (0600, its directory 0700), rotating first
// if it is already at the cap.
func Open(path string, limit int64) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &File{path: path, max: limit}
	if err := l.open(); err != nil {
		return nil, err
	}
	if l.size >= limit {
		if err := l.rotate(); err != nil {
			_ = l.f.Close()
			return nil, err
		}
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// rotate moves the current file to path.1, replacing an older one, and
// opens a new one. If the move fails, the old file is reopened and logging
// goes on there: a log that cannot rotate must not stop the daemon. Only a
// failure to reopen is an error.
func (l *File) rotate() error {
	_ = l.f.Close()
	_ = os.Rename(l.path, l.path+".1")
	return l.open()
}

// Write appends p, rotating first if p would take the file past the cap, so
// a line is never split between files.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
