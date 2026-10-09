package shim

import (
	"math"
	"slices"
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
	// ConfigDir is docker's --config: where it reads its context from.
	ConfigDir string
	// MultiTarget is set for a start or restart of several containers:
	// Target is only the first.
	MultiTarget bool
	// Targets are all the containers a start or restart names.
	Targets []string
	// ComposeEnvFiles are a compose call's --env-files, in order, which
	// Compose reads instead of the project's .env.
	ComposeEnvFiles []string
	// ComposeDetached is set for an up that detaches (-d, --wait): only its
	// dry run shows what it starts.
	ComposeDetached bool
	// ComposeFiles are a compose call's -f files, and ComposeProjectDir its
	// --project-directory, as given: the shim finds the project's name
	// from them as Compose does (#33).
	ComposeFiles      []string
	ComposeProjectDir string
	// Name is the container name given with --name, for run and create;
	// "" if none. With Target it lets the daemon tell the call's container
	// from others that appear at the same time (#33).
	Name string
	// MemoryBytes is the memory limit given with -m/--memory; 0 if none.
	MemoryBytes uint64
	// Host is the engine given with -H/--host or --url, and HasHost whether
	// one was: docker reads -H "" as its default socket, not DOCKER_HOST.
	// Context is the one named with --context or -c/--connection. A remote
	// engine's memory is not this Mac's. Both are raw: they may hold a user
	// name. HostLast is whether Host was given after Context.
	Host     string
	HasHost  bool
	Context  string
	HostLast bool
}

// Parse tells what the call name args (argv[1:]) would start. It is pure and
// cheap: every docker, podman and tart call goes through it.
func Parse(name string, args []string) Call {
	switch name {
	case "docker", "podman":
		c, _ := parseEngine(name, args)
		return c
	case "tart":
		return parseTart(args)
	}
	return Call{}
}

// parseEngine parses a docker or podman call. at is where a run or create
// takes one more option last: before its image (and a "--" ahead of it), or
// right after the subcommand when the image is not known; for compose,
// right after "compose"; -1 otherwise.
func parseEngine(name string, all []string) (c Call, at int) {
	args := all
	version := false
	args, res, _ := scanPast(args, engineGlobal, isEngineCommand, func(f, v string) {
		switch f {
		case "-H", "--host", "--url":
			c.Host, c.HasHost, c.HostLast = v, true, true
		case "--context", "-c", "--connection":
			c.Context, c.HostLast = v, false
		case "--config":
			c.ConfigDir = v
		case "-v":
			version = IsTrue(v) // docker -v and podman -v print the version
		}
	})
	if res == askedHelp || version || len(args) == 0 {
		return Call{}, -1
	}
	words := []string{name}
	if args[0] == "container" {
		words, args = append(words, "container"), args[1:]
		if len(args) == 0 || args[0] == "compose" {
			return Call{}, -1
		}
	}
	opAt := len(all) - len(args) // args is what is left of all
	c.Op = args[0]
	var flags flagSet
	switch c.Op {
	case "run", "create":
		flags = containerRun
	case "start", "restart":
		flags = containerStart
	case "compose":
		if len(words) > 1 {
			return Call{}, -1
		}
		return parseCompose(c, append(words, "compose"), args[1:]), opAt + 1
	default:
		return Call{}, -1
	}
	var mem, cname string
	seen := func(f, v string) {
		switch f {
		case "-m", "--memory":
			mem = v
		case "--name":
			cname = v
		}
	}
	var pos []string
	var guessed bool
	if c.Op == "run" || c.Op == "create" {
		// Flags after the image are its command's: scan stops there.
		pos, res, guessed = scanPast(args[1:], flags, nil, seen)
	} else {
		pos, res, guessed = scanAll(args[1:], flags, seen) // start db --help
	}
	if res == askedHelp {
		return Call{}, -1
	}
	c.Kind, c.MemoryBytes = "container", parseBytes(mem)
	at = -1
	if c.Op == "run" || c.Op == "create" {
		c.Name = cname
		at = opAt + 1
		if !guessed && len(pos) > 0 {
			at = len(all) - len(pos) // pos is what is left: the image on
			if res == endOfFlags {
				at-- // before the "--" that ended the options (not a flag's value)
			}
		}
	}
	if !guessed && len(pos) > 0 {
		c.Target = pos[0] // past a guess, it may be a flag's value
		c.MultiTarget = (c.Op == "start" || c.Op == "restart") && len(pos) > 1
		if c.MultiTarget {
			c.Targets = pos
		}
	}
	return c.named(append(words, c.Op)), at
}

