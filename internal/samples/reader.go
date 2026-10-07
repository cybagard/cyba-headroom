package samples

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// maxLine bounds one sample line; a sample is a few KB.
const maxLine = 4 << 20

// Stats counts what a Read saw.
type Stats struct {
	// Samples passed to the callback.
	Samples int
	// Skipped lines: truncated by a crash, malformed, or of an unknown
	// schema version.
	Skipped int
	// BadFiles could not be read to the end, such as a corrupt archive.
	// Samples read from them before the damage still count.
	BadFiles int
}

type dayFile struct {
	day  string
	gz   bool
	path string
}

// Read calls fn, in file order, for every sample in dir whose time is in
// [from, to). A missing dir holds no samples. Damage is counted in Stats,
// never fatal: a crash can truncate the last line. An error from fn stops the
// read and is returned.
func Read(dir string, from, to time.Time, fn func(Sample) error) (Stats, error) {
	var st Stats
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	var files []dayFile
	for _, e := range entries {
		day, gz, ok := parseName(e.Name())
		if !ok || !mayOverlap(day, from, to) {
			continue
		}
		files = append(files, dayFile{day, gz, filepath.Join(dir, e.Name())})
	}
	// By day; an archive holds a day's earlier lines, so it goes first.
	slices.SortFunc(files, func(a, b dayFile) int {
		if a.day != b.day {
			if a.day < b.day {
				return -1
			}
			return 1
		}
		switch {
		case a.gz == b.gz:
			return 0
		case a.gz:
			return -1
		}
		return 1
	})
	for _, f := range files {
		if err := readFile(f, from, to, fn, &st); err != nil {
			return st, err
		}
	}
	return st, nil
}

// mayOverlap reports whether a file named for day (a local date) can hold
// samples in [from, to). A day is padded on both sides, so this holds in any
// time zone.
func mayOverlap(day string, from, to time.Time) bool {
	start, err := time.Parse(dayLayout, day)
	if err != nil {
		return false
	}
	return start.AddDate(0, 0, -1).Before(to) && start.AddDate(0, 0, 2).After(from)
}

// readFile reads one day file. Only an error from fn is returned; damage is
// counted.
func readFile(f dayFile, from, to time.Time, fn func(Sample) error, st *Stats) error {
	file, err := os.Open(f.path)
	if err != nil {
		st.BadFiles++
		return nil
	}
	defer func() { _ = file.Close() }()
	var r io.Reader = file
	if f.gz {
		zr, err := gzip.NewReader(file)
		if err != nil {
			st.BadFiles++
			return nil
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil || s.V != Version {
			st.Skipped++
			continue
		}
		if s.T.Before(from) || !s.T.Before(to) {
			continue
		}
		st.Samples++
		if err := fn(s); err != nil {
			return err
		}
	}
	if sc.Err() != nil {
		st.BadFiles++
	}
	return nil
}
