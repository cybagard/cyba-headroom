package protocol_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// A check request carries Compose's working dir to the daemon and back
// (#158); without one, the line is what it was before, so an older daemon
// reads it as ever.
func TestACheckRequestCarriesTheComposeDir(t *testing.T) {
	in := protocol.Request{V: protocol.Version, Op: protocol.OpCheck,
		Check: &protocol.CheckRequest{Kind: "compose", Command: "docker compose up", Target: "app", ComposeDir: "/Users/dev/project-a"}}
	line, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `"compose_dir":"/Users/dev/project-a"`) {
		t.Errorf("line %s", line)
	}
	var out protocol.Request
	if err := json.Unmarshal(line, &out); err != nil || out.Check == nil || out.Check.ComposeDir != "/Users/dev/project-a" {
		t.Fatalf("read back %+v, %v", out.Check, err)
	}
	in.Check.ComposeDir = ""
	if line, _ := json.Marshal(in); strings.Contains(string(line), "compose_dir") {
		t.Errorf("no dir, line %s", line)
	}
}

// A request from a newer shim, with a field this daemon does not know,
// still decodes; one from an older shim has no dir.
func TestACheckRequestToleratesAnUnknownField(t *testing.T) {
	var r protocol.Request
	line := `{"v":` + strconv.Itoa(protocol.Version) + `,"op":"check","check":{"kind":"compose","command":"docker compose up","later_field":"x"}}`
	if err := json.Unmarshal([]byte(line), &r); err != nil || r.Check == nil || r.Check.Kind != "compose" || r.Check.ComposeDir != "" {
		t.Fatalf("read %+v, %v", r.Check, err)
	}
}