// Labelled returns a docker or podman run or create call's args with a
// --label key=value added as its last option, so the container it creates
// carries it: Docker keeps the last --label of a key, after --label-file. ok is false for any other call, which creates no
// container (start, compose, tart) or none at all; args is not changed.
func Labelled(name string, args []string, key, value string) (out []string, ok bool) {
	if name != "docker" && name != "podman" {
		return nil, false
	}
	c, at := parseEngine(name, args)
	if c.Kind != "container" || at < 0 {
		return nil, false
	}
	out = append(out, args[:at]...)
	out = append(out, "--label", key+"="+value)
	return append(out, args[at:]...), true
}

// SetsLabel reports whether args set the label key: --label key=v,
// --label=key=v, -l key=v, -lkey=v, or -l in a cluster after run's
// boolean shorthands (-qdl key=v), as the run and create table knows them;
// an unknown one counts as boolean, so a newer one cannot hide it. The command after the image is scanned too: Labelled cannot
// always tell where it starts, and a false match only refuses a call that
// spells out headroom's own label.
func SetsLabel(args []string, key string) bool {
	names := func(v string) bool {
		return strings.HasPrefix(v, key) && (len(v) == len(key) || v[len(key)] == '=')
	}
	for i, a := range args {
		var v string
		switch {
		case a == "--label":
			if i+1 < len(args) {
				v = args[i+1]
			}
		case strings.HasPrefix(a, "--label="):
			v = strings.TrimPrefix(a, "--label=")
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			// -l, -dl, -ldev.…: booleans, then l and its value. Any other
			// flag that takes a value takes the rest of the cluster.
			for j := 1; j < len(a); j++ {
				f := "-" + a[j:j+1]
				if f == "-l" {
					v = strings.TrimPrefix(a[j+1:], "=")
					if v == "" && j+1 == len(a) && i+1 < len(args) {
						v = args[i+1]
					}
					break
				}
				if containerRun[f] {
					break
				}
			}
		}
		if names(v) {
			return true
		}
	}
	return false
}

// DryRun returns a docker compose call's args with --dry-run added as a
// compose option, so Compose says what the call would do and does none of
// it. ok is false for any other call, for a compose run (its one-off
// container is always new), one whose file is on stdin (the call needs it)
// and one that sets --dry-run itself.
func DryRun(args []string) (out []string, ok bool) {
	c, at := parseEngine("docker", args)
	if c.Kind != "compose" || at < 0 || c.Op == "run" || slices.Contains(c.ComposeFiles, "-") {
		return nil, false
	}
	if slices.ContainsFunc(args, func(a string) bool { return a == "--dry-run" || strings.HasPrefix(a, "--dry-run=") }) {
		// It sets --dry-run itself, and the last value wins: with
		// --dry-run=false after ours, the dry run would be the real call.
		return nil, false
	}
	out = append(out, args[:at]...)
	out = append(out, "--dry-run")
	return append(out, args[at:]...), true
}

// ComposeConfig returns the args that ask Compose for a docker compose
// call's project as JSON: the call's own global options (-p, -f,
// --env-file, --config, --context), which Compose interpolates, then
// config; with every profile's services if all. ok is false for any other
// call.
func ComposeConfig(args []string, all bool) (out []string, ok bool) {
	c, at := parseEngine("docker", args)
	if c.Kind != "compose" || at < 0 {
		return nil, false
	}
	rest, res, _ := scanPast(args[at:], composeGlobal, isComposeCommand, nil)
	if res == askedHelp || len(rest) == 0 {
		return nil, false
	}
	out = slices.Clone(args[:len(args)-len(rest)])
	if all {
		out = append(out, "--profile", "*")
	}
	return append(out, "config", "--format", "json"), true
}

