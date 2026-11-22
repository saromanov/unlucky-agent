// Package runner executes a scenario and collects the result.
package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/saromanov/unlucky-agent/internal/baseline"
	"github.com/saromanov/unlucky-agent/internal/fixture"
	"github.com/saromanov/unlucky-agent/internal/manifest"
	"github.com/saromanov/unlucky-agent/internal/oracle"
	"github.com/saromanov/unlucky-agent/internal/sandbox"
	"github.com/saromanov/unlucky-agent/internal/scenario"
)

// Verdicts.
const (
	VerdictPass    = "PASS"
	VerdictFail    = "FAIL"
	VerdictError   = "ERROR"
	VerdictSkipped = "SKIPPED_UNSUPPORTED"
)

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

	start = time.Now()
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
	default:
		// baseline none: no rollback is performed.
	}
	res.RestoreMS = time.Since(start).Milliseconds()

	restoredManifest, err := capture("restored")
	if err != nil {
		return nil, err
	}

	pol := oracle.Policy{
		Exclude:     cfg.Scenario.Expect.Exclude,
		Preserve:    cfg.Scenario.Expect.Preserve,
		AllowAdded:  []string{".git/objects/**", ".git/index", ".git/logs/**", ".git/lost-found/**"},
		Expected:    cfg.Scenario.Expect.Restore,
		RequireMeta: cfg.Scenario.Expect.Require == "meta",
	}
	diff, err := oracle.DiffFiles(baseManifest, restoredManifest, pol)
	if err != nil {
		return nil, err
	}
	res.Diff = diff
	res.Recall = diff.Recall()
	res.MetaScore = diff.MetaScore()

	if postEditManifest != "" {
		clobbered, err := oracle.Clobber(postEditManifest, restoredManifest, pol)
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
		stepTimeout := timeout
		if step.TimeoutS > 0 {
			stepTimeout = time.Duration(step.TimeoutS) * time.Second
		}
		switch kind {
		case scenario.StepShell:
			if _, err := drv.Exec(ctx, step.Shell, stepTimeout); err != nil {
				return fmt.Errorf("damage[%d]: %w", i, err)
			}
		case scenario.StepPostEdit:
			if _, err := drv.Exec(ctx, step.PostEdit, stepTimeout); err != nil {
				return fmt.Errorf("damage[%d] post_edit: %w", i, err)
			}
		case scenario.StepToolCall:
			cmd := step.ToolCall.Args["cmd"]
			if cmd == "" {
				return fmt.Errorf("damage[%d]: toolcall without args.cmd is not supported yet", i)
			}
			if _, err := drv.Exec(ctx, cmd, stepTimeout); err != nil {
				return fmt.Errorf("damage[%d] toolcall: %w", i, err)
			}
		case scenario.StepBarrier:
			return fmt.Errorf("damage[%d]: barrier requires a two-process sandbox and is unavailable in v0.1", i)
		}
	}
	return nil
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
