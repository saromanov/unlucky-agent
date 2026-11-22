package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

// execCommandContext is a seam for tests.
var execCommandContext = exec.CommandContext

// Local runs scenario commands as host subprocesses. This is the v0.1
// behavior, acceptable only for reviewed scenarios on a throwaway machine.
type Local struct {
	work string
	home string
}

// NewLocal returns a driver that executes commands on the host.
func NewLocal() *Local { return &Local{} }

func (l *Local) Name() string { return "local" }

// Prepare records the fixture staging tree as the execution target.
func (l *Local) Prepare(_ context.Context, host HostDirs) error {
	if host.Work == "" || host.Home == "" {
		return fmt.Errorf("sandbox local: work and home must be set")
	}
	l.work = host.Work
	l.home = host.Home
	return nil
}

// Guard is a no-op: there is no isolation boundary to verify.
func (l *Local) Guard(_ context.Context) error { return nil }

// Exec runs the script as a host subprocess inside the staging work tree.
func (l *Local) Exec(ctx context.Context, script string, timeout time.Duration) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := execCommandContext(cctx, "sh", "-c", script)
	cmd.Dir = l.work
	cmd.Env = []string{
		"HOME=" + l.home,
		"PATH=" + os.Getenv("PATH"),
		"TZ=UTC",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, out)
	}
	return out, nil
}

// CaptureRoots returns the live trees: no copy is needed on the host.
func (l *Local) CaptureRoots(_ string) ([]manifest.Root, func(), error) {
	if l.work == "" {
		return nil, nil, fmt.Errorf("sandbox local: Prepare was not called")
	}
	roots := []manifest.Root{
		{Name: "work", Dir: l.work},
		{Name: "home", Dir: l.home},
	}
	return roots, func() {}, nil
}

// Close is a no-op for the local driver.
func (l *Local) Close() error { return nil }
