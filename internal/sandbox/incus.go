package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

// Defaults for the incus driver.
const (
	DefaultIncusImage = "images:debian/12/cloud"
	incusHomeSizeMB   = 64
)

// Incus runs scenario commands inside an unprivileged Incus container.
type Incus struct {
	image     string
	incusBin  string
	evidence  string
	memoryMB  int
	workSize  int
	scratchSz int
	pids      int64
	extraArgs []string

	name    string
	started bool
	killed  bool
	logs    []byte
}

// IncusConfig configures the incus driver.
type IncusConfig struct {
	// Image is the sandbox image. The cloud variant guarantees the uid 1000
	// user; the image must contain sh, find, tar and du.
	Image string
	// EvidenceDir is the only host bind mount, created by the driver.
	EvidenceDir string
	// MemoryMB is the container memory limit.
	MemoryMB int
	// WorkSizeMB is the work tmpfs size; it comes from the scenario disk_mb.
	WorkSizeMB int
	// ScratchSizeMB is the scratch tmpfs size; snapshot archives live there.
	ScratchSizeMB int
	// PidsLimit bounds the process count inside the container.
	PidsLimit int64
	// IncusBin overrides the incus executable path.
	IncusBin string
	// ExtraRunArgs are extra `incus config set key=value` pairs; used by
	// tests to break the sandbox layout on purpose.
	ExtraRunArgs []string
}

// NewIncus returns an incus sandbox driver.
func NewIncus(cfg IncusConfig) (*Incus, error) {
	if cfg.Image == "" {
		cfg.Image = DefaultIncusImage
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = DefaultMemoryMB
	}
	if cfg.WorkSizeMB == 0 {
		cfg.WorkSizeMB = DefaultWorkSizeMB
	}
	if cfg.ScratchSizeMB == 0 {
		cfg.ScratchSizeMB = DefaultScratchSizeMB
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = DefaultPids
	}
	if cfg.IncusBin == "" {
		cfg.IncusBin = "incus"
	}
	if cfg.EvidenceDir == "" {
		return nil, fmt.Errorf("sandbox incus: EvidenceDir is required")
	}
	if _, err := exec.LookPath(cfg.IncusBin); err != nil {
		return nil, fmt.Errorf("sandbox incus: %s not found", cfg.IncusBin)
	}
	if err := os.MkdirAll(cfg.EvidenceDir, 0o755); err != nil {
		return nil, err
	}
	return &Incus{
		image:     cfg.Image,
		incusBin:  cfg.IncusBin,
		evidence:  cfg.EvidenceDir,
		memoryMB:  cfg.MemoryMB,
		workSize:  cfg.WorkSizeMB,
		scratchSz: cfg.ScratchSizeMB,
		pids:      cfg.PidsLimit,
		extraArgs: cfg.ExtraRunArgs,
	}, nil
}

func (i *Incus) Name() string { return "incus" }

// Prepare creates and starts the container, then installs the staged fixture.
func (i *Incus) Prepare(ctx context.Context, host HostDirs) error {
	if host.Work == "" || host.Home == "" {
		return fmt.Errorf("sandbox incus: work and home must be set")
	}
	name, err := containerName()
	if err != nil {
		return err
	}
	i.name = name

	if out, err := i.cli(ctx, "init", i.image, name); err != nil {
		return fmt.Errorf("sandbox incus: init: %w: %s", err, out)
	}
	i.started = true
	if err := i.configure(ctx); err != nil {
		return err
	}
	if out, err := i.cli(ctx, "start", name); err != nil {
		return fmt.Errorf("sandbox incus: start: %w: %s", err, out)
	}
	if out, err := i.cli(ctx, "exec", name, "--", "mkdir", "-p",
		containerWork, containerHome, containerScratch); err != nil {
		return fmt.Errorf("sandbox incus: mkdir: %w: %s", err, out)
	}
	if err := i.mountTmpfs(ctx); err != nil {
		return err
	}
	if err := i.copyIn(ctx, host.Work, containerWork); err != nil {
		return fmt.Errorf("sandbox incus: copy work: %w", err)
	}
	if err := i.copyIn(ctx, host.Home, containerHome); err != nil {
		return fmt.Errorf("sandbox incus: copy home: %w", err)
	}
	return nil
}

// configure applies the limits, the evidence device and the test seam before
// the first start.
func (i *Incus) configure(ctx context.Context) error {
	if err := i.removeNICs(ctx); err != nil {
		return err
	}
	args := make([]string, 0, 7+len(i.extraArgs))
	args = append(args,
		"config", "set", i.name,
		fmt.Sprintf("limits.memory=%dMiB", i.memoryMB),
		fmt.Sprintf("limits.processes=%d", i.pids),
		"security.privileged=false",
		"security.devlxd=false",
	)
	args = append(args, i.extraArgs...)
	if out, err := i.cli(ctx, args...); err != nil {
		return fmt.Errorf("sandbox incus: config set: %w: %s", err, out)
	}
	if out, err := i.cli(ctx, "config", "device", "add", i.name, "evidence", "disk",
		"source="+i.evidence, "path="+containerEvidence); err != nil {
		return fmt.Errorf("sandbox incus: evidence device: %w: %s", err, out)
	}
	return nil
}

