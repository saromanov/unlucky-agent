package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// skipWithoutDocker gates the integration tests: they need a docker daemon
// and the sandbox image built with `make image-sandbox`.
func skipWithoutDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("UNLUCKY_DOCKER_TESTS") != "1" {
		t.Skip("set UNLUCKY_DOCKER_TESTS=1")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker unavailable")
	}
}

// diskBackedDir returns a host directory the guard must treat as disk-backed.
// A bind of a tmpfs or overlay tree is ignored by design, so the test picks a
// directory on a real filesystem.
func diskBackedDir(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/etc", "/var", "/usr", "/home", "/"} {
		var st unix.Statfs_t
		if err := unix.Statfs(dir, &st); err != nil {
			continue
		}
		switch uint32(st.Type) {
		case unix.TMPFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
			continue
		}
		return dir
	}
	t.Skip("no disk-backed host directory available for the bind test")
	return ""
}

func stageDocker(t *testing.T, cfg DockerConfig) *Docker {
	t.Helper()
	work, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "h.txt"), []byte("home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := NewDocker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Prepare(ctx, HostDirs{Root: t.TempDir(), Work: work, Home: home}); err != nil {
		_ = d.Close()
		t.Fatal(err)
	}
	return d
}

func TestDockerExecAndGuard(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	d := stageDocker(t, DockerConfig{EvidenceDir: t.TempDir()})
	defer func() { _ = d.Close() }()

	if err := d.Guard(ctx); err != nil {
		t.Fatalf("guard must pass in the sandbox layout: %v", err)
	}

	out, err := d.Exec(ctx, "printf hi", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(out), "hi") {
		t.Fatalf("exec output %q does not contain hi", out)
	}

	roots, undo, err := d.CaptureRoots(t.TempDir())
	if err != nil {
		t.Fatalf("capture roots: %v", err)
	}
	defer undo()
	var workRoot string
	for _, r := range roots {
		if r.Name == "work" {
			workRoot = r.Dir
		}
	}
	if workRoot == "" {
		t.Fatalf("work root is missing from %+v", roots)
	}
	if _, err := os.Stat(filepath.Join(workRoot, "f.txt")); err != nil {
		t.Fatalf("fixture file was not copied out: %v", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDockerGuardRejectsHostBind(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	hostDir := diskBackedDir(t)
	d := stageDocker(t, DockerConfig{
		EvidenceDir:  t.TempDir(),
		ExtraRunArgs: []string{"-v", hostDir + ":/hijack"},
	})
	defer func() { _ = d.Close() }()

	err := d.Guard(ctx)
	if err == nil {
		t.Fatal("guard must reject a host bind mount")
	}
	if !errors.Is(err, ErrGuardFailed) {
		t.Fatalf("error must wrap ErrGuardFailed: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, GuardMarker) || !strings.Contains(msg, "disk-backed mount") {
		t.Fatalf("unexpected guard error: %v", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestPerfectRestoreScript(t *testing.T) {
	script, err := perfectRestoreScript([]string{"work/src/HUMAN.md", "home/.config/app/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/sandbox/work/src/HUMAN.md",
		"/sandbox/home/.config/app/config.toml",
		"perfect-work.tar",
		"perfect-home.tar",
		"set -e",
		"find /sandbox/work -mindepth 1 -delete",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script must contain %q:\n%s", want, script)
		}
	}
}

func TestPerfectRestoreScriptRejectsBadPaths(t *testing.T) {
	for _, preserve := range [][]string{{"etc/passwd"}, {"work/../etc/passwd"}, {"work/"}} {
		if _, err := perfectRestoreScript(preserve); err == nil {
			t.Fatalf("expected an error for %q", preserve)
		}
	}
}

func TestRunArgsTmpfsSizes(t *testing.T) {
	def, err := NewDocker(DockerConfig{EvidenceDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(def.runArgs("c"), " ")
	for _, want := range []string{
		containerWork + ":rw,nosuid,size=64m",
		containerHome + ":rw,nosuid,noexec,size=64m",
		containerScratch + ":rw,nosuid,size=256m",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("default run args must contain %q:\n%s", want, args)
		}
	}

	big, err := NewDocker(DockerConfig{
		EvidenceDir:   t.TempDir(),
		WorkSizeMB:    4672,
		ScratchSizeMB: 4608,
	})
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Join(big.runArgs("c"), " ")
	if !strings.Contains(args, containerWork+":rw,nosuid,size=4672m") {
		t.Fatalf("work tmpfs size must follow the scenario resources:\n%s", args)
	}
	if !strings.Contains(args, containerScratch+":rw,nosuid,size=4608m") {
		t.Fatalf("scratch tmpfs size must follow the scenario resources:\n%s", args)
	}
}
