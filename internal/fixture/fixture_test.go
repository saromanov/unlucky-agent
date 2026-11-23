package fixture

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func TestKnown(t *testing.T) {
	for _, name := range []string{"project_git_dirty", "large_files", "special_files", "path_edges"} {
		if !Known(name) {
			t.Fatalf("%s must be a known fixture", name)
		}
	}
}

func TestProjectGitDirty(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	if err := Build("project_git_dirty", 42, work, home); err != nil {
		t.Fatal(err)
	}

	util, err := os.ReadFile(filepath.Join(work, "src", "util.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(util), "// local uncommitted edit\n") {
		t.Fatalf("util.go must keep the unstaged local edit, got %q", util)
	}

	main, err := os.ReadFile(filepath.Join(work, "src", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "demo, edited") {
		t.Fatalf("main.go must carry the staged edit, got %q", main)
	}

	if _, err := os.Stat(filepath.Join(work, "src", "extra.go")); err != nil {
		t.Fatalf("src/extra.go must exist: %v", err)
	}

	// -uno hides the untracked src/local.settings.json fixture file, so the
	// output shows only the staged and unstaged edits.
	cmd := exec.Command("git", "status", "--porcelain", "-uno")
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	want := "A  src/extra.go\nM  src/main.go\n M src/util.go\n"
	if string(out) != want {
		t.Fatalf("git status mismatch:\n got %q\nwant %q", out, want)
	}
}

func buildFixture(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	if err := Build(name, 42, work, home); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return work
}

func captureEntries(t *testing.T, work string) []manifest.Entry {
	t.Helper()
	var buf bytes.Buffer
	if _, err := manifest.Capture([]manifest.Root{{Name: "work", Dir: work}}, &buf); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&buf)
	var hdr manifest.Header
	if err := dec.Decode(&hdr); err != nil {
		t.Fatal(err)
	}
	var out []manifest.Entry
	for dec.More() {
		var e manifest.Entry
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// normalizeManifest drops fields that depend on where the tree lives, so two
// builds can be compared for determinism.
func normalizeManifest(entries []manifest.Entry) []manifest.Entry {
	out := make([]manifest.Entry, len(entries))
	for i, e := range entries {
		e.MtimeNS = 0
		e.UID = 0
		e.GID = 0
		e.Blocks = 0
		out[i] = e
	}
	return out
}

func assertDeterministic(t *testing.T, name string) {
	t.Helper()
	a := normalizeManifest(captureEntries(t, buildFixture(t, name)))
	b := normalizeManifest(captureEntries(t, buildFixture(t, name)))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s fixture is not deterministic: %d vs %d entries", name, len(a), len(b))
	}
}

func entriesByPath(entries []manifest.Entry) map[string]manifest.Entry {
	out := make(map[string]manifest.Entry, len(entries))
	for _, e := range entries {
		out[e.Path] = e
	}
	return out
}

func TestSpecialFiles(t *testing.T) {
	work := buildFixture(t, "special_files")
	byPath := entriesByPath(captureEntries(t, work))

	type want struct {
		path string
		mode uint32
		typ  string
	}
	for _, w := range []want{
		{"work/spec/ro.txt", 0o400, "file"},
		{"work/spec/rwx.sh", 0o755, "file"},
		{"work/spec/suid.sh", 0o4755, "file"},
		{"work/spec/sgid", 0o2755, "dir"},
		{"work/spec/fifo.pipe", 0o600, "fifo"},
		{"work/spec/sock.unix", 0, "socket"},
	} {
		e, ok := byPath[w.path]
		if !ok {
			t.Fatalf("%s is missing from the manifest", w.path)
		}
		if e.Type != w.typ {
			t.Fatalf("%s type=%s, want %s", w.path, e.Type, w.typ)
		}
		if w.mode != 0 && e.Mode != w.mode {
			t.Fatalf("%s mode=%04o, want %04o", w.path, e.Mode, w.mode)
		}
	}

	for link, target := range map[string]string{
		"work/spec/link_ok":     "rwx.sh",
		"work/spec/link_broken": "missing/target",
		"work/spec/cyc_a":       "cyc_b",
		"work/spec/cyc_b":       "cyc_a",
	} {
		e, ok := byPath[link]
		if !ok || e.Type != "symlink" {
			t.Fatalf("%s must be a symlink: %+v", link, e)
		}
		if e.Link != target {
			t.Fatalf("%s -> %q, want %q", link, e.Link, target)
		}
	}

	var hardA, hardB syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(work, "spec", "hard_a"), &hardA); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Lstat(filepath.Join(work, "spec", "hard_b"), &hardB); err != nil {
		t.Fatal(err)
	}
	if hardA.Nlink != 2 || hardB.Nlink != 2 {
		t.Fatalf("hardlink nlink=%d/%d, want 2/2", hardA.Nlink, hardB.Nlink)
	}
	if hardA.Ino != hardB.Ino {
		t.Fatalf("hard_a and hard_b must share an inode")
	}

	for _, rel := range []string{"spec/ro.txt", "spec/fifo.pipe", "spec/link_ok", "spec/sgid"} {
		info, err := os.Lstat(filepath.Join(work, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(FixedMtime) {
			t.Fatalf("%s mtime=%v, want %v", rel, info.ModTime(), FixedMtime)
		}
	}

	assertDeterministic(t, "special_files")
}

func TestPathEdges(t *testing.T) {
	work := buildFixture(t, "path_edges")
	entries := captureEntries(t, work)
	byPath := entriesByPath(entries)

	names := []string{
		"space name.txt",
		"tab\tname.txt",
		"line\nbreak.txt",
		"single'quote.txt",
		"double\"quote.txt",
		`back\slash.txt`,
		"-leading-dash.txt",
		"--spacer.txt",
		"日本語.txt",
		"caf\u00e9-NFC.txt",
		"cafe\u0301-NFD.txt",
	}
	for _, n := range names {
		if _, ok := byPath["work/edge/"+n]; !ok {
			t.Fatalf("edge entry %q is missing", n)
		}
	}

	nfc, nfd := "work/edge/caf\u00e9-NFC.txt", "work/edge/cafe\u0301-NFD.txt"
	if nfc == nfd {
		t.Fatal("NFC and NFD names must differ byte-wise")
	}

	many := 0
	longest := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "work/edge/many/") && e.Type == "file" {
			many++
		}
		if e.Type == "file" && len(e.Path) > len(longest) {
			longest = e.Path
		}
	}
	if many != manyFiles {
		t.Fatalf("edge/many holds %d files, want %d", many, manyFiles)
	}
	if len(longest) < 3000 || len(longest) >= 4096 {
		t.Fatalf("deepest path length %d is outside [3000, 4096)", len(longest))
	}

	assertDeterministic(t, "path_edges")
}

func TestLargeFiles(t *testing.T) {
	if os.Getenv("UNLUCKY_BIG_TESTS") != "1" {
		t.Skip("set UNLUCKY_BIG_TESTS=1")
	}
	work := buildFixture(t, "large_files")
	byPath := entriesByPath(captureEntries(t, work))

	regular, ok := byPath["work/big/regular.bin"]
	if !ok || regular.Size != largeFileSize {
		t.Fatalf("regular.bin size=%d, want %d", regular.Size, largeFileSize)
	}
	if regular.Blocks*512 < largeFileSize/2 {
		t.Fatalf("regular.bin is not dense: %d blocks", regular.Blocks)
	}

	sparse, ok := byPath["work/big/sparse.bin"]
	if !ok || sparse.Size != largeFileSize {
		t.Fatalf("sparse.bin size=%d, want %d", sparse.Size, largeFileSize)
	}
	if sparse.Blocks*512 >= largeFileSize/10 {
		t.Fatalf("sparse.bin has no real holes: %d blocks", sparse.Blocks)
	}

	assertDeterministic(t, "large_files")
}
