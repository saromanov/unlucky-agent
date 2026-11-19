package oracle

import (
	"os"
	"path/filepath"
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
