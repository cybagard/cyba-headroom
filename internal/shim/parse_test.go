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
		{"docker container start db other", Call{Kind: "container", Op: "start", Command: "docker container start db", Target: "db", MultiTarget: true, Targets: []string{"db", "other"}}},
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
		{"docker --context remote run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine", Context: "remote"}},
		{"docker -c remote -D --host=unix:///x.sock run alpine", Call{Kind: "container", Op: "run", Command: "docker run alpine", Target: "alpine", Host: "unix:///x.sock", HasHost: true, Context: "remote"}},
		{"podman --connection m1 --url=ssh://h run alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine", Host: "ssh://h", HasHost: true, Context: "m1"}},
		{"podman --url=ssh://h --connection m1 run alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine", Host: "ssh://h", HasHost: true, Context: "m1"}},
		{"podman --context b -c m1 run alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine", Context: "m1", PodmanContext: "b"}},
		{"podman --context b pod start web", Call{Kind: "pod", Op: "start", Command: "podman pod start web", Target: "web", PodmanContext: "b"}},
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
		{"podman --remote -c conn run alpine", Call{Kind: "container", Op: "run", Command: "podman run alpine", Target: "alpine", Context: "conn"}},
		{"docker --newbool -H unix:///x run img", Call{Kind: "container", Op: "run", Command: "docker run img", Target: "img", Host: "unix:///x", HasHost: true}},
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
		{"docker compose up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose up --detach web", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose up --wait", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -f a.yml -p proj up -d --build", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "proj", ComposeFiles: []string{"a.yml"}, ComposeDetached: true}},
		{"docker compose -f sub/c.yml -f other/d.yml up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"sub/c.yml", "other/d.yml"}}},
		{"docker compose --project-directory /srv/app -f c.yml up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeProjectDir: "/srv/app", ComposeFiles: []string{"c.yml"}}},
		{"docker compose --file=deploy/c.yml up", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"deploy/c.yml"}}},
		{"docker compose --project-name=proj run --rm web sh", Call{Kind: "compose", Op: "run", Command: "docker compose run", Target: "proj"}},
		{"podman compose up", Call{Kind: "compose", Op: "up", Command: "podman compose up"}},
		{"docker --context x compose up", Call{Kind: "compose", Op: "up", Command: "docker compose up", Context: "x"}},
		{"docker --config d --context x compose up", Call{Kind: "compose", Op: "up", Command: "docker compose up", Context: "x", ConfigDir: "d"}},
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
		{"docker compose up -p proj -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "proj", ComposeDetached: true}},
		{"docker compose up --project-name=proj -f a.yml web", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "proj", ComposeFiles: []string{"a.yml"}}},
		{"docker compose up -f a.yml web --dry-run", Call{}},
		{"docker compose --verbose -f a.yml up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"a.yml"}, ComposeDetached: true}},
		{"docker compose up web --dry-run", Call{}},
		{"docker compose watch web --no-up", Call{}},
		{"docker compose up web --help", Call{}},
		{"docker compose run web echo --dry-run", Call{Kind: "compose", Op: "run", Command: "docker compose run"}},
		{"docker compose up -d --dry-run", Call{}},
		// After the command, run's -p is --publish and the words after the
		// service are its command's (#278).
		{"docker compose run -p 80:80 a true", Call{Kind: "compose", Op: "run", Command: "docker compose run"}},
		{"docker compose run a true -f x", Call{Kind: "compose", Op: "run", Command: "docker compose run"}},
		{"docker compose up -d -- a", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		// A "--" among compose's global options: the options after it are
		// checked but ignored, and the command must follow one (#324).
		{"docker compose -- -p x up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -f c.yml up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- --compatibility up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- --dry-run up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -f c.yml -- -p y up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"c.yml"}, ComposeDetached: true}},
		{"docker compose -- -H tcp://203.0.113.5:2376 -p x up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -H unix:///var/run/docker.sock up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- --host=unix:///var/run/docker.sock up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- --bogus up -d", Call{}},
		{"docker compose -- up -d", Call{}},
		{"docker compose -p x -- up -d", Call{}},
		{"docker compose -- -- up", Call{}},
		{"docker compose -- -p x -- up -d", Call{}},
		{"docker compose --dry-run -- -p x up -d", Call{}},
		// Round 1: help, version and further "--" after the first are ignored
		// too; the call fails only when "--" is right before the command.
		{"docker compose -- -h up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- --help up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -h=false up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- --version up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -p x -h up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- --help -p x up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -p x -- -p y up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -- -p x up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -p x -- -- -p y up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -D up -d", Call{}},
		{"docker compose -- -x up -d", Call{}},
		{"docker compose -f c.yml -- -h up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeFiles: []string{"c.yml"}, ComposeDetached: true}},
		{"docker compose -p x -- -- -p y up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", Target: "x", ComposeDetached: true}},
		{"docker compose -- -- up -d", Call{}},
		{"docker compose -- -p -- up -d", Call{}},
		{"docker compose -- -p x -- -p y -- up -d", Call{}},
		// Round 2: after "--", a cluster is one word, its value never the next.
		{"docker compose -- -Dp up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -Df up -d", Call{Kind: "compose", Op: "up", Command: "docker compose up", ComposeDetached: true}},
		{"docker compose -- -Dp x up -d", Call{}},
		// Podman's pods, kube play and machines (#230): one cost each, keyed
		// by the pod, the kube files' pods or the machine.
		{"podman pod start web", Call{Kind: "pod", Op: "start", Command: "podman pod start web", Target: "web"}},
		{"podman pod start --pod-id-file f web db", Call{Kind: "pod", Op: "start", Command: "podman pod start web", Target: "web", MultiTarget: true, Targets: []string{"web", "db"}}},
		{"podman pod start --all", Call{Kind: "pod", Op: "start", Command: "podman pod start"}},
		{"podman pod start web --help", Call{}},
		{"podman --connection m1 pod start web", Call{Kind: "pod", Op: "start", Command: "podman pod start web", Target: "web", Context: "m1"}},
		{"podman kube play web.yml", Call{Kind: "pod", Op: "play", Command: "podman kube play", KubeFiles: []string{"web.yml"}}},
		{"podman kube play --network n1 -q a.yml b.yml", Call{Kind: "pod", Op: "play", Command: "podman kube play", KubeFiles: []string{"a.yml", "b.yml"}}},
		{"podman play kube --replace web.yml", Call{Kind: "pod", Op: "play", Command: "podman play kube", KubeFiles: []string{"web.yml"}}},
		{"podman kube play -", Call{Kind: "pod", Op: "play", Command: "podman kube play", KubeFiles: []string{"-"}}},
		{"podman kube play --future x web.yml", Call{Kind: "pod", Op: "play", Command: "podman kube play"}},
		{"podman machine start", Call{Kind: "machine", Op: "start", Command: "podman machine start"}},
		{"podman machine start -q dev", Call{Kind: "machine", Op: "start", Command: "podman machine start dev", Target: "dev"}},
		{"podman --connection remote machine start", Call{Kind: "machine", Op: "start", Command: "podman machine start", Context: "remote"}},
		{"podman machine start --log-level debug big", Call{Kind: "machine", Op: "start", Command: "podman machine start big", Target: "big"}},
		// Creating starts nothing: pod create has no --start (podman 6.1),
		// and kube play --start=false only creates. --down tears down.
		{"podman pod create --name web", Call{}},
		{"podman kube play --start=false web.yml", Call{}},
		{"podman kube play --down web.yml", Call{}},
		{"podman play kube web.yml --down", Call{}},
		{"podman kube play --help", Call{}},
		{"podman machine start --help", Call{}},
		{"podman machine stop", Call{}},
		{"podman machine init", Call{}},
		{"podman pod stop web", Call{}},
		{"podman kube down web.yml", Call{}},
		{"podman pod", Call{}},
		{"podman kube", Call{}},
		{"docker pod start web", Call{}},
		{"podman container pod start web", Call{}},
		// The rest that start something, each only with its start flag
		// (#241): machine init --now, sized by its --memory (MiB), else
		// 0, for the shim to read containers.conf (#243); pod clone --start
		// and container clone --run, keyed by the new name (NAME, else
		// --name) and sized by -m; pod restart, as pod start.
		{"podman machine init --now", Call{Kind: "machine", Op: "init", Command: "podman machine init"}},
		{"podman machine init --now m", Call{Kind: "machine", Op: "init", Command: "podman machine init m", Target: "m"}},
		{"podman machine init --now --memory 4096 vm-a", Call{Kind: "machine", Op: "init", Command: "podman machine init vm-a", Target: "vm-a", MemoryBytes: 4 << 30}},
		{"podman machine init -m=1024 -v /src:/src --now=true vm-a", Call{Kind: "machine", Op: "init", Command: "podman machine init vm-a", Target: "vm-a", MemoryBytes: 1 << 30}},
		{"podman machine init --memory 4096 vm-a", Call{}},
		{"podman machine init --now=false vm-a", Call{}},
		{"podman machine init --now vm-a --help", Call{}},
		{"podman machine init --now --future x vm-a", Call{Kind: "machine", Op: "init", Command: "podman machine init", Guessed: true}},
		{"podman pod clone --start src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new"}},
		{"podman pod clone -h host -m 1g --start src", Call{Kind: "pod", Op: "clone", Command: "podman pod clone", MemoryBytes: g}},
		{"podman pod clone --start -m 1g src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new", MemoryBytes: g}},
		{"podman pod clone --start -n new src", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new"}},
		{"podman pod clone --start --name other src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new"}},
		{"podman pod clone src new", Call{}},
		{"podman pod clone --start src new --help", Call{}},
		{"podman pod clone --start --future x src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone", Guessed: true}},
		{"podman pod restart web", Call{Kind: "pod", Op: "restart", Command: "podman pod restart web", Target: "web"}},
		{"podman pod restart web db", Call{Kind: "pod", Op: "restart", Command: "podman pod restart web", Target: "web", MultiTarget: true, Targets: []string{"web", "db"}}},
		{"podman pod restart -a", Call{Kind: "pod", Op: "restart", Command: "podman pod restart"}},
		{"podman pod restart web --help", Call{}},
		{"podman pod restart --future web", Call{Kind: "pod", Op: "restart", Command: "podman pod restart", Guessed: true}},
		{"podman container clone --run c new img", Call{Kind: "container", Op: "clone", Command: "podman container clone new", Target: "new", Name: "new"}},
		{"podman container clone -m 2g --run c", Call{Kind: "container", Op: "clone", Command: "podman container clone", MemoryBytes: 2 * g}},
		{"podman container clone --run -m 512m a b", Call{Kind: "container", Op: "clone", Command: "podman container clone b", Target: "b", Name: "b", MemoryBytes: 512 << 20}},
		{"podman container clone --run --name new c", Call{Kind: "container", Op: "clone", Command: "podman container clone new", Target: "new", Name: "new"}},
		{"podman --connection m1 container clone --run c new", Call{Kind: "container", Op: "clone", Command: "podman container clone new", Target: "new", Name: "new", Context: "m1"}},
		{"podman container clone c new", Call{}},
		{"podman container clone --run c new --help", Call{}},
		{"podman container clone --run --future x c new", Call{Kind: "container", Op: "clone", Command: "podman container clone", Guessed: true}},
		{"docker container clone --run c new", Call{}},
		{"podman clone --run c new", Call{}},
		// Podman takes --log-level and --storage-opt after the subcommand
		// too, and no other global option (#243). -v there is --volume.
		{"podman pod clone --start --log-level info src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new"}},
		{"podman pod clone --start --log-level=info src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new"}},
		{"podman pod clone --start --storage-opt x src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new"}},
		{"podman pod restart --log-level info p", Call{Kind: "pod", Op: "restart", Command: "podman pod restart p", Target: "p"}},
		{"podman pod restart --storage-opt x p", Call{Kind: "pod", Op: "restart", Command: "podman pod restart p", Target: "p"}},
		{"podman pod start --log-level info p", Call{Kind: "pod", Op: "start", Command: "podman pod start p", Target: "p"}},
		{"podman pod start --storage-opt=x p", Call{Kind: "pod", Op: "start", Command: "podman pod start p", Target: "p"}},
		{"podman kube play --log-level info a.yml", Call{Kind: "pod", Op: "play", Command: "podman kube play", KubeFiles: []string{"a.yml"}}},
		{"podman kube play --storage-opt x a.yml", Call{Kind: "pod", Op: "play", Command: "podman kube play", KubeFiles: []string{"a.yml"}}},
		{"podman container clone --run --log-level info a b", Call{Kind: "container", Op: "clone", Command: "podman container clone b", Target: "b", Name: "b"}},
		{"podman container clone --run --storage-opt x a b", Call{Kind: "container", Op: "clone", Command: "podman container clone b", Target: "b", Name: "b"}},
		{"podman machine start --storage-opt x m", Call{Kind: "machine", Op: "start", Command: "podman machine start m", Target: "m"}},
		{"podman machine init --log-level info --now m", Call{Kind: "machine", Op: "init", Command: "podman machine init m", Target: "m"}},
		{"podman machine init --storage-opt x --now m", Call{Kind: "machine", Op: "init", Command: "podman machine init m", Target: "m"}},
		{"podman pod clone -v /a:/b --start src new", Call{Kind: "pod", Op: "clone", Command: "podman pod clone new", Target: "new"}},
		{"podman machine init -v /a:/b --now m", Call{Kind: "machine", Op: "init", Command: "podman machine init m", Target: "m"}},
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

