package baseline

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPerfectRestore(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	writeFile(t, work, "a.txt", "a\n")
	writeFile(t, work, "b.txt", "b\n")
	writeFile(t, work, "sub/c.txt", "c\n")
	writeFile(t, home, ".config/app/config.toml", "debug = true\n")

	snapWork := filepath.Join(root, "snap-work")
	snapHome := filepath.Join(root, "snap-home")
	if err := PerfectSnapshot(work, home, snapWork, snapHome); err != nil {
		t.Fatal(err)
	}

	// Mutate the destination: delete, add, modify and create a new directory.
	if err := os.Remove(filepath.Join(work, "a.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, work, "extra.txt", "extra\n")
	writeFile(t, work, "b.txt", "post-T edit\n")
	writeFile(t, work, "newdir/post.txt", "post-T new dir\n")
	if err := os.Remove(filepath.Join(home, ".config/app/config.toml")); err != nil {
		t.Fatal(err)
	}

	preserved := []string{"work/b.txt", "work/newdir/post.txt"}
	if err := PerfectRestore(snapWork, snapHome, work, home, preserved); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(work, "a.txt")); got != "a\n" {
		t.Fatalf("deleted file must be back, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(work, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("extra file must be removed, stat err=%v", err)
	}
	if got := readFile(t, filepath.Join(work, "b.txt")); got != "post-T edit\n" {
		t.Fatalf("preserved file must keep the post-T content, got %q", got)
	}
	if got := readFile(t, filepath.Join(work, "newdir/post.txt")); got != "post-T new dir\n" {
		t.Fatalf("preserved file in a new directory must survive, got %q", got)
	}
	if got := readFile(t, filepath.Join(home, ".config/app/config.toml")); got != "debug = true\n" {
		t.Fatalf("home tree must be restored, got %q", got)
	}

	// Every non-preserved regular file must match the snapshot byte-for-byte.
	err := filepath.WalkDir(snapWork, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(snapWork, p)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == "b.txt" {
			return nil
		}
		want := readFile(t, p)
		if got := readFile(t, filepath.Join(work, rel)); got != want {
			t.Fatalf("%s mismatch after restore: got %q want %q", rel, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
