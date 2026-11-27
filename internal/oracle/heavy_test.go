package oracle

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

const (
	heavyEntries      = 3_000_000
	heavyAdded        = 5
	heavyMissingEvery = 250_000
	heavyChangedEvery = 100_000
	heavyHeapLimit    = 512 << 20
)

func heavyPath(i int) string {
	return fmt.Sprintf("work/%d/%03d/%03d/f-%07d", i/1_000_000, (i/1_000)%1_000, i%1_000, i)
}

func heavyDropped(i int) bool { return i%heavyMissingEvery == 0 }

func heavyChanged(i int) bool { return i%heavyChangedEvery == 0 && !heavyDropped(i) }

func writeHeavyManifest(t *testing.T, path string, observed bool) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	bw := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)

	hdr := manifest.Header{
		Manifest: 1,
		Schema:   manifest.Schema,
		Root:     "work",
		Platform: "linux",
		Hash:     manifest.HashAlgorithm,
	}
	if err := enc.Encode(hdr); err != nil {
		_ = f.Close()

		t.Fatal(err)
	}

	for i := range heavyEntries {
		if observed && heavyDropped(i) {
			continue
		}

		e := manifest.Entry{
			Root:    "work",
			Path:    heavyPath(i),
			Type:    "file",
			Mode:    0o644,
			UID:     1000,
			GID:     1000,
			Size:    4096,
			Blocks:  8,
			NLink:   1,
			Hash:    fmt.Sprintf("%064x", i),
			MtimeNS: int64(i),
		}
		if observed && heavyChanged(i) {
			e.Hash = "changed"
		}

		if err := enc.Encode(e); err != nil {
			_ = f.Close()

			t.Fatal(err)
		}
	}

	if observed {
		for i := range heavyAdded {
			e := manifest.Entry{
				Root:   "work",
				Path:   fmt.Sprintf("work/zz/added-%07d", i),
				Type:   "file",
				Mode:   0o644,
				UID:    1000,
				GID:    1000,
				Size:   1,
				Blocks: 8,
				NLink:  1,
				Hash:   fmt.Sprintf("added-%d", i),
			}
			if err := enc.Encode(e); err != nil {
				_ = f.Close()

				t.Fatal(err)
			}
		}
	}

	if err := bw.Flush(); err != nil {
		_ = f.Close()

		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHeavyDiff(t *testing.T) {
	if os.Getenv("UNLUCKY_HEAVY_DIFF") != "1" {
		t.Skip("set UNLUCKY_HEAVY_DIFF=1")
	}

	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.jsonl")
	obsPath := filepath.Join(dir, "observed.jsonl")

	writeHeavyManifest(t, basePath, false)
	writeHeavyManifest(t, obsPath, true)

	var peak uint64

	stop := make(chan struct{})
	done := make(chan struct{})

	runtime.GC()

	go func() {
		defer close(done)

		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()

		var ms runtime.MemStats

		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)

				if ms.HeapAlloc > peak {
					peak = ms.HeapAlloc
				}
			}
		}
	}()

	start := time.Now()
	res, err := DiffFiles(basePath, obsPath, Policy{})

	close(stop)
	<-done

	if err != nil {
		t.Fatal(err)
	}

	t.Logf("peak heap %d MiB over %s", peak>>20, time.Since(start))

	if peak >= heavyHeapLimit {
		t.Fatalf("peak heap %d MiB exceeds the %d MiB limit", peak>>20, heavyHeapLimit>>20)
	}

	missing, changed := 0, 0

	for i := range heavyEntries {
		switch {
		case heavyDropped(i):
			missing++
		case heavyChanged(i):
			changed++
		}
	}

	if res.Expected != heavyEntries {
		t.Fatalf("expected=%d, want %d", res.Expected, heavyEntries)
	}

	if len(res.Missing) != missing {
		t.Fatalf("missing=%d, want %d", len(res.Missing), missing)
	}

	if len(res.Changed) != changed {
		t.Fatalf("changed=%d, want %d", len(res.Changed), changed)
	}

	if len(res.Added) != heavyAdded {
		t.Fatalf("added=%d, want %d", len(res.Added), heavyAdded)
	}

	if res.Matched != heavyEntries-missing-changed {
		t.Fatalf("matched=%d, want %d", res.Matched, heavyEntries-missing-changed)
	}

	if res.Skipped != 0 || len(res.Meta) != 0 {
		t.Fatalf("unexpected skipped=%d meta=%d", res.Skipped, len(res.Meta))
	}
}
