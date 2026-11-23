package oracle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

func captureTo(t *testing.T, roots []manifest.Root) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Capture(roots, f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiffRestoredTree(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "a.txt"), "a", 0o644)
	write(t, filepath.Join(base, "sub", "b.txt"), "b", 0o644)

	before := captureTo(t, []manifest.Root{{Name: "work", Dir: base}})
	res, err := DiffFiles(before, before, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pass() {
		t.Fatalf("identical trees must pass: %+v", res)
	}
	if res.Expected != 3 || res.Matched != 3 {
		t.Fatalf("expected 3 entries (dir, dir, 2 files), got expected=%d matched=%d", res.Expected, res.Matched)
	}
}

func TestDiffDetectsMissingAndChanged(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "a.txt"), "a", 0o644)
	write(t, filepath.Join(base, "b.txt"), "b", 0o644)
	before := captureTo(t, []manifest.Root{{Name: "work", Dir: base}})

	if err := os.Remove(filepath.Join(base, "a.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "b.txt"), "changed", 0o644)
	after := captureTo(t, []manifest.Root{{Name: "work", Dir: base}})

	res, err := DiffFiles(before, after, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pass() {
		t.Fatal("expected FAIL")
	}
	if len(res.Missing) != 1 || res.Missing[0].Path != "work/a.txt" {
		t.Fatalf("missing=%+v", res.Missing)
	}
	if len(res.Changed) != 1 || res.Changed[0].Path != "work/b.txt" {
		t.Fatalf("changed=%+v", res.Changed)
	}
	if res.Recall() >= 1 {
		t.Fatalf("recall must be below 1, got %v", res.Recall())
	}
}

func TestDiffMetaAndExclude(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, ".git", "index"), "x", 0o644)
	write(t, filepath.Join(base, "run.sh"), "#!/bin/sh", 0o755)
	before := captureTo(t, []manifest.Root{{Name: "work", Dir: base}})

	if err := os.Chmod(filepath.Join(base, "run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, ".git", "index"), "y", 0o644)
	after := captureTo(t, []manifest.Root{{Name: "work", Dir: base}})

	res, err := DiffFiles(before, after, Policy{Exclude: []string{"work/.git/**"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Meta) != 1 || res.Meta[0].Path != "work/run.sh" {
		t.Fatalf("expected a permission meta defect, got %+v", res.Meta)
	}
	if len(res.Changed) != 0 {
		t.Fatalf("the excluded .git must not appear in changed: %+v", res.Changed)
	}
}

func TestDiffAdded(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "a.txt"), "a", 0o644)
	before := captureTo(t, []manifest.Root{{Name: "work", Dir: base}})

	write(t, filepath.Join(base, "new.txt"), "n", 0o644)
	after := captureTo(t, []manifest.Root{{Name: "work", Dir: base}})

	res, err := DiffFiles(before, after, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0].Path != "work/new.txt" {
		t.Fatalf("added=%+v", res.Added)
	}
	if !res.Pass() {
		t.Fatal("an added file must not fail the gates")
	}
}

func TestClobber(t *testing.T) {
	post := t.TempDir()
	write(t, filepath.Join(post, "human.txt"), "h", 0o644)
	postManifest := captureTo(t, []manifest.Root{{Name: "work", Dir: post}})

	restored := t.TempDir()
	restoredManifest := captureTo(t, []manifest.Root{{Name: "work", Dir: restored}})

	got, err := Clobber(postManifest, restoredManifest, Policy{Preserve: []string{"work/human.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one clobbered path, got %+v", got)
	}
}

func TestDiffBlocksDrift(t *testing.T) {
	cases := []struct {
		name     string
		base     int64
		observed int64
		wantMeta bool
	}{
		{"densified sparse", 8, 4194304, true},
		{"within tolerance", 8, 72, false},
		{"above tolerance", 8, 73, true},
		{"observed smaller above tolerance", 4194368, 8, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := manifest.Entry{Root: "work", Path: "work/a.bin", Type: "file", Mode: 0o644,
				Size: 4096, Blocks: c.base, NLink: 1, Hash: "abc"}
			obs := base
			obs.Blocks = c.observed

			res := Diff([]manifest.Entry{base}, []manifest.Entry{obs}, Policy{})
			if c.wantMeta {
				if len(res.Meta) != 1 {
					t.Fatalf("expected a blocks meta defect, got %+v", res.Meta)
				}
				want := fmt.Sprintf("blocks %d->%d", c.base, c.observed)
				if !strings.Contains(res.Meta[0].Detail, want) {
					t.Fatalf("detail %q must contain %q", res.Meta[0].Detail, want)
				}
				return
			}
			if len(res.Meta) != 0 || res.Matched != 1 {
				t.Fatalf("block drift within tolerance must match: meta=%+v matched=%d", res.Meta, res.Matched)
			}
		})
	}
}

func TestDiffNLinkDefect(t *testing.T) {
	base := manifest.Entry{Root: "work", Path: "work/hard_a", Type: "file", Mode: 0o644,
		Size: 4, Blocks: 8, NLink: 2, Hash: "abc"}
	obs := base
	obs.NLink = 1

	res := Diff([]manifest.Entry{base}, []manifest.Entry{obs}, Policy{})
	if len(res.Changed) != 0 {
		t.Fatalf("a lost hardlink is a meta defect, not a content one: %+v", res.Changed)
	}
	if len(res.Meta) != 1 || !strings.Contains(res.Meta[0].Detail, "nlink 2->1") {
		t.Fatalf("expected nlink 2->1, got %+v", res.Meta)
	}
	if res.MetaScore() >= 1 {
		t.Fatalf("meta score must drop, got %v", res.MetaScore())
	}
}

func TestDiffMetaSeparateFromChanged(t *testing.T) {
	base := manifest.Entry{Root: "work", Path: "work/hard_a", Type: "file", Mode: 0o644,
		Size: 4, Blocks: 8, NLink: 2, Hash: "aaaa"}
	obs := base
	obs.Hash = "bbbb"
	obs.NLink = 1

	res := Diff([]manifest.Entry{base}, []manifest.Entry{obs}, Policy{})
	if len(res.Changed) != 1 || res.Changed[0].Path != "work/hard_a" {
		t.Fatalf("content change must land in changed: %+v", res.Changed)
	}
	if len(res.Meta) != 0 {
		t.Fatalf("a content defect must not be reported as meta: %+v", res.Meta)
	}
}

func writeManifest(t *testing.T, entries ...manifest.Entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(manifest.Header{Manifest: 1, Schema: manifest.Schema, Root: "work", Platform: "linux", Hash: manifest.HashAlgorithm}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiffFilesReadsJSONL(t *testing.T) {
	base := manifest.Entry{Root: "work", Path: "work/a.bin", Type: "file", Mode: 0o644,
		Size: 4096, Blocks: 8, NLink: 1, Hash: "abc"}
	obs := base
	obs.Blocks = 4194304

	res, err := DiffFiles(writeManifest(t, base), writeManifest(t, obs), Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Meta) != 1 || res.Meta[0].Path != "work/a.bin" {
		t.Fatalf("expected a meta defect from JSONL manifests, got %+v", res.Meta)
	}
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
