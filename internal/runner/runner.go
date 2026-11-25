// Package runner executes a scenario and collects the result.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

const (
	checkpointRecoveryPath = "the checkpoint failed; no restore point exists; " +
		"recovery path: fix the cause (for example, free disk space) and retake the checkpoint before editing"
	diskMeasurementRecoveryPath = "the run state is intact; " +
		"recovery path: use a sandbox image with coreutils du installed and rerun the scenario"
	restoreRecoveryPath = "the checkpoint snapshot is intact; " +
		"the restore can be repeated after freeing disk space"
	externalRestoreRecoveryPath = "the restore is an external command; " +
		"its snapshot may have been consumed; recovery path: rerun the tool's checkpoint and restore"
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
	ScenarioID        string            `json:"scenario_id"`
	Title             string            `json:"title"`
	Baseline          string            `json:"baseline"`
	RestoreCmd        string            `json:"restore_cmd,omitempty"`
	Verdict           string            `json:"verdict"`
	Reason            string            `json:"reason,omitempty"`
	RecoveryPath      string            `json:"recovery_path,omitempty"`
	Sandbox           string            `json:"sandbox,omitempty"`
	DamageDetected    bool              `json:"damage_detected"`
	Recall            float64           `json:"recall"`
	MetaScore         float64           `json:"meta_score"`
	Clobbered         int               `json:"clobbered"`
	Diff              *oracle.Result    `json:"diff,omitempty"`
	DamageMS          int64             `json:"damage_ms"`
	RestoreMS         int64             `json:"restore_ms"`
	CheckpointTimesUS []int64           `json:"checkpoint_times_us,omitempty"`
	DiskSnapshotBytes []int64           `json:"disk_snapshot_bytes,omitempty"`
	DiskWorkBytes     int64             `json:"disk_work_bytes,omitempty"`
	PeakDiskBytes     int64             `json:"peak_disk_bytes,omitempty"`
	Steps             []StepTiming      `json:"steps,omitempty"`
	Root              string            `json:"root,omitempty"`
	Artifacts         map[string]string `json:"artifacts,omitempty"`
	Notes             string            `json:"notes,omitempty"`
}

// StepTiming is the wall-clock duration of one damage step execution.
type StepTiming struct {
	Index int    `json:"index"`
	Kind  string `json:"kind"`
	US    int64  `json:"us"`
}

