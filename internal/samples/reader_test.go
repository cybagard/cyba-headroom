package samples

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// threeDays writes samples at 12:00 and 13:00 on Oct 6, 7 and 8; the first
// two days end up gzipped.
func threeDays(t *testing.T) string {
	t.Helper()
	w, dir, _ := newTestWriter(t)
	for d := -1; d <= 1; d++ {
		at := day1.AddDate(0, 0, d)
		w.now = func() time.Time { return at }
		w.write(snapAt(at))
		w.write(snapAt(at.Add(time.Hour)))
	}
	w.close()
	return dir
}

func readAll(t *testing.T, dir string, from, to time.Time) ([]time.Time, Stats) {
	t.Helper()
	var got []time.Time
	st, err := Read(dir, from, to, func(s Sample) error {
		got = append(got, s.T)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, st
}

func TestReadAcrossPlainAndGzippedDaysInOrder(t *testing.T) {
	dir := threeDays(t)
	if !exists(filepath.Join(dir, "2026-10-06.jsonl.gz")) || !exists(filepath.Join(dir, "2026-10-08.jsonl")) {
		t.Fatal("fixture: want gzipped and plain days")
	}
	got, st := readAll(t, dir, time.Time{}, day1.AddDate(1, 0, 0))
	if len(got) != 6 || st.Samples != 6 || st.Skipped != 0 {
		t.Fatalf("got %d samples, stats %+v", len(got), st)
	}
	for i := 1; i < len(got); i++ {
		if !got[i].After(got[i-1]) {
			t.Fatalf("out of order: %v", got)
		}
	}
}

func TestReadFiltersByTime(t *testing.T) {
	dir := threeDays(t)
	// [Oct 6 13:00, Oct 8 12:00): from is inclusive, to exclusive.
	got, _ := readAll(t, dir, day1.Add(-23*time.Hour), day1.Add(24*time.Hour))
	if len(got) != 3 || !got[0].Equal(day1.Add(-23*time.Hour)) || !got[2].Equal(day1.Add(time.Hour)) {
		t.Fatalf("got %v", got)
	}
}

func TestReadSkipsBrokenLinesAndUnknownVersions(t *testing.T) {
	dir := threeDays(t)
	f, err := os.OpenFile(filepath.Join(dir, "2026-10-08.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":99,"t":"2026-10-08T14:00:00Z"}` + "\n" + "not json\n" + `{"v":1,"t":"2026-10-08T15:0`)
	_ = f.Close()
	got, st := readAll(t, dir, time.Time{}, day1.AddDate(1, 0, 0))
	if len(got) != 6 || st.Skipped != 3 {
		t.Fatalf("got %d samples, stats %+v; want 6 read and 3 skipped", len(got), st)
	}
}

func TestReadSkipsDaysOutsideTheRangeUnopened(t *testing.T) {
	dir := threeDays(t)
	// A corrupt archive for a day far outside the range is never opened.
	if err := os.WriteFile(filepath.Join(dir, "2026-09-01.jsonl.gz"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, st := readAll(t, dir, day1.Add(-48*time.Hour), day1.AddDate(1, 0, 0))
	if st.BadFiles != 0 {
		t.Fatalf("stats %+v: opened a day outside the range", st)
	}
	_, st = readAll(t, dir, time.Time{}, day1.AddDate(1, 0, 0))
	if st.BadFiles != 1 || st.Samples != 6 {
		t.Fatalf("stats %+v: a corrupt archive must be counted, not fatal", st)
	}
}

func TestReadStopsOnCallbackError(t *testing.T) {
	dir := threeDays(t)
	stop := errors.New("enough")
	n := 0
	_, err := Read(dir, time.Time{}, day1.AddDate(1, 0, 0), func(Sample) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err = %v after %d calls", err, n)
	}
}

func TestReadMissingDirIsEmpty(t *testing.T) {
	got, st := readAll(t, filepath.Join(t.TempDir(), "none"), time.Time{}, day1)
	if len(got) != 0 || st != (Stats{}) {
		t.Fatalf("got %v %+v", got, st)
	}
}

func TestReadDropsDuplicatesFromAnInterruptedGzip(t *testing.T) {
	// A crash between renaming the archive into place and removing the plain
	// file leaves both, so the next run appends the same lines again. A
	// reader racing the gzip can see both files, too. Each sample counts once.
	dir := threeDays(t)
	plain := filepath.Join(dir, "2026-10-07.jsonl")
	w, _, _ := newTestWriter(t)
	w.dir = dir
	w.now = func() time.Time { return day1 }
	w.write(snapAt(day1)) // appends to 10-07, which was archived: a duplicate T
	w.close()
	if !exists(plain) {
		t.Fatal("fixture: want a plain file next to the archive")
	}
	got, st := readAll(t, dir, time.Time{}, day1.AddDate(1, 0, 0))
	if len(got) != 6 || st.Duplicates != 1 {
		t.Fatalf("got %d samples, stats %+v; want 6 and 1 duplicate", len(got), st)
	}
}

func TestReadSkipsAnOverlongLineOnly(t *testing.T) {
	dir := threeDays(t)
	f, err := os.OpenFile(filepath.Join(dir, "2026-10-08.jsonl"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_, _ = f.Seek(0, 0)
	_, _ = f.Write([]byte(strings.Repeat("x", maxLine+10) + "\n"))
	_, _ = f.Write(b)
	_ = f.Close()
	got, st := readAll(t, dir, time.Time{}, day1.AddDate(1, 0, 0))
	if len(got) != 6 || st.Skipped != 1 || st.BadFiles != 0 {
		t.Fatalf("got %d samples, stats %+v; want the long line skipped and the rest read", len(got), st)
	}
}

func TestModelIdlenessSurvivesWriteAndRead(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	ttl, last := 90*time.Minute, day1.Add(-time.Hour)
	s := snapAt(day1)
	s.LMStudio = &protocol.LMStudio{Installed: true, Running: true, Models: []protocol.LoadedModel{
		{Key: "m", SizeBytes: 1 << 30, Status: "generating", TTL: &ttl, LastUsedAt: &last}}}
	w.write(s)
	w.close()
	var got []Model
	if _, err := Read(dir, time.Time{}, day1.AddDate(0, 0, 1), func(s Sample) error {
		got = s.LMStudio.Models
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TTL == nil || *got[0].TTL != ttl || got[0].LastUsedAt == nil ||
		!got[0].LastUsedAt.Equal(last) || got[0].Status != "generating" {
		t.Fatalf("read back %+v", got)
	}
}
