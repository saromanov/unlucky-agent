package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

// Sandbox layout inside the container.
const (
	containerWork     = "/sandbox/work"
	containerHome     = "/sandbox/home"
	containerScratch  = "/sandbox/scratch"
	containerEvidence = "/sandbox/evidence"
)

// Defaults for the docker run parameters.
const (
	DefaultImage    = "unlucky-agent-sandbox"
	DefaultMemoryMB = 512
	DefaultPids     = 4096
)

// Docker runs scenario commands inside a hardened container.
type Docker struct {
	image     string
	dockerBin string
	evidence  string
	memoryMB  int
	pids      int64
	extraArgs []string // test seam: deliberately broken mounts

	name    string
	started bool
	killed  bool
	logs    []byte
}

// DockerConfig configures the docker driver.
type DockerConfig struct {
	// Image is the sandbox image. It must contain sh, find and tar; the
	// scenarios add git where needed.
	Image string
	// EvidenceDir is the only host bind mount, created by the driver.
	EvidenceDir string
	// MemoryMB is the container memory limit.
	MemoryMB int
	// PidsLimit bounds the process count inside the container.
	PidsLimit int64
	// DockerBin overrides the docker executable path.
	DockerBin string
	// ExtraRunArgs are appended to `docker run` before the image; used by
	// tests to mount a host path the guard must reject.
	ExtraRunArgs []string
}

// NewDocker returns a docker sandbox driver.
func NewDocker(cfg DockerConfig) (*Docker, error) {
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = DefaultMemoryMB
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = DefaultPids
	}
	if cfg.DockerBin == "" {
		cfg.DockerBin = "docker"
	}
	if cfg.EvidenceDir == "" {
		return nil, fmt.Errorf("sandbox docker: EvidenceDir is required")
	}
	if err := os.MkdirAll(cfg.EvidenceDir, 0o755); err != nil {
		return nil, err
	}
	return &Docker{
		image:     cfg.Image,
		dockerBin: cfg.DockerBin,
		evidence:  cfg.EvidenceDir,
		memoryMB:  cfg.MemoryMB,
		pids:      cfg.PidsLimit,
		extraArgs: cfg.ExtraRunArgs,
	}, nil
}

func (d *Docker) Name() string { return "docker" }

// runArgs builds the hardened `docker run` argument list, without the image
// and the container command.
func (d *Docker) runArgs(name string) []string {
	return []string{
		"run", "-d", "--name", name,
		"--network", "none",
		"--user", "1000:1000",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", fmt.Sprint(d.pids),
		"--memory", fmt.Sprintf("%dm", d.memoryMB),
		"--tmpfs", containerWork + ":rw,nosuid,size=64m,uid=1000,gid=1000,mode=0755",
		"--tmpfs", containerHome + ":rw,nosuid,noexec,size=64m,uid=1000,gid=1000,mode=0700",
		"--tmpfs", containerScratch + ":rw,nosuid,size=256m,uid=1000,gid=1000,mode=0755",
		"--mount", "type=bind,src=" + d.evidence + ",dst=" + containerEvidence,
	}
}

// Prepare starts the container and installs the staged fixture.
func (d *Docker) Prepare(ctx context.Context, host HostDirs) error {
	if host.Work == "" || host.Home == "" {
		return fmt.Errorf("sandbox docker: work and home must be set")
	}
	name, err := containerName()
	if err != nil {
		return err
	}
	d.name = name

	args := d.runArgs(name)
	args = append(args, d.extraArgs...)
	args = append(args, d.image, "sleep", "infinity")

	out, err := d.cli(ctx, args...)
	if err != nil {
		return fmt.Errorf("sandbox docker: run: %w: %s", err, out)
	}
	d.started = true

	// The fixture is streamed in with tar instead of `docker cp`: a copy
	// keeps the host uid, and the container has no CAP_CHOWN to fix it.
	// Extracting as uid 1000 gives the sandbox user ownership directly.
	if err := d.copyIn(ctx, host.Work, containerWork); err != nil {
		return fmt.Errorf("sandbox docker: copy work: %w", err)
	}
	if err := d.copyIn(ctx, host.Home, containerHome); err != nil {
		return fmt.Errorf("sandbox docker: copy home: %w", err)
	}
	return nil
}

