package shellblock

import "testing"

const (
	zshV1 = "# >>> headroom shell-setup v1 >>> (do not edit; `headroom shell-setup --remove` removes it)\n" +
		"typeset -U path; path=('/Users/dev/.config/headroom/shims' $path)\n" +
		"# <<< headroom shell-setup <<<\n"
	bashV1 = "# >>> headroom shell-setup v1 >>> (do not edit; `headroom shell-setup --remove` removes it)\n" +
		"export PATH='/Users/dev/.config/headroom/shims':\"$PATH\"\n" +
		"# <<< headroom shell-setup <<<\n"
	// old is an earlier version's block, with another shim dir.
	old = "# >>> headroom shell-setup v0 >>>\n" +
		"path=(/old/shims $path)\n" +
		"# <<< headroom shell-setup <<<\n"
)

func TestBlock(t *testing.T) {
	if got := Block("zsh", "/Users/dev/.config/headroom/shims"); got != zshV1 {
		t.Errorf("zsh:\n%s", got)
	}
	if got := Block("bash", "/Users/dev/.config/headroom/shims"); got != bashV1 {
		t.Errorf("bash:\n%s", got)
	}
	// A quote in the dir stays inside the word.
	want := "typeset -U path; path=('/Users/dev/it'\\''s' $path)\n"
	if got := Line("zsh", "/Users/dev/it's"); got != want {
		t.Errorf("quoted: %q, want %q", got, want)
	}
}

func TestApply(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		changed        bool
	}{
		{"empty file", "", "\n" + zshV1, true},
		{"trailing newline", "a=1\n", "a=1\n\n" + zshV1, true},
		{"no trailing newline", "a=1", "a=1\n" + zshV1, true},
		{"blank last line", "a=1\n\n", "a=1\n\n\n" + zshV1, true},
		{"CRLF", "a=1\r\n", "a=1\r\n\n" + zshV1, true},
		{"current block", "a=1\n\n" + zshV1, "a=1\n\n" + zshV1, false},
		{"current block, later lines", "a=1\n\n" + zshV1 + "b=2\n", "a=1\n\n" + zshV1 + "b=2\n", false},
		{"old block replaced in place", "a=1\n\n" + old + "b=2\n", "a=1\n\n" + zshV1 + "b=2\n", true},
		{"CRLF block", "a=1\r\n\r\n" + crlf(zshV1), "a=1\r\n\r\n" + crlf(zshV1), false},
	} {
		got, changed := Apply(c.in, zshV1)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: Apply = %q, %v; want %q, %v", c.name, got, changed, c.want, c.changed)
		}
		if again, changed := Apply(got, zshV1); again != got || changed {
			t.Errorf("%s: a second Apply changed it: %q", c.name, again)
		}
	}
}

func crlf(s string) string {
	out := ""
	for _, r := range s {
		if r == '\n' {
			out += "\r"
		}
		out += string(r)
	}
	return out
}

// Remove after Apply gives back the original bytes.
func TestRemoveUndoesApply(t *testing.T) {
	for _, in := range []string{"", "a=1\n", "a=1", "a=1\n\n", "a=1\r\n", "a=1\r\nb=2"} {
		applied, _ := Apply(in, zshV1)
		got, changed, err := Remove(applied)
		if got != in || !changed || err != nil {
			t.Errorf("Remove(Apply(%q)) = %q, %v, %v", in, got, changed, err)
		}
	}
}

func TestRemove(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		changed        bool
	}{
		{"no block", "a=1\n", "a=1\n", false},
		{"a created file", zshV1, "", true},
		{"lines after the block", "a=1\n\n" + zshV1 + "b=2\n", "a=1\nb=2\n", true},
		{"a block that is not last keeps the line before it", "a=1\n" + zshV1 + "b=2\n", "a=1\nb=2\n", true},
		{"an old block", "a=1\n\n" + old, "a=1\n", true},
		{"two blocks", "a=1\n\n" + zshV1 + "\n" + old, "a=1\n", true},
		{"a file converted to CRLF", "a=1\r\n\r\n" + crlf(zshV1), "a=1\r\n", true},
	} {
		got, changed, err := Remove(c.in)
		if got != c.want || changed != c.changed || err != nil {
			t.Errorf("%s: Remove = %q, %v, %v; want %q, %v", c.name, got, changed, err, c.want, c.changed)
		}
	}
}

// A begin marker with no end: Remove refuses, and changes nothing.
func TestRemoveRefusesAnUnendedBlock(t *testing.T) {
	in := "a=1\n\n" + zshV1[:len(zshV1)-len("# <<< headroom shell-setup <<<\n")] + "b=2\n"
	got, changed, err := Remove(in)
	if err == nil || got != in || changed {
		t.Fatalf("Remove = %q, %v, %v", got, changed, err)
	}
	if out, changed := Apply(in, zshV1); out != in || changed {
		t.Fatalf("Apply changed it: %q", out)
	}
}
