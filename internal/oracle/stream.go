package oracle

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

// errUnsorted marks a manifest whose entries are not in path order.
var errUnsorted = errors.New("manifest entries are not sorted by path")

// unsortedError names the manifest that broke path order.
type unsortedError struct {
	path string
}

func (e *unsortedError) Error() string { return e.path + ": " + errUnsorted.Error() }

func (e *unsortedError) Unwrap() error { return errUnsorted }

// DiffFiles compares the expected manifest with the observed one as a
// merge-join over two sorted streams. An unsorted manifest is sorted
// externally and compared again, so the result does not depend on input order.
func DiffFiles(expectedPath, observedPath string, pol Policy) (*Result, error) {
	basePath, obsPath := expectedPath, observedPath
	for range 3 {
		res, err := mergeFiles(basePath, obsPath, pol)
		if err == nil {
			return res, nil
		}

		var unsorted *unsortedError
		if !errors.As(err, &unsorted) {
			return nil, err
		}

		sorted, cleanup, sortErr := sortedTemp(unsorted.path)
		if sortErr != nil {
			return nil, fmt.Errorf("sort %s: %w", unsorted.path, sortErr)
		}

		defer cleanup()

		if unsorted.path == basePath {
			basePath = sorted
		} else {
			obsPath = sorted
		}
	}

	return nil, errors.New("unsorted manifest after external sort")
}

func mergeFiles(expectedPath, observedPath string, pol Policy) (*Result, error) {
	base, err := openEntryStream(expectedPath)
	if err != nil {
		return nil, fmt.Errorf("expected manifest: %w", err)
	}

	defer func() { _ = base.close() }()

	obs, err := openEntryStream(observedPath)
	if err != nil {
		return nil, fmt.Errorf("observed manifest: %w", err)
	}

	defer func() { _ = obs.close() }()

	res := &Result{}

	for base.ok || obs.ok {
		if obs.ok && (!base.ok || obs.cur.Path < base.cur.Path) {
			if addedPath(obs.cur, pol) {
				change := Change{Path: obs.cur.Path, Kind: KindAdded, Detail: obs.cur.Type}
				res.Added = append(res.Added, change)
			}

			if err := obs.next(); err != nil {
				return nil, fmt.Errorf("observed manifest: %w", err)
			}

			continue
		}

		b := base.cur

		var o manifest.Entry

		matched := false

		for obs.ok && obs.cur.Path == b.Path {
			o = obs.cur
			matched = true

			if err := obs.next(); err != nil {
				return nil, fmt.Errorf("observed manifest: %w", err)
			}
		}

		if err := base.next(); err != nil {
			return nil, fmt.Errorf("expected manifest: %w", err)
		}

		classify(res, b, o, matched, pol)
	}

	return res, nil
}

func addedPath(e manifest.Entry, pol Policy) bool {
	if pol.Excluded(e.Path) {
		return false
	}

	return !pol.AddedAllowed(e.Path)
}

// sortedTemp externally sorts a manifest into a temporary file.
func sortedTemp(path string) (string, func(), error) {
	in, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}

	defer func() { _ = in.Close() }()

	out, err := os.CreateTemp("", "unlucky-diff-*.jsonl")
	if err != nil {
		return "", nil, err
	}

	cleanup := func() { _ = os.Remove(out.Name()) }

	if err := manifest.SortStream(in, out); err != nil {
		_ = out.Close()

		cleanup()

		return "", nil, err
	}

	if err := out.Close(); err != nil {
		cleanup()

		return "", nil, err
	}

	return out.Name(), cleanup, nil
}

// entryStream decodes a manifest file entry by entry and rejects out-of-order
// paths as soon as they appear.
type entryStream struct {
	f    *os.File
	dec  *json.Decoder
	path string
	last string
	seen bool
	cur  manifest.Entry
	ok   bool
}

func openEntryStream(path string) (*entryStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))

	var hdr manifest.Header

	if err := dec.Decode(&hdr); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("%s: header: %w", path, err)
	}

	s := &entryStream{f: f, dec: dec, path: path}
	if err := s.next(); err != nil {
		_ = f.Close()

		return nil, err
	}

	return s, nil
}

func (s *entryStream) next() error {
	var e manifest.Entry

	err := s.dec.Decode(&e)
	if errors.Is(err, io.EOF) {
		s.ok = false

		return nil
	}

	if err != nil {
		s.ok = false

		return fmt.Errorf("%s: %w", s.path, err)
	}

	if s.seen && e.Path < s.last {
		s.ok = false

		return &unsortedError{path: s.path}
	}

	s.seen = true
	s.last = e.Path
	s.cur = e
	s.ok = true

	return nil
}

func (s *entryStream) close() error { return s.f.Close() }