// removeNICs drops every network interface of the container: an instance
// device is removed, an inherited one is masked with a none device.
func (i *Incus) removeNICs(ctx context.Context) error {
	out, err := i.cli(ctx, "config", "device", "list", i.name,
		"--expanded", "--columns", "nt", "--format", "csv,noheader")
	if err != nil {
		return fmt.Errorf("sandbox incus: device list: %w: %s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 2 || strings.TrimSpace(fields[1]) != "nic" {
			continue
		}
		dev := strings.TrimSpace(fields[0])
		if _, err := i.cli(ctx, "config", "device", "remove", i.name, dev); err == nil {
			continue
		}
		if out, err := i.cli(ctx, "config", "device", "add", i.name, dev, "none"); err != nil {
			return fmt.Errorf("sandbox incus: device mask %s: %w: %s", dev, err, out)
		}
	}
	return nil
}

// mountTmpfs installs the work, home and scratch tmpfs mounts through raw.lxc
// and restarts the container: mount entries apply on start only.
func (i *Incus) mountTmpfs(ctx context.Context) error {
	if out, err := i.cli(ctx, "config", "set", i.name, "raw.lxc", i.mountEntries()); err != nil {
		return fmt.Errorf("sandbox incus: raw.lxc: %w: %s", err, out)
	}
	if out, err := i.cli(ctx, "restart", "--force", i.name); err != nil {
		return fmt.Errorf("sandbox incus: restart: %w: %s", err, out)
	}
	return nil
}

// mountEntries builds the lxc.mount.entry lines for the three tmpfs trees.
func (i *Incus) mountEntries() string {
	var b strings.Builder
	fmt.Fprintf(&b, "lxc.mount.entry = tmpfs %s tmpfs rw,nosuid,size=%dm,uid=1000,gid=1000,mode=0755,create=dir\n",
		strings.TrimPrefix(containerWork, "/"), i.workSize)
	fmt.Fprintf(&b, "lxc.mount.entry = tmpfs %s tmpfs rw,nosuid,noexec,size=%dm,uid=1000,gid=1000,mode=0700,create=dir\n",
		strings.TrimPrefix(containerHome, "/"), incusHomeSizeMB)
	fmt.Fprintf(&b, "lxc.mount.entry = tmpfs %s tmpfs rw,nosuid,size=%dm,uid=1000,gid=1000,mode=0755,create=dir\n",
		strings.TrimPrefix(containerScratch, "/"), i.scratchSz)
	return b.String()
}

// copyIn streams the src tree into dst inside the container as uid 1000.
// --sparse keeps fixture holes: GNU tar rebuilds them on extraction.
func (i *Incus) copyIn(ctx context.Context, src, dst string) error {
	tarCmd := execCommandContext(ctx, "tar", "--sparse", "-C", src, "-cf", "-", ".")
	execCmd := execCommandContext(ctx, i.incusBin, "exec", i.name,
		"--user", "1000", "--", "tar", "--sparse", "-C", dst, "-xf", "-")
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
func (i *Incus) Guard(ctx context.Context) error {
	out, err := i.execRaw(ctx, incusGuardScript, 30*time.Second)
	if err != nil {
		return fmt.Errorf("%w: guard probe failed: %s", ErrGuardFailed, err)
	}
	if fails := parseIncusGuard(string(out)); len(fails) > 0 {
		return GuardFailure(fails)
	}
	return nil
}

// Exec runs `sh -c script` inside the container. On timeout the container
// is stopped and the collected logs are attached to the error.
func (i *Incus) Exec(ctx context.Context, script string, timeout time.Duration) ([]byte, error) {
	if i.killed {
		return nil, fmt.Errorf("sandbox incus: container %s was killed by a previous timeout", i.name)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := i.execRaw(cctx, script, timeout)
	if cctx.Err() == context.DeadlineExceeded {
		i.killAndCollectLogs(context.Background())
		return out, fmt.Errorf("timeout after %s: container killed; logs:\n%s", timeout, i.logs)
	}
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, out)
	}
	return out, nil
}

// execRaw performs the incus exec call.
func (i *Incus) execRaw(ctx context.Context, script string, timeout time.Duration) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := execCommandContext(cctx, i.incusBin,
		"exec", i.name,
		"--user", "1000",
		"--cwd", containerWork,
		"--env", "HOME="+containerHome,
		"--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--env", "TZ=UTC",
		"--env", "LANG=C.UTF-8",
		"--env", "LC_ALL=C.UTF-8",
		"--", "sh", "-c", script,
	)
	return cmd.CombinedOutput()
}

