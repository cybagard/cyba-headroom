package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/shellblock"
)

// zshFiles are the files zsh reads that shell-setup writes: .zlogin for
// login shells, .zshrc for interactive ones, each last, after the PATH
// changes of the files before it (#370).
var zshFiles = []string{".zlogin", ".zshrc"}

// bashFiles are the login files bash reads, the first that exists only.
var bashFiles = []string{".bash_profile", ".bash_login", ".profile"}

// shellFrom is the shell shell-setup writes for, from $SHELL; for another
// or none (fish, nu), zsh, macOS's default, with a note saying so.
func shellFrom(sh string) (shell, note string) {
	switch filepath.Base(sh) {
	case "zsh", "bash":
		return filepath.Base(sh), ""
	}
	if sh == "" {
		return "zsh", "$SHELL is not set: writing for zsh, macOS's default"
	}
	return "zsh", "$SHELL is " + sh + ": writing for zsh, macOS's default; fish, nu and csh are not supported"
}

// shellCandidates are every file shell's block may be in.
func shellCandidates(shell string, getenv func(string) string) []string {
	var out []string
	if shell == "bash" {
		for _, f := range bashFiles {
			out = append(out, filepath.Join(getenv("HOME"), f))
		}
		return out
	}
	dir := getenv("ZDOTDIR")
	if dir == "" {
		dir = getenv("HOME")
	}
	for _, f := range zshFiles {
		out = append(out, filepath.Join(dir, f))
	}
	return out
}

// shellTargets are the files shell-setup --apply writes for shell: zsh's
// two, or the first of bash's login files that exists, else a new
// .bash_profile.
func shellTargets(shell string, getenv func(string) string, stat func(string) (fs.FileInfo, error)) []string {
	all := shellCandidates(shell, getenv)
	if shell != "bash" {
		return all
	}
	for _, p := range all {
		if _, err := stat(p); err == nil {
			return []string{p}
		}
	}
	return all[:1]
}

// maxDotfile is the largest startup file shell-setup reads.
const maxDotfile = 16 << 20

// dotfile is one of a shell's startup files, as shell-setup found it.
type dotfile struct {
	path    string
	exists  bool
	mode    fs.FileMode
	content string
	// read is true if content is the file's: it is missing, or was read.
	read bool
	// refused says why shell-setup may not write the file; "" if it may.
	refused string
}

// readDotfile reads p without following a link. shell-setup writes only a
// regular file the euid owns: renaming over a link would replace it, and
// another user's file would become ours (#370). A refused file's content
// is still read, through a link, to say whether it holds a block, but only
// if it is a regular file: a FIFO or a device would never end.
func readDotfile(p string, lstat func(string) (fs.FileInfo, error)) dotfile {
	d := dotfile{path: p, mode: 0o644}
	fi, err := lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.read = true
		return d
	case err != nil:
		d.exists, d.refused = true, oneLine(err)
		return d
	}
	d.exists, d.mode = true, fi.Mode().Perm()
	link := fi.Mode()&fs.ModeSymlink != 0
	st, _ := fi.Sys().(*syscall.Stat_t)
	switch {
	case link:
		d.refused = "a symlink"
	case !fi.Mode().IsRegular():
		d.refused = "not a regular file"
		return d
	case st == nil:
		d.refused = "its owner is unknown"
	case int(st.Uid) != os.Geteuid():
		d.refused = fmt.Sprintf("owned by uid %d, not you", st.Uid)
	}
	content, err := readRegular(p, !link)
	switch {
	case link && errors.Is(err, fs.ErrNotExist): // a dangling link: no block
		d.read = true
	case err != nil && d.refused == "":
		d.refused = oneLine(err)
	case err != nil && link:
		d.refused += " (" + oneLine(err) + ")"
	case err == nil:
		d.content, d.read = content, true
	}
	return d
}

// readRegular reads p if it is a regular file of at most maxDotfile bytes.
// It opens p without blocking, so a FIFO swapped in after the Lstat cannot
// hang it, and without following a link if nofollow.
func readRegular(p string, nofollow bool) (string, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if nofollow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(p, flags, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return "", err
	case !fi.Mode().IsRegular():
		return "", errors.New("not a regular file")
	case fi.Size() > maxDotfile:
		return "", fmt.Errorf("larger than %d MiB", maxDotfile>>20)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDotfile+1))
	if err == nil && len(b) > maxDotfile {
		err = fmt.Errorf("larger than %d MiB", maxDotfile>>20)
	}
	return string(b), err
}

// apply writes block into d, and says what it did.
func (d dotfile) apply(block string) (string, error) {
	if d.refused != "" {
		return "", errors.New(d.refused)
	}
	if !d.exists {
		return "created", writeFile(d.path, []byte(block), d.mode)
	}
	_, had, err := shellblock.Remove(d.content)
	if err != nil {
		return "", err
	}
	out, changed := shellblock.Apply(d.content, block)
	switch {
	case !changed:
		return "unchanged", nil
	case had:
		return "replaced", writeFile(d.path, []byte(out), d.mode)
	}
	return "added", writeFile(d.path, []byte(out), d.mode)
}

