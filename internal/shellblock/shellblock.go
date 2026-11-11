// Package shellblock adds and removes the marked block that `headroom
// shell-setup` writes into a login shell's startup file (#370). It works on
// the file's content only; the caller reads and writes the file.
package shellblock

import (
	"errors"
	"strings"
)

// Marker lines. A begin line of any version starts a block; Apply replaces
// it with the current one.
const (
	beginPrefix = "# >>> headroom shell-setup "
	begin       = beginPrefix + "v1 >>> (do not edit; `headroom shell-setup --remove` removes it)"
	end         = "# <<< headroom shell-setup <<<"
)

// ErrNoEnd is Remove's error for a begin marker with no end marker after it.
var ErrNoEnd = errors.New("a headroom shell-setup begin marker has no end marker")

// Line is the line that puts dir first on PATH in shell ("zsh" or "bash").
// zsh's typeset -U keeps one copy when nested login shells run it again.
func Line(shell, dir string) string {
	q := "'" + strings.ReplaceAll(dir, "'", `'\''`) + "'"
	if shell == "bash" {
		return "export PATH=" + q + `:"$PATH"` + "\n"
	}
	return "typeset -U path; path=(" + q + " $path)\n"
}

// Block is Line between the markers.
func Block(shell, dir string) string {
	return begin + "\n" + Line(shell, dir) + end + "\n"
}

// IsBlock reports whether content starts with a block: a file the command
// created, rather than one it appended to.
func IsBlock(content string) bool {
	return strings.HasPrefix(content, beginPrefix)
}

// find returns the byte range of the first block in content: from the start
// of its begin line to the end of its end line. start is -1 if there is
// none; stop is -1 if it has no end.
func find(content string) (start, stop int) {
	start = -1
	for off := 0; off < len(content); {
		next := strings.IndexByte(content[off:], '\n')
		lineEnd := len(content)
		if next >= 0 {
			lineEnd = off + next + 1
		}
		line := strings.TrimRight(content[off:lineEnd], "\r\n")
		switch {
		case start < 0 && strings.HasPrefix(line, beginPrefix):
			start = off
		case start >= 0 && line == end:
			return start, lineEnd
		}
		off = lineEnd
	}
	return start, -1
}

// Apply adds block at the end of content, after a newline if content does
// not end in one, else after a blank line. A block already there is
// replaced in place if it differs; changed reports whether out differs.
// A begin marker with no end leaves content as it is (Remove reports it).
func Apply(content, block string) (out string, changed bool) {
	start, stop := find(content)
	if start >= 0 {
		if stop < 0 || strings.ReplaceAll(content[start:stop], "\r", "") == block {
			return content, false
		}
		return content[:start] + block + content[stop:], true
	}
	return content + "\n" + block, true
}

// Remove deletes every block from content, with the newline Apply put
// before it: a blank line, or the newline it added to a last line that had
// none. A begin marker with no end marker is an error, and content is left
// as it is.
func Remove(content string) (out string, changed bool, err error) {
	out = content
	for {
		start, stop := find(out)
		if start < 0 {
			return out, changed, nil
		}
		if stop < 0 {
			return content, false, ErrNoEnd
		}
		before := out[:start]
		// A blank line before the block goes; so does the break ending
		// the line before it, if the block is last.
		if rest, ok := cutBreak(before); ok && (rest == "" || strings.HasSuffix(rest, "\n") || stop == len(out)) {
			before = rest
		}
		out, changed = before+out[stop:], true
	}
}

// cutBreak removes the line break (LF or CRLF) that ends s, if any.
func cutBreak(s string) (string, bool) {
	if !strings.HasSuffix(s, "\n") {
		return s, false
	}
	return strings.TrimSuffix(s[:len(s)-1], "\r"), true
}
