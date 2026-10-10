package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonLogsToAFile(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(dir+"/config.toml", []byte("[docker]\nsocket = \""+dir+"/none.sock\"\n[samples]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "logs", "daemon.log")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errb syncBuffer
	go func() {
		done <- Run(Env{Args: []string{"headroom", "daemon", "--log", logPath}, Stdout: &errb, Stderr: &errb,
			Getenv: func(k string) string { return map[string]string{"HEADROOM_CONFIG_DIR": dir}[k] }, Context: ctx})
	}()
	waitFor(t, func() bool { b, _ := os.ReadFile(logPath); return strings.Contains(string(b), "daemon started") })
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if fi, _ := os.Stat(logPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", fi.Mode().Perm())
	}
	if strings.Contains(errb.String(), "daemon started") {
		t.Error("logged to stderr as well")
	}
}

func TestCommandArgsAreChecked(t *testing.T) {
	for _, args := range [][]string{
		{"daemon", "--bogus"}, {"daemon", "--log"}, {"install", "--bin"}, {"install", "extra"},
		{"uninstall", "--bogus"}, {"logs", "-x"},
	} {
		code, _, stderr := run(t, map[string]string{"HOME": t.TempDir()}, append([]string{"headroom"}, args...)...)
		if code != 2 {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
}

func TestUnreachableHintMentionsInstall(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_, _, stderr := run(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, "headroom")
	if !strings.Contains(stderr, "headroom install") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func logDirIn(home string) string { return filepath.Join(home, "Library", "Logs", "headroom") }

func TestLogsPrintsTheTail(t *testing.T) {
	home := t.TempDir()
	dir := logDirIn(home)
	_ = os.MkdirAll(dir, 0o700)
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	_ = os.WriteFile(filepath.Join(dir, "daemon.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "daemon.stderr.log"), []byte("panic: boom\n"), 0o600)
	code, out, _ := run(t, map[string]string{"HOME": home}, "headroom", "logs")
	if code != 0 || strings.Count(out, "line ") != 50 || !strings.Contains(out, "panic: boom") {
		t.Fatalf("exit %d, out:\n%s", code, out)
	}
}

func TestLogsWithoutLogsSaysSo(t *testing.T) {
	code, out, stderr := run(t, map[string]string{"HOME": t.TempDir()}, "headroom", "logs")
	if code != 0 || !strings.Contains(out+stderr, "no logs") {
		t.Fatalf("exit %d out %q stderr %q", code, out, stderr)
	}
}

func TestLogsFollowAcrossRotation(t *testing.T) {
	home := t.TempDir()
	dir := logDirIn(home)
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "daemon.log")
	_ = os.WriteFile(path, []byte("first-line\n"), 0o600)
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Run(Env{Args: []string{"headroom", "logs", "-f"}, Stdout: &out, Stderr: &out,
			Getenv: func(k string) string { return map[string]string{"HOME": home}[k] }, Context: ctx,
			watchEvery: 5 * time.Millisecond})
	}()
	waitFor(t, func() bool { return strings.Contains(out.String(), "first-line") })
	appendTo := func(p, s string) {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	appendTo(path, "before rotation\n")
	waitFor(t, func() bool { return strings.Contains(out.String(), "before rotation") })
	_ = os.Rename(path, path+".1")
	appendTo(path, "after rotation\n")
	waitFor(t, func() bool { return strings.Contains(out.String(), "after rotation") })
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Count(out.String(), "first-line") != 1 {
		t.Errorf("repeated lines:\n%s", out.String())
	}
}

// A line logged after the tail is shown but before the follow begins is
// shown once (#149).
func TestLogsFollowShowsALineLoggedAsItStarts(t *testing.T) {
	home := t.TempDir()
	dir := logDirIn(home)
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "daemon.log")
	_ = os.WriteFile(path, []byte("first-line\n"), 0o600)
	appendTo := func(s string) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(s)
		return errors.Join(err, f.Close())
	}
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Run(Env{Args: []string{"headroom", "logs", "-f"}, Stdout: &out, Stderr: &out,
			Getenv: func(k string) string { return map[string]string{"HOME": home}[k] }, Context: ctx,
			watchEvery: 5 * time.Millisecond,
			tailed: func() {
				if err := appendTo("in the window\n"); err != nil {
					t.Error(err)
				}
			}})
	}()
	waitFor(t, func() bool { return strings.Contains(out.String(), "first-line") })
	if err := appendTo("later\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "later") })
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := out.String(); strings.Count(got, "first-line") != 1 || strings.Count(got, "in the window") != 1 {
		t.Errorf("want first-line and the window's line once each:\n%s", got)
	}
}

func TestDaemonStartupErrorsReachTheLog(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = os.WriteFile(dir+"/config.toml", []byte("[polcy]\n"), 0o600) // typo: an unknown key
	logPath := filepath.Join(dir, "logs", "daemon.log")
	code, _, stderr := run(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, "headroom", "daemon", "--log", logPath)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(logPath)
	if !strings.Contains(string(b), "unknown keys") || !strings.Contains(stderr, "unknown keys") {
		t.Fatalf("log %q, stderr %q: want the error in both", b, stderr)
	}
}

func TestTrimCrashLog(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(strings.Repeat("x", 100))
	trimCrashLog(f, 50)
	if fi, _ := f.Stat(); fi.Size() != 0 {
		t.Fatalf("size %d after trim", fi.Size())
	}
	_, _ = f.WriteString("small")
	trimCrashLog(f, 50)
	if fi, _ := f.Stat(); fi.Size() != 5 {
		t.Fatalf("trimmed a small log: %d", fi.Size())
	}
}

func TestLogsHintWhenInstalledButEmpty(t *testing.T) {
	home := t.TempDir()
	_, logPath, crash := logPaths(home)
	_ = os.MkdirAll(filepath.Dir(crash), 0o700)
	_ = os.WriteFile(crash, nil, 0o600) // what install leaves
	_ = logPath
	code, out, _ := run(t, map[string]string{"HOME": home}, "headroom", "logs")
	if code != 0 || !strings.Contains(out, "no logs yet") {
		t.Fatalf("exit %d out %q", code, out)
	}
}

func TestTailReadsOnlyTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	var b strings.Builder
	for i := 0; i < 100000; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("y", 40))
		b.WriteString("\n")
	}
	b.WriteString("last line\n")
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
	var out strings.Builder
	tail(&out, path, 3)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || lines[3] != "last line" {
		t.Fatalf("got %q", out.String())
	}
}
