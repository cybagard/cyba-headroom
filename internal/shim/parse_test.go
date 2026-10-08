package shim

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
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
		{"docker run --memory=512m -e A=b -v /a:/b --name db postgres:17", Call{Kind: "container", Op: "run", Command: "docker run postgres:17", Target: "postgres:17", Name: "db", MemoryBytes: 512 << 20}},
		{"docker create --name=web nginx", Call{Kind: "container", Op: "create", Command: "docker create nginx", Target: "nginx", Name: "web"}},
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
		{"docker restart -t 5 db", Call{Kind: "container", Op: "restart", Command: "docker restart db", Target: "db"}},
		{"podman container restart --running", Call{Kind: "container", Op: "restart", Command: "podman container restart"}},
		{"docker run -m= alpine true", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker run -m=1g alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine", MemoryBytes: g}},
		{"docker run --help=false alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker container start db other", Call{Kind: "container", Op: "start", Command: "docker container start db", Target: "db", MultiTarget: true}},
		{"docker compose --env-file ops/.env up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeEnvFiles: []string{"ops/.env"}}},
		{"docker compose --env-file base.env --env-file local.env up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeEnvFiles: []string{"base.env", "local.env"}}},
		{"podman run --pod p1 --creds u:pw alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine"}},
		// An unknown flag: still gated, but the image is a guess, so none.
		{"docker run --future-flag x alpine", Call{Kind: "container", Op: "run", Command: "docker run"}},
		{"docker run --future-flag=x alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		// Targets that could hold a value never reach Command.
		{"docker run user:secret@registry.example/app", Call{Kind: "container", Op: "run", Command: "docker run", Target: "user:secret@registry.example/app"}},
		{"docker run", Call{Kind: "container", Op: "run", Command: "docker run"}},
		// Global flags.
		{"docker --context remote run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine", Endpoint: "remote"}},
		{"docker -c remote -D --host=unix:///x.sock run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine", Endpoint: "unix:///x.sock"}},
		{"podman --connection m1 --url=ssh://h run alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine", Endpoint: "ssh://h"}},
		// A global flag newer than the tables: boolean if a command follows it,
		// else it takes a value.
		{"docker --unknown run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"podman -r --log-level=debug run -m 16g img", Call{Kind: "container", Op: "run", Command: "podman run img", Target: "img", MemoryBytes: 16 * g}},
		{"docker --unknown value run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine"}},
		{"docker --unknown", Call{}},
		{"podman --syslog network create n1", Call{}},
		{"docker --newbool volume create v", Call{}},
		{"docker -v=false run -m 32g img", Call{Kind: "container", Op: "run", Command: "docker run img", Target: "img", MemoryBytes: 32 * g}},
		{"docker start db --help", Call{}},
		{"docker restart db -t 5", Call{Kind: "container", Op: "restart", Command: "docker restart db", Target: "db"}},
		{"docker run img\u202egnp", Call{Kind: "container", Op: "run", Command: "docker run", Target: "img\u202egnp"}},
		{"podman --remote -c conn run alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine", Endpoint: "conn"}},
		{"docker --newbool -H unix:///x run img", Call{Kind: "container", Op: "run", Command: "docker run img", Target: "img", Endpoint: "unix:///x"}},
		{"docker run --newflag x alpine stress -m 64g", Call{Kind: "container", Op: "run", Command: "docker run"}},
		{"docker run --newflag x -dm2g img", Call{Kind: "container", Op: "run", Command: "docker run", MemoryBytes: 2 * g}},
		{"docker run --newbool -m 1g img", Call{Kind: "container", Op: "run", Command: "docker run", MemoryBytes: g}},
		{"docker -v run img", Call{}},
		{"docker --version=true run img", Call{}},
		{"docker run --newflag x -m 8g img", Call{Kind: "container", Op: "run", Command: "docker run", MemoryBytes: 8 * g}},
		{"docker run --newflag x --memory=2g -m3g img", Call{Kind: "container", Op: "run", Command: "docker run", MemoryBytes: 3 * g}},
		{"docker --unknown ps", Call{}},
		// Compose.
		{"docker compose up", Call{Kind: "compose", Op: "up", Command: "docker compose up"}},
		{"docker compose -f a.yml -p proj up -d --build", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "proj", ComposeFiles: []string{"a.yml"}}},
		{"docker compose -f sub/c.yml -f other/d.yml up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"sub/c.yml", "other/d.yml"}}},
		{"docker compose --project-directory /srv/app -f c.yml up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeProjectDir: "/srv/app", ComposeFiles: []string{"c.yml"}}},
		{"docker compose --file=deploy/c.yml up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"deploy/c.yml"}}},
		{"docker compose --project-name=proj run --rm web sh", Call{Kind: "compose", Op: "run", Command: "docker compose run", Target: "proj"}},
		{"podman compose up", Call{Kind: "compose", Op: "up", Command: "podman compose up"}},
		{"docker --context x compose up", Call{Kind: "compose", Op: "up", Command: "docker compose up", Endpoint: "x"}},
		{"docker compose start", Call{Kind: "compose", Op: "start", Command: "docker compose start"}},
		{"docker compose restart -t 5 web", Call{Kind: "compose", Op: "restart", Command: "docker compose restart"}},
		{"docker compose create --scale web=3", Call{Kind: "compose", Op: "create", Command: "docker compose create"}},
		{"docker compose --dry-run up", Call{}},
		{"docker compose --dry-run=0 up", Call{Kind: "compose", Op: "up", Command: "docker compose up"}},
		{"docker compose up --dry-run=FALSE", Call{Kind: "compose", Op: "up", Command: "docker compose up"}},
		{"docker compose run -p 8080:80 web", Call{Kind: "compose", Op: "run", Command: "docker compose run"}},
		{"docker compose -p proj run -p 8080:80 web", Call{Kind: "compose", Op: "run", Command: "docker compose run", Target: "proj"}},
		{"podman compose --podman-path /x up", Call{Kind: "compose", Op: "up", Command: "podman compose up"}},
		{"podman compose --in-pod up", Call{Kind: "compose", Op: "up", Command: "podman compose up"}},
		{"docker compose --newflag", Call{}},
		{"docker compose scale web=5", Call{Kind: "compose", Op: "scale", Command: "docker compose scale"}},
		{"docker compose watch", Call{Kind: "compose", Op: "watch", Command: "docker compose watch"}},
		{"docker compose watch --no-up", Call{}},
		{"docker compose up -p proj -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "proj"}},
		{"docker compose up --project-name=proj -f a.yml web", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "proj", ComposeFiles: []string{"a.yml"}}},
		{"docker compose up -f a.yml web --dry-run", Call{}},
		{"docker compose --verbose -f a.yml up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"a.yml"}}},
		{"docker compose up web --dry-run", Call{}},
		{"docker compose watch web --no-up", Call{}},
		{"docker compose up web --help", Call{}},
		{"docker compose run web echo --dry-run", Call{Kind: "compose", Op: "run", Command: "docker compose run"}},
		{"docker compose up -d --dry-run", Call{}},
		// Tart.
		{"tart run ci-vm", Call{Kind: "tart", Op: "run", Command: "tart run ci-vm", Target: "ci-vm"}},
		{"tart run --no-graphics --dir src:/tmp/src ci-vm", Call{Kind: "tart", Op: "run", Command: "tart run ci-vm", Target: "ci-vm"}},
		{"tart run ci-vm --no-graphics --dir a:/b", Call{Kind: "tart", Op: "run", Command: "tart run ci-vm", Target: "ci-vm"}},
		{"tart run --future x ci-vm", Call{Kind: "tart", Op: "run", Command: "tart run"}},
		// clone makes a VM but runs nothing: disk, not memory (#29).
		{"tart clone base ci-vm", Call{}},
		// Pass through.
		{"docker ps", Call{}},
		{"docker", Call{}},
		{"docker --version", Call{}},
		{"docker -v", Call{}},
		{"docker --help run", Call{}},
		{"docker run --help", Call{}},
		{"docker run --help=true", Call{}},
		{"docker --help=1 run alpine", Call{}},
		{"tart run ci-vm --help", Call{}},
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
		if got := Parse(argv[0], argv[1:]); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.argv, got, c.want)
		}
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]uint64{
		"512": 512, "512b": 512, "4k": 4 << 10, "4K": 4 << 10, "256m": 256 << 20, "256MB": 256 << 20,
		"2g": 2 << 30, "2GiB": 2 << 30, "1.5g": 3 << 29, "1t": 1 << 40,
		"1 g": 1 << 30, "1ib": 1,
		"": 0, "lots": 0, "-1g": 0, "1x": 0, "1e9": 0, "99999999999p": 0, "g": 0, ".5g": 0, "1.g": 0, "1.2.3g": 0, "2bi": 0, "+1g": 0,
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
		{containerStart, []string{"docker-start", "podman-start", "docker-restart", "podman-restart"}},
		{composeGlobal, []string{"docker-compose"}},
		{composeUp, []string{"docker-compose-up"}},
		{composeRun, []string{"docker-compose-run"}},
		{composeStart, []string{"docker-compose-start"}},
		{composeRestart, []string{"docker-compose-restart"}},
		{composeCreate, []string{"docker-compose-create"}},
		{composeScale, []string{"docker-compose-scale"}},
		{composeWatch, []string{"docker-compose-watch"}},
		{tartRun, []string{"tart-run"}},
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
			if !reflect.DeepEqual(c, Call{}) {
				t.Fatalf("pass-through with fields: %+v", c)
			}
			return
		}
		words := strings.Split(c.Command, " ")
		if words[0] != argv[0] {
			t.Fatalf("Command %q does not start with %q", c.Command, argv[0])
		}
		for _, w := range words {
			if w == "" || strings.HasPrefix(w, "-") || strings.ContainsAny(w, "=@") || strings.ContainsFunc(w, hidden) {
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

// TestCommandListsMatchTheCLIs checks the command lists, which tell an
// unknown flag's value from a command, against the CLIs' help text.
func TestCommandListsMatchTheCLIs(t *testing.T) {
	for _, c := range []struct {
		list func(string) bool
		help []string
	}{
		{isEngineCommand, []string{"docker", "podman"}},
		{isComposeCommand, []string{"docker-compose"}},
	} {
		want := map[string]bool{}
		for _, h := range c.help {
			for _, cmd := range helpCommands(t, h) {
				want[cmd] = true
				if !c.list(cmd) {
					t.Errorf("%s: %q is not in the list", h, cmd)
				}
			}
		}
		if c.list("no-such-command") {
			t.Errorf("%v: the list takes any word", c.help)
		}
	}
}

var (
	commandsHeading = regexp.MustCompile(`^([A-Z][\w ]* )?Commands:$`)
	commandLine     = regexp.MustCompile(`^  ([a-z][a-z0-9-]*)\*?\s+\S`)
)

func helpCommands(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".help"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case commandsHeading.MatchString(l):
			in = true
		case l != "" && !strings.HasPrefix(l, " "):
			in = false
		case in:
			if m := commandLine.FindStringSubmatch(l); m != nil {
				out = append(out, m[1])
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no commands found", name)
	}
	return out
}
