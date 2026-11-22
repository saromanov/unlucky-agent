// Package sandbox isolates scenario execution from the host.
//
// The runner executes damage and restore commands through a Driver. The
// local driver reproduces the v0.1 behavior: a plain subprocess on the host.
// The docker driver runs every command inside a hardened container, behind a
// pre-flight guard and host canary markers (issue 01).
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

// Verdict-affecting error markers.
const (
	// GuardMarker prefixes guard failures. The run must stop before the
	// first damage step when it appears.
	GuardMarker = "GUARD_FAILED"
	// CanaryMarker prefixes canary failures.
	CanaryMarker = "CANARY_FAILED"
)

// ErrGuardFailed reports that the pre-flight guard refused the run.
var ErrGuardFailed = errors.New(GuardMarker)

// HostDirs are host-side directories prepared by the runner: the fixture
// staging tree and the run output root.
type HostDirs struct {
	Root string
	Work string
	Home string
}

// Driver executes scenario commands inside an isolation boundary.
type Driver interface {
	// Name is the driver id used in reports and CLI flags.
	Name() string

	// Prepare installs the fixture staged in HostDirs into the sandbox.
	// For container drivers this copies the tree in; for the local driver
	// the staging tree is used as is.
	Prepare(ctx context.Context, host HostDirs) error

	// Guard verifies the isolation boundary before the first damage step.
	// A container driver refuses the run when a check fails.
	Guard(ctx context.Context) error

	// Exec runs `sh -c script` with the working directory set to the
	// sandbox work tree and HOME pointing at the sandbox home.
	// A timeout kills the sandbox and returns an error carrying the
	// collected logs.
	Exec(ctx context.Context, script string, timeout time.Duration) ([]byte, error)

	// CaptureRoots materializes the work and home trees for manifest
	// capture on the host. The returned undo function removes the copies.
	// The local driver returns the live directories and a no-op undo.
	CaptureRoots(hostDir string) ([]manifest.Root, func(), error)

	// Close tears the sandbox down.
	Close() error
}

// CPBaseline is implemented by drivers that must run the cp-r baseline
// rollback inside their own sandbox instead of on the host tree.
type CPBaseline interface {
	CPSnapshot(ctx context.Context, timeout time.Duration) error
	CPRestore(ctx context.Context, timeout time.Duration) error
}

// PerfectBaseline is implemented by drivers that must run the perfect
// reference rollback inside their own sandbox instead of on the host tree.
type PerfectBaseline interface {
	PerfectSnapshot(ctx context.Context, timeout time.Duration) error
	PerfectRestore(ctx context.Context, timeout time.Duration, preserve []string) error
}

// GuardFailure converts a list of failed checks into the run error.
func GuardFailure(checks []string) error {
	if len(checks) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrGuardFailed, join(checks, "; "))
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
