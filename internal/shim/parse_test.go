package shim

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	g := uint64(1) << 30
	cases := []struct {
		argv string
		want Call
	}{
		// Containers.
		{"docker run --rm alpine true", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker run -it --rm -m 2g postgres:17 psql", Call{Kind: "container", Op: "run", Command: "docker run postgres:17", Target: "postgres:17", MemoryBytes: 2 * g}},
		{"docker run --memory=512m -e A=b -v /a:/b --name db postgres:17", Call{Kind: "container", Op: "run", Command: "docker run postgres:17", Target: "postgres:17", MemoryBytes: 512 << 20}},
		{"docker run -dm1.5G -p 80:80 nginx", Call{Kind: "container", Op: "run", Command: "docker run nginx", Target: "nginx", MemoryBytes: 3 * g / 2}},
		{"docker run -h myhost alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker run -m 1g -m 3g alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine", MemoryBytes: 3 * g}},
		{"docker run -m lots alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker run -- alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker run alpine --help", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker container run alpine", Call{Kind: "container", Op: "run", Command: "docker container run alpine", Target: "alpine"}},
		{"docker create redis", Call{Kind: "container", Op: "create", Command: "docker create redis", Target: "redis"}},
		{"docker container create -m 256m redis", Call{Kind: "container", Op: "create", Command: "docker container create redis", Target: "redis", MemoryBytes: 256 << 20}},
		{"docker start -ai db", Call{Kind: "container", Op: "start", Command: "docker start db", Target: "db"}},
		{"docker container start db other", Call{Kind: "container", Op: "start", Command: "docker container start db", Target: "db"}},
		{"podman run --pod p1 --creds u:pw alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine"}},
		// An unknown flag: still gated, but the image is a guess, so none.
		{"docker run --future-flag x alpine", Call{Kind: "container", Op: "run", Command: "docker run"}},
		{"docker run --future-flag=x alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		// Targets that could hold a value never reach Command.
		{"docker run user:secret@registry.example/app", Call{Kind: "container", Op: "run", Command: "docker run", Target: "user:secret@registry.example/app"}},
		{"docker run", Call{Kind: "container", Op: "run", Command: "docker run"}},
		// Global flags.
		{"docker --context remote run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker -c remote -D --host=unix:///x.sock run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"podman --connection m1 --url=ssh://h run alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine"}},
		{"docker --unknown run alpine", Call{}},
		// Compose.
		{"docker compose up", Call{Kind: "compose", Op: "up", Command: "docker compose up"}},
		{"docker compose -f a.yml -p proj up -d --build", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "proj"}},
		{"docker compose --project-name=proj run --rm web sh", Call{Kind: "compose", Op: "run", Command: "docker compose run", Target: "proj"}},
		{"podman compose up", Call{Kind: "compose", Op: "up", Command: "podman compose up"}},
		{"docker --context x compose up", Call{Kind: "compose", Op: "up", Command: "docker compose up"}},
		// Tart.
		{"tart run ci-vm", Call{Kind: "tart", Op: "run", Command: "tart run ci-vm", Target: "ci-vm"}},
		{"tart run --no-graphics --dir src:/tmp/src ci-vm", Call{Kind: "tart", Op: "run", Command: "tart run ci-vm", Target: "ci-vm"}},
		{"tart clone ghcr.io/cirruslabs/macos-sequoia-base:latest ci-vm", Call{Kind: "tart", Op: "clone", Command: "tart clone ci-vm", Target: "ci-vm"}},
		{"tart clone --concurrency 8 base ci-vm", Call{Kind: "tart", Op: "clone", Command: "tart clone ci-vm", Target: "ci-vm"}},
		// Pass through.
		{"docker ps", Call{}},
		{"docker", Call{}},
		{"docker --version", Call{}},
		{"docker -v", Call{}},
		{"docker --help run", Call{}},
		{"docker run --help", Call{}},
		{"docker run -it --help alpine", Call{}},
		{"docker exec -it db sh", Call{}},
		{"docker build -t x .", Call{}},
		{"docker container ls", Call{}},
		{"docker container", Call{}},
		{"docker image ls", Call{}},
		{"docker compose down", Call{}},
		{"docker compose up --help", Call{}},
		{"docker compose -h", Call{}},
		{"docker compose", Call{}},
		{"tart list", Call{}},
		{"tart run --help", Call{}},
		{"tart run -h", Call{}},
		{"tart --version", Call{}},
		{"tart", Call{}},
		{"kubectl run x", Call{}},
	}
	for _, c := range cases {
		argv := strings.Fields(c.argv)
		if got := Parse(argv[0], argv[1:]); got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.argv, got, c.want)
		}
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]uint64{
		"512": 512, "512b": 512, "4k": 4 << 10, "4K": 4 << 10, "256m": 256 << 20, "256MB": 256 << 20,
		"2g": 2 << 30, "2GiB": 2 << 30, "1.5g": 3 << 29, "1t": 1 << 40,
		"": 0, "lots": 0, "-1g": 0, "1x": 0, "1e9": 0, "99999999999p": 0,
	} {
		if got := parseBytes(in); got != want {
			t.Errorf("parseBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestFlagTablesMatchTheCLIs checks the flag tables against the help text of
// the real CLIs (testdata, captured with `<cli> <cmd> --help`): every flag
// listed, and whether it takes a value. Recapture after a CLI upgrade.
func TestFlagTablesMatchTheCLIs(t *testing.T) {
	for _, c := range []struct {
		table flagSet
		help  []string
	}{
		{engineGlobal, []string{"docker", "podman"}},
		{containerRun, []string{"docker-run", "docker-create", "podman-run", "podman-create"}},
		{containerStart, []string{"docker-start", "podman-start"}},
		{composeGlobal, []string{"docker-compose"}},
		{composeUp, []string{"docker-compose-up"}},
		{composeRun, []string{"docker-compose-run"}},
		{tartRun, []string{"tart-run"}},
		{tartClone, []string{"tart-clone"}},
	} {
		want := flagSet{}
		for _, h := range c.help {
			for f, v := range helpFlags(t, h) {
				if old, ok := want[f]; ok && old != v {
					t.Errorf("%s: %s takes a value in one CLI and not another", h, f)
				}
				want[f] = v
			}
		}
		var missing, extra []string
		for f, v := range want {
			if got, ok := c.table[f]; !ok || got != v {
				missing = append(missing, f)
			}
		}
		for f := range c.table {
			if _, ok := want[f]; !ok {
				extra = append(extra, f)
			}
		}
		slices.Sort(missing)
		slices.Sort(extra)
		if len(missing)+len(extra) > 0 {
			t.Errorf("%v: missing or wrong %q, not in the help %q", c.help, missing, extra)
		}
	}
}

var (
	// cobra (docker, podman): "  -m, --memory bytes   Memory limit"; a
	// type after the name means the flag takes a value.
	cobraFlag = regexp.MustCompile(`^  (?:-(\w), |    )--([a-z0-9][a-z0-9-]*)( \S+)?(?:\s|$)`)
	// swift-argument-parser (tart): "  --dir <path>   ..."
	swiftFlag = regexp.MustCompile(`^  (?:-(\w), )?--([a-z0-9][a-z0-9-]*)( <[^>]*>)?(?:\s|$)`)
)

func helpFlags(t *testing.T, name string) flagSet {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".help"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rx := cobraFlag
	if strings.HasPrefix(name, "tart") {
		rx = swiftFlag
	}
	out := flagSet{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := rx.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		out["--"+m[2]] = m[3] != ""
		if m[1] != "" {
			out["-"+m[1]] = m[3] != ""
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no flags found", name)
	}
	return out
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"docker run -m 2g alpine", "docker compose -p x up", "tart clone a b", "docker --context=x container start db", "podman run --name=a=b x@y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		argv := strings.Split(s, " ")
		c := Parse(argv[0], argv[1:])
		if c.Kind == "" {
			if c != (Call{}) {
				t.Fatalf("pass-through with fields: %+v", c)
			}
			return
		}
		words := strings.Split(c.Command, " ")
		if words[0] != argv[0] {
			t.Fatalf("Command %q does not start with %q", c.Command, argv[0])
		}
		for _, w := range words {
			if w == "" || strings.HasPrefix(w, "-") || strings.ContainsAny(w, "=@") || strings.ContainsFunc(w, isSpaceOrControl) {
				t.Fatalf("Command %q holds an unsafe word %q", c.Command, w)
			}
		}
	})
}

func BenchmarkParse(b *testing.B) {
	argv := strings.Fields("docker --context x run -it --rm -m 2g -e A=b -v /a:/b --name db -p 5432:5432 postgres:17 psql")
	for b.Loop() {
		Parse(argv[0], argv[1:])
	}
}