// Percentile returns the nearest-rank percentile of values, p in [0, 100].
// It returns 0 for an empty slice.
func Percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
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

	if err := runPreCheckpoint(ctx, drv, cfg.Scenario, cfg.Timeout); err != nil {
		res.Verdict = VerdictError
		res.Reason = "checkpoint pre-steps: " + err.Error()
		res.RecoveryPath = "no checkpoint was taken: the pre-checkpoint setup failed; " +
			"recovery path: fix the environment and rerun the scenario"
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
		captureFail(res, "base", err)
		return res, nil
	}

	cpBase, hasCP := drv.(sandbox.CPBaseline)
	perfectBase, hasPerfect := drv.(sandbox.PerfectBaseline)
	snap := filepath.Join(root, "snapshot")
	snapWork := filepath.Join(root, "snapshot-work")
	snapHome := filepath.Join(root, "snapshot-home")
	tarSnap := filepath.Join(root, "snapshot.tar")

	checkpoint := func() error {
		switch cfg.Baseline {
		case "cp-r":
			if hasCP {
				if err := cpBase.CPSnapshot(ctx, cfg.Timeout); err != nil {
					return err
				}
			} else if err := baseline.Snapshot(work, snap); err != nil {
				return err
			}
		case "perfect":
			if hasPerfect {
				if err := perfectBase.PerfectSnapshot(ctx, cfg.Timeout); err != nil {
					return err
				}
			} else if err := baseline.PerfectSnapshot(work, home, snapWork, snapHome); err != nil {
				return err
			}
		case "tar":
			if err := baseline.TarSnapshot(work, tarSnap); err != nil {
				return err
			}
		default:
			return nil
		}
		return syncCheckpoint(drv, cfg.Baseline, root)
	}

	diskWork, err := driverDirBytes(ctx, drv, "work", work)
	if err != nil {
		diskFail(res, err)
		return res, nil
	}
	res.DiskWorkBytes = diskWork
	if err := res.noteDisk(ctx, drv, root, work, home, 0); err != nil {
		diskFail(res, err)
		return res, nil
	}
	if cfg.Baseline != "none" {
		for range checkpointRepeat(cfg.Scenario) {
			start := time.Now()
			if err := checkpoint(); err != nil {
				res.Verdict = VerdictError
				res.Reason = "checkpoint: " + err.Error()
				res.RecoveryPath = checkpointRecoveryPath
				return res, nil
			}
			res.CheckpointTimesUS = append(res.CheckpointTimesUS, time.Since(start).Microseconds())
			snapshotBytes, err := snapshotStorageBytes(ctx, drv, cfg.Baseline, root)
			if err != nil {
				diskFail(res, err)
				return res, nil
			}
			res.DiskSnapshotBytes = append(res.DiskSnapshotBytes, snapshotBytes)
			if err := res.noteDisk(ctx, drv, root, work, home, snapshotBytes); err != nil {
				diskFail(res, err)
				return res, nil
			}
		}
	}

	start := time.Now()
	if err := runDamage(ctx, drv, cfg.Scenario, cfg.Timeout, res); err != nil {
		res.Verdict = VerdictError
		res.Reason = err.Error()
		return res, nil
	}
	res.DamageMS = time.Since(start).Milliseconds()
	if err := res.noteDisk(ctx, drv, root, work, home, res.lastSnapshotBytes()); err != nil {
		diskFail(res, err)
		return res, nil
	}

	damagedManifest, err := capture("damaged")
	if err != nil {
		captureFail(res, "damaged", err)
		return res, nil
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
			captureFail(res, "post_edit", err)
			return res, nil
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
			captureFail(res, "pre_restore", err)
			return res, nil
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
			res.RecoveryPath = restoreRecoveryPath
			if cfg.RestoreCmd != "" {
				res.RecoveryPath = externalRestoreRecoveryPath
			}
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
				res.RecoveryPath = externalRestoreRecoveryPath
				return res, nil
			}
		case cfg.Baseline == "cp-r":
			if hasCP {
				if err := cpBase.CPRestore(ctx, cfg.Timeout); err != nil {
					res.Verdict = VerdictError
					res.Reason = "restore baseline: " + err.Error()
					res.RecoveryPath = restoreRecoveryPath
					return res, nil
				}
			} else if err := baseline.RestoreExact(snap, work); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore baseline: " + err.Error()
				res.RecoveryPath = restoreRecoveryPath
				return res, nil
			}
		case cfg.Baseline == "perfect":
			if hasPerfect {
				if err := perfectBase.PerfectRestore(ctx, cfg.Timeout, preserved); err != nil {
					res.Verdict = VerdictError
					res.Reason = "restore baseline: " + err.Error()
					res.RecoveryPath = restoreRecoveryPath
					return res, nil
				}
			} else if err := baseline.PerfectRestore(snapWork, snapHome, work, home, preserved); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore baseline: " + err.Error()
				res.RecoveryPath = restoreRecoveryPath
				return res, nil
			}
		case cfg.Baseline == "tar":
			if err := baseline.TarRestoreExact(tarSnap, work); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore baseline: " + err.Error()
				res.RecoveryPath = restoreRecoveryPath
				return res, nil
			}
		default:
			// baseline none: no rollback is performed.
		}
		if cfg.RestoreCmd != "" || cfg.Baseline != "none" {
			if err := syncRestore(drv, cfg.Baseline, work, home); err != nil {
				res.Verdict = VerdictError
				res.Reason = "restore: " + err.Error()
				res.RecoveryPath = restoreRecoveryPath
				return res, nil
			}
		}
		res.RestoreMS = time.Since(restoreStart).Milliseconds()
	}
	if err := res.noteDisk(ctx, drv, root, work, home, res.lastSnapshotBytes()); err != nil {
		diskFail(res, err)
		return res, nil
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
		captureFail(res, "restored", err)
		return res, nil
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

func captureFail(res *Result, name string, err error) {
	res.Verdict = VerdictError
	res.Reason = "manifest capture " + name + ": " + err.Error()
	res.RecoveryPath = "the run state could not be recorded; " +
		"recovery path: free disk space for the output directory and rerun the scenario"
}

func validBaseline(name string) bool {
	for _, n := range baseline.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// treeBytes returns the allocated size of regular files under path, the sum of
// st_blocks*512. A missing path counts as zero.
func treeBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		}
		return nil
	})
	return total
}

