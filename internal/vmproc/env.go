package vmproc

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// errNoPath is a process environment without PATH.
var errNoPath = errors.New("no PATH in the environment")

// pathFromProcArgs returns the PATH value from a kern.procargs2 buffer: a
// 4-byte argc, the exec path, NUL padding, argc arguments, then the
// environment, each NUL-terminated, ending at an empty string. Nothing
// else in the environment is kept: it can hold secrets.
func pathFromProcArgs(b []byte) (string, error) {
	if len(b) < 4 {
		return "", errNoPath
	}
	argc := int(binary.LittleEndian.Uint32(b))
	_, rest, ok := bytes.Cut(b[4:], []byte{0})
	if !ok {
		return "", errNoPath
	}
	rest = bytes.TrimLeft(rest, "\x00")
	for range argc {
		if _, rest, ok = bytes.Cut(rest, []byte{0}); !ok {
			return "", errNoPath
		}
	}
	for {
		var kv []byte
		if kv, rest, ok = bytes.Cut(rest, []byte{0}); !ok || len(kv) == 0 {
			return "", errNoPath
		}
		if v, found := bytes.CutPrefix(kv, []byte("PATH=")); found {
			return string(v), nil
		}
	}
}
