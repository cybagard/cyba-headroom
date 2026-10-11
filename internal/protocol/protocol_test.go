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

// A container is named by its own ID whatever the number of engines, so
// its key in the lease book and attribution does not change when a
// second engine starts or stops (#227); its engine is a field beside it.
func TestAContainersIDIsTheSameWithOneEngineOrTwo(t *testing.T) {
	const id = "585f8ebeefcc32175f161d005a37cf22651c0b9ee7e4d46496a19be28943e4e0"
	desktop := protocol.Engine{Kind: protocol.EngineDockerDesktop, ID: "00000000-0000-4000-8000-000000000001", Running: true,
		Containers: []protocol.Container{{ID: id, Name: "db"}}}
	other := protocol.Engine{Kind: protocol.EngineDocker, ID: "00000000-0000-4000-8000-000000000002", Running: true,
		Containers: []protocol.Container{{ID: "c2", Name: "web"}}}
	one := (&protocol.Snapshot{Engines: []protocol.Engine{desktop}}).Containers()
	two := (&protocol.Snapshot{Engines: []protocol.Engine{desktop, other}}).Containers()
	if len(one) != 1 || len(two) != 2 || one[0].ID != id || two[0].ID != id || two[1].ID != "c2" {
		t.Fatalf("one engine: %+v; two: %+v; want %s each time", one, two, id)
	}
	if two[0].Engine != desktop.ID || two[1].Engine != other.ID {
		t.Fatalf("engines = %q, %q, want each container's own", two[0].Engine, two[1].Engine)
	}
}
