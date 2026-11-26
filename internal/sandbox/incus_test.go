package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// skipWithoutIncus gates the integration tests: they need a running Incus
// daemon and network access to pull the default image.
func skipWithoutIncus(t *testing.T) {
	t.Helper()
	if os.Getenv("UNLUCKY_INCUS_TESTS") != "1" {
		t.Skip("set UNLUCKY_INCUS_TESTS=1")
	}
	if err := exec.Command("incus", "info").Run(); err != nil {
		t.Skip("incus unavailable")
	}
}

type fakeIncus struct {
	bin   string
	calls string
}

func (f *fakeIncus) output(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.calls)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fakeIncus) driver(t *testing.T, cfg IncusConfig) *Incus {
	t.Helper()
	cfg.IncusBin = f.bin
	if cfg.EvidenceDir == "" {
		cfg.EvidenceDir = t.TempDir()
	}
	d, err := NewIncus(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newFakeIncus(t *testing.T, mountinfo string) *fakeIncus {
	t.Helper()
	dir := t.TempDir()
	f := &fakeIncus{bin: filepath.Join(dir, "incus"), calls: filepath.Join(dir, "calls.log")}
	mountPath := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(mountPath, []byte(mountinfo), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + f.calls + `'
case "$1" in
exec)
  case "$*" in
  *"tar --sparse"*)
    cat >/dev/null
    ;;
  *mountinfo-begin*)
    cat '` + mountPath + `'
    ;;
  *"du -sk"*)
    printf '12\t/sandbox/work\n'
    ;;
  *)
    printf 'exec ok\n'
    ;;
  esac
  ;;
config)
  case "$3" in
  list)
    printf 'eth0,nic\nroot,disk\n'
    ;;
  remove)
    if [ -n "$FAKE_INCUS_REMOVE_FAIL" ]; then exit 1; fi
    ;;
  esac
  ;;
info)
  printf 'container log line\n'
  ;;
esac
exit 0
`
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func newSlowFakeIncus(t *testing.T) *fakeIncus {
	t.Helper()
	dir := t.TempDir()
	f := &fakeIncus{bin: filepath.Join(dir, "incus"), calls: filepath.Join(dir, "calls.log")}
	stopFlag := filepath.Join(dir, "stopped")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + f.calls + `'
case "$1" in
exec)
  i=0
  while [ "$i" -lt 500 ]; do
    if [ -e '` + stopFlag + `' ]; then exit 137; fi
    sleep 0.02
    i=$((i+1))
  done
  exit 137
  ;;
stop)
  : > '` + stopFlag + `'
  ;;
info)
  printf 'container log line\n'
  ;;
esac
exit 0
`
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func incusMounts() []string {
	return []string{
		"100 99 0:50 / / rw,relatime - ext4 /dev/vda1 rw",
		"101 100 0:51 / /proc rw - proc proc rw",
		"102 100 0:52 / /proc/cpuinfo rw - fuse.lxcfs lxcfs rw",
		"103 100 0:54 / /dev rw - tmpfs tmpfs rw",
		"104 100 0:56 / /sandbox/work rw,nosuid - tmpfs tmpfs rw,size=64m",
		"105 100 0:57 / /sandbox/home rw,nosuid,noexec - tmpfs tmpfs rw,size=64m",
		"106 100 0:58 / /sandbox/scratch rw,nosuid - tmpfs tmpfs rw,size=256m",
		"107 100 0:59 / /sandbox/evidence rw - ext4 /dev/vdb1 rw",
		"108 100 0:60 / /etc/hosts rw - ext4 /dev/vda1 rw",
	}
}

func incusGuardOutput(mounts []string) string {
	return "uid=1000\nhome=/sandbox/home\nlinks=lo \nmountinfo-begin\n" +
		strings.Join(mounts, "\n") + "\nmountinfo-end\n"
}

func TestParseIncusGuard(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantSub string
	}{
		{name: "happy", out: incusGuardOutput(incusMounts())},
		{
			name:    "root uid",
			out:     strings.Replace(incusGuardOutput(incusMounts()), "uid=1000", "uid=0", 1),
			wantSub: "uid is",
		},
		{
			name:    "host home",
			out:     strings.Replace(incusGuardOutput(incusMounts()), "home=/sandbox/home", "home=/root", 1),
			wantSub: "HOME",
		},
		{
			name:    "no lxcfs marker",
			out:     incusGuardOutput(incusMounts()[0:2]),
			wantSub: "fuse.lxcfs",
		},
		{
			name: "work is not tmpfs",
			out: incusGuardOutput(append(incusMounts()[0:4],
				"104 100 0:56 / /sandbox/work rw,nosuid - ext4 /dev/vda1 rw")),
			wantSub: "want tmpfs",
		},
		{
			name: "home is not mounted",
			out: incusGuardOutput(append(incusMounts()[0:4],
				incusMounts()[6:]...)),
			wantSub: "not mounted",
		},
		{
			name: "host bind",
			out: incusGuardOutput(append(incusMounts(),
				"109 100 0:61 / /hijack rw - ext4 /dev/sdb1 rw")),
			wantSub: "disk-backed mount",
		},
		{
			name: "host home bind",
			out: incusGuardOutput(append(incusMounts(),
				"109 100 0:61 / /home rw - ext4 /dev/sdb1 rw")),
			wantSub: "disk-backed mount",
		},
		{
			name:    "network link present",
			out:     strings.Replace(incusGuardOutput(incusMounts()), "links=lo ", "links=lo eth0 ", 1),
			wantSub: `network interface "eth0" present, want lo only`,
		},
		{
			name:    "network links missing",
			out:     strings.Replace(incusGuardOutput(incusMounts()), "links=lo \n", "", 1),
			wantSub: "network interface list missing",
		},
		{
			name: "host tmpfs bind",
			out: incusGuardOutput(append(incusMounts(),
				"109 100 0:61 / /tmp rw - tmpfs tmpfs rw,size=1024m")),
			wantSub: `tmpfs mount "/tmp" outside sandbox layout`,
		},
		{
			name: "fusectl and binfmt_misc mounts",
			out: incusGuardOutput(append(incusMounts(),
				"109 100 0:61 / /sys/fs/fuse/connections rw - fusectl fusectl rw",
				"110 100 0:62 / /proc/sys/fs/binfmt_misc rw - binfmt_misc binfmt_misc rw")),
		},
		{
			name: "runtime tmpfs mounts",
			out: incusGuardOutput(append(incusMounts(),
				"109 100 0:61 / /dev/shm rw,nosuid,nodev - tmpfs tmpfs rw",
				"110 100 0:62 / /run rw,nosuid,nodev - tmpfs tmpfs rw",
				"111 100 0:63 / /run/lock rw,nosuid,nodev,noexec - tmpfs tmpfs rw")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fails := parseIncusGuard(tt.out)
			if tt.wantSub == "" {
				if len(fails) != 0 {
					t.Fatalf("guard must pass, got %v", fails)
				}
				return
			}
			if len(fails) == 0 {
				t.Fatalf("guard must fail, want %q", tt.wantSub)
			}
			joined := strings.Join(fails, "; ")
			if !strings.Contains(joined, tt.wantSub) {
				t.Fatalf("fail mismatch: got %q, want substring %q", joined, tt.wantSub)
			}
		})
	}
}

func TestIncusGuardAllowsRootFSAndDockerStyleFiles(t *testing.T) {
	mounts := incusMounts()
	fails := checkIncusMountinfo(mounts)
	if len(fails) != 0 {
		t.Fatalf("rootfs, tmpfs trees and evidence must pass, got %v", fails)
	}
}

func TestIncusGuardProbe(t *testing.T) {
	f := newFakeIncus(t, incusGuardOutput(incusMounts()))
	d := f.driver(t, IncusConfig{})
	if err := d.Guard(context.Background()); err != nil {
		t.Fatalf("guard must pass: %v", err)
	}
}

func TestIncusGuardProbeRejectsMissingLxcfs(t *testing.T) {
	f := newFakeIncus(t, incusGuardOutput(incusMounts()[0:2]))
	d := f.driver(t, IncusConfig{})
	err := d.Guard(context.Background())
	if err == nil {
		t.Fatal("guard must reject a container without lxcfs")
	}
	if !errors.Is(err, ErrGuardFailed) {
		t.Fatalf("error must wrap ErrGuardFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "fuse.lxcfs") {
		t.Fatalf("unexpected guard error: %v", err)
	}
}

func TestNewIncusRequiresBinary(t *testing.T) {
	_, err := NewIncus(IncusConfig{
		EvidenceDir: t.TempDir(),
		IncusBin:    filepath.Join(t.TempDir(), "missing-incus"),
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing binary must fail with a not found error, got %v", err)
	}
}

func TestIncusPrepareCommands(t *testing.T) {
	f := newFakeIncus(t, incusGuardOutput(incusMounts()))
	d := f.driver(t, IncusConfig{
		MemoryMB:      1024,
		PidsLimit:     2048,
		WorkSizeMB:    4672,
		ScratchSizeMB: 4608,
		ExtraRunArgs:  []string{"security.nesting=true"},
	})
	work, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Prepare(context.Background(), HostDirs{Root: t.TempDir(), Work: work, Home: home}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	out := f.output(t)
	checks := []string{
		"init images:debian/12/cloud unlucky-agent-",
		"config device remove " + d.name + " eth0",
		"config set " + d.name + " limits.memory=1024MiB limits.processes=2048 security.privileged=false security.devlxd=false security.nesting=true",
		"config device add " + d.name + " evidence disk source=" + d.evidence + " path=/sandbox/evidence",
		"start " + d.name,
		"exec " + d.name + " -- mkdir -p /sandbox/work /sandbox/home /sandbox/scratch",
		"config set " + d.name + " raw.lxc lxc.mount.entry = tmpfs sandbox/work tmpfs rw,nosuid,size=4672m,uid=1000,gid=1000,mode=0755,create=dir",
		"lxc.mount.entry = tmpfs sandbox/home tmpfs rw,nosuid,noexec,size=64m,uid=1000,gid=1000,mode=0700,create=dir",
		"lxc.mount.entry = tmpfs sandbox/scratch tmpfs rw,nosuid,size=4608m",
		"restart --force " + d.name,
		"exec " + d.name + " --user 1000 -- tar --sparse -C /sandbox/work -xf -",
		"exec " + d.name + " --user 1000 -- tar --sparse -C /sandbox/home -xf -",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Fatalf("incus calls must contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "remove "+d.name+" root") {
		t.Fatalf("a disk device must not be removed:\n%s", out)
	}
	if !d.started {
		t.Fatal("prepare must mark the driver as started")
	}
}

func TestIncusPrepareRequiresPaths(t *testing.T) {
	f := newFakeIncus(t, incusGuardOutput(incusMounts()))
	d := f.driver(t, IncusConfig{})
	if err := d.Prepare(context.Background(), HostDirs{}); err == nil {
		t.Fatal("prepare without work and home must fail")
	}
}

func TestIncusInheritedNICMask(t *testing.T) {
	t.Setenv("FAKE_INCUS_REMOVE_FAIL", "1")
	f := newFakeIncus(t, incusGuardOutput(incusMounts()))
	d := f.driver(t, IncusConfig{})
	work, home := t.TempDir(), t.TempDir()
	if err := d.Prepare(context.Background(), HostDirs{Root: t.TempDir(), Work: work, Home: home}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	out := f.output(t)
	if !strings.Contains(out, "config device add "+d.name+" eth0 none") {
		t.Fatalf("an inherited nic must be masked with a none device:\n%s", out)
	}
}

func TestIncusExec(t *testing.T) {
	f := newFakeIncus(t, incusGuardOutput(incusMounts()))
	d := f.driver(t, IncusConfig{})
	out, err := d.Exec(context.Background(), "printf hi", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(string(out), "exec ok") {
		t.Fatalf("exec output %q does not contain the fake marker", out)
	}
	if !strings.Contains(f.output(t), "--user 1000 --cwd /sandbox/work") {
		t.Fatalf("exec must run as uid 1000 in the work tree:\n%s", f.output(t))
	}
}

func TestIncusExecTimeout(t *testing.T) {
	f := newSlowFakeIncus(t)
	d := f.driver(t, IncusConfig{})
	_, err := d.Exec(context.Background(), "sleep 5", 100*time.Millisecond)
	if err == nil {
		t.Fatal("exec must time out")
	}
	msg := err.Error()
	if !strings.Contains(msg, "timeout after") || !strings.Contains(msg, "container log line") {
		t.Fatalf("timeout error must carry the logs: %v", err)
	}
	out := f.output(t)
	if !strings.Contains(out, "stop --force "+d.name) {
		t.Fatalf("timeout must stop the container:\n%s", out)
	}
	if _, err := d.Exec(context.Background(), "true", time.Second); err == nil ||
		!strings.Contains(err.Error(), "was killed by a previous timeout") {
		t.Fatalf("exec after a timeout must fail, got %v", err)
	}
}

func TestIncusDuBytes(t *testing.T) {
	f := newFakeIncus(t, incusGuardOutput(incusMounts()))
	d := f.driver(t, IncusConfig{})
	ctx := context.Background()
	got, err := d.SnapshotBytes(ctx, "cp-r")
	if err != nil {
		t.Fatal(err)
	}
	if got != 12*1024 {
		t.Fatalf("snapshot bytes = %d, want %d", got, 12*1024)
	}
	if got, err := d.SnapshotBytes(ctx, "none"); err != nil || got != 0 {
		t.Fatalf("none baseline must be zero, got %d, %v", got, err)
	}
	if got, err := d.DirBytes(ctx, "home"); err != nil || got != 12*1024 {
		t.Fatalf("home bytes = %d, %v", got, err)
	}
	if _, err := d.DirBytes(ctx, "elsewhere"); err == nil {
		t.Fatal("unknown tree must fail")
	}
}

func TestIncusCloseWithoutPrepare(t *testing.T) {
	f := newFakeIncus(t, incusGuardOutput(incusMounts()))
	d := f.driver(t, IncusConfig{})
	if err := d.Close(); err != nil {
		t.Fatalf("close before prepare must be a no-op: %v", err)
	}
}

func stageIncus(t *testing.T, cfg IncusConfig) *Incus {
	t.Helper()
	work, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "h.txt"), []byte("home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg.EvidenceDir == "" {
		cfg.EvidenceDir = t.TempDir()
	}
	d, err := NewIncus(cfg)
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

func TestIncusExecAndGuard(t *testing.T) {
	skipWithoutIncus(t)
	ctx := context.Background()
	d := stageIncus(t, IncusConfig{})
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

func TestIncusPrepareKeepsSparse(t *testing.T) {
	skipWithoutIncus(t)
	work := t.TempDir()
	const size = 8 << 20

	sparse := filepath.Join(work, "sparse.bin")
	fh, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 4<<10)
	for i := range block {
		block[i] = 0x5a
	}
	for _, off := range []int64{0, 4 << 20} {
		if _, err := fh.WriteAt(block, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := fh.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := NewIncus(IncusConfig{EvidenceDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()
	if err := d.Prepare(ctx, HostDirs{Root: t.TempDir(), Work: work, Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	out, err := d.Exec(ctx, "stat -c %b /sandbox/work/sparse.bin", 30*time.Second)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	blocks, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("parse stat output %q: %v", out, err)
	}
	if blocks*512 >= size/10 {
		t.Fatalf("copy-in densified the sparse file: %d blocks", blocks)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