// killAndCollectLogs stops the container and stores the instance log.
func (i *Incus) killAndCollectLogs(ctx context.Context) {
	i.killed = true
	_, _ = i.cli(ctx, "stop", "--force", i.name)
	if out, err := i.cli(ctx, "info", i.name, "--show-log"); err == nil {
		i.logs = out
	}
}

// CaptureRoots streams the sandbox work and home trees out for manifest
// capture on the host. The undo function removes the copies.
func (i *Incus) CaptureRoots(hostDir string) ([]manifest.Root, func(), error) {
	work := filepath.Join(hostDir, "work")
	home := filepath.Join(hostDir, "home")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, nil, err
	}
	if err := i.copyOut(containerWork, work); err != nil {
		return nil, nil, fmt.Errorf("sandbox incus: copy out work: %w", err)
	}
	if err := i.copyOut(containerHome, home); err != nil {
		return nil, nil, fmt.Errorf("sandbox incus: copy out home: %w", err)
	}
	undo := func() { _ = os.RemoveAll(hostDir) }
	return []manifest.Root{
		{Name: "work", Dir: work},
		{Name: "home", Dir: home},
	}, undo, nil
}

// copyOut streams the src tree from the container into the dst host directory.
func (i *Incus) copyOut(src, dst string) error {
	execCmd := execCommandContext(context.Background(), i.incusBin,
		"exec", i.name, "--user", "1000", "--", "tar", "--sparse", "-C", src, "-cf", "-", ".")
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
func (i *Incus) CPSnapshot(ctx context.Context, timeout time.Duration) error {
	script := "tar --sparse -C " + containerWork + " -cf " + containerScratch + "/snapshot.tar ."
	if _, err := i.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("sandbox incus: cp-r snapshot: %w", err)
	}
	return nil
}

// CPRestore implements CPBaseline: wipe the work tree and unpack the
// snapshot, matching baseline.RestoreExact semantics.
func (i *Incus) CPRestore(ctx context.Context, timeout time.Duration) error {
	script := "find " + containerWork + " -mindepth 1 -delete && " +
		"tar -C " + containerWork + " -xf " + containerScratch + "/snapshot.tar"
	if _, err := i.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("sandbox incus: cp-r restore: %w", err)
	}
	return nil
}

// PerfectSnapshot implements PerfectBaseline: it archives both the work and
// the home tree at the checkpoint, inside the container.
func (i *Incus) PerfectSnapshot(ctx context.Context, timeout time.Duration) error {
	script := "tar -C " + containerWork + " -cf " + containerScratch + "/perfect-work.tar . && " +
		"tar -C " + containerHome + " -cf " + containerScratch + "/perfect-home.tar ."
	if _, err := i.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("sandbox incus: perfect snapshot: %w", err)
	}
	return nil
}

// PerfectRestore implements PerfectBaseline: it wipes both trees, unpacks the
// snapshots and puts preserved post-T edits back.
func (i *Incus) PerfectRestore(ctx context.Context, timeout time.Duration, preserve []string) error {
	script, err := perfectRestoreScript(preserve)
	if err != nil {
		return fmt.Errorf("sandbox incus: perfect restore: %w", err)
	}
	if _, err := i.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("sandbox incus: perfect restore: %w", err)
	}
	return nil
}

// SnapshotBytes implements SnapshotSizer: it reports the allocated size of the
// scratch tree that holds the checkpoint archives.
func (i *Incus) SnapshotBytes(ctx context.Context, baseline string) (int64, error) {
	if baseline == "none" {
		return 0, nil
	}
	return i.duBytes(ctx, containerScratch)
}

// DirBytes implements StorageSizer: it reports the allocated size of the
// container work or home tree.
func (i *Incus) DirBytes(ctx context.Context, name string) (int64, error) {
	switch name {
	case "work":
		return i.duBytes(ctx, containerWork)
	case "home":
		return i.duBytes(ctx, containerHome)
	default:
		return 0, fmt.Errorf("sandbox incus: dir bytes: unknown tree %q", name)
	}
}

// Close deletes the container.
func (i *Incus) Close() error {
	if !i.started {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := i.cli(ctx, "delete", "--force", i.name); err != nil {
		return fmt.Errorf("sandbox incus: delete: %w: %s", err, out)
	}
	return nil
}

// duBytes reports the allocated size of a container path in bytes.
func (i *Incus) duBytes(ctx context.Context, dir string) (int64, error) {
	out, err := i.Exec(ctx, "du -sk "+dir, 30*time.Second)
	if err != nil {
		return 0, fmt.Errorf("sandbox incus: du %s: %w", dir, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("sandbox incus: du %s: empty output %q", dir, out)
	}
	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sandbox incus: du %s: parse %q: %w", dir, fields[0], err)
	}
	return kib * 1024, nil
}

func (i *Incus) cli(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, i.incusBin, args...)
	return cmd.CombinedOutput()
}
