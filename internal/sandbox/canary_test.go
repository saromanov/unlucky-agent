package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanaryDetectsChanges(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}

	c, err := NewCanary(dirs)
	if err != nil {
		t.Fatal(err)
	}

	if len(c.Paths()) != len(dirs) {
		t.Fatalf("expected %d markers, got %v", len(dirs), c.Paths())
	}

	if err := c.Verify(); err != nil {
		t.Fatalf("fresh canary must pass: %v", err)
	}

	c.Close()

	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, canaryFileName)); !os.IsNotExist(err) {
			t.Fatalf("marker in %s must be removed, err=%v", dir, err)
		}
	}
}

func TestCanaryReportsTamper(t *testing.T) {
	c, err := NewCanary([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	defer c.Close()

	path := c.Paths()[0]
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = c.Verify()
	if err == nil {
		t.Fatal("tampered marker must fail the canary")
	}

	if !strings.Contains(err.Error(), CanaryMarker) {
		t.Fatalf("error must name %s: %v", CanaryMarker, err)
	}
}

func TestCanaryReportsMissing(t *testing.T) {
	c, err := NewCanary([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	defer c.Close()

	if err := os.Remove(c.Paths()[0]); err != nil {
		t.Fatal(err)
	}

	err = c.Verify()
	if err == nil {
		t.Fatal("missing marker must fail the canary")
	}

	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error must report a missing marker: %v", err)
	}
}

func TestCanaryRequiresWritableDir(t *testing.T) {
	if _, err := NewCanary([]string{"/nonexistent-dir-xyz"}); err == nil {
		t.Fatal("a canary without a writable marker must fail")
	}
}
