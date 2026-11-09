package vmproc_test

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// procargs2 builds a kern.procargs2 buffer: argc, the exec path, NUL
// padding, the arguments, then the environment.
func procargs2(exec string, args, env []string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(args)))
	b = append(b, exec...)
	b = append(b, 0, 0, 0, 0)
	for _, s := range append(args, env...) {
		b = append(append(b, s...), 0)
	}
	return append(b, 0, 0)
}

func TestPathFromProcArgs(t *testing.T) {
	for _, c := range []struct {
		name string
		buf  []byte
		want string
		ok   bool
	}{
		{"PATH after the args", procargs2("/bin/zsh", []string{"-zsh", "PATH=/not/env"}, []string{"SECRET=x", "PATH=/Users/dev/bin:/usr/bin", "TERM=xterm"}), "/Users/dev/bin:/usr/bin", true},
		{"no PATH", procargs2("/bin/zsh", []string{"zsh"}, []string{"HOME=/Users/dev"}), "", false},
		{"empty PATH", procargs2("/bin/zsh", []string{"zsh"}, []string{"PATH="}), "", true},
		{"no environment", procargs2("/bin/zsh", []string{"zsh"}, nil), "", false},
		{"short", []byte{1, 0}, "", false},
		{"argc past the end", procargs2("/bin/zsh", []string{"zsh"}, nil)[:9], "", false},
	} {
		got, err := vmproc.PathFromProcArgs(c.buf)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("%s: = %q, %v; want %q, ok %v", c.name, got, err, c.want, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the error names another variable: %v", c.name, err)
		}
	}
}
