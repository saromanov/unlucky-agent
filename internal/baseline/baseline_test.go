package baseline

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func needGNUTar(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is not installed")
	}
	out, err := exec.Command("tar", "--version").Output()
	if err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("GNU tar is required")
	}
}

// specialTree builds a tree with a sparse file, a hardlink pair and a fifo.
func specialTree(t *testing.T) string {
	t.Helper()
	work := t.TempDir()

	sparse := filepath.Join(work, "sparse.bin")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 4<<10)
	for i := range block {
		block[i] = 0x5a
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

	writeFile(t, work, "hard_a", "hardlink target\n")
	if err := os.Link(filepath.Join(work, "hard_a"), filepath.Join(work, "hard_b")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(work, "fifo.pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	return work
}

func statOf(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return &st
}

func wipeTree(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTarRestoreKeepsSparseAndHardlinks(t *testing.T) {
	needGNUTar(t)
	work := specialTree(t)
	before := statOf(t, filepath.Join(work, "sparse.bin"))

	tarPath := filepath.Join(t.TempDir(), "snapshot.tar")
	if err := TarSnapshot(work, tarPath); err != nil {
		t.Fatal(err)
	}
	wipeTree(t, work)
	writeFile(t, work, "extra.txt", "extra\n")
	if err := TarRestoreExact(tarPath, work); err != nil {
		t.Fatal(err)
	}

	after := statOf(t, filepath.Join(work, "sparse.bin"))
	if after.Size != 1<<20 {
		t.Fatalf("sparse.bin size=%d", after.Size)
	}
	if after.Blocks > before.Blocks+64 {
		t.Fatalf("tar densified the sparse file: %d -> %d blocks", before.Blocks, after.Blocks)
	}
	if after.Blocks*512 >= 1<<20/10 {
		t.Fatalf("tar did not preserve holes: %d blocks", after.Blocks)
	}

	if n := statOf(t, filepath.Join(work, "hard_a")).Nlink; n != 2 {
		t.Fatalf("tar must keep hardlinks, nlink=%d", n)
	}
	if st := statOf(t, filepath.Join(work, "fifo.pipe")); st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		t.Fatalf("tar must restore the fifo, mode=%o", st.Mode)
	}
	if _, err := os.Stat(filepath.Join(work, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("tar restore must remove extra files, stat err=%v", err)
	}
}

func TestCpRRestoreRecordsDefects(t *testing.T) {
	work := specialTree(t)
	before := statOf(t, filepath.Join(work, "sparse.bin"))

	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := Snapshot(work, snap); err != nil {
		t.Fatal(err)
	}
	wipeTree(t, work)
	if err := RestoreExact(snap, work); err != nil {
		t.Fatal(err)
	}

	// cp-r copies every byte it can, including fifo nodes.
	if st := statOf(t, filepath.Join(work, "fifo.pipe")); st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		t.Fatalf("cp-r must copy the fifo, mode=%o", st.Mode)
	}
	// Losing the hardlink and the holes is the defect the bench records.
	if n := statOf(t, filepath.Join(work, "hard_a")).Nlink; n != 1 {
		t.Fatalf("cp-r duplicates hardlinks, nlink=%d", n)
	}
	after := statOf(t, filepath.Join(work, "sparse.bin"))
	if after.Blocks <= before.Blocks*10 || after.Blocks*512 < 1<<20/2 {
		t.Fatalf("cp-r must densify the sparse file: %d -> %d blocks", before.Blocks, after.Blocks)
	}
}
