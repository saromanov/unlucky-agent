// Package runner executes a scenario and collects the result.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saromanov/unlucky-agent/internal/baseline"
	"github.com/saromanov/unlucky-agent/internal/fixture"
	"github.com/saromanov/unlucky-agent/internal/manifest"
	"github.com/saromanov/unlucky-agent/internal/oracle"
	"github.com/saromanov/unlucky-agent/internal/sandbox"
	"github.com/saromanov/unlucky-agent/internal/scenario"
	"github.com/saromanov/unlucky-agent/internal/shim"
)

// Verdicts.
const (
	VerdictPass    = "PASS"
	VerdictFail    = "FAIL"
	VerdictError   = "ERROR"
	VerdictSkipped = "SKIPPED_UNSUPPORTED"
	VerdictFlaky   = "FLAKY"
)

// writerLock is the barrier the concurrent writer must hold.
const writerLock = "writer.lock"

// Config holds run parameters.
type Config struct {
	Scenario   *scenario.Scenario
	Baseline   string
	RestoreCmd string
	OutDir     string
	Keep       bool
	Timeout    time.Duration
	// Driver isolates scenario execution. Nil selects the local driver,
	// which keeps the v0.1 behavior.
	Driver sandbox.Driver
}

// Result is the outcome of a single run.
type Result struct {
	ScenarioID     string            `json:"scenario_id"`
	Title          string            `json:"title"`
	Baseline       string            `json:"baseline"`
	RestoreCmd     string            `json:"restore_cmd,omitempty"`
	Verdict        string            `json:"verdict"`
	Reason         string            `json:"reason,omitempty"`
	Sandbox        string            `json:"sandbox,omitempty"`
	DamageDetected bool              `json:"damage_detected"`
	Recall         float64           `json:"recall"`
	MetaScore      float64           `json:"meta_score"`
	Clobbered      int               `json:"clobbered"`
	Diff           *oracle.Result    `json:"diff,omitempty"`
	DamageMS       int64             `json:"damage_ms"`
	RestoreMS      int64             `json:"restore_ms"`
	Root           string            `json:"root,omitempty"`
	Artifacts      map[string]string `json:"artifacts,omitempty"`
	Notes          string            `json:"notes,omitempty"`
}

