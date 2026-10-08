package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/samples"
	"github.com/cybagard/cyba-headroom/internal/suggest"
)

// runSuggest prints what the recorded samples suggest for [budget] and
// [policy], and with --write merges it into config.toml (#23).
func runSuggest(e Env) int {
	since, write := 14*24*time.Hour, false
	for a := e.Args[2:]; len(a) > 0; a = a[1:] {
		switch a[0] {
		case "--write":
			write = true
		case "--since":
			if len(a) < 2 {
				return suggestUsage(e)
			}
			d, err := parseSince(a[1])
			if err != nil {
				fmt.Fprintf(e.Stderr, "headroom: suggest: --since %q: %v\n", a[1], err)
				return 2
			}
			since, a = d, a[1:]
		default:
			return suggestUsage(e)
		}
	}
	cfg, err := config.Load(e.Getenv)
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	to := time.Now()
	from := to.Add(-since)
	agg := suggest.New(suggest.Options{Interval: cfg.Daemon.Interval.Duration, Loc: time.Local, Current: cfg.Budget.Params()})
	st, err := samples.Read(cfg.SamplesDir(), from, to, func(s samples.Sample) error {
		agg.Add(s)
		return nil
	})
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	if st.Samples == 0 {
		fmt.Fprintf(e.Stdout, "headroom suggest: no samples in %s since %s. Is the daemon recording? (`headroom install`, `headroom logs`)\n",
			cfg.SamplesDir(), from.Format("2006-01-02 15:04"))
		return 0
	}
	r := agg.Result()
	printSuggestion(e.Stdout, r, st, since)
	if !write {
		return 0
	}
	return writeSuggestion(e, cfg, r)
}

func suggestUsage(e Env) int {
	fmt.Fprintln(e.Stderr, "headroom: usage: headroom suggest [--since 14d] [--write]")
	return 2
}

// parseSince reads a positive duration: Nd for days, or a Go duration.
func parseSince(s string) (time.Duration, error) {
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, errors.New("want e.g. 14d or 72h")
		}
		if days > 10000 {
			return 0, errors.New("at most 10000d")
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, errors.New("want e.g. 14d or 72h")
		}
	}
	if d <= 0 {
		return 0, errors.New("must be positive")
	}
	return d, nil
}

func printSuggestion(w io.Writer, r suggest.Result, st samples.Stats, since time.Duration) {
	fmt.Fprintf(w, "headroom suggest: the last %s of samples\n\n", humanDays(since))
	fmt.Fprintf(w, "%-10s  %6s  %8s  %11s  %8s  %8s  %9s\n", "DAY", "HOURS", "WORKING", "PEAK AGENTS", "WARN MIN", "CRIT MIN", "PEAK SWAP")
	for _, d := range r.Days {
		fmt.Fprintf(w, "%-10s  %6.1f  %8.1f  %11d  %8.1f  %8.1f  %6.1f GB\n",
			d.Date, d.Hours, d.WorkingHours, d.PeakWorking, d.WarnMinutes, d.CritMinutes, float64(d.PeakSwap)/(1<<30))
	}
	fmt.Fprintf(w, "\nread %d samples (%d broken lines skipped, %d duplicates, %d unreadable files); left out of the budget maths: %d with stale inputs, %d during swap-outs\n",
		st.Samples, st.Skipped, st.Duplicates, st.BadFiles, r.Stale, r.SwappingOut)

	fmt.Fprintln(w, "\n# Suggested settings. GB means GiB. Each value says how it was found.")
	section := ""
	for _, v := range r.Values {
		if v.Section != section {
			section = v.Section
			fmt.Fprintf(w, "\n[%s]\n", section)
		}
		if v.OK {
			fmt.Fprintf(w, "%s = %s  # %s\n", v.Key, gbLiteral(v.GB), v.Rule)
		} else {
			fmt.Fprintf(w, "# %s: %s\n", v.Key, v.Why)
		}
	}
	if len(r.Advice) > 0 {
		fmt.Fprintln(w, "\n# Advice (headroom changes nothing outside its own config)")
		for _, a := range r.Advice {
			fmt.Fprintf(w, "- %s\n", a)
		}
	}
}

func humanDays(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return d.String()
}

func gbLiteral(gb float64) string { return strconv.FormatFloat(gb, 'f', -1, 64) }

// writeSuggestion merges the values with enough evidence into config.toml.
// The merged file must load before it replaces the old one, which is kept as
// config.toml.bak.
func writeSuggestion(e Env, cfg config.Config, r suggest.Result) int {
	var set []config.Setting
	for _, v := range r.Values {
		if v.OK {
			set = append(set, config.Setting{Section: v.Section, Key: v.Key, Value: gbLiteral(v.GB)})
		}
	}
	if len(set) == 0 {
		fmt.Fprintln(e.Stdout, "\nnothing written: no value has enough evidence yet")
		return 0
	}
	path := filepath.Join(cfg.Dir, config.FileName)
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	merged, err := config.Merge(old, set)
	if err == nil {
		err = checkConfig(merged)
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, "headroom: not writing the config:", err)
		return 1
	}
	// A new backup each time: a later --write must not overwrite the copy of
	// the hand-written config an earlier one kept.
	bak := ""
	if old != nil {
		bak = backupPath(path)
		if err := writeFile(bak, old, 0o600); err != nil {
			fmt.Fprintln(e.Stderr, "headroom:", err)
			return 1
		}
	}
	if err := writeFile(path, merged, 0o600); err != nil {
		fmt.Fprintln(e.Stderr, "headroom:", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "\nwrote %d setting(s) to %s", len(set), path)
	if bak != "" {
		fmt.Fprintf(e.Stdout, " (the old file is %s)", bak)
	}
	fmt.Fprintln(e.Stdout, "; restart the daemon to use them: headroom install")
	return 0
}

// backupPath is a backup name for path that no file has yet:
// config.toml.bak-20261008-061500, with -2, -3, ... if taken.
func backupPath(path string) string {
	base := path + ".bak-" + time.Now().Format("20060102-150405")
	p := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
}

// checkConfig loads data as a config file, in a scratch directory.
func checkConfig(data []byte) error {
	dir, err := os.MkdirTemp("", "headroom-config")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), data, 0o600); err != nil {
		return err
	}
	_, err = config.LoadDir(dir)
	return err
}
