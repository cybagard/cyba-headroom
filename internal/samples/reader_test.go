package samples

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

// A worktree's project survives a write and a read, and a sample with none
// loads with no project (#102).
func TestWorktreeProjectSurvivesWriteAndRead(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	s := snapAt(day1)
	s.Orca = &protocol.Orca{Installed: true, Running: true, Worktrees: []protocol.Worktree{
		{ID: "w1", Path: "/Users/dev/src/project-a", Name: "main", Project: "project-a"}}}
	w.write(s)
	w.close()
	f, err := os.OpenFile(filepath.Join(dir, "2026-10-07.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":2,"t":"2026-10-07T13:00:00Z","worktrees":[{"id":"w2","path":"/Users/dev/src/project-b","name":"main","memory_bytes":0,"cpu_percent":0}]}` + "\n")
	_ = f.Close()
	var got []Worktree
	if _, err := Read(dir, time.Time{}, day1.AddDate(0, 0, 1), func(s Sample) error {
		got = append(got, s.Worktrees...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "w1" || got[0].Project != "project-a" || got[1].ID != "w2" || got[1].Project != "" || got[1].Name != "main" {
		t.Fatalf("read back %+v", got)
	}
}

// A sample with two engines, a Podman VM and an unknown VM survives a write
// and a read (#229).
func TestEnginesAndVMsSurviveWriteAndRead(t *testing.T) {
	w, dir, _ := newTestWriter(t)
	s := snapAt(day1)
	s.Engines = []protocol.Engine{
		{Kind: protocol.EngineDockerDesktop, Socket: "/Users/dev/.docker/run/docker.sock", ID: "AAAA", Running: true,
			VMRunning: true, VMLimitBytes: 8 << 30, VMFootprintBytes: 4 << 30,
			Containers: []protocol.Container{{ID: "c1", Name: "db", Image: "postgres:17", MemoryBytes: 1 << 30,
				Labels: map[string]string{protocol.ComposeWorkingDirLabel: "/Users/dev/project-a"}, Mounts: []string{"/Users/dev/project-a/data"}}}},
		{Kind: protocol.EnginePodman, Socket: "/Users/dev/.local/share/podman.sock", ID: "podman:dev", Machine: "dev", Running: true,
			VMError: "no VM process", VMLimitBytes: 2 << 30,
			Containers: []protocol.Container{{ID: "c2", Name: "cache", Image: "redis:7", MemoryBytes: 1 << 20}}},
	}
	s.Podman = &protocol.Podman{VMs: []protocol.PodmanVM{{Machine: "dev", PID: 300, FootprintBytes: 3 << 30}}}
	s.VMs = &protocol.VMs{Other: []protocol.OtherVM{{PID: 400, Name: "qemu-system-aarch64", FootprintBytes: 5 << 30}}}
	w.write(s)
	w.close()
	var got []Sample
	st, err := Read(dir, time.Time{}, day1.AddDate(0, 0, 1), func(s Sample) error {
		got = append(got, s)
		return nil
	})
	if err != nil || st.Samples != 1 || len(got) != 1 {
		t.Fatalf("read %d samples, stats %+v, err %v", len(got), st, err)
	}
	want := Sample{
		Engines: []Engine{
			{Kind: "docker-desktop", Socket: "/Users/dev/.docker/run/docker.sock", Running: true,
				VMRunning: true, VMLimitBytes: 8 << 30, VMFootprintBytes: 4 << 30,
				Containers: []Container{{Name: "db", Image: "postgres:17", MemoryBytes: 1 << 30,
					ComposeDir: "/Users/dev/project-a", Mounts: []string{"/Users/dev/project-a/data"}}}},
			{Kind: "podman", Socket: "/Users/dev/.local/share/podman.sock", Machine: "dev", Running: true,
				VMUnknown: true, VMLimitBytes: 2 << 30,
				Containers: []Container{{Name: "cache", Image: "redis:7", MemoryBytes: 1 << 20}}},
		},
		Podman:   []PodmanVM{{Machine: "dev", PID: 300, FootprintBytes: 3 << 30}},
		OtherVMs: []OtherVM{{PID: 400, Name: "qemu-system-aarch64", FootprintBytes: 5 << 30}},
	}
	g := got[0]
	if g.V != 2 || !reflect.DeepEqual(g.Engines, want.Engines) || !reflect.DeepEqual(g.Podman, want.Podman) ||
		!reflect.DeepEqual(g.OtherVMs, want.OtherVMs) {
		t.Fatalf("read back v%d\nengines %+v\npodman %+v\nother_vms %+v\nwant\n%+v", g.V, g.Engines, g.Podman, g.OtherVMs, want)
	}
}

// A sample of schema v1 is skipped and counted as outdated, not an error and
// not damage (#229).
func TestReadSkipsV1SamplesWithANote(t *testing.T) {
	dir := threeDays(t)
	f, err := os.OpenFile(filepath.Join(dir, "2026-10-08.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":1,"t":"2026-10-08T14:00:00Z","docker":{"vm_running":true,"vm_limit_bytes":8589934592,"vm_footprint_bytes":4294967296}}` + "\n")
	_ = f.Close()
	got, st := readAll(t, dir, time.Time{}, day1.AddDate(1, 0, 0))
	if len(got) != 6 || st.Outdated != 1 || st.Skipped != 0 {
		t.Fatalf("got %d samples, stats %+v; want 6 read and 1 outdated", len(got), st)
	}
	// One outside the range is not counted: its day file is read only
	// because it may overlap.
	if _, st = readAll(t, dir, day1.Add(24*time.Hour), day1.Add(25*time.Hour+time.Minute)); st.Outdated != 0 {
		t.Fatalf("stats %+v; want no outdated sample in range", st)
	}
}

// A sample with more engines or VMs than a Mac runs is skipped as broken: a
// crafted line must not make a replay slow (#229, review B1).
func TestReadSkipsSamplesWithTooManyEnginesOrVMs(t *testing.T) {
	line := func(at string, key, item string, n int) string {
		return `{"v":2,"t":"` + at + `","` + key + `":[` + strings.TrimSuffix(strings.Repeat(item+",", n), ",") + `]}` + "\n"
	}
	dir := threeDays(t)
	f, err := os.OpenFile(filepath.Join(dir, "2026-10-08.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(line("2026-10-08T14:00:00Z", "engines", `{}`, 64) + line("2026-10-08T14:00:05Z", "engines", `{}`, 65) +
		line("2026-10-08T14:00:10Z", "podman", `{}`, 256) + line("2026-10-08T14:00:15Z", "podman", `{}`, 257) +
		line("2026-10-08T14:00:20Z", "other_vms", `{}`, 256) + line("2026-10-08T14:00:25Z", "other_vms", `{}`, 257))
	_ = f.Close()
	got, st := readAll(t, dir, time.Time{}, day1.AddDate(1, 0, 0))
	if len(got) != 9 || st.Skipped != 3 {
		t.Fatalf("got %d samples, stats %+v; want 9 read and the 3 over the caps skipped", len(got), st)
	}
}
