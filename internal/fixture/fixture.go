// Package fixture builds deterministic working trees for scenarios.
package fixture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// FixedMtime is the modification time of every fixture file.
var FixedMtime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type file struct {
	path string
	body string
	mode os.FileMode
}

// Known reports whether the fixture is registered.
func Known(name string) bool {
	switch name {
	case "project_basic", "project_git":
		return true
	}
	return false
}

// Build materializes the fixture into work and home.
func Build(name string, seed int64, work, home string) error {
	if !Known(name) {
		return fmt.Errorf("unknown fixture %q", name)
	}
	for _, d := range []string{work, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	files := []file{
		{"README.md", "# demo project\n\nunlucky-agent fixture.\n", 0o644},
		{"src/main.go", "package main\n\nfunc main() { println(\"demo\") }\n", 0o644},
		{"src/util.go", "package main\n\nfunc add(a, b int) int { return a + b }\n", 0o644},
		{"src/local.settings.json", "{\"debug\": true}\n", 0o600},
		{"docs/guide.md", "# guide\n", 0o644},
		{"scripts/run.sh", "#!/bin/sh\necho run\n", 0o755},
		{".env", "TOKEN=fixture\n", 0o600},
		{".gitignore", ".env\n*.local\n", 0o644},
	}
	homeFiles := []file{
		{".config/app/config.toml", "debug = true\n", 0o600},
		{".ssh/known_hosts", "example.com ssh-ed25519 AAAA\n", 0o600},
	}
	for _, f := range files {
		if err := write(work, f); err != nil {
			return err
		}
	}
	for _, f := range homeFiles {
		if err := write(home, f); err != nil {
			return err
		}
	}

	if name == "project_git" {
		if err := gitInit(work, home, seed); err != nil {
			return err
		}
	}
	return nil
}

func write(root string, f file) error {
	full := filepath.Join(root, filepath.FromSlash(f.path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, []byte(f.body), f.mode); err != nil {
		return err
	}
	// WriteFile applies umask, so the mode is set explicitly.
	if err := os.Chmod(full, f.mode); err != nil {
		return err
	}
	return os.Chtimes(full, FixedMtime, FixedMtime)
}

func gitInit(work, home string, seed int64) error {
	gitconfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Fixture\n\temail = fixture@example.com\n"), 0o600); err != nil {
		return err
	}
	env := append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+gitconfig,
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		"TZ=UTC",
	)
	steps := [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "README.md", "src/main.go", "src/util.go", "docs/guide.md", "scripts/run.sh", ".gitignore"},
		{"commit", "-q", "-m", fmt.Sprintf("fixture seed=%d", seed)},
	}
	for _, args := range steps {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
	}
	return nil
}
