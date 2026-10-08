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
	// Op is run, create, start, restart, up or clone.
	Op string
	// Command names the call without its flags or arguments, which may hold
	// secrets: "docker run postgres:17", "docker compose up", "tart run ci-vm".
	Command string
	// Target is the image, container, compose project (-p) or VM; "" if
	// unknown. It is the raw argument: use Command for anything shown.
	Target string
	// MemoryBytes is the memory limit given with -m/--memory; 0 if none.
	MemoryBytes uint64
	// Endpoint is the engine chosen with --context, -H/--host,
	// -c/--connection or --url; "" for the default. A remote engine's
	// memory is not this Mac's. It is raw: it may hold a user name.
	Endpoint string
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
	var c Call
	for {
		version := false
		rest, res := scan(args, engineGlobal, func(f, v string) {
			switch f {
			case "--context", "-H", "--host", "-c", "--connection", "--url":
				c.Endpoint = v
			case "-v":
				version = true // docker -v and podman -v print the version
			}
		})
		if res == askedHelp || version || len(rest) == 0 {
			return Call{}
		}
		if res != unknownFlag {
			args = rest
			break
		}
		// A global flag newer than the tables: it takes a value unless a
		// subcommand follows it. Gating too much beats letting a call by.
		switch {
		case len(rest) > 1 && engineCommand(rest[1]):
			args = rest[1:]
		case len(rest) > 2:
			args = rest[2:]
		default:
			return Call{}
		}
	}
	words := []string{name}
	if args[0] == "container" {
		words, args = append(words, "container"), args[1:]
		if len(args) == 0 || args[0] == "compose" {
			return Call{}
		}
	}
	c.Op = args[0]
	var flags flagSet
	switch c.Op {
	case "run", "create":
		flags = containerRun
	case "start", "restart":
		flags = containerStart
	case "compose":
		if len(words) > 1 {
			return Call{}
		}
		return parseCompose(c.Endpoint, append(words, "compose"), args[1:])
	default:
		return Call{}
	}
	var mem string
	pos, res := scan(args[1:], flags, func(f, v string) {
		if f == "-m" || f == "--memory" {
			mem = v
		}
	})
	if res == askedHelp {
		return Call{}
	}
	c.Kind, c.MemoryBytes = "container", parseBytes(mem)
	switch {
	case res == unknownFlag:
		// The image is a guess from here on, but a memory limit after the
		// unknown flag still counts.
		if m := lastMemory(pos); m > 0 {
			c.MemoryBytes = m
		}
	case len(pos) > 0:
		c.Target = pos[0]
	}
	return c.named(append(words, c.Op))
}

// lastMemory is the last -m/--memory limit anywhere in args; 0 if none.
func lastMemory(args []string) uint64 {
	var mem uint64
	for i, a := range args {
		v := ""
		switch {
		case a == "-m" || a == "--memory":
			if i+1 < len(args) {
				v = args[i+1]
			}
		case strings.HasPrefix(a, "--memory="):
			v = a[len("--memory="):]
		case strings.HasPrefix(a, "-m") && !strings.HasPrefix(a, "--"):
			v = strings.TrimPrefix(a[2:], "=")
		}
		if b := parseBytes(v); b > 0 {
			mem = b
		}
	}
	return mem
}

// engineCommand reports whether w is a docker or podman command Parse looks
// for.
func engineCommand(w string) bool {
	switch w {
	case "run", "create", "start", "restart", "compose", "container":
		return true
	}
	return false
}

func parseCompose(endpoint string, words, args []string) Call {
	var project string
	dryRun, noUp := false, false
	for {
		rest, res := scan(args, composeGlobal, func(f, v string) {
			switch f {
			case "-p", "--project-name":
				project = v
			case "--dry-run":
				dryRun = isTrue(v)
			}
		})
		if res == askedHelp || len(rest) == 0 {
			return Call{}
		}
		if res != unknownFlag {
			args = rest
			break
		}
		// A flag the table lacks (another provider's, such as
		// podman-compose's --podman-path): as for the engine, it takes a
		// value unless a compose command follows it.
		switch {
		case len(rest) > 1 && composeFlags(rest[1]) != nil:
			args = rest[1:]
		case len(rest) > 2:
			args = rest[2:]
		default:
			return Call{}
		}
	}
	op := args[0]
	flags := composeFlags(op)
	if flags == nil {
		return Call{}
	}
	// The subcommand's own flags: -p is --publish here, not the project.
	_, res := scan(args[1:], flags, func(f, v string) {
		switch f {
		case "--dry-run":
			dryRun = isTrue(v)
		case "--no-up":
			noUp = isTrue(v)
		}
	})
	if res == askedHelp || dryRun || noUp {
		return Call{} // starts nothing
	}
	c := Call{Kind: "compose", Op: op, Target: project, Endpoint: endpoint}
	return c.named(append(words, op))
}