// docker-compose is docker compose with fewer of docker's options, which it
// reads anywhere in its leading run of options: every row measured on the
// real v5.6.1 (#266: the plan, its amendment 1 and review rounds 2 and 3).
// A call it fails on gives Call{}, or is gated: never a wrong allow.
func TestDockerComposeLeadingRun(t *testing.T) {
	const sock = "unix:///var/run/docker.sock"
	type want struct {
		compose               bool
		host, context, target string
		files                 []string
	}
	up := func(host, context string) want { return want{compose: true, host: host, context: context} }
	for _, tc := range []struct {
		argv string
		want want
	}{
		// The plan's table.
		{"-f a.yml up -d", want{compose: true, files: []string{"a.yml"}}},
		{"-p proj run web", want{compose: true, target: "proj"}},
		{"-H S up -d", up(sock, "")},
		{"--context c up -d", up("", "c")},
		{"-H S --context c -D up -d", up(sock, "c")},
		{"--host=S --log-level debug --tls --tlsverify --debug up -d", up(sock, "")},
		{"-H=S --context=c --log-level=info up -d", up(sock, "c")},
		{"-c x up -d", want{}},
		{"--config x up -d", want{}},
		{"-l info up -d", want{}},
		{"--tlscacert ca.pem up -d", want{}},
		{"-HS up -d", want{}},
		{"-DH S up -d", want{}},
		{"--tls=false up -d", want{}},
		{"-f a.yml -H S up -d", want{}},
		{"-v", want{}},
		{"-v up -d", want{}},
		{"--version", want{}},
		{"down", want{}},
		{"", want{}},
		// Amendment 1.
		{"--compatibility -H S up -d", up(sock, "")},
		{"--dry-run -H S up -d", want{}}, // a dry run starts nothing
		{"--dry-run=false -H S up -d", up(sock, "")},
		{"--project-name=x -H S up -d", want{compose: true, host: sock, target: "x"}},
		{"--ansi=never --host=S up -d", up(sock, "")},
		{"--profile=a --context nope up -d", up("", "nope")},
		{"-D --compatibility -H S up -d", up(sock, "")},
		{"-p x -H S up -d", want{}},
		{"-f c.yml -H S up -d", want{}},
		// Round 2: a shorthand's joined value.
		{"-fc.yml -H S up -d", want{compose: true, host: sock, files: []string{"c.yml"}}},
		{"-pproj -H S up -d", want{compose: true, host: sock, target: "proj"}},
		{"-fc.yml -D up -d", want{compose: true, files: []string{"c.yml"}}},
		// Round 3: compose's hidden flags, and any other.
		{"--no-ansi -H S up -d", up(sock, "")},
		{"--verbose -H S up -d", up(sock, "")},
		{"--verbose=true -H S up -d", up(sock, "")},
		{"--workdir=. -H S up -d", up(sock, "")},
		{"--bogus -H S up -d", up(sock, "")},
		{"-- -H S up -d", want{}},
		{"--profile a -H S up -d", want{}},
		{"--workdir . -H S up -d", want{}},
		// #324: after "--", compose's options are checked but ignored, and
		// the command must follow one; the first pass reads Docker's
		// options straight after "--", which do not count.
		{"-- -p x up -d", up("", "")},
		{"-- -f c.yml up -d", up("", "")},
		{"-- --compatibility up -d", up("", "")},
		{"-- --dry-run up -d", up("", "")},
		{"--ansi=never -- -f c.yml up -d", up("", "")},
		{"-f c.yml -- -p y up -d", want{compose: true, files: []string{"c.yml"}}},
		{"-- -H tcp://203.0.113.5:2376 -p x up -d", up("", "")},
		{"-- -H S -p x up -d", up("", "")},
		{"-- -p x -H S up -d", up("", "")},
		{"-- --bogus up -d", want{}},
		{"-- --host=S up -d", want{}},
		{"-- -D up -d", want{}},
		{"-- up -d", want{}},
		{"-p x -- up -d", want{}},
		{"-- -- up", want{}},
		{"-- -p x -- up -d", want{}},
		{"--dry-run -- -p x up -d", want{}},
		// Round 1: help, version and further "--" after the first are ignored
		// too; the call fails only when "--" is right before the command.
		{"-- -h up -d", up("", "")},
		{"-- --help up -d", up("", "")},
		{"-- -h=false up -d", up("", "")},
		{"-- --version up -d", up("", "")},
		{"-- -p x -h up -d", up("", "")},
		{"-- --help -p x up -d", up("", "")},
		{"-- -p x -- -p y up -d", up("", "")},
		{"-- -- -p x up -d", up("", "")},
		{"-- -p x -- -- -p y up -d", up("", "")},
		{"-- -x up -d", want{}},
		{"-f c.yml -- -h up -d", want{compose: true, files: []string{"c.yml"}}},
		{"-p x -- -- -p y up -d", want{compose: true, target: "x"}},
		{"-- -- up -d", want{}},
		{"-- -p -- up -d", want{}},
		{"-- -p x -- -p y -- up -d", want{}},
		{"-- -H S -- -p x up -d", up("", "")},
		// Round 2: after "--", a cluster is one word, its value never the next.
		{"-- -Dp up -d", up("", "")},
		{"-- -Df up -d", up("", "")},
		{"-- -Dp x up -d", want{}},
		// #352: an option's empty value does not end the run.
		{"--profile '' -H S up -d", up(sock, "")},
		{"-p '' -D up -d", up("", "")},
	} {
		args := fieldsWithEmpty(strings.ReplaceAll(tc.argv, "S", sock))
		c := Parse("docker-compose", args)
		got := want{compose: c.Kind == "compose", host: c.Host, context: c.Context, target: c.Target, files: c.ComposeFiles}
		if !reflect.DeepEqual(got, tc.want) || c.HasHost != (tc.want.host != "") {
			t.Errorf("docker-compose %s:\n got %+v\nwant %+v", tc.argv, got, tc.want)
		}
	}
}

