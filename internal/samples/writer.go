package samples

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	// Housekeeping (gzip, retention) runs on its own goroutine, so a day's
	// gzip never delays a write. One run at a time; a request while one runs
	// makes it run again for the newest day.
	hk              sync.WaitGroup
	hkBusy, hkAgain atomic.Bool
	beforeHousekeep func() // test hook

	// mu makes switching to a day file and gzipping a day file exclusive, so
	// housekeeping never gzips the file being written.
	mu      sync.Mutex
	current string
	openAt  time.Time // when current was opened; retention counts from it

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

// Run writes offered samples until ctx ends, then writes any still queued,
// closes the file and waits for housekeeping.
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
		if _, err = w.f.Write(append(line, '\n')); err != nil {
			// The write may have stopped mid-line (disk full). Reopening
			// ends that line before the next sample.
			w.closeFile()
		}
	}
	w.report(err)
}

// open makes day's file the current one.
func (w *Writer) open(day string) error {
	if w.f != nil && w.day == day {
		return nil
	}
	w.closeFile()
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	w.mu.Lock()
	w.current, w.openAt = day, w.now()
	f, err := os.OpenFile(filepath.Join(w.dir, day+".jsonl"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if err := endPartialLine(f); err != nil {
		_ = f.Close()
		return err
	}
	w.f, w.day = f, day
	w.startHousekeeping()
	return nil
}

// startHousekeeping runs housekeep in the background, or asks a run in
// progress to go again.
func (w *Writer) startHousekeeping() {
	if !w.hkBusy.CompareAndSwap(false, true) {
		w.hkAgain.Store(true)
		return
	}
	w.hk.Go(func() {
		defer w.hkBusy.Store(false)
		if w.beforeHousekeep != nil {
			w.beforeHousekeep()
		}
		for {
			w.housekeep()
			if !w.hkAgain.Swap(false) {
				return
			}
		}
	})
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
		w.mu.Lock()
		switch {
		case day == w.current:
		case !start.AddDate(0, 0, 1).After(w.openAt.Add(-w.retention)):
			err = os.Remove(path)
		case !gz:
			err = compress(path)
		}
		w.mu.Unlock()
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

// compress moves path into path.gz, streaming, so a day's size never sits in
// memory. An existing archive is kept and the new lines are appended as a
// further gzip member, which readers see as one stream. The archive is
// replaced by rename, so it is always whole. A crash between that rename and
// removing path leaves both, and the next run appends path again; readers
// drop the repeated samples (see Read).
func compress(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst := path + ".gz"
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	err = tmp.Chmod(0o600)
	if err == nil {
		err = copyFile(tmp, dst)
	}
	if err == nil {
		zw := gzip.NewWriter(tmp)
		if _, err = io.Copy(zw, src); err == nil {
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

// copyFile appends the file at path to w; a missing file adds nothing.
func copyFile(w io.Writer, path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// close closes the current file and waits for housekeeping to finish.
func (w *Writer) close() {
	w.closeFile()
	w.hk.Wait()
}

func (w *Writer) closeFile() {
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
