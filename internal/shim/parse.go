package shim

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Call is what a docker, podman or tart call would start (R5, #27). Only
// calls with a Kind are gated; everything else passes straight through.
type Call struct {
	// Kind is container, compose or tart (protocol.CheckRequest.Kind); ""
	// means the call starts nothing and passes through.
	Kind string
	// Op is run, create, start, up or clone.
	Op string
	// Command names the call without its flags or arguments, which may hold
	// secrets: "docker run postgres:17", "docker compose up", "tart run ci-vm".
	Command string
	// Target is the image, container, compose project (-p) or VM; "" if
	// unknown. It is the raw argument: use Command for anything shown.
	Target string
	// MemoryBytes is the memory limit given with -m/--memory; 0 if none.
	MemoryBytes uint64
}

// Parse tells what the call name args (argv[1:]) would start. It is pure and
// cheap: every docker, podman and tart call goes through it.
func Parse(name string, args []string) Call {
	switch name {
	case "docker", "podman":
		return parseEngine(name, args)
	case "tart":
		return parseTart(args)
	}
	return Call{}
}

func parseEngine(name string, args []string) Call {
	version := false
	rest, ok := scan(args, engineGlobal, func(f, _ string) { version = version || f == "-v" || f == "--version" })
	// An unknown global flag may take a value: what follows is a guess, so
	// the call passes through.
	if !ok || version || len(rest) == 0 {
		return Call{}
	}
	words := []string{name}
	if rest[0] == "container" {
		words, rest = append(words, "container"), rest[1:]
		if len(rest) == 0 || rest[0] == "compose" {
			return Call{}
		}
	}
	switch op := rest[0]; op {
	case "run", "create":
		var mem string
		c := Call{Kind: "container", Op: op}
		pos, ok := scan(rest[1:], containerRun, func(f, v string) {
			if f == "-m" || f == "--memory" {
				mem = v
			}
		})
		if pos == nil && !ok {
			return Call{} // --help
		}
		c.MemoryBytes = parseBytes(mem)
		if ok && len(pos) > 0 {
			c.Target = pos[0]
		}
		return c.named(append(words, op))
	case "start":
		c := Call{Kind: "container", Op: op}
		pos, ok := scan(rest[1:], containerStart, nil)
		if pos == nil && !ok {
			return Call{}
		}
		if ok && len(pos) > 0 {
			c.Target = pos[0]
		}
		return c.named(append(words, op))
	case "compose":
		return parseCompose(append(words, op), rest[1:])
	}
	return Call{}
}

func parseCompose(words, args []string) Call {
	var project string
	rest, ok := scan(args, composeGlobal, func(f, v string) {
		if f == "-p" || f == "--project-name" {
			project = v
		}
	})
	if !ok || len(rest) == 0 {
		return Call{}
	}
	flags := map[string]flagSet{"up": composeUp, "run": composeRun}[rest[0]]
	if flags == nil {
		return Call{}
	}
	if pos, ok := scan(rest[1:], flags, nil); pos == nil && !ok {
		return Call{}
	}
	c := Call{Kind: "compose", Op: rest[0], Target: project}
	return c.named(append(words, rest[0]))
}

func parseTart(args []string) Call {
	if len(args) == 0 {
		return Call{}
	}
	flags := map[string]flagSet{"run": tartRun, "clone": tartClone}[args[0]]
	if flags == nil {
		return Call{}
	}
	c := Call{Kind: "tart", Op: args[0]}
	pos, ok := scan(args[1:], flags, nil)
	if pos == nil && !ok {
		return Call{}
	}
	if ok {
		// run <vm>; clone <source> <new-name>: the VM this call makes.
		if i := map[string]int{"run": 0, "clone": 1}[c.Op]; len(pos) > i {
			c.Target = pos[i]
		}
	}
	return c.named([]string{"tart", c.Op})
}

// named sets Command to words and, if it is safe to show, the target. A
// compose project stays out: it would read as a service.
func (c Call) named(words []string) Call {
	if c.Kind != "compose" && safeWord(c.Target) {
		words = append(words, c.Target)
	}
	c.Command = strings.Join(words, " ")
	return c
}

// safeWord reports whether w may be shown: no flag, nothing that could hold
// a value or credentials (= or @), no spaces or control characters.
func safeWord(w string) bool {
	return w != "" && len(w) <= 128 && !strings.HasPrefix(w, "-") && !strings.ContainsAny(w, "=@") &&
		!strings.ContainsFunc(w, isSpaceOrControl)
}

func isSpaceOrControl(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

// flagSet maps the flags of a command ("-m", "--memory") to whether they take
// a value. The tables below are checked against the CLIs' help text.
type flagSet map[string]bool

// scan reads the flags in args up to the first positional argument, calling
// seen for each (with its value, or "" for a boolean), and returns the
// arguments from there on. ok is false at an unknown flag, which may or may
// not take a value; rest is then nil. Help (--help, --version, and -h where
// it is not a flag of its own) returns nil and false too.
func scan(args []string, flags flagSet, seen func(flag, value string)) (rest []string, ok bool) {
	if seen == nil {
		seen = func(string, string) {}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return args[i+1:], true
		case a == "--help" || a == "--version":
			return nil, false
		case strings.HasPrefix(a, "--"):
			f, v, hasValue := strings.Cut(a, "=")
			takes, known := flags[f]
			if !known {
				if hasValue {
					continue // --flag=value: unknown, but whole
				}
				return []string{}, false
			}
			if takes && !hasValue {
				if i+1 == len(args) {
					return []string{}, true
				}
				i++
				v = args[i]
			}
			seen(f, v)
		case len(a) > 1 && a[0] == '-':
			// A cluster of short flags: -it, -dm2g, -p 80:80.
			for j := 1; j < len(a); j++ {
				f := "-" + a[j:j+1]
				takes, known := flags[f]
				if f == "-h" && !takes {
					return nil, false
				}
				if !known {
					return []string{}, false
				}
				if !takes {
					seen(f, "")
					continue
				}
				v := strings.TrimPrefix(a[j+1:], "=")
				if v == "" {
					if i+1 == len(args) {
						return []string{}, true
					}
					i++
					v = args[i]
				}
				seen(f, v)
				break
			}
		default:
			return args[i:], true
		}
	}
	return []string{}, true
}

// byteSize is docker's memory syntax: a number, an optional unit (b, k, m, g,
// t or p, powers of 1024) and optional "i" and "b".
var byteSize = regexp.MustCompile(`^(?i)(\d+(?:\.\d+)?) ?([kmgtp])?i?b?$`)

// parseBytes reads a -m/--memory value; 0 if it is missing or invalid.
func parseBytes(s string) uint64 {
	m := byteSize.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	if m[2] != "" {
		n *= math.Pow(1024, float64(strings.Index("kmgtp", strings.ToLower(m[2]))+1))
	}
	if n >= 1<<60 {
		return 0
	}
	return uint64(n)
}

// flags builds a flagSet from space-separated flags that take a value and
// ones that do not.
func flags(value, boolean string) flagSet {
	s := flagSet{}
	for _, f := range strings.Fields(value) {
		s[f] = true
	}
	for _, f := range strings.Fields(boolean) {
		s[f] = false
	}
	return s
}

// The flags of docker and podman (as both, since docker may be Podman) and
// tart, from their help text in testdata.
var (
	engineGlobal = flags(
		"--config --connection --context --host --identity --log-level --out --ssh --storage-opt --tls-ca --tls-cert --tls-details --tls-key --tlscacert --tlscert --tlskey --url -H -c -l",
		"--debug --help --tls --tlsverify --version -D -v")
	containerRun = flags(
		"--add-host --annotation --arch --attach --authfile --blkio-weight --blkio-weight-device --cap-add --cap-drop "+
			"--cgroup-conf --cgroup-parent --cgroupns --cgroups --chrootdirs --cidfile --cpu-period --cpu-quota --cpu-rt-period "+
			"--cpu-rt-runtime --cpu-shares --cpus --cpuset-cpus --cpuset-mems --creds --detach-keys --device --device-cgroup-rule "+
			"--device-read-bps --device-read-iops --device-write-bps --device-write-iops --dns --dns-option --dns-search "+
			"--domainname --entrypoint --env --env-file --env-merge --expose --gidmap --gpus --group-add --group-entry "+
			"--health-cmd --health-interval --health-log-destination --health-max-log-count --health-max-log-size "+
			"--health-on-failure --health-retries --health-start-interval --health-start-period --health-startup-cmd "+
			"--health-startup-interval --health-startup-retries --health-startup-success --health-startup-timeout "+
			"--health-timeout --hostname --hosts-file --hostuser --image-volume --init-ctr --init-path --ip --ip6 --ipc "+
			"--isolation --label --label-file --link --link-local-ip --log-driver --log-opt --mac-address --memory "+
			"--memory-reservation --memory-swap --memory-swappiness --mount --name --network --network-alias --oom-score-adj "+
			"--os --passwd-entry --personality --pid --pids-limit --platform --pod --pod-id-file --publish --pull --rdt-class "+
			"--requires --restart --retry --retry-delay --runtime --sdnotify --seccomp-policy --secret --security-opt "+
			"--shm-size --shm-size-systemd --stop-signal --stop-timeout --storage-opt --subgidname --subuidname --sysctl "+
			"--systemd --timeout --tmpfs --tz --uidmap --ulimit --umask --unsetenv --user --userns --uts --variant --volume "+
			"--volume-driver --volumes-from --workdir -a -c -e -h -l -m -p -u -v -w",
		"--detach --disable-content-trust --help --http-proxy --init --interactive --no-healthcheck --no-hostname "+
			"--no-hosts --oom-kill-disable --passwd --privileged --publish-all --quiet --read-only --read-only-tmpfs "+
			"--replace --rm --rmi --rootfs --sig-proxy --tls-verify --tty --unsetenv-all --use-api-socket -P -d -i -q -t")
	containerStart = flags("--detach-keys --filter -f", "--all --attach --interactive -a -i")
	composeGlobal  = flags(
		"--ansi --env-file --file --parallel --profile --progress --project-directory --project-name -f -p",
		"--all-resources --compatibility --dry-run")
	composeUp = flags(
		"--attach --exit-code-from --no-attach --pull --scale --timeout --wait-timeout -t",
		"--abort-on-container-exit --abort-on-container-failure --always-recreate-deps --attach-dependencies --build "+
			"--detach --dry-run --force-recreate --menu --no-build --no-color --no-deps --no-log-prefix --no-recreate "+
			"--no-start --quiet-build --quiet-pull --remove-orphans --renew-anon-volumes --timestamps --wait --watch --yes "+
			"-V -d -w -y")
	composeRun = flags(
		"--cap-add --cap-drop --entrypoint --env --env-from-file --label --name --publish --pull --user --volume --workdir "+
			"-e -l -p -u -v -w",
		"--build --detach --dry-run --interactive --no-deps --no-tty --quiet --quiet-build --quiet-pull --remove-orphans "+
			"--rm --service-ports --use-aliases -P -T -d -i -q")
	tartRun = flags(
		"--dir --disk --net-bridged --net-softnet-allow --net-softnet-block --net-softnet-control-fd --net-softnet-expose "+
			"--provisioning-opts --root-disk-opts --rosetta --serial-path",
		"--capture-system-keys --help --nested --net-host --net-softnet --no-audio --no-clipboard --no-graphics "+
			"--no-keyboard --no-pointer --no-trackpad --no-usb-accessories --recovery --serial --suspendable --version "+
			"--vnc --vnc-experimental -h")
	tartClone = flags("--concurrency --prune-limit", "--help --insecure --overwrite --stacked --version -h")
)