// TestComposeDashDashMatchesCompose holds every call with a "--" among
// compose's global options measured on the real Compose v5.6.1 (#324: the
// plan, addenda 1 and 2, review rounds 1 to 3), in both forms: s is the
// standalone docker-compose, p the plugin. Unreachable engines; "runs" is
// a call that dialled one. g: runs, and is gated; f: fails, and is not;
// p: runs, but starts nothing (a dry run, down, config), and is not gated;
// F: fails, but is gated (the safe direction): "--" as -p's or -f's value,
// docker's options before "--" in the plugin, which does not take them,
// and two -H, which docker refuses.
func TestComposeDashDashMatchesCompose(t *testing.T) {
	for _, tc := range []struct{ argv, sp string }{
		{"-- -p x up -d", "gg"},
		{"-- -px up -d", "gg"},
		{"-- -p=x up -d", "gg"},
		{"-- --project-name=x up -d", "gg"},
		{"-- --project-name x up -d", "gg"},
		{"-- -f c.yml up -d", "gg"},
		{"-- -fc.yml up -d", "gg"},
		{"-- --file=c.yml up -d", "gg"},
		{"-- --compatibility up -d", "gg"},
		{"-- --compatibility=true up -d", "gg"},
		{"-- --compatibility=false up -d", "gg"},
		{"-- --dry-run up -d", "gg"},
		{"-- --dry-run=true up -d", "gg"},
		{"-- --dry-run=false up -d", "gg"},
		{"-- --all-resources up -d", "gg"},
		{"-- --ansi never up -d", "gg"},
		{"-- --ansi=never up -d", "gg"},
		{"-- --profile p up -d", "gg"},
		{"-- --env-file e up -d", "gg"},
		{"-- --parallel 2 up -d", "gg"},
		{"-- --progress plain up -d", "gg"},
		{"-- --project-directory . up -d", "gg"},
		{"-- --workdir . up -d", "gg"},
		{"-- --verbose up -d", "fg"},
		{"-- -x up -d", "ff"},
		{"-- --bogus up -d", "ff"},
		{"-- --bogus=1 up -d", "gg"},
		{"-- -h up -d", "gg"},
		{"-- --help up -d", "gg"},
		{"-- -v up -d", "gg"},
		{"-- --version up -d", "gg"},
		{"-- -- up -d", "ff"},
		{"-- -p x -- up -d", "ff"},
		{"-- -p x -- -p y up -d", "gg"},
		{"-- -p -- up -d", "ff"},
		{"-- -p x up -d -- a", "gg"},
		{"-p x -- up -d", "ff"},
		{"-f c.yml -- -p y up -d", "gg"},
		{"-f c.yml -- up -d", "ff"},
		{"-f -- -p y up -d", "FF"},
		{"-p -- -p y up -d", "gg"},
		{"--ansi=never -- -f c.yml up -d", "gg"},
		{"--dry-run -- -p x up -d", "pp"},
		{"--dry-run=false -- -p x up -d", "gg"},
		{"-H tcp://127.0.0.1:1 -- -p x up -d", "gF"},
		{"-- -H tcp://127.0.0.1:1 up -d", "fg"},
		{"-- --host=tcp://127.0.0.1:1 up -d", "fg"},
		{"-- --host tcp://127.0.0.1:1 up -d", "fg"},
		{"-- -Htcp://127.0.0.1:1 up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 -p x up -d", "gg"},
		{"-- --host=tcp://127.0.0.1:1 -p x up -d", "gg"},
		{"-- -p x -H tcp://127.0.0.1:1 up -d", "gg"},
		{"-- -D up -d", "ff"},
		{"-- -D -p x up -d", "gg"},
		{"-- -D=true -p x up -d", "gg"},
		{"-- --debug -p x up -d", "gg"},
		{"-- -l debug -p x up -d", "gg"},
		{"-- --log-level=debug up -d", "fg"},
		{"-- --log-level debug -p x up -d", "gg"},
		{"-- -c foo -p x up -d", "gg"},
		{"-- --context foo up -d", "fg"},
		{"-- --context foo -p x up -d", "gg"},
		{"-- --tls up -d", "ff"},
		{"-- --tls -p x up -d", "gg"},
		{"-- --tlsverify=true -p x up -d", "gg"},
		{"-- --config /tmp -p x up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 -H tcp://127.0.0.1:2 -p x up -d", "Fg"},
		{"-- -pDx up -d", "gg"},
		{"-- -dp x up -d", "ff"},
		{"-p x -- -- up -d", "ff"},
		{"-- up -d -p x", "ff"},
		{"-- -p x start", "gg"},
		{"-- -p x run a true", "gg"},
		{"-- -p x create", "gg"},
		{"-- -p x restart", "gg"},
		{"-- -p x config", "pp"},
		{"-- -- -p x up -d", "gg"},
		{"-p x -- -- -p y up -d", "gg"},
		{"-- -p x -- -- -p y up -d", "gg"},
		{"-- -p x -- -p y -- up -d", "ff"},
		{"-- --help=false up -d", "gg"},
		{"-- -h=false up -d", "gg"},
		{"-- --version=false up -d", "gg"},
		{"-- -p x -h up -d", "gg"},
		{"-f c.yml -- -h up -d", "gg"},
		{"-- --help -p x up -d", "gg"},
		{"-- -v -p x up -d", "gg"},
		{"-- -f=c.yml up -d", "gg"},
		{"-- -p up up -d", "gg"},
		{"-- --profile a up -d", "gg"},
		{"-- -hp x up -d", "ff"},
		{"-- -ph up -d", "gg"},
		{"-- -Dp x up -d", "ff"},
		{"-- -pD up -d", "gg"},
		{"-- --bogus x up -d", "gg"},
		{"-- -x val up -d", "gg"},
		{"-- -p x --bogus up -d", "ff"},
		{"-- -H=tcp://127.0.0.1:1 up -d", "fg"},
		{"-- --debug up -d", "ff"},
		{"-- --debug=false up -d", "gg"},
		{"-- -D=true up -d", "gg"},
		{"-- --tlsverify up -d", "ff"},
		{"-- --context default up -d", "fg"},
		{"-- --context=default up -d", "fg"},
		{"-- --log-level debug up -d", "fg"},
		{"-- -H tcp://127.0.0.1:1 -- -p x up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 -- up -d", "ff"},
		{"-- -H tcp://127.0.0.1:1 -- -- -p x up -d", "gg"},
		{"-- -D -- -p x up -d", "gg"},
		{"-- --debug=false -p x up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 -D up -d", "ff"},
		{"-- -H tcp://127.0.0.1:1 --tls -p x up -d", "gg"},
		{"-H tcp://127.0.0.1:1 -- up -d", "ff"},
		{"-H tcp://127.0.0.1:1 -- -H tcp://127.0.0.1:1 up -d", "fF"},
		{"-D -- -p x up -d", "gF"},
		{"-p x -- -p y up -d", "gg"},
		{"-p -- up -d", "FF"},
		{"-p -- -p x up -d", "gg"},
		{"-f -- -p x up -d", "FF"},
		{"-- -f -- -p x up -d", "gg"},
		{"-- -p -- -p x up -d", "gg"},
		{"-- -- -- up -d", "ff"},
		{"-- -- -- -p x up -d", "gg"},
		{"-- up -d", "ff"},
		{"--", "ff"},
		{"-- -p", "ff"},
		{"-- -p x", "ff"},
		{"-- -p x down", "pp"},
		{"--dry-run=true -- -p x up -d", "pp"},
		{"-- -p x up -d --dry-run", "pp"},
		{"-- --bogus up up -d", "gg"},
		{"-- -p x -h -- -p y up -d", "gg"},
		{"--compatibility -- -p x up -d", "gg"},
		{"--ansi=never -- --ansi never up -d", "gg"},
		{"-D -- -D up -d", "ff"},
		{"-D -- -H tcp://127.0.0.1:1 up -d", "fF"},
		{"-H tcp://127.0.0.1:1 -- -D up -d", "ff"},
		{"--debug -- --tls up -d", "ff"},
		{"-p x -- -H tcp://127.0.0.1:1 up -d", "gg"},
		{"--ansi never -- -H tcp://127.0.0.1:1 up -d", "gg"},
		{"--compatibility -- -D up -d", "ff"},
		{"-H tcp://127.0.0.1:1 --compatibility -- -D up -d", "ff"},
		{"--compatibility -H tcp://127.0.0.1:1 -- -D up -d", "ff"},
		{"--context default -- -H tcp://127.0.0.1:1 up -d", "fF"},
		{"-H=tcp://127.0.0.1:1 -- -D up -d", "ff"},
		{"-D -- up -d", "ff"},
		{"-D -- -- up -d", "ff"},
		{"-D -- -D -- up -d", "ff"},
		{"-D -- -D -D up -d", "ff"},
		{"-H tcp://127.0.0.1:1 -- -H tcp://127.0.0.1:1 -- up -d", "ff"},
		{"-H tcp://127.0.0.1:1 -- -H tcp://127.0.0.1:1 -- -p x up -d", "FF"},
		{"--log-level debug -- --log-level debug up -d", "fF"},
		{"--tls -- --tls up -d", "ff"},
		{"-H tcp://127.0.0.1:1 -H tcp://127.0.0.1:1 -- up -d", "ff"},
		{"-H tcp://127.0.0.1:1 -- -- -H tcp://127.0.0.1:1 up -d", "fF"},
		{"-- -H tcp://127.0.0.1:1 -H tcp://127.0.0.1:1 up -d", "fg"},
		{"-H tcp://127.0.0.1:1 -- -H tcp://127.0.0.1:1 -D up -d", "ff"},
		{"-H tcp://127.0.0.1:1 -p x -- -H tcp://127.0.0.1:1 up -d", "gF"},
		{"-H tcp://127.0.0.1:1 -H tcp://127.0.0.1:1 up -d", "FF"},
		{"-H tcp://127.0.0.1:1 -H tcp://127.0.0.1:1 -p x up -d", "FF"},
		{"-- -H tcp://127.0.0.1:1 -h up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 --version up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 -v up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 --compatibility up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 -x up -d", "ff"},
		{"-- -H tcp://127.0.0.1:1 --bogus=1 up -d", "gg"},
		{"-- --tls=true up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 -D=true up -d", "gg"},
		{"-- -p -x up -d", "ff"},
		{"-- --ansi --dry-run up -d", "gg"},
		{"-f c.yml -- -H tcp://127.0.0.1:1 up -d", "gg"},
		{"-- -Dp up -d", "gg"},
		{"-- -Df up -d", "gg"},
		{"-- -DH up -d", "gg"},
		{"-- -hp up -d", "gg"},
		{"-- -xp up -d", "gg"},
		{"-- -Dpx up -d", "gg"},
		{"-- -p x -Dp up -d", "gg"},
		{"-- --compatibility -Dp up -d", "gg"},
		{"-- -- -Dp up -d", "gg"},
		{"-f c.yml -- -Dp up -d", "gg"},
		{"-- -Dp up up -d", "gg"},
		{"-- --no-ansi up -d", "gg"},
		{"-- --workdir up -d", "ff"},
		{"-- --verbose x up -d", "gf"},
		{"-- -D -- -D up -d", "ff"},
		{"-- -H tcp://127.0.0.1:1 -- -H tcp://127.0.0.1:1 up -d", "fg"},
		{"-- -v x up -d", "ff"},
		{"-- --help x up -d", "ff"},
		{"-- --compatibility x up -d", "ff"},
		{"-- -- x up -d", "gg"},
		{"-- - up -d", "gg"},
		{"-- x up -d", "gg"},
		{"-- up up -d", "gg"},
		{"-f c.yml -- foo up -d", "gg"},
		{"-- -p x -- up up -d", "gg"},
		{"-- -D x up -d", "gg"},
		{"-- -H tcp://127.0.0.1:1 x up -d", "gf"},
		{"-- -h x up -d", "ff"},
		{"-- --dry-run x up -d", "ff"},
		{"-p x -- -D up -d", "ff"},
		{"-- -H tcp://127.0.0.1:1 -- -D up -d", "ff"},
		{"-- -p x -- -H tcp://127.0.0.1:1 up -d", "gg"},
		{"-- -- -H tcp://127.0.0.1:1 up -d", "fg"},
		{"-- -- -D up -d", "ff"},
		{"--compatibility -- -H tcp://127.0.0.1:1 up -d", "fg"},
		{"-p x -- -H tcp://127.0.0.1:1 -p y up -d", "gg"},
		{"-- -D -- -D -- -p x up -d", "gg"},
		{"-- -D -D up -d", "ff"},
		{"-- -D -D x up -d", "gg"},
		{"-- --verbose -- up -d", "ff"},
		{"-- --no-ansi x up -d", "ff"},
		{"-- --all-resources x up -d", "ff"},
		{"-- --version x up -d", "ff"},
		{"-pproj -- -D up -d", "ff"},
		{"-- -- -- x up -d", "gg"},
		{"-p x -- -- up up -d", "gg"},
		{"-- -f c.yml -- x up -d", "gg"},
		{"-- --debug x up -d", "gg"},
		{"-- -p x -D y up -d", "gg"},
		{"-- -p x --debug y up -d", "gg"},
		{"-- --tlsverify x up -d", "gg"},
		{"-- --context x up -d", "fg"},
		{"-- -l x up -d", "gg"},
		{"-- -x v up -d", "gg"},
		{"-- -- up", "ff"},
		// Review round 4: standalone's first pass turns a --verbose past
		// "--" into docker's --debug, up to the first word.
		{"-- -p x --verbose up -d", "gg"},
		{"-- -f c.yml --verbose up -d", "gg"},
		{"-- --ansi never --verbose up -d", "gg"},
		{"-p x --verbose -- -p y up -d", "gg"},
		{"--verbose -- -p x up -d", "gg"},
		{"-- -p x -- --verbose up -d", "gg"},
		{"-- -p --verbose up -d", "fg"},
		{"-- -H tcp://127.0.0.1:1 --verbose up -d", "fg"},
		{"-- -D --verbose up -d", "fg"},
		{"-- -px --verbose up -d", "gg"},
		{"--project-name x -- --verbose up -d", "gg"},
		{"-- --verbose -- -p x up -d", "gg"},
		{"-- --verbose=true up -d", "gg"},
		{"-- -p x --verbose x up -d", "ff"},
		{"-- --verbose -p x up -d", "gg"},
		{"-- -p x --verbose -- -p y up -d", "gg"},
		{"-p x -- -- --verbose up -d", "gg"},
		{"-- --tls --verbose -p x up -d", "gg"},
		{"-- -f c.yml --verbose=true up -d", "gg"},
		{"-- -p x --verbose=false up -d", "gg"},
		{"-- -p x --verbose run --rm a true", "gg"},
		// #347: standalone's first pass walks over a bare "-" or an empty
		// word past "--", and lifts a docker option after it.
		{"-- - -D up -d", "gf"},
		{"-- '' -D up -d", "gf"},
		{"-- -p - -D up -d", "gf"},
		{"-- - --debug up -d", "gf"},
		{"-- '' up -d", "gg"},
		{"-- - -H tcp://127.0.0.1:1 up -d", "gg"},
		{"- -D up -d", "ff"},
		{"'' -D up -d", "ff"},
		{"- up -d", "ff"},
		{"'' up -d", "ff"},
		{"-p x - -D up -d", "ff"},
		{"-D - up -d", "ff"},
		{"-H tcp://127.0.0.1:1 - up -d", "ff"},
		{"-p - up -d", "FF"},
		{"-p '' up -d", "gg"},
		// #352: before "--" too, standalone's first pass walks over an
		// empty word, an option's empty value among them.
		{"-p '' -D up -d", "gF"},
		{"--project-name '' -D up -d", "gF"},
		{"--profile '' -D up -d", "gF"},
		{"--project-directory '' -D up -d", "gF"},
		{"--progress '' -D up -d", "gF"},
		{"--ansi '' -D up -d", "gF"},
		{"--profile '' --debug up -d", "gF"},
		{"--profile '' -H tcp://127.0.0.1:1 up -d", "gF"},
		{"-p '' -D -p y up -d", "gF"},
		{"-f '' -D up -d", "FF"},
		{"--env-file '' -D up -d", "FF"},
		{"-p x -D up -d", "fF"},
		{"--profile x -D up -d", "fF"},
		{"--ansi never -D up -d", "fF"},
		{"--profile '' up -d", "gg"},
		{"-p '' -f c.yml up -d", "gg"},
		{"-p '' --compatibility up -d", "gg"},
		{"-D -p '' up -d", "gg"},
	} {
		args := fieldsWithEmpty(tc.argv)
		for i, form := range []struct {
			name string
			args []string
		}{{"docker-compose", args}, {"docker", append([]string{"compose"}, args...)}} {
			want := tc.sp[i]
			c := Parse(form.name, form.args)
			if gated := c.Kind == "compose"; gated != (want == 'g' || want == 'F') {
				t.Errorf("%s %s: gated %v, want %c", form.name, tc.argv, gated, want)
				continue
			}
			// An engine option after "--" is ignored (rule 3).
			sep := slices.Index(args, "--")
			if sep < 0 {
				sep = len(args)
			}
			if c.HasHost && !slices.ContainsFunc(args[:sep], func(a string) bool { return strings.HasPrefix(a, "-H") || strings.HasPrefix(a, "--host") }) {
				t.Errorf("%s %s: host %q from after --", form.name, tc.argv, c.Host)
			}
			// The probes leave out the "--" and the words after it up to
			// the command, unless it is an option's value.
			if want != 'g' || sep > 0 && sep < len(args) && (args[sep-1] == "-p" || args[sep-1] == "-f") {
				continue
			}
			if out, ok := ComposeConfig(form.name, form.args, false); !ok || slices.Contains(out, "--") {
				t.Errorf("%s %s: ComposeConfig = %q, %v", form.name, tc.argv, out, ok)
			}
			if out, ok := DryRun(form.name, form.args); ok && slices.Contains(out[:max(slices.IndexFunc(out, isComposeCommand), 0)], "--") {
				t.Errorf("%s %s: DryRun = %q", form.name, tc.argv, out)
			}
		}
	}
}

