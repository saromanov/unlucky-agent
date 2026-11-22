package fixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func TestKnown(t *testing.T) {
	if !Known("project_git_dirty") {
		t.Fatal("project_git_dirty must be a known fixture")
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
