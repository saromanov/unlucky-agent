// Package shim builds shell scripts that synchronize concurrent scenario
// branches on files and advisory locks.
package shim

import (
	"fmt"
	"regexp"
	"time"
)

// Dir returns the barrier directory relative to the sandbox work tree.
func Dir() string { return ".barriers" }

const pollInterval = "0.02"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidName reports whether a barrier name is safe to embed in a script.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// SignalScript returns a script that creates or refreshes the barrier marker.
func SignalScript(name string) string {
	if !ValidName(name) {
		return invalidScript
	}

	return fmt.Sprintf("mkdir -p %s && : > %s", Dir(), marker(name))
}

// WaitScript returns a script that waits for the barrier marker file.
func WaitScript(name string, timeout time.Duration) string {
	if !ValidName(name) {
		return invalidScript
	}

	return fmt.Sprintf(`mkdir -p %s
i=0
while [ ! -e %s ]; do
  i=$((i+1))
  if [ $i -ge %d ]; then
    echo "barrier %s: wait timed out after %s" >&2
    exit 1
  fi
  sleep %s
done`, Dir(), marker(name), polls(timeout), name, timeout, pollInterval)
}

// WaitHeldScript returns a script that waits until the barrier marker exists
// and its advisory lock is held by another process.
func WaitHeldScript(name string, timeout time.Duration) string {
	if !ValidName(name) {
		return invalidScript
	}

	return fmt.Sprintf(`mkdir -p %s
i=0
while :; do
  if [ -e %s ] && ! flock -n %s -c :; then
    exit 0
  fi
  i=$((i+1))
  if [ $i -ge %d ]; then
    echo "barrier %s: lock is not held after %s" >&2
    exit 1
  fi
  sleep %s
done`, Dir(), marker(name), marker(name), polls(timeout), name, timeout, pollInterval)
}

// WaitFreeScript returns a script that waits until the barrier lock can be
// acquired. A missing marker file counts as free.
func WaitFreeScript(name string, timeout time.Duration) string {
	if !ValidName(name) {
		return invalidScript
	}

	return fmt.Sprintf(`if [ ! -e %s ]; then
  exit 0
fi
flock -w %d %s -c :`, marker(name), flockWait(timeout), marker(name))
}

const invalidScript = "echo 'shim: invalid barrier name' >&2; exit 1"

func marker(name string) string { return Dir() + "/" + name }

// polls converts a timeout into the number of 20 ms poll iterations.
func polls(timeout time.Duration) int {
	n := int(timeout / (20 * time.Millisecond))
	if n < 1 {
		return 1
	}

	return n
}

// flockWait converts a timeout into whole seconds accepted by flock -w.
func flockWait(timeout time.Duration) int {
	s := int(timeout / time.Second)
	if timeout%time.Second > 0 {
		s++
	}

	if s < 1 {
		return 1
	}

	return s
}
