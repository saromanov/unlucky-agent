package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCaptureEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	stats, err := Capture([]Root{{Name: "work", Dir: dir}}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entries != 3 {
		t.Fatalf("expected 3 entries, got %d", stats.Entries)
	}

	entries := decode(t, buf.Bytes())
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if e, ok := byPath["work/run.sh"]; !ok || e.Mode != 0o755 {
		t.Fatalf("exec bit was not preserved: %+v", e)
	}
	if e, ok := byPath["work/link"]; !ok || e.Type != "symlink" || e.Link != "a.txt" {
		t.Fatalf("symlink captured incorrectly: %+v", e)
	}
	if e := byPath["work/a.txt"]; e.Size != 5 || e.Hash == "" {
		t.Fatalf("file captured incorrectly: %+v", e)
	}
}

func TestCaptureSorted(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"c.txt", "a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if _, err := Capture([]Root{{Name: "work", Dir: dir}}, &buf); err != nil {
		t.Fatal(err)
	}
	entries := decode(t, buf.Bytes())
	prev := ""
	for _, e := range entries {
		if strings.Compare(prev, e.Path) > 0 {
			t.Fatalf("order violated: %q after %q", e.Path, prev)
		}
		prev = e.Path
	}
}

func TestCaptureMissingRoot(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Capture([]Root{{Name: "work", Dir: "/nonexistent-unlucky"}}, &buf); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}

func decode(t *testing.T, data []byte) []Entry {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	var hdr Header
	if err := dec.Decode(&hdr); err != nil {
		t.Fatal(err)
	}
	var out []Entry
	for dec.More() {
		var e Entry
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestCaptureSpecialTypes(t *testing.T) {
	dir := t.TempDir()

	sparse := filepath.Join(dir, "sparse.bin")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 4<<10)
	for i := range block {
		block[i] = 0x33
	}
	for _, off := range []int64{0, 512 << 10} {
		if _, err := f.WriteAt(block, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(dir, "sock.unix")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sock}); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	stats, err := Capture([]Root{{Name: "work", Dir: dir}}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entries != 3 {
		t.Fatalf("expected 3 entries, got %d", stats.Entries)
	}

	byPath := map[string]Entry{}
	for _, e := range decode(t, buf.Bytes()) {
		byPath[e.Path] = e
	}
	if e := byPath["work/fifo.pipe"]; e.Type != "fifo" {
		t.Fatalf("fifo captured incorrectly: %+v", e)
	}
	if e := byPath["work/sock.unix"]; e.Type != "socket" {
		t.Fatalf("socket captured incorrectly: %+v", e)
	}
	sp := byPath["work/sparse.bin"]
	if sp.Type != "file" || sp.Size != 1<<20 || sp.Hash == "" {
		t.Fatalf("sparse file captured incorrectly: %+v", sp)
	}
	if sp.Blocks*512 >= 1<<20/10 {
		t.Fatalf("sparse file must keep its holes: %d blocks", sp.Blocks)
	}
}
