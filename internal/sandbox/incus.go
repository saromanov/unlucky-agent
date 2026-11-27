package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

const (
	DefaultIncusImage = "images:debian/12/cloud"
	incusHomeSizeMB   = 64
)

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

type IncusConfig struct {
	Image         string
	EvidenceDir   string
	MemoryMB      int
	WorkSizeMB    int
	ScratchSizeMB int
	PidsLimit     int64
	IncusBin      string
	ExtraRunArgs  []string
}

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
	evidence, err := filepath.Abs(cfg.EvidenceDir)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(evidence, 0o777); err != nil {
		return nil, err
	}
	return &Incus{
		image:     cfg.Image,
		incusBin:  cfg.IncusBin,
		evidence:  evidence,
		memoryMB:  cfg.MemoryMB,
		workSize:  cfg.WorkSizeMB,
		scratchSz: cfg.ScratchSizeMB,
		pids:      cfg.PidsLimit,
		extraArgs: cfg.ExtraRunArgs,
	}, nil
}

func (i *Incus) Name() string { return "incus" }

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
		"security.guestapi=false",
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

func (i *Incus) removeNICs(ctx context.Context) error {
	out, err := i.cli(ctx, "query", "/1.0/instances/"+i.name)
	if err != nil {
		return fmt.Errorf("sandbox incus: device list: %w: %s", err, out)
	}
	var info struct {
		ExpandedDevices map[string]struct {
			Type string `json:"type"`
		} `json:"expanded_devices"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return fmt.Errorf("sandbox incus: device list: %w", err)
	}
	devs := make([]string, 0, len(info.ExpandedDevices))
	for dev, cfg := range info.ExpandedDevices {
		if cfg.Type == "nic" {
			devs = append(devs, dev)
		}
	}
	sort.Strings(devs)
	for _, dev := range devs {
		if _, err := i.cli(ctx, "config", "device", "remove", i.name, dev); err == nil {
			continue
		}
		if out, err := i.cli(ctx, "config", "device", "add", i.name, dev, "none"); err != nil {
			return fmt.Errorf("sandbox incus: device mask %s: %w: %s", dev, err, out)
		}
	}
	return nil
}

func (i *Incus) mountTmpfs(ctx context.Context) error {
	if out, err := i.cli(ctx, "config", "set", i.name, "raw.lxc", i.mountEntries()); err != nil {
		return fmt.Errorf("sandbox incus: raw.lxc: %w: %s", err, out)
	}
	if out, err := i.cli(ctx, "restart", "--force", i.name); err != nil {
		return fmt.Errorf("sandbox incus: restart: %w: %s", err, out)
	}
	return nil
}

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
	if execErr != nil {
		return fmt.Errorf("%w: %s", execErr, stderr.Bytes())
	}
	if tarErr != nil {
		return fmt.Errorf("tar: %w", tarErr)
	}
	return nil
}

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

func (i *Incus) killAndCollectLogs(ctx context.Context) {
	i.killed = true
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _ = i.cli(ctx, "stop", "--force", i.name)
	if out, err := i.cli(ctx, "info", i.name, "--show-log"); err == nil {
		i.logs = out
	}
}

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

func (i *Incus) CPSnapshot(ctx context.Context, timeout time.Duration) error {
	script := "tar --sparse -C " + containerWork + " -cf " + containerScratch + "/snapshot.tar ."
	if _, err := i.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("sandbox incus: cp-r snapshot: %w", err)
	}
	return nil
}

func (i *Incus) CPRestore(ctx context.Context, timeout time.Duration) error {
	script := "find " + containerWork + " -mindepth 1 -delete && " +
		"tar -C " + containerWork + " -xf " + containerScratch + "/snapshot.tar"
	if _, err := i.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("sandbox incus: cp-r restore: %w", err)
	}
	return nil
}

func (i *Incus) PerfectSnapshot(ctx context.Context, timeout time.Duration) error {
	script := "tar -C " + containerWork + " -cf " + containerScratch + "/perfect-work.tar . && " +
		"tar -C " + containerHome + " -cf " + containerScratch + "/perfect-home.tar ."
	if _, err := i.Exec(ctx, script, timeout); err != nil {
		return fmt.Errorf("sandbox incus: perfect snapshot: %w", err)
	}
	return nil
}

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

func (i *Incus) SnapshotBytes(ctx context.Context, baseline string) (int64, error) {
	if baseline == "none" {
		return 0, nil
	}
	return i.duBytes(ctx, containerScratch)
}

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