// parseCompose parses a compose call; engine holds the docker call's global
// options, which Compose, a CLI plugin, runs under.
func parseCompose(engine Call, words, args []string) Call {
	var project, projectDir string
	var files, envFiles []string
	dryRun, noUp, detached := false, false, false
	args, res, _ := scanPast(args, composeGlobal, isComposeCommand, func(f, v string) {
		switch f {
		case "-p", "--project-name":
			project = v
		case "--project-directory":
			projectDir = v
		case "--env-file":
			envFiles = append(envFiles, v)
		case "-f", "--file":
			files = append(files, v)
		case "--dry-run":
			dryRun = IsTrue(v)
		}
	})
	if res == askedHelp || len(args) == 0 {
		return Call{}
	}
	op := args[0]
	flags := composeFlags(op)
	if flags == nil {
		return Call{}
	}
	// The command's flags include compose's global ones, but run's own -p
	// is --publish, not the project.
	seen := func(f, v string) {
		if f == "--project-name" || (f == "-p" && op != "run") {
			project = v
		}
		switch f {
		case "--project-directory":
			projectDir = v
		case "--env-file":
			envFiles = append(envFiles, v)
		case "--file":
			files = append(files, v)
		case "-f":
			if op != "run" && op != "restart" {
				files = append(files, v) // some commands' own -f is something else
			}
		case "--dry-run":
			dryRun = IsTrue(v)
		case "--no-up":
			noUp = IsTrue(v)
		case "-d", "--detach", "--wait":
			detached = detached || op == "up" && IsTrue(v)
		}
	}
	if op == "run" {
		// Flags after the service are its command's.
		_, res, _ = scanPast(args[1:], flags, nil, seen)
	} else {
		_, res, _ = scanAll(args[1:], flags, seen) // flags may follow services
	}
	if res == askedHelp || dryRun || noUp {
		return Call{} // starts nothing
	}
	c := Call{Kind: "compose", Op: op, Target: project, ComposeFiles: files, ComposeProjectDir: projectDir,
		ComposeEnvFiles: envFiles, ComposeDetached: detached,
		ConfigDir: engine.ConfigDir, Host: engine.Host, HasHost: engine.HasHost, Context: engine.Context,
		HostLast: engine.HostLast}
	return c.named(append(words, op))
}

// isComposeCommand reports whether w is a compose command; it tells an
// unknown global flag's value from the command after it.
func isComposeCommand(w string) bool { return composeCommands[w] }

// isEngineCommand reports whether w is a docker or podman command.
func isEngineCommand(w string) bool { return engineCommands[w] }

// composeFlags is the flag table of a compose command that starts
// containers; nil for the others.
func composeFlags(op string) flagSet {
	switch op {
	case "up":
		return composeUpAll
	case "run":
		return composeRunAll
	case "start":
		return composeStartAll
	case "restart":
		return composeRestartAll
	case "create":
		return composeCreateAll
	case "scale":
		return composeScaleAll
	case "watch":
		return composeWatchAll // builds and starts the services first
	}
	return nil
}