// copyIn streams the src tree into dst inside the container as uid 1000.
func (d *Docker) copyIn(ctx context.Context, src, dst string) error {
	tarCmd := execCommandContext(ctx, "tar", "-C", src, "-cf", "-", ".")
	execCmd := execCommandContext(ctx, d.dockerBin, "exec", "-i", "-u", "1000", d.name, "tar", "-C", dst, "-xf", "-")
	pipe, err := tarCmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	execCmd.Stdin = pipe
	execCmd.Stderr = &stderr
	if err := tarCmd.Start(); err != nil {
		return err
	}
	if err := execCmd.Start(); err != nil {
		_ = tarCmd.Process.Kill()
		_ = tarCmd.Wait()
		return err
	}
	tarErr := tarCmd.Wait()
	execErr := execCmd.Wait()
	if tarErr != nil {
		return fmt.Errorf("tar: %w", tarErr)
	}
	if execErr != nil {
		return fmt.Errorf("%w: %s", execErr, stderr.Bytes())
	}
	return nil
}

// Guard verifies the isolation boundary before the first damage step.
func (d *Docker) Guard(ctx context.Context) error {
	out, err := d.execRaw(ctx, guardScript, 30*time.Second)
	if err != nil {
		return fmt.Errorf("%w: guard probe failed: %s", ErrGuardFailed, err)
	}
	if fails := parseGuard(string(out)); len(fails) > 0 {
		return GuardFailure(fails)
	}
	return nil
}

// Exec runs `sh -c script` inside the container. On timeout the container
// is killed and the collected logs are attached to the error.
func (d *Docker) Exec(ctx context.Context, script string, timeout time.Duration) ([]byte, error) {
	if d.killed {
		return nil, fmt.Errorf("sandbox docker: container %s was killed by a previous timeout", d.name)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := d.execRaw(cctx, script, timeout)
	if cctx.Err() == context.DeadlineExceeded {
		d.killAndCollectLogs(context.Background())
		return out, fmt.Errorf("timeout after %s: container killed; logs:\n%s", timeout, d.logs)
	}
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, out)
	}
	return out, nil
}

// execRaw performs the docker exec call.
func (d *Docker) execRaw(ctx context.Context, script string, _ time.Duration) ([]byte, error) {
	cmd := execCommandContext(ctx, d.dockerBin,
		"exec",
		"-u", "1000",
		"-w", containerWork,
		"-e", "HOME="+containerHome,
		"-e", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"-e", "TZ=UTC",
		"-e", "LANG=C.UTF-8",
		"-e", "LC_ALL=C.UTF-8",
		d.name, "sh", "-c", script,
	)
	return cmd.CombinedOutput()
}

// killAndCollectLogs kills the container and stores `docker logs` output.
func (d *Docker) killAndCollectLogs(ctx context.Context) {
	d.killed = true
	_, _ = d.cli(ctx, "kill", d.name)
	if out, err := d.cli(ctx, "logs", d.name); err == nil {
		d.logs = out
	}
}

// CaptureRoots copies the sandbox work and home trees out for manifest
// capture on the host. The undo function removes the copies.
//
// The trees are streamed out with tar: `docker cp` cannot read tmpfs mounts,
// it only sees the container rootfs and the bind mounts.
func (d *Docker) CaptureRoots(hostDir string) ([]manifest.Root, func(), error) {
	work := filepath.Join(hostDir, "work")
	home := filepath.Join(hostDir, "home")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, nil, err
	}
	if err := d.copyOut(containerWork, work); err != nil {
		return nil, nil, fmt.Errorf("sandbox docker: copy out work: %w", err)
	}
	if err := d.copyOut(containerHome, home); err != nil {
		return nil, nil, fmt.Errorf("sandbox docker: copy out home: %w", err)
	}
	undo := func() { os.RemoveAll(hostDir) }
	return []manifest.Root{
		{Name: "work", Dir: work},
		{Name: "home", Dir: home},
	}, undo, nil
}

// copyOut streams the src tree from the container into the dst host directory.
func (d *Docker) copyOut(src, dst string) error {
	execCmd := execCommandContext(context.Background(), d.dockerBin, "exec", "-u", "1000", d.name, "tar", "-C", src, "-cf", "-", ".")
	tarCmd := execCommandContext(context.Background(), "tar", "-C", dst, "-xf", "-")
	pipe, err := execCmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	tarCmd.Stdin = pipe
	execCmd.Stderr = &stderr
	if err := execCmd.Start(); err != nil {
		return err
	}
	if err := tarCmd.Start(); err != nil {
		_ = execCmd.Process.Kill()
		_ = execCmd.Wait()
		return err
	}
	execErr := execCmd.Wait()
	tarErr := tarCmd.Wait()
	if execErr != nil {
		return fmt.Errorf("%w: %s", execErr, stderr.Bytes())
	}
	if tarErr != nil {
		return fmt.Errorf("tar: %w", tarErr)
	}
	return nil
}

