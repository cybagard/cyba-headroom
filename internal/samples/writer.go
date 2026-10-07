package samples

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// dayLayout names a day's file: samples/2026-10-07.jsonl, gzipped once the
// day is over.
const dayLayout = "2006-01-02"

// Writer appends samples to one JSONL file per local day. The daemon offers
// it every published snapshot; one goroutine (Run) does all file work, so a
// slow or failing disk never delays a tick.
type Writer struct {
	dir       string
	retention time.Duration
	log       *slog.Logger
	loc       *time.Location
	now       func() time.Time

	ch      chan *protocol.Snapshot
	dropped atomic.Uint64

	// Owned by the Run goroutine.
	f           *os.File
	day         string
	lastErr     string
	lastDropped uint64
}

// NewWriter returns a writer into dir that keeps retention's worth of days.
func NewWriter(dir string, retention time.Duration, log *slog.Logger) *Writer {
	return &Writer{
		dir: dir, retention: retention, log: log, loc: time.Local, now: time.Now,
		ch: make(chan *protocol.Snapshot, 1),
	}
}

// Offer queues s for writing without blocking. If the writer is still busy
// with the previous sample, s is dropped and counted.
func (w *Writer) Offer(s *protocol.Snapshot) {
	select {
	case w.ch <- s:
	default:
		w.dropped.Add(1)
	}
}

// Run writes offered samples until ctx ends, then writes any still queued
// and closes the file.
func (w *Writer) Run(ctx context.Context) {
	defer w.close()
	for {
		select {
		case s := <-w.ch:
			w.write(s)
		case <-ctx.Done():
			select {
			case s := <-w.ch:
				w.write(s)
			default:
			}
			return
		}
	}
}

// write appends one sample, opening the day's file as needed.
func (w *Writer) write(s *protocol.Snapshot) {
	line, err := json.Marshal(FromSnapshot(s))
	if err == nil {
		err = w.open(s.CollectedAt.In(w.loc).Format(dayLayout))
	}
	if err == nil {
		_, err = w.f.Write(append(line, '\n'))
	}
	w.report(err)
}

// open makes day's file the current one.
func (w *Writer) open(day string) error {
	if w.f != nil && w.day == day {
		return nil
	}
	w.close()
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(w.dir, day+".jsonl"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := endPartialLine(f); err != nil {
		_ = f.Close()
		return err
	}
	w.f, w.day = f, day
	w.housekeep()
	return nil
}

// endPartialLine ends a line a crash left unfinished, so the next sample
// starts on its own line; the reader skips the broken one.
func endPartialLine(f *os.File) error {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return err
	}
	if last[0] != '\n' {
		_, err = f.Write([]byte{'\n'})
	}
	return err
}

// housekeep deletes days past retention and gzips every other day but the
// current one: at start (days left by an earlier run) and on each new day.
// Failures are logged; they never stop the current day's writes.
func (w *Writer) housekeep() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		w.log.Warn("samples: housekeeping failed", "dir", w.dir, "err", err)
		return
	}
	cutoff := w.now().Add(-w.retention)
	for _, e := range entries {
		// Only Run writes here, so any temp file is left over from a crash.
		if strings.HasSuffix(e.Name(), ".tmp") && strings.Contains(e.Name(), ".jsonl.gz.") {
			_ = os.Remove(filepath.Join(w.dir, e.Name()))
			continue
		}
		day, gz, ok := parseName(e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(w.dir, e.Name())
		start, err := time.ParseInLocation(dayLayout, day, w.loc)
		if err != nil {
			continue
		}
		switch {
		case !start.AddDate(0, 0, 1).After(cutoff):
			err = os.Remove(path)
		case !gz && day != w.day:
			err = compress(path)
		}
		if err != nil {
			w.log.Warn("samples: housekeeping failed", "file", path, "err", err)
		}
	}
}

// parseName splits a day file's name: 2026-10-07.jsonl or 2026-10-07.jsonl.gz.
func parseName(name string) (day string, gz bool, ok bool) {
	base, gz := strings.CutSuffix(name, ".gz")
	day, ok = strings.CutSuffix(base, ".jsonl")
	if !ok || len(day) != len(dayLayout) {
		return "", false, false
	}
	if _, err := time.Parse(dayLayout, day); err != nil {
		return "", false, false
	}
	return day, gz, true
}

// compress moves path into path.gz. An existing archive is kept and the new
// lines are appended as a further gzip member, which readers see as one
// stream. The archive is replaced by rename, so a crash leaves either the old
// or the new one, never half of one.
func compress(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dst := path + ".gz"
	prev, err := os.ReadFile(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	_, err = tmp.Write(prev)
	if err == nil {
		zw := gzip.NewWriter(tmp)
		if _, err = zw.Write(src); err == nil {
			err = zw.Close()
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	return os.Remove(path)
}

func (w *Writer) close() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
}

// report logs a write error once per change, and drops once per new count.
func (w *Writer) report(err error) {
	if err != nil {
		if msg := err.Error(); msg != w.lastErr {
			w.log.Warn("samples: write failed", "dir", w.dir, "err", err)
			w.lastErr = msg
		}
		return
	}
	if w.lastErr != "" {
		w.log.Info("samples: writing again", "dir", w.dir)
		w.lastErr = ""
	}
	if d := w.dropped.Load(); d != w.lastDropped {
		w.log.Warn("samples: dropped while the writer was busy", "total", d)
		w.lastDropped = d
	}
}
