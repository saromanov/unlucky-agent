// Package runner executes a scenario and collects the result.
package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/saromanov/unlucky-agent/internal/baseline"
	"github.com/saromanov/unlucky-agent/internal/fixture"
	"github.com/saromanov/unlucky-agent/internal/manifest"
	"github.com/saromanov/unlucky-agent/internal/oracle"
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
}

// Result is the outcome of a single run.
type Result struct {
	ScenarioID     string            `json:"scenario_id"`
	Title          string            `json:"title"`
	Baseline       string            `json:"baseline"`
	RestoreCmd     string            `json:"restore_cmd,omitempty"`
	Verdict        string            `json:"verdict"`
	Reason         string            `json:"reason,omitempty"`
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

// Run executes a scenario locally in a temporary directory.
//
// v0.1 does not isolate the process from the host filesystem at the kernel
// level: damage commands only touch paths inside the temporary directory.
// Container isolation lands in M1 (see docs/ARCHITECTURE.md).
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
		if _, err := manifest.Capture([]manifest.Root{
			{Name: "work", Dir: work},
			{Name: "home", Dir: home},
		}, f); err != nil {
			f.Close()
			return "", err
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

	var snap string
	if cfg.Baseline == "cp-r" {
		snap = filepath.Join(root, "snapshot")
		if err := baseline.Snapshot(work, snap); err != nil {
			return nil, fmt.Errorf("baseline snapshot: %w", err)
		}
	}

	start := time.Now()
	if err := runDamage(ctx, cfg.Scenario, work, home, cfg.Timeout); err != nil {
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
		if err := shell(ctx, work, home, cfg.RestoreCmd, cfg.Timeout); err != nil {
			res.Verdict = VerdictError
			res.Reason = "restore: " + err.Error()
			return res, nil
		}
	case cfg.Baseline == "cp-r":
		if err := baseline.RestoreExact(snap, work); err != nil {
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

func runDamage(ctx context.Context, s *scenario.Scenario, work, home string, timeout time.Duration) error {
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
			if err := shell(ctx, work, home, step.Shell, stepTimeout); err != nil {
				return fmt.Errorf("damage[%d]: %w", i, err)
			}
		case scenario.StepPostEdit:
			if err := shell(ctx, work, home, step.PostEdit, stepTimeout); err != nil {
				return fmt.Errorf("damage[%d] post_edit: %w", i, err)
			}
		case scenario.StepToolCall:
			cmd := step.ToolCall.Args["cmd"]
			if cmd == "" {
				return fmt.Errorf("damage[%d]: toolcall without args.cmd is not supported yet", i)
			}
			if err := shell(ctx, work, home, cmd, stepTimeout); err != nil {
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

func shell(ctx context.Context, work, home, script string, timeout time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", script)
	cmd.Dir = work
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"TZ=UTC",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
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