// IsTrue reads a boolean value as the CLIs do (Go's ParseBool).
func IsTrue(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

func parseTart(args []string) Call {
	// tart run starts a VM. clone makes one but runs nothing: it costs
	// disk, not memory or a slot, so it passes like the rest (#29).
	if len(args) == 0 || args[0] != "run" {
		return Call{}
	}
	c := Call{Kind: "tart", Op: "run"}
	// swift-argument-parser takes options before, between and after the
	// positionals.
	pos, res, guessed := scanAll(args[1:], tartRun, nil)
	if res == askedHelp {
		return Call{}
	}
	if !guessed && len(pos) > 0 {
		c.Target = pos[0] // past a guess, it may be an option's value
	}
	return c.named([]string{"tart", "run"})
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
// a value or credentials (= or @), nothing hidden.
func safeWord(w string) bool {
	return w != "" && len(w) <= 128 && !strings.HasPrefix(w, "-") && !strings.ContainsAny(w, "=@") &&
		!strings.ContainsFunc(w, hidden)
}

// hidden reports whether r would not show as itself: spaces, control
// characters, and format characters such as bidi overrides.
func hidden(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

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
					if strings.HasPrefix(a[j+1:], "=") {
						seen(f, a[j+2:]) // -v=false
						break
					}
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

// scanPast is scan reading on past flags missing from the table. Such a flag
// takes a value unless the next word is a flag or, by isCommand, a command:
// a guess, which guessed reports. Gating too much beats letting a call by.
func scanPast(args []string, flags flagSet, isCommand func(string) bool, seen func(flag, value string)) (rest []string, res scanResult, guessed bool) {
	for {
		rest, res = scan(args, flags, seen)
		if res != unknownFlag {
			return rest, res, guessed
		}
		guessed = true
		switch {
		case len(rest) < 2:
			return nil, scanned, true
		case strings.HasPrefix(rest[1], "-") || (isCommand != nil && isCommand(rest[1])):
			args = rest[1:]
		default:
			args = rest[2:]
		}
	}
}

// scanAll reads flags anywhere among the positionals, as commands that
// intersperse them do, and returns the positionals.
func scanAll(args []string, flags flagSet, seen func(flag, value string)) (pos []string, res scanResult, guessed bool) {
	for {
		rest, r, g := scanPast(args, flags, nil, seen)
		guessed = guessed || g
		switch {
		case r == askedHelp:
			return nil, r, guessed
		case r == endOfFlags:
			return append(pos, rest...), r, guessed
		case len(rest) == 0:
			return pos, scanned, guessed
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

// parseBytes reads a -m/--memory value in docker's syntax: a number, an
// optional unit (b, k, m, g, t or p, powers of 1024) and optional "i" and
// "b". 0 if it is missing or invalid.
func parseBytes(s string) uint64 {
	s = strings.TrimSuffix(strings.ToLower(s), "b")
	s = strings.TrimSuffix(s, "i")
	mult := 1.0
	if n := len(s); n > 0 {
		if i := strings.IndexByte("kmgtp", s[n-1]); i >= 0 {
			mult, s = math.Pow(1024, float64(i+1)), strings.TrimSuffix(s[:n-1], " ")
		}
	}
	// Digits with at most one inner dot: no sign, exponent or bare dot.
	if s == "" || s[0] == '.' || s[len(s)-1] == '.' || strings.Count(s, ".") > 1 || strings.Trim(s, "0123456789.") != "" {
		return 0
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n*mult >= 1<<60 {
		return 0
	}
	return uint64(n * mult)
}

// withGlobals is t plus compose's global flags it does not shadow.
func withGlobals(t flagSet) flagSet {
	s := flagSet{}
	for f, v := range composeGlobal {
		s[f] = v
	}
	for f, v := range t {
		s[f] = v
	}
	return s
}

// words builds a set of space-separated words.
func words(list string) map[string]bool {
	s := map[string]bool{}
	for _, w := range strings.Fields(list) {
		s[w] = true
	}
	return s
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
	// Compose takes its global flags after the command too.
	composeUpAll      = withGlobals(composeUp)
	composeRunAll     = withGlobals(composeRun)
	composeStartAll   = withGlobals(composeStart)
	composeRestartAll = withGlobals(composeRestart)
	composeCreateAll  = withGlobals(composeCreate)
	composeScaleAll   = withGlobals(composeScale)
	composeWatchAll   = withGlobals(composeWatch)

	// The commands, from the help text, that tell an unknown flag's value
	// from the command after it.
	engineCommands = words("agent ai artifact attach bake build builder buildx commit compose container context cp " +
		"create debug desktop dhi diff events exec export extension farm generate healthcheck help history image images " +
		"import info init inspect kill kube load login logout logs machine manifest mcp model network offload pass pause " +
		"plugin pod port ps pull push quadlet rename restart rm rmi run save scout search secret start stats stop swarm " +
		"system tag top unpause untag update version volume wait")
	composeCommands = words("attach bridge build commit config cp create down events exec export images kill logs ls " +
		"pause port ps publish pull push restart rm run scale start stats stop top unpause up version volumes wait watch")

	tartRun = flags(
		"--dir --disk --net-bridged --net-softnet-allow --net-softnet-block --net-softnet-control-fd --net-softnet-expose "+
			"--provisioning-opts --root-disk-opts --rosetta --serial-path",
		"--capture-system-keys --help --nested --net-host --net-softnet --no-audio --no-clipboard --no-graphics "+
			"--no-keyboard --no-pointer --no-trackpad --no-usb-accessories --recovery --serial --suspendable --version "+
			"--vnc --vnc-experimental -h")
)
