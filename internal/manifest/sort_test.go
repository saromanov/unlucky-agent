package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func withChunkEntries(t *testing.T, limit int) {
	t.Helper()

	saved := SortChunkEntries
	SortChunkEntries = limit

	t.Cleanup(func() { SortChunkEntries = saved })
}

func sortTestEntries(n int) []Entry {
	out := make([]Entry, 0, n)
	for i := range n {
		out = append(out, Entry{
			Root:  "work",
			Path:  fmt.Sprintf("work/dir/file-%02d", i),
			Type:  "file",
			Mode:  0o644,
			Size:  int64(i),
			Hash:  fmt.Sprintf("hash-%02d", i),
			NLink: 1,
		})
	}

	if len(out)%2 == 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}

	return out
}

func TestSortStreamChunked(t *testing.T) {
	withChunkEntries(t, 3)

	entries := sortTestEntries(10)

	var in bytes.Buffer

	enc := json.NewEncoder(&in)

	hdr := Header{Manifest: 1, Schema: Schema, Root: "work", Platform: "linux", Hash: HashAlgorithm}

	if err := enc.Encode(hdr); err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := SortStream(&in, &out); err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(&out)

	var gotHdr Header

	if err := dec.Decode(&gotHdr); err != nil {
		t.Fatal(err)
	}

	if gotHdr != hdr {
		t.Fatalf("header changed: %+v", gotHdr)
	}

	var got []Entry

	for dec.More() {
		var e Entry
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}

		got = append(got, e)
	}

	if len(got) != len(entries) {
		t.Fatalf("expected %d entries, got %d", len(entries), len(got))
	}

	for i := 1; i < len(got); i++ {
		if got[i-1].Path > got[i].Path {
			t.Fatalf("order violated: %q after %q", got[i].Path, got[i-1].Path)
		}
	}
}

func TestSortStreamInMemory(t *testing.T) {
	entries := sortTestEntries(10)

	var in bytes.Buffer

	enc := json.NewEncoder(&in)
	if err := enc.Encode(Header{Manifest: 1, Schema: Schema, Root: "work"}); err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := SortStream(&in, &out); err != nil {
		t.Fatal(err)
	}

	got := decode(t, out.Bytes())
	if len(got) != len(entries) {
		t.Fatalf("expected %d entries, got %d", len(entries), len(got))
	}

	for i := 1; i < len(got); i++ {
		if got[i-1].Path > got[i].Path {
			t.Fatalf("order violated: %q after %q", got[i].Path, got[i-1].Path)
		}
	}
}

func TestCaptureSpillMatchesMemory(t *testing.T) {
	dir := t.TempDir()
	for i := range 12 {
		name := filepath.Join(dir, fmt.Sprintf("f-%02d.txt", i))
		if err := os.WriteFile(name, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var want bytes.Buffer

	wantStats, err := Capture([]Root{{Name: "work", Dir: dir}}, &want)
	if err != nil {
		t.Fatal(err)
	}

	withChunkEntries(t, 2)

	var got bytes.Buffer

	gotStats, err := Capture([]Root{{Name: "work", Dir: dir}}, &got)
	if err != nil {
		t.Fatal(err)
	}

	if gotStats != wantStats {
		t.Fatalf("stats changed: %+v != %+v", gotStats, wantStats)
	}

	if got.String() != want.String() {
		t.Fatalf("external sort changed the manifest output:\n%s\n---\n%s",
			got.String(), want.String())
	}
}