// fieldsWithEmpty is strings.Fields, with two single quotes as the empty word.
func fieldsWithEmpty(s string) []string {
	f := strings.Fields(s)
	for i, w := range f {
		if w == "''" {
			f[i] = ""
		}
	}
	return f
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
		{podStart, []string{"podman-pod-start"}},
		{kubePlay, []string{"podman-kube-play", "podman-play-kube"}},
		{machineStart, []string{"podman-machine-start"}},
		{machineInit, []string{"podman-machine-init"}},
		{podClone, []string{"podman-pod-clone"}},
		{podRestart, []string{"podman-pod-restart"}},
		{containerClone, []string{"podman-container-clone"}},
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

// TestPodCreateStartsNothing pins why pod create passes through: it
// creates a pod and starts nothing, with no --start to ask for that. A
// Podman that adds one fails here.
func TestPodCreateStartsNothing(t *testing.T) {
	if _, ok := helpFlags(t, "podman-pod-create")["--start"]; ok {
		t.Fatal("podman pod create has --start: gate it when given (#230)")
	}
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

func TestDryRunAddsTheFlagAfterCompose(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"compose", "up", "-d"}, []string{"compose", "--dry-run", "up", "-d"}},
		{[]string{"--context", "x", "compose", "-f", "a.yml", "up", "--scale", "web=3", "web"},
			[]string{"--context", "x", "compose", "--dry-run", "-f", "a.yml", "up", "--scale", "web=3", "web"}},
		{[]string{"compose", "restart"}, []string{"compose", "--dry-run", "restart"}},
		{[]string{"compose", "run", "web"}, nil},
		{[]string{"compose", "-f", "-", "up"}, nil},
		{[]string{"run", "alpine"}, nil},
		// A call that sets --dry-run itself is not dry-run: the last value
		// wins, so --dry-run=false after ours would make it a real up.
		{[]string{"compose", "--dry-run=false", "up", "-d"}, nil},
		{[]string{"compose", "up", "--dry-run=0", "-d"}, nil},
		{[]string{"compose", "--dry-run", "up"}, nil},
		// Compose's global options after the command move before it, in
		// their order: the parser reads them there (#278).
		{[]string{"compose", "up", "-p", "proj", "-d"}, []string{"compose", "--dry-run", "-p", "proj", "up", "-d"}},
		{[]string{"compose", "up", "-d", "-f", "c.yml"}, []string{"compose", "--dry-run", "-f", "c.yml", "up", "-d"}},
		{[]string{"compose", "up", "-d", "-fc.yml"}, []string{"compose", "--dry-run", "-fc.yml", "up", "-d"}},
		{[]string{"compose", "up", "--env-file", "e", "-d"}, []string{"compose", "--dry-run", "--env-file", "e", "up", "-d"}},
		{[]string{"compose", "up", "--profile", "x", "-d"}, []string{"compose", "--dry-run", "--profile", "x", "up", "-d"}},
		{[]string{"compose", "-f", "a.yml", "up", "-d", "--file=b.yml", "web", "-p", "proj"},
			[]string{"compose", "--dry-run", "-f", "a.yml", "--file=b.yml", "-p", "proj", "up", "-d", "web"}},
		{[]string{"compose", "up", "-dp", "proj"}, []string{"compose", "--dry-run", "-p", "proj", "up", "-d"}}, // a cluster's own flags stay
		{[]string{"compose", "up", "-d", "--", "-f", "x"}, []string{"compose", "--dry-run", "up", "-d", "--", "-f", "x"}},
		{[]string{"compose", "restart", "-f", "x"}, []string{"compose", "--dry-run", "restart", "-f", "x"}}, // not a file
		{[]string{"compose", "up", "-d", "-p"}, []string{"compose", "--dry-run", "up", "-d", "-p"}},         // no value to move
		// A "--" among compose's global options, and the options after it,
		// are left out: Compose ignores them (#324).
		{[]string{"compose", "-f", "c.yml", "--", "-p", "y", "up", "-d"}, []string{"compose", "--dry-run", "-f", "c.yml", "up", "-d"}},
		{[]string{"compose", "--", "--dry-run", "up", "-d"}, []string{"compose", "--dry-run", "up", "-d"}},
		{[]string{"compose", "--dry-run", "--", "-p", "x", "up", "-d"}, nil},
		{[]string{"compose", "--", "up", "-d"}, nil},
		{[]string{"compose", "-f", "c.yml", "--", "-h", "--", "-p", "y", "up", "-d"}, []string{"compose", "--dry-run", "-f", "c.yml", "up", "-d"}},
	} {
		got, ok := DryRun("docker", tc.args)
		if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
			t.Errorf("DryRun(%q) = %q, %v; want %q", tc.args, got, ok, tc.want)
		}
	}
	// docker-compose has no compose word: --dry-run follows its docker options.
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"up", "-d"}, []string{"--dry-run", "up", "-d"}},
		{[]string{"-H", "h", "-f", "a.yml", "up", "-d"}, []string{"-H", "h", "--dry-run", "-f", "a.yml", "up", "-d"}},
		{[]string{"--context=x", "-D", "restart"}, []string{"--context=x", "-D", "--dry-run", "restart"}},
		{[]string{"run", "web"}, nil},
		{[]string{"--dry-run", "up"}, nil},
		{[]string{"-c", "x", "up"}, nil},
		{[]string{"--compatibility", "-H", "h", "-f", "a.yml", "up", "-d"}, []string{"-H", "h", "--dry-run", "--compatibility", "-f", "a.yml", "up", "-d"}},
		{[]string{"-fa.yml", "-H", "h", "up", "-d"}, []string{"-H", "h", "--dry-run", "-fa.yml", "up", "-d"}},
		{[]string{"up", "-p", "proj", "-d"}, []string{"--dry-run", "-p", "proj", "up", "-d"}},
		{[]string{"-H", "h", "up", "-d", "-fc.yml"}, []string{"-H", "h", "--dry-run", "-fc.yml", "up", "-d"}},
		{[]string{"up", "--env-file", "e", "--profile", "x", "-d"}, []string{"--dry-run", "--env-file", "e", "--profile", "x", "up", "-d"}},
		{[]string{"-f", "c.yml", "--", "-p", "y", "up", "-d"}, []string{"--dry-run", "-f", "c.yml", "up", "-d"}},
		{[]string{"-H", "h", "--", "-H", "x", "-p", "y", "up", "-d"}, []string{"-H", "h", "--dry-run", "up", "-d"}},
		{[]string{"--", "--dry-run", "up", "-d"}, []string{"--dry-run", "up", "-d"}},
		{[]string{"--", "-H", "h", "up", "-d"}, nil},
		{[]string{"-p", "x", "--", "--", "--version", "-p", "y", "up", "-d"}, []string{"--dry-run", "-p", "x", "up", "-d"}},
	} {
		got, ok := DryRun("docker-compose", tc.args)
		if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
			t.Errorf("DryRun(docker-compose %q) = %q, %v; want %q", tc.args, got, ok, tc.want)
		}
	}
	// podman compose hands its args to the provider: podman's own options
	// before compose pick the socket, and stay (#349).
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"compose", "up", "-d"}, []string{"compose", "--dry-run", "up", "-d"}},
		{[]string{"--url", "U", "compose", "-f", "a.yml", "up", "-d"}, []string{"--url", "U", "compose", "--dry-run", "-f", "a.yml", "up", "-d"}},
		{[]string{"-c", "conn", "compose", "-p", "x", "up", "-d"}, []string{"-c", "conn", "compose", "--dry-run", "-p", "x", "up", "-d"}},
	} {
		got, ok := DryRun("podman", tc.args)
		if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
			t.Errorf("DryRun(podman %q) = %q, %v; want %q", tc.args, got, ok, tc.want)
		}
	}
}

