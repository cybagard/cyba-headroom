package logfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/logfile"
)

func open(t *testing.T, limit int64) (*logfile.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logs", "daemon.log")
	f, err := logfile.Open(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWritesPrivately(t *testing.T) {
	f, path := open(t, 1<<20)
	if _, err := f.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "hello\n" {
		t.Fatalf("got %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
}

func TestRotatesAtTheCapKeepingOneOldFile(t *testing.T) {
	f, path := open(t, 10)
	for _, l := range []string{"aaaa\n", "bbbb\n", "cccc\n", "dddd\n", "eeee\n"} {
		if _, err := f.Write([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	// 10 bytes fit two lines; each further write that would pass the cap
	// rotates first, so no line is split across files.
	if got := read(t, path); got != "eeee\n" {
		t.Errorf("current = %q", got)
	}
	if got := read(t, path+".1"); got != "cccc\ndddd\n" {
		t.Errorf("previous = %q", got)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("kept more than one old file")
	}
	if fi, _ := os.Stat(path + ".1"); fi.Mode().Perm() != 0o600 {
		t.Errorf("old file mode %v", fi.Mode().Perm())
	}
}

func TestAppendsAfterRestart(t *testing.T) {
	f, path := open(t, 1<<20)
	_, _ = f.Write([]byte("one\n"))
	_ = f.Close()
	g, err := logfile.Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	_, _ = g.Write([]byte("two\n"))
	if got := read(t, path); got != "one\ntwo\n" {
		t.Fatalf("got %q", got)
	}
}

func TestStartsOverWhenTheExistingFileIsFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := logfile.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write([]byte("new\n"))
	if got := read(t, path); got != "new\n" {
		t.Fatalf("got %q", got)
	}
}