// Run executes a scenario in a temporary directory.
//
// Commands run through a sandbox driver. The default driver is local, which
// preserves the v0.1 behavior: damage commands only touch paths inside the
// temporary directory. A container driver adds kernel-level isolation and a
// pre-flight guard before the first damage step.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Scenario == nil {
		return nil, fmt.Errorf("runner: scenario is not set")
	}
	if cfg.OutDir == "" {
		return nil, fmt.Errorf("runner: output directory is not set")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.Baseline == "" {
		cfg.Baseline = "none"
	}
	if !validBaseline(cfg.Baseline) {
		return nil, fmt.Errorf("runner: unknown baseline %q", cfg.Baseline)
	}
	// The tar baseline runs GNU tar on the host staging tree; the container
	// driver has its own cp-r implementation.
	if cfg.Baseline == "tar" && cfg.Driver != nil && cfg.Driver.Name() != "local" {
		return nil, fmt.Errorf("runner: baseline tar is host-only, use local sandbox or cp-r")
	}

	root, err := os.MkdirTemp("", "unlucky-"+cfg.Scenario.ID+"-*")
	if err != nil {
		return nil, err
	}
	if !cfg.Keep {
		defer os.RemoveAll(root)
	}
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	if err := fixture.Build(cfg.Scenario.Fixture, cfg.Scenario.Seed, work, home); err != nil {
		return nil, err
	}

	res := &Result{
		ScenarioID: cfg.Scenario.ID,
		Title:      cfg.Scenario.Title,
		Baseline:   cfg.Baseline,
		RestoreCmd: cfg.RestoreCmd,
		Root:       root,
		Artifacts:  map[string]string{},
	}

	drv := cfg.Driver
	if drv == nil {
		drv = sandbox.NewLocal()
	}
	res.Sandbox = drv.Name()
	defer func() { _ = drv.Close() }()
	if err := drv.Prepare(ctx, sandbox.HostDirs{Root: root, Work: work, Home: home}); err != nil {
		return nil, err
	}

	// The guard runs before the first damage step: a failed boundary check
	// must refuse the run.
	if err := drv.Guard(ctx); err != nil {
		res.Verdict = VerdictError
		res.Reason = err.Error()
		return res, nil
	}

	capture := func(name string) (string, error) {
		p := filepath.Join(cfg.OutDir, fmt.Sprintf("%s.%s.jsonl", cfg.Scenario.ID, name))
		if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
			return "", err
		}
		tmp := p + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			return "", err
		}
		hostDir, err := os.MkdirTemp(root, "cap-"+name+"-")
		if err != nil {
			f.Close()
			return "", err
		}
		roots, undo, err := drv.CaptureRoots(hostDir)
		if err != nil {
			f.Close()
			return "", err
		}
		_, capErr := manifest.Capture(roots, f)
		undo()
		if capErr != nil {
			f.Close()
			return "", capErr
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, p); err != nil {
			return "", err
		}
		res.Artifacts[name] = p
		return p, nil
	}

	baseManifest, err := capture("base")
	if err != nil {
		return nil, err
	}

	cpBase, hasCP := drv.(sandbox.CPBaseline)
	perfectBase, hasPerfect := drv.(sandbox.PerfectBaseline)
	var snap string
	if cfg.Baseline == "cp-r" {
		if hasCP {
			if err := cpBase.CPSnapshot(ctx, cfg.Timeout); err != nil {
				return nil, fmt.Errorf("baseline snapshot: %w", err)
			}
		} else {
			snap = filepath.Join(root, "snapshot")
			if err := baseline.Snapshot(work, snap); err != nil {
				return nil, fmt.Errorf("baseline snapshot: %w", err)
			}
		}
	}
	snapWork := filepath.Join(root, "snapshot-work")
	snapHome := filepath.Join(root, "snapshot-home")
	if cfg.Baseline == "perfect" {
		if hasPerfect {
			if err := perfectBase.PerfectSnapshot(ctx, cfg.Timeout); err != nil {
				return nil, fmt.Errorf("baseline snapshot: %w", err)
			}
		} else if err := baseline.PerfectSnapshot(work, home, snapWork, snapHome); err != nil {
			return nil, fmt.Errorf("baseline snapshot: %w", err)
		}
	}
	tarSnap := filepath.Join(root, "snapshot.tar")
	if cfg.Baseline == "tar" {
		if err := baseline.TarSnapshot(work, tarSnap); err != nil {
			return nil, fmt.Errorf("baseline snapshot: %w", err)
		}
	}

	start := time.Now()
	if err := runDamage(ctx, drv, cfg.Scenario, cfg.Timeout); err != nil {
		res.Verdict = VerdictError
		res.Reason = err.Error()
		return res, nil
	}
	res.DamageMS = time.Since(start).Milliseconds()

	damagedManifest, err := capture("damaged")
	if err != nil {
		return nil, err
	}
	damageDiff, err := oracle.DiffFiles(baseManifest, damagedManifest, oracle.Policy{})
	if err != nil {
		return nil, err
	}
	res.DamageDetected = len(damageDiff.Missing)+len(damageDiff.Changed) > 0
	if !res.DamageDetected {
		res.Verdict = VerdictError
		res.Reason = "damage did not change the state (no_damage)"
		return res, nil
	}

	var postEditManifest string
	if hasPostEdit(cfg.Scenario) {
		p, err := capture("post_edit")
		if err != nil {
			return nil, err
		}
		postEditManifest = p
	}

	pol := oracle.Policy{
		Exclude:     cfg.Scenario.Expect.Exclude,
		Preserve:    cfg.Scenario.Expect.Preserve,
		AllowAdded:  []string{shim.Dir() + "/**", ".git/objects/**", ".git/index", ".git/logs/**", ".git/lost-found/**"},
		Expected:    cfg.Scenario.Expect.Restore,
		RequireMeta: cfg.Scenario.Expect.Require == "meta",
	}

	writerDone := make(chan error, 1)
	writerCtx, cancelWriter := context.WithCancel(ctx)
	defer cancelWriter()
	var preRestoreManifest string
	if cfg.Scenario.Restore.ConcurrentWrite != "" {
		script := cfg.Scenario.Restore.ConcurrentWrite
		go func() {
			_, err := drv.Exec(writerCtx, script, cfg.Timeout)
			writerDone <- err
		}()
		waitHeld := shim.WaitHeldScript(writerLock, cfg.Timeout)
		if _, err := drv.Exec(ctx, waitHeld, cfg.Timeout); err != nil {
			res.Verdict = VerdictError
			res.Reason = "concurrent writer did not start: " + err.Error()
			return res, nil
		}
		p, err := capture("pre_restore")
		if err != nil {
			return nil, err
		}
		preRestoreManifest = p
	}

	// The concurrent writer defines the live state: its paths must survive the
	// rollback. A post-edit step takes precedence for preserve resolution.
	preserveSource := postEditManifest
	clobberSource := postEditManifest
	if preRestoreManifest != "" {
		if preserveSource == "" {
			preserveSource = preRestoreManifest
		}
		clobberSource = preRestoreManifest
	}

	// The perfect baseline must keep the concrete post-T edits, so their
	// manifest paths are resolved before the restore step.
	var preserved []string
	if preserveSource != "" {
		entries, err := oracle.LoadEntries(preserveSource)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if pol.Preserved(e.Path) {
				preserved = append(preserved, e.Path)
			}
		}
		sort.Strings(preserved)
	}

	restoreStart := time.Now()
	if cfg.Scenario.Restore.Interrupt != nil {
		if err := runInterruptRestore(ctx, drv, cfg, root, res); err != nil {
			res.Verdict = VerdictError
			res.Reason = err.Error()
			return res, nil
		}
		if res.Verdict != "" {
			return res, nil
		}
	} else {
		switch {
		case cfg.RestoreCmd != "":
			if _, err := drv.Exec(ctx, cfg.RestoreCmd, cfg.Timeout); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore: " + err.Error()
				return res, nil
			}
		case cfg.Baseline == "cp-r":
			if hasCP {
				if err := cpBase.CPRestore(ctx, cfg.Timeout); err != nil {
					res.Verdict = VerdictError
					res.Reason = "restore baseline: " + err.Error()
					return res, nil
				}
			} else if err := baseline.RestoreExact(snap, work); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore baseline: " + err.Error()
				return res, nil
			}
		case cfg.Baseline == "perfect":
			if hasPerfect {
				if err := perfectBase.PerfectRestore(ctx, cfg.Timeout, preserved); err != nil {
					res.Verdict = VerdictError
					res.Reason = "restore baseline: " + err.Error()
					return res, nil
				}
			} else if err := baseline.PerfectRestore(snapWork, snapHome, work, home, preserved); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore baseline: " + err.Error()
				return res, nil
			}
		case cfg.Baseline == "tar":
			if err := baseline.TarRestoreExact(tarSnap, work); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore baseline: " + err.Error()
				return res, nil
			}
		default:
			// baseline none: no rollback is performed.
		}
		res.RestoreMS = time.Since(restoreStart).Milliseconds()
	}

	if cfg.Scenario.Restore.ConcurrentWrite != "" {
		if _, err := drv.Exec(ctx, shim.SignalScript("writer-stop"), cfg.Timeout); err != nil {
			res.Verdict = VerdictError
			res.Reason = "concurrent writer stop: " + err.Error()
			return res, nil
		}
		if err := <-writerDone; err != nil {
			res.Verdict = VerdictError
			res.Reason = "concurrent writer: " + err.Error()
			return res, nil
		}
	}

	restoredManifest, err := capture("restored")
	if err != nil {
		return nil, err
	}

	diff, err := oracle.DiffFiles(baseManifest, restoredManifest, pol)
	if err != nil {
		return nil, err
	}
	res.Diff = diff
	res.Recall = diff.Recall()
	res.MetaScore = diff.MetaScore()

	if clobberSource != "" {
		clobbered, err := oracle.Clobber(clobberSource, restoredManifest, pol)
		if err != nil {
			return nil, err
		}
		diff.Clobbered = clobbered
	}
	res.Clobbered = len(diff.Clobbered)

	if diff.Pass() && (pol.RequireMeta == false || diff.MetaScore() == 1) {
		res.Verdict = VerdictPass
	} else {
		res.Verdict = VerdictFail
		res.Reason = verdictReason(diff)
	}
	return res, nil
}

