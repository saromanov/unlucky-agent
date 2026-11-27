package shim

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestValidName(t *testing.T) {
	valid := []string{"a", "A1", "x_y.z-1", "0", "writer.lock"}
	for _, name := range valid {
		if !ValidName(name) {
			t.Fatalf("%q must be valid", name)
		}
	}

	invalid := []string{"", ".hidden", "-x", "a b", "a/b", "a;b", "a$(id)", "café"}
	for _, name := range invalid {
		if ValidName(name) {
			t.Fatalf("%q must be rejected", name)
		}
	}
}

func runScript(t *testing.T, dir, script string) ([]byte, error) {
	t.Helper()

	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir

	return cmd.CombinedOutput()
}

func TestSignalThenWait(t *testing.T) {
	dir := t.TempDir()
	if out, err := runScript(t, dir, SignalScript("go")); err != nil {
		t.Fatalf("signal: %v: %s", err, out)
	}

	if _, err := os.Stat(filepath.Join(dir, Dir(), "go")); err != nil {
		t.Fatalf("marker is not created: %v", err)
	}

	start := time.Now()

	if out, err := runScript(t, dir, WaitScript("go", 2*time.Second)); err != nil {
		t.Fatalf("wait: %v: %s", err, out)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait must return as soon as the marker exists, took %s", elapsed)
	}
}

func TestWaitTimesOut(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()

	if out, err := runScript(t, dir, WaitScript("never", 100*time.Millisecond)); err == nil {
		t.Fatalf("wait must time out, got: %s", out)
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout must be bounded, took %s", elapsed)
	}
}

func TestWaitFreeWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	if out, err := runScript(t, dir, WaitFreeScript("absent", time.Second)); err != nil {
		t.Fatalf("a missing marker counts as free: %v: %s", err, out)
	}
}

func TestInvalidNameIsRejected(t *testing.T) {
	dir := t.TempDir()

	out, err := runScript(t, dir, SignalScript("bad;name"))
	if err == nil {
		t.Fatalf("an invalid name must fail the script, got: %s", out)
	}

	if _, err := os.Stat(filepath.Join(dir, Dir())); err == nil {
		t.Fatal("an invalid name must not create the barrier directory")
	}
}

func TestHelperHoldFlock(t *testing.T) {
	path := os.Getenv("SHIM_HELPER_FLOCK")
	if path == "" {
		t.Skip("helper process")
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	fmt.Println("held")
	time.Sleep(30 * time.Second)
}

func TestWaitHeldAndWaitFree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir()), 0o755); err != nil {
		t.Fatal(err)
	}

	lock := filepath.Join(dir, Dir(), "w.lock")

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHoldFlock")

	cmd.Env = append(os.Environ(), "SHIM_HELPER_FLOCK="+lock)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	waited := false
	cleanup := func() {
		if waited {
			return
		}

		waited = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	t.Cleanup(cleanup)

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper did not report the lock: %v", err)
	}

	if line != "held\n" {
		t.Fatalf("unexpected helper output %q", line)
	}

	if out, err := runScript(t, dir, WaitHeldScript("w.lock", 2*time.Second)); err != nil {
		t.Fatalf("wait-held must succeed: %v: %s", err, out)
	}

	if out, err := runScript(t, dir, WaitFreeScript("w.lock", 100*time.Millisecond)); err == nil {
		t.Fatalf("wait-free must time out while the lock is held, got: %s", out)
	}

	cleanup()

	if out, err := runScript(t, dir, WaitFreeScript("w.lock", 2*time.Second)); err != nil {
		t.Fatalf("wait-free must succeed after the holder exits: %v: %s", err, out)
	}
}
