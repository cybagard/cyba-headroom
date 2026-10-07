package samples

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

var day1 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func newTestWriter(t *testing.T) (*Writer, string, *bytes.Buffer) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "samples")
	var log bytes.Buffer
	w := NewWriter(dir, 30*24*time.Hour, slog.New(slog.NewTextHandler(&log, nil)))
	w.loc = time.UTC
	w.now = func() time.Time { return day1 }
	return w, dir, &log
}

func snapAt(t time.Time) *protocol.Snapshot {
	return &protocol.Snapshot{CollectedAt: t, Host: &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"}}
}

// lines returns the decoded samples in a plain JSONL file.
func lines(t *testing.T, path string) []Sample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []Sample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, s)
	}
	return out
}

func TestWriterAppendsOneLinePerSample(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	w.write(snapAt(day1))
	w.write(snapAt(day1.Add(5 * time.Second)))
	w.close()

	path := filepath.Join(dir, "2026-10-07.jsonl")
	got := lines(t, path)
	if len(got) != 2 || !got[1].T.Equal(day1.Add(5*time.Second)) || got[0].Host.TotalBytes != 64<<30 {
		t.Fatalf("got %+v", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestOfferNeverBlocks(t *testing.T) {
	w, _, _ := newTestWriter(t)
	// Nothing drains the channel: the first offer fills it, the rest drop.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			w.Offer(snapAt(day1))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Offer blocked")
	}
	if got := w.dropped.Load(); got != 4 {
		t.Fatalf("dropped = %d, want 4", got)
	}
}

func TestRunWritesOfferedSamplesAndFlushesOnStop(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()
	w.Offer(snapAt(day1))
	cancel()
	<-stopped
	if got := lines(t, filepath.Join(dir, "2026-10-07.jsonl")); len(got) != 1 {
		t.Fatalf("got %d samples, want the offered one flushed", len(got))
	}
}

// gzLines returns the decoded samples in a gzipped day file.
func gzLines(t *testing.T, path string) []Sample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var out []Sample
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s Sample
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, s)
	}
	return out
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestNewDayGzipsThePrevious(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	w.write(snapAt(day1))
	w.write(snapAt(day1.Add(time.Hour)))
	w.now = func() time.Time { return day1.Add(24 * time.Hour) }
	w.write(snapAt(day1.Add(24 * time.Hour)))
	w.close()

	if exists(filepath.Join(dir, "2026-10-07.jsonl")) {
		t.Error("previous day left uncompressed")
	}
	gz := filepath.Join(dir, "2026-10-07.jsonl.gz")
	if got := gzLines(t, gz); len(got) != 2 {
		t.Fatalf("gzipped day has %d samples, want 2", len(got))
	}
	if fi, _ := os.Stat(gz); fi.Mode().Perm() != 0o600 {
		t.Errorf("gz mode %v, want 0600", fi.Mode().Perm())
	}
	if got := lines(t, filepath.Join(dir, "2026-10-08.jsonl")); len(got) != 1 {
		t.Fatalf("new day has %d samples, want 1", len(got))
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}

func TestLeftoverDaysAreGzippedAtStart(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "2026-10-05.jsonl")
	if err := os.WriteFile(old, []byte(`{"v":1,"t":"2026-10-05T10:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.write(snapAt(day1))
	w.close()
	if exists(old) || len(gzLines(t, old+".gz")) != 1 {
		t.Fatal("a day left by an earlier run was not gzipped")
	}
}

func TestGzipAppendsToAnExistingArchive(t *testing.T) {
	// The clock went back a day (time zone change): the old day gains lines
	// after it was archived. Nothing already archived may be lost.
	w, dir, _ := newTestWriter(t)
	w.write(snapAt(day1))
	w.write(snapAt(day1.Add(24 * time.Hour)))
	w.write(snapAt(day1.Add(time.Hour)))
	w.write(snapAt(day1.Add(25 * time.Hour)))
	w.close()
	if got := gzLines(t, filepath.Join(dir, "2026-10-07.jsonl.gz")); len(got) != 2 {
		t.Fatalf("archive has %d samples, want both", len(got))
	}
}

func TestRetentionDeletesOldDays(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	w.retention = 2 * 24 * time.Hour
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := []string{"2026-10-05.jsonl.gz", "2026-10-06.jsonl.gz", "notes.txt"}
	// A temp file left by a crash during gzip goes too.
	for _, n := range append([]string{"2026-10-01.jsonl.gz", "2026-10-04.jsonl.gz", "2026-10-03.jsonl", "2026-10-06.jsonl.gz.123.tmp"}, keep...) {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w.write(snapAt(day1))
	w.close()
	m, _ := filepath.Glob(filepath.Join(dir, "*"))
	var names []string
	for _, p := range m {
		names = append(names, filepath.Base(p))
	}
	want := append(slices.Clone(keep), "2026-10-07.jsonl")
	if strings.Join(names, " ") != strings.Join(sorted(want), " ") {
		t.Fatalf("files = %v, want %v", names, sorted(want))
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	slices.Sort(out)
	return out
}

func TestRestartAfterCrashStartsAFreshLine(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-10-07.jsonl")
	if err := os.WriteFile(path, []byte(`{"v":1,"t":"2026-10-07T10:00:00Z"}`+"\n"+`{"v":1,"t":"2026-1`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.write(snapAt(day1))
	w.close()
	b, _ := os.ReadFile(path)
	ls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(ls) != 3 || !strings.HasPrefix(ls[2], `{"v":1`) {
		t.Fatalf("file = %q, want the partial line closed and a fresh line after it", b)
	}
}

func TestWriteErrorsAreLoggedOncePerChange(t *testing.T) {
	w, dir, log := newTestWriter(t)
	// A file where the directory should be: every write fails the same way.
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w.write(snapAt(day1))
	}
	if n := strings.Count(log.String(), "write failed"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, log)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	w.write(snapAt(day1))
	w.close()
	if !strings.Contains(log.String(), "writing again") || len(lines(t, filepath.Join(dir, "2026-10-07.jsonl"))) != 1 {
		t.Fatalf("did not recover:\n%s", log)
	}
}

func TestFailedWriteStartsAFreshLineNextTime(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	w.write(snapAt(day1))
	path := filepath.Join(dir, "2026-10-07.jsonl")
	// A write cut short (disk full) leaves a fragment; the next write fails.
	if err := os.WriteFile(path, append(must(os.ReadFile(path)), `{"v":1,"t":"20`...), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = w.f.Close()
	w.write(snapAt(day1.Add(5 * time.Second))) // fails on the closed file
	w.write(snapAt(day1.Add(10 * time.Second)))
	w.close()
	b := must(os.ReadFile(path))
	ls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(ls) != 3 || !strings.HasPrefix(ls[2], `{"v":1,"t":"2026-10-07T12:00:10Z"`) {
		t.Fatalf("file = %q; want the fragment ended and the next sample on its own line", b)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestHousekeepingDoesNotHoldUpWrites(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	release := make(chan struct{})
	w.beforeHousekeep = func() { <-release }
	done := make(chan struct{})
	go func() {
		w.write(snapAt(day1)) // opens the day: starts housekeeping
		w.write(snapAt(day1.Add(5 * time.Second)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write waited for housekeeping")
	}
	close(release)
	w.close() // waits for housekeeping
	if got := lines(t, filepath.Join(dir, "2026-10-07.jsonl")); len(got) != 2 {
		t.Fatalf("got %d samples", len(got))
	}
}
