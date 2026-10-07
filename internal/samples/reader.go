package samples

import (
	"bufio"
	"bytes"
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
	// Duplicates were samples seen before for the same day and time: left by
	// a crash mid-gzip, or seen in both files while the daemon gzips a day.
	Duplicates int
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
	r := &dayReader{from: from, to: to, fn: fn, st: &st, br: bufio.NewReaderSize(nil, maxLine)}
	for _, f := range files {
		if f.day != r.day {
			r.day, r.seen = f.day, map[int64]bool{}
		}
		if err := r.read(f); err != nil {
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

// dayReader reads day files in order, dropping repeated samples within a day.
type dayReader struct {
	from, to time.Time
	fn       func(Sample) error
	st       *Stats
	br       *bufio.Reader
	day      string
	seen     map[int64]bool
}

// read reads one day file. Only an error from fn is returned; damage is
// counted.
func (r *dayReader) read(f dayFile) error {
	file, err := os.Open(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // gzipped away since the listing
	}
	if err != nil {
		r.st.BadFiles++
		return nil
	}
	defer func() { _ = file.Close() }()
	var src io.Reader = file
	if f.gz {
		zr, err := gzip.NewReader(file)
		if err != nil {
			r.st.BadFiles++
			return nil
		}
		defer func() { _ = zr.Close() }()
		src = zr
	}
	r.br.Reset(src)
	for {
		line, tooLong, err := readLine(r.br)
		if tooLong {
			r.st.Skipped++
		} else if len(bytes.TrimSpace(line)) > 0 {
			if ferr := r.sample(line); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			r.st.BadFiles++
			return nil
		}
	}
}

// sample decodes one line and hands it to fn if it is new and in range.
func (r *dayReader) sample(line []byte) error {
	var s Sample
	if err := json.Unmarshal(line, &s); err != nil || s.V != Version {
		r.st.Skipped++
		return nil
	}
	if s.T.Before(r.from) || !s.T.Before(r.to) {
		return nil
	}
	key := s.T.UnixNano()
	if r.seen[key] {
		r.st.Duplicates++
		return nil
	}
	r.seen[key] = true
	r.st.Samples++
	return r.fn(s)
}

// readLine returns the next line. A line longer than the reader's buffer is
// consumed and reported as tooLong, so one bad line costs only itself.
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	line, err = br.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return line, false, err
	}
	for errors.Is(err, bufio.ErrBufferFull) {
		_, err = br.ReadSlice('\n')
	}
	return nil, true, err
}