// remove deletes the block from d, and the file too if shell-setup created
// it and nothing else was added since. It says what it did: "" if d has no
// block.
func (d dotfile) remove() (string, error) {
	if !d.read {
		return "", errors.New(d.refused)
	}
	out, changed, err := shellblock.Remove(d.content)
	switch {
	case err != nil:
		return "", err
	case !changed:
		return "", nil
	case d.refused != "":
		return "", errors.New(d.refused)
	case out == "" && shellblock.IsBlock(d.content):
		return "deleted", os.Remove(d.path)
	}
	return "removed", writeFile(d.path, []byte(out), d.mode)
}

// state is what --apply would do to d, for the dry run.
func (d dotfile) state(block string) string {
	if d.refused != "" {
		return "refused: " + d.refused
	}
	if !d.exists {
		return "missing: --apply creates it with the block"
	}
	_, had, err := shellblock.Remove(d.content)
	switch {
	case err != nil:
		return "refused: " + err.Error()
	case !had:
		return "no block: --apply adds it at the end"
	}
	if _, changed := shellblock.Apply(d.content, block); changed {
		return "an old block: --apply replaces it"
	}
	return "has the block"
}

// removeShellBlocks removes the block from every file any shell's may be
// in, for uninstall: the files it changed, and those it could not, with
// the reason.
func removeShellBlocks(getenv func(string) string, lstat func(string) (fs.FileInfo, error)) (removed, kept []string) {
	for _, shell := range []string{"zsh", "bash"} {
		for _, p := range shellCandidates(shell, getenv) {
			d := readDotfile(p, lstat)
			did, err := d.remove()
			switch {
			case err != nil:
				kept = append(kept, fmt.Sprintf("%s (%s; remove the headroom shell-setup block by hand)", p, err))
			case did != "":
				removed = append(removed, p)
			}
		}
	}
	return removed, kept
}

// runShellSetup shows, adds or removes the block that puts the shim dir
// first on PATH in the shell's startup files (#370). It never prompts.
func runShellSetup(e Env) int {
	usage := func() int {
		fmt.Fprintln(e.Stderr, "headroom: usage: headroom shell-setup [--apply | --remove] [--shell zsh|bash]")
		return 2
	}
	mode, shell := "", ""
	for a := e.Args[2:]; len(a) > 0; a = a[1:] {
		switch {
		case (a[0] == "--apply" || a[0] == "--remove") && mode == "":
			mode = a[0]
		case a[0] == "--shell" && len(a) > 1 && (a[1] == "zsh" || a[1] == "bash"):
			shell, a = a[1], a[1:]
		default:
			return usage()
		}
	}
	if e.Getenv("HOME") == "" {
		fmt.Fprintln(e.Stderr, "headroom: HOME is not set")
		return 1
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	lstat := e.lstat
	if lstat == nil {
		lstat = os.Lstat
	}
	if shell == "" {
		var note string
		if shell, note = shellFrom(e.Getenv("SHELL")); note != "" {
			fmt.Fprintln(e.Stderr, "headroom:", note)
		}
	}
	block, line := shellblock.Block(shell, cfg.ShimDir), shellblock.Line(shell, cfg.ShimDir)
	code := 0
	refuse := func(p, why string) {
		fmt.Fprintf(e.Stderr, "headroom: %s: %s: not changed; add this line at its end by hand:\n  %s", p, why, line)
		code = 1
	}
	if mode == "--remove" {
		for _, p := range shellCandidates(shell, e.Getenv) {
			d := readDotfile(p, lstat)
			switch did, err := d.remove(); {
			case err != nil:
				fmt.Fprintf(e.Stderr, "headroom: %s: %s: not changed; remove the headroom shell-setup block by hand\n", p, err)
				code = 1
			case did == "deleted":
				fmt.Fprintf(e.Stdout, "removed:   %s (deleted: it held only the block)\n", p)
			case did != "":
				fmt.Fprintf(e.Stdout, "removed:   %s\n", p)
			}
		}
		return code
	}
	targets := shellTargets(shell, e.Getenv, os.Stat)
	if mode == "" {
		fmt.Fprintf(e.Stdout, "The block that puts %s first on PATH in %s's login and interactive shells:\n\n%s\n", cfg.ShimDir, shell, block)
		for _, p := range targets {
			fmt.Fprintf(e.Stdout, "  %s: %s\n", p, readDotfile(p, lstat).state(block))
		}
		other := map[string]string{"zsh": "bash", "bash": "zsh"}[shell]
		fmt.Fprintf(e.Stdout, "\nRun `headroom shell-setup --apply` to write it, `--shell %s` for %s's files.\n", other, other)
		return 0
	}
	for _, p := range targets {
		did, err := readDotfile(p, lstat).apply(block)
		if err != nil {
			refuse(p, err.Error())
			continue
		}
		fmt.Fprintf(e.Stdout, "%-10s %s\n", did+":", p)
	}
	return code
}