// snapshotAreas returns the host paths that hold the checkpoint state.
func snapshotAreas(baselineName, root string) []string {
	switch baselineName {
	case "cp-r":
		return []string{filepath.Join(root, "snapshot")}
	case "perfect":
		return []string{filepath.Join(root, "snapshot-work"), filepath.Join(root, "snapshot-home")}
	case "tar":
		return []string{filepath.Join(root, "snapshot.tar")}
	default:
		return nil
	}
}

// snapshotStorageBytes reports the allocated size of the checkpoint state.
func snapshotStorageBytes(ctx context.Context, drv sandbox.Driver, baselineName, root string) (int64, error) {
	if sizer, ok := drv.(sandbox.SnapshotSizer); ok {
		return sizer.SnapshotBytes(ctx, baselineName)
	}
	var total int64
	for _, p := range snapshotAreas(baselineName, root) {
		total += treeBytes(p)
	}
	return total, nil
}

// driverDirBytes reports the allocated size of a scenario tree: the sandbox
// tree for a StorageSizer driver, the host staging tree otherwise.
func driverDirBytes(ctx context.Context, drv sandbox.Driver, name, hostDir string) (int64, error) {
	if sizer, ok := drv.(sandbox.StorageSizer); ok {
		return sizer.DirBytes(ctx, name)
	}
	return treeBytes(hostDir), nil
}