// CPSnapshot implements CPBaseline: the cp-r reference rollback runs fully
// inside the container.
func (d *Docker) CPSnapshot(ctx context.Context, timeout time.Duration) error {
	script := "tar -C " + containerWork + " -cf " + containerScratch + "/snapshot.tar ."
	if _, err := d.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("docker cp-r snapshot: %w", err)
	}
	return nil
}

// CPRestore implements CPBaseline: wipe the work tree and unpack the
// snapshot, matching baseline.RestoreExact semantics.
func (d *Docker) CPRestore(ctx context.Context, timeout time.Duration) error {
	script := "find " + containerWork + " -mindepth 1 -delete && " +
		"tar -C " + containerWork + " -xf " + containerScratch + "/snapshot.tar"
	if _, err := d.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("docker cp-r restore: %w", err)
	}
	return nil
}

// PerfectSnapshot implements PerfectBaseline: it archives both the work and
// the home tree at the checkpoint, inside the container.
func (d *Docker) PerfectSnapshot(ctx context.Context, timeout time.Duration) error {
	script := "tar -C " + containerWork + " -cf " + containerScratch + "/perfect-work.tar . && " +
		"tar -C " + containerHome + " -cf " + containerScratch + "/perfect-home.tar ."
	if _, err := d.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("docker perfect snapshot: %w", err)
	}
	return nil
}

// PerfectRestore implements PerfectBaseline: it wipes both trees, unpacks the
// snapshots and puts preserved post-T edits back.
func (d *Docker) PerfectRestore(ctx context.Context, timeout time.Duration, preserve []string) error {
	script, err := perfectRestoreScript(preserve)
	if err != nil {
		return fmt.Errorf("docker perfect restore: %w", err)
	}
	if _, err := d.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("docker perfect restore: %w", err)
	}
	return nil
}

// perfectRestoreScript builds the restore script. Preserved paths are
// manifest-style paths rooted at "work/" or "home/".
func perfectRestoreScript(preserve []string) (string, error) {
	paths := make([]string, 0, len(preserve))
	for _, p := range preserve {
		var base string
		var rel string
		switch {
		case strings.HasPrefix(p, "work/"):
			base, rel = containerWork, strings.TrimPrefix(p, "work/")
		case strings.HasPrefix(p, "home/"):
			base, rel = containerHome, strings.TrimPrefix(p, "home/")
		default:
			return "", fmt.Errorf("unsupported preserve path %q", p)
		}
		if rel == "" || strings.Contains(rel, "..") {
			return "", fmt.Errorf("invalid preserve path %q", p)
		}
		paths = append(paths, path.Join(base, rel))
	}

	keepRoot := containerScratch + "/keep"
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("rm -rf " + shellQuote(keepRoot) + "\n")
	for _, p := range paths {
		kept := path.Join(keepRoot, p)
		b.WriteString("if [ -e " + shellQuote(p) + " ]; then mkdir -p " + shellQuote(path.Dir(kept)) +
			" && mv " + shellQuote(p) + " " + shellQuote(kept) + "; fi\n")
	}
	b.WriteString("find " + containerWork + " -mindepth 1 -delete\n")
	b.WriteString("find " + containerHome + " -mindepth 1 -delete\n")
	b.WriteString("tar -C " + containerWork + " -xf " + shellQuote(containerScratch+"/perfect-work.tar") + "\n")
	b.WriteString("tar -C " + containerHome + " -xf " + shellQuote(containerScratch+"/perfect-home.tar") + "\n")
	for _, p := range paths {
		kept := path.Join(keepRoot, p)
		b.WriteString("if [ -e " + shellQuote(kept) + " ]; then mkdir -p " + shellQuote(path.Dir(p)) +
			" && mv " + shellQuote(kept) + " " + shellQuote(p) + "; fi\n")
	}
	b.WriteString("rm -rf " + shellQuote(keepRoot) + "\n")
	return b.String(), nil
}

// shellQuote single-quotes a path for the container shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Close removes the container.
func (d *Docker) Close() error {
	if !d.started {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := d.cli(ctx, "rm", "-f", d.name); err != nil {
		return fmt.Errorf("sandbox docker: rm: %w: %s", err, out)
	}
	return nil
}

func (d *Docker) cli(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.dockerBin, args...)
	return cmd.CombinedOutput()
}

func containerName() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "unlucky-agent-" + hex.EncodeToString(b), nil
}
