package manifest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// SortThreshold is the entry count above which sorting switches from an
// in-memory sort to an external merge sort, as planned in
// docs/ARCHITECTURE.md section 5.
const SortThreshold = 1_000_000

// SortChunkEntries bounds one in-memory chunk of the external merge sort.
// Tests lower it to exercise the spill path on small inputs.
var SortChunkEntries = SortThreshold

// SortStream reads a manifest stream and writes the same header followed by
// the entries in path order. Entries are buffered up to SortChunkEntries and
// spilled to temporary chunk files when the input is larger.
func SortStream(r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	var hdr Header
	err := dec.Decode(&hdr)
	if err != nil {
		return fmt.Errorf("manifest: header: %w", err)
	}
	s := newEntrySorter(SortChunkEntries)
	defer s.cleanup()

	for {
		var e Entry
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
		if err := s.add(e); err != nil {
			return err
		}
	}
	return s.write(w, hdr)
}

// entrySorter accumulates entries and spills sorted chunks to a temporary
// directory once the in-memory buffer is full.
type entrySorter struct {
	limit   int
	dir     string
	entries []Entry
	chunks  []string
}

func newEntrySorter(limit int) *entrySorter {
	if limit <= 0 {
		limit = SortThreshold
	}
	return &entrySorter{limit: limit}
}

func (s *entrySorter) add(e Entry) error {
	s.entries = append(s.entries, e)
	if len(s.entries) < s.limit {
		return nil
	}
	return s.spill()
}

func (s *entrySorter) spill() error {
	if len(s.entries) == 0 {
		return nil
	}
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "unlucky-sort-*")
		if err != nil {
			return err
		}
		s.dir = dir
	}
	sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].Path < s.entries[j].Path })

	name := filepath.Join(s.dir, fmt.Sprintf("chunk-%06d.jsonl", len(s.chunks)))
	if err := writeEntries(name, s.entries); err != nil {
		return err
	}
	s.chunks = append(s.chunks, name)
	s.entries = s.entries[:0]
	return nil
}

// write emits the header and all buffered entries, merging chunk files when
// the input did not fit into memory.
func (s *entrySorter) write(w io.Writer, hdr Header) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(hdr); err != nil {
		return err
	}
	if len(s.chunks) == 0 {
		sort.Slice(s.entries, func(i, j int) bool { return s.entries[i].Path < s.entries[j].Path })
		for _, e := range s.entries {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return bw.Flush()
	}
	if err := s.spill(); err != nil {
		return err
	}
	if err := s.merge(enc); err != nil {
		return err
	}
	return bw.Flush()
}

func (s *entrySorter) cleanup() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	s.dir = ""
	s.chunks = nil
	s.entries = nil
}

// merge streams the chunk files into enc, picking the smallest path head. The
// chunk count is the entry count divided by the limit, so a linear scan over
// the heads stays cheap.
func (s *entrySorter) merge(enc *json.Encoder) error {
	readers := make([]*chunkReader, 0, len(s.chunks))
	defer func() {
		for _, r := range readers {
			_ = r.close()
		}
	}()
	for _, name := range s.chunks {
		r, err := openChunkReader(name)
		if err != nil {
			return err
		}
		readers = append(readers, r)
	}
	for {
		best := -1
		for i, r := range readers {
			if !r.ok {
				continue
			}
			if best < 0 || r.cur.Path < readers[best].cur.Path {
				best = i
			}
		}
		if best < 0 {
			return nil
		}
		if err := enc.Encode(readers[best].cur); err != nil {
			return err
		}
		if err := readers[best].next(); err != nil {
			return err
		}
	}
}

type chunkReader struct {
	f   *os.File
	dec *json.Decoder
	cur Entry
	ok  bool
}

func openChunkReader(name string) (*chunkReader, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	r := &chunkReader{f: f, dec: json.NewDecoder(bufio.NewReaderSize(f, 1<<20))}
	if err := r.next(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func (r *chunkReader) next() error {
	var e Entry
	err := r.dec.Decode(&e)
	if errors.Is(err, io.EOF) {
		r.ok = false
		return nil
	}
	if err != nil {
		r.ok = false
		return err
	}
	r.cur = e
	r.ok = true
	return nil
}

func (r *chunkReader) close() error { return r.f.Close() }

func writeEntries(name string, entries []Entry) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