// syncCheckpoint flushes the checkpoint state: the checkpoint is not complete
// until its data is durable, and consecutive measurements must not depend on
// leftover writeback. Only the local driver needs the flush: container storage
// is tmpfs, which has no writeback to wait for and nothing durable to keep.
func syncCheckpoint(drv sandbox.Driver, baselineName, root string) error {
	if drv.Name() != "local" {
		return nil
	}
	if err := syncPaths(snapshotAreas(baselineName, root)); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// syncRestore flushes the restored trees; perfect restores both work and home.
// It follows the same local-only rule as syncCheckpoint.
func syncRestore(drv sandbox.Driver, baselineName, work, home string) error {
	if drv.Name() != "local" {
		return nil
	}
	areas := []string{work}
	if baselineName == "perfect" {
		areas = append(areas, home)
	}
	if err := syncPaths(areas); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// syncPaths fsyncs every regular file and directory under paths. A missing
// path is skipped.
func syncPaths(paths []string) error {
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if info.Mode().IsRegular() {
			if err := syncFile(p); err != nil {
				return err
			}
			return syncDir(filepath.Dir(p))
		}
		if !info.IsDir() {
			continue
		}
		if err := filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return syncDir(q)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			return syncFile(q)
		}); err != nil {
			return err
		}
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// lastSnapshotBytes returns the snapshot size of the last checkpoint.
func (r *Result) lastSnapshotBytes() int64 {
	if len(r.DiskSnapshotBytes) == 0 {
		return 0
	}
	return r.DiskSnapshotBytes[len(r.DiskSnapshotBytes)-1]
}

// noteDisk samples the run disk and keeps the peak allocated size. The sample
// is the work tree plus home plus the checkpoint state. For the local driver
// the whole run root is walked: it already holds the snapshot areas. A sandbox
// driver reports each tree itself, so the host staging copies are not mixed in.
func (r *Result) noteDisk(ctx context.Context, drv sandbox.Driver, root, work, home string, snapshotBytes int64) error {
	var sample int64
	if _, ok := drv.(sandbox.StorageSizer); ok {
		workBytes, err := driverDirBytes(ctx, drv, "work", work)
		if err != nil {
			return err
		}
		homeBytes, err := driverDirBytes(ctx, drv, "home", home)
		if err != nil {
			return err
		}
		sample = workBytes + homeBytes + snapshotBytes
	} else {
		sample = treeBytes(root)
	}
	if sample > r.PeakDiskBytes {
		r.PeakDiskBytes = sample
	}
	return nil
}

// diskFail turns a failed disk measurement into an ERROR result: a sandbox
// that cannot report its usage must not produce zeros in the cost report.
func diskFail(res *Result, err error) {
	res.Verdict = VerdictError
	res.Reason = "disk measurement: " + err.Error()
	res.RecoveryPath = diskMeasurementRecoveryPath
}

func runPreCheckpoint(ctx context.Context, drv sandbox.Driver, s *scenario.Scenario, timeout time.Duration) error {
	for i, step := range s.Checkpoint.Pre {
		prefix := fmt.Sprintf("checkpoint.pre[%d]", i)
		for iter := range stepRepeat(step) {
			if err := execStep(ctx, drv, step, iter, prefix, stepTimeout(step, timeout)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkpointRepeat returns the number of checkpoints to take for a scenario.
func checkpointRepeat(s *scenario.Scenario) int {
	if s.Checkpoint.Repeat > 0 {
		return s.Checkpoint.Repeat
	}
	return 1
}

// stepRepeat returns the execution count of a damage step.
func stepRepeat(step scenario.DamageStep) int {
	if step.Repeat > 0 {
		return step.Repeat
	}
	return 1
}

func runDamage(ctx context.Context, drv sandbox.Driver, s *scenario.Scenario, timeout time.Duration, res *Result) error {
	for i, step := range s.Damage {
		kind, err := step.Kind()
		if err != nil {
			return err
		}
		prefix := fmt.Sprintf("damage[%d]", i)
		if kind == scenario.StepParallel {
			start := time.Now()
			if err := runParallel(ctx, drv, i, step, timeout, res); err != nil {
				return err
			}
			res.Steps = append(res.Steps, StepTiming{
				Index: i,
				Kind:  string(kind),
				US:    time.Since(start).Microseconds(),
			})
			continue
		}
		for iter := range stepRepeat(step) {
			start := time.Now()
			if err := execStep(ctx, drv, step, iter, prefix, stepTimeout(step, timeout)); err != nil {
				return err
			}
			res.Steps = append(res.Steps, StepTiming{
				Index: i,
				Kind:  string(kind),
				US:    time.Since(start).Microseconds(),
			})
		}
	}
	return nil
}

// expandIteration replaces the {{i}} placeholder with the iteration index.
func expandIteration(script string, iter int) string {
	return strings.ReplaceAll(script, "{{i}}", strconv.Itoa(iter))
}

// runParallel executes the branches of a parallel group concurrently and
// records one timing per nested step. The durations overlap: each record
// measures its own execution, so a tool call inside a group still reaches the
// cost report.
func runParallel(ctx context.Context, drv sandbox.Driver, idx int, step scenario.DamageStep, timeout time.Duration, res *Result) error {
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
				kind, err := nested.Kind()
				if err != nil {
					mu.Lock()
					if werr == nil {
						werr = fmt.Errorf("%s: %w", prefix, err)
					}
					mu.Unlock()
					cancel()
					return
				}
				start := time.Now()
				if err := execStep(gctx, drv, nested, 0, prefix, stepTimeout(nested, stepTimeout(step, timeout))); err != nil {
					mu.Lock()
					if werr == nil {
						werr = err
					}
					mu.Unlock()
					cancel()
					return
				}
				elapsed := time.Since(start).Microseconds()
				mu.Lock()
				res.Steps = append(res.Steps, StepTiming{
					Index: idx,
					Kind:  string(kind),
					US:    elapsed,
				})
				mu.Unlock()
			}
		}(b, branch)
	}
	wg.Wait()
	return werr
}

// execStep runs one damage step and prefixes its errors with the journal label.
func execStep(ctx context.Context, drv sandbox.Driver, step scenario.DamageStep, iter int, prefix string, timeout time.Duration) error {
	kind, err := step.Kind()
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	switch kind {
	case scenario.StepShell:
		if _, err := drv.Exec(ctx, expandIteration(step.Shell, iter), timeout); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
	case scenario.StepPostEdit:
		if _, err := drv.Exec(ctx, expandIteration(step.PostEdit, iter), timeout); err != nil {
			return fmt.Errorf("%s post_edit: %w", prefix, err)
		}
	case scenario.StepToolCall:
		cmd := expandIteration(step.ToolCall.Args["cmd"], iter)
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
	if err := syncRestore(drv, cfg.Baseline, filepath.Join(root, "work"), filepath.Join(root, "home")); err != nil {
		return fmt.Errorf("restore: %w", err)
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
		base.RecoveryPath = ""
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
