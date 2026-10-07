package samples

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
