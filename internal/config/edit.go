package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Setting is one key to set in a config file section.
type Setting struct {
	Section, Key, Value string
}

var (
	sectionLine = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_.-]+)\s*\]\s*(#.*)?$`)
	arrayLine   = regexp.MustCompile(`^\s*\[\[`)
)

// Merge sets each setting in a TOML config file's text, line by line, so the
// user's other keys, comments and layout stay as they are (#23):
//   - a key already in its section gets the new value, keeping its trailing
//     comment;
//   - a missing key goes after the last key of its section;
//   - a missing section is appended.
//
// Values are written as given: callers pass TOML literals. The result should
// be checked with LoadDir before it replaces the file.
func Merge(file []byte, settings []Setting) ([]byte, error) {
	text := strings.TrimRight(string(file), "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	for _, l := range lines {
		if arrayLine.MatchString(l) {
			return nil, fmt.Errorf("config: arrays of tables (%q) are not supported here", strings.TrimSpace(l))
		}
	}
	for _, s := range settings {
		lines = set(lines, s)
	}
	if len(lines) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// set applies one setting to lines.
func set(lines []string, s Setting) []string {
	keyLine := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(s.Key) + `\s*=\s*)(.*?)(\s+#.*)?$`)
	start, end := -1, len(lines) // the section's body is lines[start:end]
	for i, l := range lines {
		m := sectionLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if start >= 0 {
			end = i
			break
		}
		if m[1] == s.Section {
			start = i + 1
		}
	}
	if start < 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		return append(lines, "["+s.Section+"]", s.Key+" = "+s.Value)
	}
	// A new key goes after the section's last key, not after comments that
	// may introduce the next section.
	last := start - 1
	for i := start; i < end; i++ {
		if m := keyLine.FindStringSubmatch(lines[i]); m != nil {
			lines[i] = m[1] + s.Value + m[3]
			return lines
		}
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			last = i
		}
	}
	return append(lines[:last+1], append([]string{s.Key + " = " + s.Value}, lines[last+1:]...)...)
}