// composeFlags is the flag table of a compose command that starts
// containers; nil for the others.
func composeFlags(op string) flagSet {
	switch op {
	case "up":
		return composeUp
	case "run":
		return composeRun
	case "start":
		return composeStart
	case "restart":
		return composeRestart
	case "create":
		return composeCreate
	case "scale":
		return composeScale
	case "watch":
		return composeWatch // builds and starts the services first
	}
	return nil
}

// isTrue reads a boolean flag's value as the CLIs do (Go's ParseBool).
func isTrue(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

func parseTart(args []string) Call {
	if len(args) == 0 {
		return Call{}
	}
	c := Call{Kind: "tart", Op: args[0]}
	var flags flagSet
	n := 0 // which positional names the VM this call makes
	switch c.Op {
	case "run":
		flags = tartRun
	case "clone":
		flags, n = tartClone, 1 // clone <source> <new-name>
	default:
		return Call{}
	}
	// swift-argument-parser takes options before, between and after the
	// positionals.
	var pos []string
	for args = args[1:]; ; {
		rest, res := scan(args, flags, nil)
		if res == askedHelp {
			return Call{}
		}
		if res == unknownFlag {
			pos = nil // what follows may be its value
			break
		}
		if res == endOfFlags {
			pos = append(pos, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
	if len(pos) > n {
		c.Target = pos[n]
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

// scanResult is how scan stopped.
type scanResult int

const (
	scanned     scanResult = iota // at the first positional argument, or the end
	endOfFlags                    // after "--": all of rest is positional
	unknownFlag                   // at a flag not in the table; rest starts with it
	askedHelp                     // --help, --version, or -h where it is no flag of its own
)

// scan reads the flags in args up to the first positional argument, calling
// seen (if set) for each with its value ("true" for a boolean given bare),
// and returns the arguments from there on.
func scan(args []string, flags flagSet, seen func(flag, value string)) ([]string, scanResult) {
	if seen == nil {
		seen = func(string, string) {}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return args[i+1:], endOfFlags
		case strings.HasPrefix(a, "--"):
			f, v, hasValue := strings.Cut(a, "=")
			if f == "--help" || f == "--version" {
				// --help=false asks for nothing; a bad value fails the call.
				if b, err := strconv.ParseBool(v); !hasValue || err != nil || b {
					return nil, askedHelp
				}
				continue
			}
			takes, known := flags[f]
			switch {
			case !known && hasValue:
				continue // --flag=value: unknown, but whole
			case !known:
				return args[i:], unknownFlag
			case takes && !hasValue:
				if i+1 == len(args) {
					return nil, scanned
				}
				i++
				v = args[i]
			case !takes && !hasValue:
				v = "true"
			}
			seen(f, v)
		case len(a) > 1 && a[0] == '-':
			// A cluster of short flags: -it, -dm2g, -m=2g, -p 80:80.
			for j := 1; j < len(a); j++ {
				f := "-" + a[j:j+1]
				takes, known := flags[f]
				if f == "-h" && !takes {
					return nil, askedHelp
				}
				if !known {
					return args[i:], unknownFlag
				}
				if !takes {
					seen(f, "true")
					continue
				}
				v, explicit := a[j+1:], false
				if strings.HasPrefix(v, "=") {
					v, explicit = v[1:], true // -m= is an empty value
				}
				if v == "" && !explicit {
					if i+1 == len(args) {
						return nil, scanned
					}
					i++
					v = args[i]
				}
				seen(f, v)
				break
			}
		default:
			return args[i:], scanned
		}
	}
	return nil, scanned
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
	containerStart = flags( // start and restart
		"--detach-keys --filter --signal --time --timeout -f -s -t",
		"--all --attach --interactive --running -a -i")
	composeGlobal = flags(
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
	composeStart   = flags("--wait-timeout", "--dry-run --wait")
	composeRestart = flags("--timeout -t", "--dry-run --no-deps")
	composeCreate  = flags("--pull --scale",
		"--build --dry-run --force-recreate --no-build --no-recreate --quiet-pull --remove-orphans --yes -y")
	composeScale = flags("", "--dry-run --no-deps")
	composeWatch = flags("", "--dry-run --no-up --prune --quiet")
	tartRun      = flags(
		"--dir --disk --net-bridged --net-softnet-allow --net-softnet-block --net-softnet-control-fd --net-softnet-expose "+
			"--provisioning-opts --root-disk-opts --rosetta --serial-path",
		"--capture-system-keys --help --nested --net-host --net-softnet --no-audio --no-clipboard --no-graphics "+
			"--no-keyboard --no-pointer --no-trackpad --no-usb-accessories --recovery --serial --suspendable --version "+
			"--vnc --vnc-experimental -h")
	tartClone = flags("--concurrency --prune-limit", "--help --insecure --overwrite --stacked --version -h")
)