func validBaseline(name string) bool {
	for _, n := range baseline.Names() {
		if n == name {
			return true
		}
	}
	return false
}

func runDamage(ctx context.Context, drv sandbox.Driver, s *scenario.Scenario, timeout time.Duration) error {
	for i, step := range s.Damage {
		kind, err := step.Kind()
		if err != nil {
			return err
		}
		prefix := fmt.Sprintf("damage[%d]", i)
		if kind == scenario.StepParallel {
			if err := runParallel(ctx, drv, i, step, timeout); err != nil {
				return err
			}
			continue
		}
		if err := execStep(ctx, drv, step, prefix, stepTimeout(step, timeout)); err != nil {
			return err
		}
	}
	return nil
}

// runParallel executes the branches of a parallel group concurrently.
func runParallel(ctx context.Context, drv sandbox.Driver, idx int, step scenario.DamageStep, timeout time.Duration) error {
	gctx, cancel := context.WithTimeout(ctx, stepTimeout(step, timeout))
	defer cancel()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		werr error
	)
	for b, branch := range step.Parallel {
		wg.Add(1)
		go func(b int, branch []scenario.DamageStep) {
			defer wg.Done()
			prefix := fmt.Sprintf("damage[%d] parallel[%d]", idx, b)
			for _, nested := range branch {
				if err := execStep(gctx, drv, nested, prefix, stepTimeout(nested, stepTimeout(step, timeout))); err != nil {
					mu.Lock()
					if werr == nil {
						werr = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}(b, branch)
	}
	wg.Wait()
	return werr
}

// execStep runs one damage step and prefixes its errors with the journal label.
func execStep(ctx context.Context, drv sandbox.Driver, step scenario.DamageStep, prefix string, timeout time.Duration) error {
	kind, err := step.Kind()
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	switch kind {
	case scenario.StepShell:
		if _, err := drv.Exec(ctx, step.Shell, timeout); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
	case scenario.StepPostEdit:
		if _, err := drv.Exec(ctx, step.PostEdit, timeout); err != nil {
			return fmt.Errorf("%s post_edit: %w", prefix, err)
		}
	case scenario.StepToolCall:
		cmd := step.ToolCall.Args["cmd"]
		if cmd == "" {
			return fmt.Errorf("%s: toolcall without args.cmd is not supported yet", prefix)
		}
		if _, err := drv.Exec(ctx, cmd, timeout); err != nil {
			return fmt.Errorf("%s toolcall: %w", prefix, err)
		}
	case scenario.StepBarrier:
		script, err := barrierScript(step.Barrier, timeout)
		if err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		if _, err := drv.Exec(ctx, script, timeout); err != nil {
			return fmt.Errorf("%s barrier: %w", prefix, err)
		}
	case scenario.StepParallel:
		return fmt.Errorf("%s: nested parallel group", prefix)
	}
	return nil
}

func stepTimeout(step scenario.DamageStep, fallback time.Duration) time.Duration {
	if step.TimeoutS > 0 {
		return time.Duration(step.TimeoutS) * time.Second
	}
	return fallback
}

// barrierScript maps a barrier step onto the shim script executed by the driver.
func barrierScript(spec string, timeout time.Duration) (string, error) {
	op, name, err := scenario.ParseBarrier(spec)
	if err != nil {
		return "", err
	}
	switch op {
	case scenario.BarrierSignal:
		return shim.SignalScript(name), nil
	case scenario.BarrierWait:
		return shim.WaitScript(name, timeout), nil
	case scenario.BarrierWaitHeld:
		return shim.WaitHeldScript(name, timeout), nil
	case scenario.BarrierWaitFree:
		return shim.WaitFreeScript(name, timeout), nil
	default:
		return "", fmt.Errorf("unknown barrier op %q", op)
	}
}

// runInterruptRestore kills the first restore pass with SIGKILL and repeats it:
// the verdict records whether a repeated restore is idempotent.
func runInterruptRestore(ctx context.Context, drv sandbox.Driver, cfg Config, root string, res *Result) error {
	spec := cfg.Scenario.Restore.Interrupt
	script := cfg.RestoreCmd
	if script == "" {
		if cfg.Baseline != "tar" || drv.Name() != "local" {
			res.Verdict = VerdictSkipped
			res.Reason = "interrupted restore requires the tar baseline and the local sandbox"
			return nil
		}
		script = "find . -mindepth 1 -maxdepth 1 ! -name " + shim.Dir() + " -exec rm -rf -- {} + && tar -xpf " +
			shellQuote(filepath.Join(root, "snapshot.tar"))
	}
	intr, ok := drv.(sandbox.Interruptible)
	if !ok {
		res.Verdict = VerdictSkipped
		res.Reason = fmt.Sprintf("sandbox %s does not support interruptible exec", drv.Name())
		return nil
	}

	start := time.Now()
	_, err := intr.ExecInterrupt(ctx, script, cfg.Timeout, time.Duration(spec.KillAfterMS)*time.Millisecond)
	first := time.Since(start).Milliseconds()
	if err != nil && !errors.Is(err, sandbox.ErrInterrupted) {
		return fmt.Errorf("restore: %w", err)
	}
	if err != nil {
		res.Notes = err.Error()
	} else {
		res.Notes = "restore finished before the kill signal"
	}
	res.Notes += "; the verdict records whether the tool tolerates a repeated restore after SIGKILL"

	start = time.Now()
	if _, err := drv.Exec(ctx, script, cfg.Timeout); err != nil {
		return fmt.Errorf("restore: second pass: %w", err)
	}
	res.RestoreMS = first + time.Since(start).Milliseconds()
	return nil
}

// shellQuote single-quotes a path for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Aggregate collapses repeat runs of one scenario into a single result:
// identical verdicts pass through; divergent verdicts become FLAKY.
func Aggregate(results []*Result) *Result {
	if len(results) == 0 {
		return nil
	}
	if len(results) == 1 {
		return results[0]
	}
	base := *results[0]
	verdicts := make([]string, 0, len(results))
	diverged := false
	for i, r := range results {
		verdicts = append(verdicts, r.Verdict)
		if i > 0 && r.Verdict != results[0].Verdict {
			diverged = true
		}
	}
	if diverged {
		base.Verdict = VerdictFlaky
		base.Reason = fmt.Sprintf("verdicts diverged over %d runs: %s",
			len(verdicts), strings.Join(verdicts, ", "))
	}
	return &base
}

func hasPostEdit(s *scenario.Scenario) bool {
	for _, step := range s.Damage {
		if step.PostEdit != "" {
			return true
		}
	}
	return false
}

func verdictReason(diff *oracle.Result) string {
	var parts []string
	if n := len(diff.Missing); n > 0 {
		parts = append(parts, fmt.Sprintf("missing %d", n))
	}
	if n := len(diff.Changed); n > 0 {
		parts = append(parts, fmt.Sprintf("changed %d", n))
	}
	if n := len(diff.Meta); n > 0 {
		parts = append(parts, fmt.Sprintf("meta %d", n))
	}
	if n := len(diff.Clobbered); n > 0 {
		parts = append(parts, fmt.Sprintf("clobbered post-T edits: %d", n))
	}
	if len(parts) == 0 {
		return "hard gates not passed"
	}
	return join(parts, "; ")
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
