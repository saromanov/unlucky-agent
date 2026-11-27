package sandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Canary is a set of marker files on the host with known content. A run
// fails loudly when a marker changes or disappears: that means scenario
// commands escaped the sandbox and touched the host filesystem.
type Canary struct {
	files map[string]string // path -> sha256
}

// canaryFileName marks every canary file.
const canaryFileName = ".unlucky-agent-canary"

// NewCanary writes a marker file into each directory and records its hash.
// A directory that cannot be written (read-only $HOME, for example) is
// skipped: the canary protects, it does not fail the run on setup.
func NewCanary(dirs []string) (*Canary, error) {
	c := &Canary{files: map[string]string{}}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}

	for _, dir := range dirs {
		p := filepath.Join(dir, canaryFileName)
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			continue
		}

		sum, err := fileHash(p)
		if err != nil {
			os.Remove(p)

			return nil, fmt.Errorf("canary: %w", err)
		}

		c.files[p] = sum
	}

	if len(c.files) == 0 {
		return nil, fmt.Errorf("canary: no marker could be written in %v", dirs)
	}

	return c, nil
}

// Verify re-hashes every marker and reports changes. The returned error is
// prefixed with CANARY_FAILED so the report names the failure class.
func (c *Canary) Verify() error {
	var fails []string

	for p, want := range c.files {
		got, err := fileHash(p)

		switch {
		case os.IsNotExist(err):
			fails = append(fails, "missing "+p)
		case err != nil:
			fails = append(fails, fmt.Sprintf("unreadable %s: %v", p, err))
		case got != want:
			fails = append(fails, "changed "+p)
		}
	}

	if len(fails) > 0 {
		return fmt.Errorf("%s: %s", CanaryMarker, join(fails, "; "))
	}

	return nil
}

// Close removes the marker files.
func (c *Canary) Close() {
	for p := range c.files {
		os.Remove(p)
	}
}

// Paths returns the marker locations, sorted for determinism.
func (c *Canary) Paths() []string {
	out := make([]string, 0, len(c.files))
	for p := range c.files {
		out = append(out, p)
	}

	sort.Strings(out)

	return out
}

func fileHash(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:]), nil
}