func TestComposeConfigKeepsTheCallsGlobals(t *testing.T) {
	got, ok := ComposeConfig("docker", []string{"--context", "x", "compose", "-p", "evil", "--env-file", "e", "up", "-d", "web"}, true)
	want := []string{"--context", "x", "compose", "-p", "evil", "--env-file", "e", "--profile", "*", "config", "--format", "json"}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("ComposeConfig = %q, %v; want %q", got, ok, want)
	}
	if got, _ := ComposeConfig("docker", []string{"compose", "-p", "x", "up"}, false); !slices.Equal(got, []string{"compose", "-p", "x", "config", "--format", "json"}) {
		t.Fatalf("one profile set: %q", got)
	}
	if _, ok := ComposeConfig("docker", []string{"run", "alpine"}, false); ok {
		t.Fatal("not compose")
	}
	got, ok = ComposeConfig("docker-compose", []string{"-H", "h", "-f", "a.yml", "up", "-d"}, false)
	want = []string{"-H", "h", "-f", "a.yml", "config", "--format", "json"}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("docker-compose: ComposeConfig = %q, %v; want %q", got, ok, want)
	}
	got, ok = ComposeConfig("docker-compose", []string{"--compatibility", "-H", "h", "-f", "a.yml", "up", "-d"}, false)
	want = []string{"-H", "h", "--compatibility", "-f", "a.yml", "config", "--format", "json"}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("docker-compose, a docker option after a compose one: ComposeConfig = %q, %v; want %q", got, ok, want)
	}
	got, ok = ComposeConfig("docker-compose", []string{"-fa.yml", "-pproj", "-H", "h", "up", "-d"}, false)
	want = []string{"-H", "h", "-fa.yml", "-pproj", "config", "--format", "json"}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("docker-compose, joined shorthands: ComposeConfig = %q, %v; want %q", got, ok, want)
	}
	if _, ok := ComposeConfig("docker-compose", []string{"--config", "d", "up"}, false); ok {
		t.Fatal("docker-compose --config: the call fails")
	}
	// Compose's global options after the command move before it, in their
	// order: the parser reads them there, so the probe asks about the
	// project the Call is keyed on (#278).
	for _, tc := range []struct {
		name       string
		args, want []string
	}{
		{"docker", []string{"compose", "up", "-p", "proj", "-d"}, []string{"compose", "-p", "proj", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "-d", "-f", "c.yml"}, []string{"compose", "-f", "c.yml", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "-d", "-fc.yml"}, []string{"compose", "-fc.yml", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "-d", "-f", "c.yml", "--env-file", "e"}, []string{"compose", "-f", "c.yml", "--env-file", "e", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "--env-file", "e", "-d"}, []string{"compose", "--env-file", "e", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "--profile", "x", "-d"}, []string{"compose", "--profile", "x", "config", "--format", "json"}},
		{"docker", []string{"compose", "-f", "a.yml", "up", "-d", "--file=b.yml", "web", "-p", "proj"},
			[]string{"compose", "-f", "a.yml", "--file=b.yml", "-p", "proj", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "-dp", "proj"}, []string{"compose", "-p", "proj", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "-d", "--", "-f", "x"}, []string{"compose", "config", "--format", "json"}},
		// run's -p is --publish, and the words after its service are its
		// command's; restart's -f is not a file.
		{"docker", []string{"compose", "run", "-p", "80:80", "a", "true", "-f", "x"}, []string{"compose", "config", "--format", "json"}},
		{"docker", []string{"compose", "restart", "-f", "x"}, []string{"compose", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "-d", "-p"}, []string{"compose", "config", "--format", "json"}},
		{"docker", []string{"compose", "up", "-dp"}, []string{"compose", "config", "--format", "json"}},
		{"docker-compose", []string{"up", "-p", "proj", "-d"}, []string{"-p", "proj", "config", "--format", "json"}},
		{"docker-compose", []string{"-H", "h", "up", "-d", "-fc.yml"}, []string{"-H", "h", "-fc.yml", "config", "--format", "json"}},
		{"docker-compose", []string{"up", "--env-file", "e", "--profile", "x", "-d"}, []string{"--env-file", "e", "--profile", "x", "config", "--format", "json"}},
		// A "--" among compose's global options, and the options after it,
		// are left out: Compose ignores them (#324).
		{"docker", []string{"compose", "-f", "c.yml", "--", "-p", "y", "up", "-d"}, []string{"compose", "-f", "c.yml", "config", "--format", "json"}},
		{"docker", []string{"--context", "x", "compose", "--", "-p", "y", "-H", "h", "up"}, []string{"--context", "x", "compose", "config", "--format", "json"}},
		{"docker-compose", []string{"-f", "c.yml", "--", "-p", "y", "up", "-d"}, []string{"-f", "c.yml", "config", "--format", "json"}},
		{"docker-compose", []string{"-H", "h", "--", "-p", "y", "up", "-d"}, []string{"-H", "h", "config", "--format", "json"}},
		{"docker", []string{"compose", "-f", "c.yml", "--", "--help", "--", "-p", "y", "up", "-d"}, []string{"compose", "-f", "c.yml", "config", "--format", "json"}},
		{"docker-compose", []string{"-p", "x", "--", "--", "--version", "-p", "y", "up", "-d"}, []string{"-p", "x", "config", "--format", "json"}},
		// podman compose: podman's own options stay before compose (#349).
		{"podman", []string{"compose", "up", "-d"}, []string{"compose", "config", "--format", "json"}},
		{"podman", []string{"--url", "U", "compose", "-f", "a.yml", "up", "-d"}, []string{"--url", "U", "compose", "-f", "a.yml", "config", "--format", "json"}},
		{"podman", []string{"-c", "conn", "compose", "-p", "x", "up", "-d"}, []string{"-c", "conn", "compose", "-p", "x", "config", "--format", "json"}},
	} {
		if got, ok := ComposeConfig(tc.name, tc.args, false); !ok || !slices.Equal(got, tc.want) {
			t.Errorf("ComposeConfig(%s %q) = %q, %v; want %q", tc.name, tc.args, got, ok, tc.want)
		}
	}
}
