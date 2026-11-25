package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/saromanov/unlucky-agent/internal/manifest"
	"github.com/saromanov/unlucky-agent/internal/oracle"
	"github.com/saromanov/unlucky-agent/internal/sandbox"
	"github.com/saromanov/unlucky-agent/internal/scenario"
)

func loadScenario(t *testing.T, id string) *scenario.Scenario {
	t.Helper()
	s, err := scenario.Load(filepath.Join("..", "..", "scenarios", id+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runScenario(t *testing.T, id, baselineName string) *Result {
	t.Helper()
	return runScenarioDriver(t, id, baselineName, nil)
}

func runScenarioDriver(t *testing.T, id, baselineName string, drv sandbox.Driver) *Result {
	t.Helper()
	res, err := Run(context.Background(), Config{
		Scenario: loadScenario(t, id),
		Baseline: baselineName,
		OutDir:   t.TempDir(),
		Timeout:  30 * time.Second,
		Driver:   drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBaselineCpRRestoresTrackedAndUntracked(t *testing.T) {
	res := runScenario(t, "S02", "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore S02: verdict=%s reason=%s diff=%+v", res.Verdict, res.Reason, res.Diff)
	}
	if res.Recall != 1 {
		t.Fatalf("recall=%v", res.Recall)
	}
	if !res.DamageDetected {
		t.Fatal("damage must be detected")
	}
}

func TestBaselineNoneFails(t *testing.T) {
	res := runScenario(t, "S02", "none")
	if res.Verdict != VerdictFail {
		t.Fatalf("without rollback FAIL is expected, got %s", res.Verdict)
	}
	if res.Recall >= 1 {
		t.Fatalf("recall must be < 1: %v", res.Recall)
	}
	if len(res.Diff.Missing) != 4 {
		t.Fatalf("expected 4 missing entries (directory and 3 files), got %+v", res.Diff.Missing)
	}
}

func TestSafetyScenarioDetectsClobber(t *testing.T) {
	res := runScenario(t, "S21", "cp-r")
	if res.Verdict != VerdictFail {
		t.Fatalf("cp-r clobbers the post-T edit, FAIL expected, got %s", res.Verdict)
	}
	if res.Clobbered != 1 {
		t.Fatalf("expected 1 clobbered path, got %d (%+v)", res.Clobbered, res.Diff.Clobbered)
	}
}

func TestCleanScenario(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git is not installed")
	}
	res := runScenario(t, "S04", "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore S04: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
}

func TestUnknownFixture(t *testing.T) {
	s := loadScenario(t, "S02")
	s.Fixture = "nope"
	if _, err := Run(context.Background(), Config{
		Scenario: s,
		OutDir:   t.TempDir(),
	}); err == nil {
		t.Fatal("expected an error for an unknown fixture")
	}
}

func TestDriverParityS02(t *testing.T) {
	def := runScenarioDriver(t, "S02", "cp-r", nil)
	local := runScenarioDriver(t, "S02", "cp-r", sandbox.NewLocal())
	if def.Sandbox != "local" || local.Sandbox != "local" {
		t.Fatalf("both runs must report the local sandbox: %q %q", def.Sandbox, local.Sandbox)
	}
	if def.Verdict != VerdictPass || local.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore S02: default=%s local=%s", def.Verdict, local.Verdict)
	}
	if def.Verdict != local.Verdict || def.Recall != local.Recall {
		t.Fatalf("driver changed the verdict: default=%s/%v local=%s/%v",
			def.Verdict, def.Recall, local.Verdict, local.Recall)
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func changePaths(changes []oracle.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	slices.Sort(out)
	return out
}

func TestS01NoneFailsExactMissingSet(t *testing.T) {
	needGit(t)
	res := runScenario(t, "S01", "none")
	if res.Verdict != VerdictFail {
		t.Fatalf("without rollback FAIL is expected, got %s", res.Verdict)
	}
	want := []string{
		"work/src",
		"work/src/local.settings.json",
		"work/src/main.go",
		"work/src/util.go",
	}
	if got := changePaths(res.Diff.Missing); !slices.Equal(got, want) {
		t.Fatalf("missing set mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestS01PerfectPasses(t *testing.T) {
	needGit(t)
	res := runScenario(t, "S01", "perfect")
	if res.Verdict != VerdictPass {
		t.Fatalf("perfect must restore S01: verdict=%s reason=%s diff=%+v", res.Verdict, res.Reason, res.Diff)
	}
	if res.Recall != 1 || res.Clobbered != 0 {
		t.Fatalf("recall=%v clobbered=%d", res.Recall, res.Clobbered)
	}
	if !res.DamageDetected {
		t.Fatal("damage must be detected")
	}
}

func TestS03NoneFailsExactSet(t *testing.T) {
	needGit(t)
	res := runScenario(t, "S03", "none")
	if res.Verdict != VerdictFail {
		t.Fatalf("without rollback FAIL is expected, got %s", res.Verdict)
	}
	wantMissing := []string{"work/src/extra.go"}
	if got := changePaths(res.Diff.Missing); !slices.Equal(got, wantMissing) {
		t.Fatalf("missing set mismatch:\n got %v\nwant %v", got, wantMissing)
	}
	wantChanged := []string{"work/src/main.go", "work/src/util.go"}
	if got := changePaths(res.Diff.Changed); !slices.Equal(got, wantChanged) {
		t.Fatalf("changed set mismatch:\n got %v\nwant %v", got, wantChanged)
	}
}

func TestS03PerfectPasses(t *testing.T) {
	needGit(t)
	res := runScenario(t, "S03", "perfect")
	if res.Verdict != VerdictPass {
		t.Fatalf("perfect must restore S03: verdict=%s reason=%s diff=%+v", res.Verdict, res.Reason, res.Diff)
	}
	if res.Recall != 1 {
		t.Fatalf("recall=%v", res.Recall)
	}
}

func TestPerfectPreservesPostEdit(t *testing.T) {
	res := runScenario(t, "S21", "perfect")
	if res.Verdict != VerdictPass {
		t.Fatalf("perfect must keep the post-T edit: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if res.Clobbered != 0 {
		t.Fatalf("expected 0 clobbered paths, got %d (%+v)", res.Clobbered, res.Diff.Clobbered)
	}
}

func TestPerfectCoversHome(t *testing.T) {
	res := runScenario(t, "S05", "perfect")
	if res.Verdict != VerdictPass {
		t.Fatalf("perfect must restore home: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
}

func TestOracleMatrixPerfectVsNone(t *testing.T) {
	needGit(t)
	for _, id := range []string{"S01", "S02", "S03", "S04", "S05", "S21"} {
		if res := runScenario(t, id, "perfect"); res.Verdict != VerdictPass {
			t.Fatalf("perfect must pass %s: verdict=%s reason=%s", id, res.Verdict, res.Reason)
		}
		if res := runScenario(t, id, "none"); res.Verdict != VerdictFail {
			t.Fatalf("none must fail %s: verdict=%s reason=%s", id, res.Verdict, res.Reason)
		}
	}
}

func hasMeta(changes []oracle.Change, path, detail string) bool {
	for _, c := range changes {
		if c.Path == path && strings.Contains(c.Detail, detail) {
			return true
		}
	}
	return false
}

func needGNUTar(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is not installed")
	}
	out, err := exec.Command("tar", "--version").Output()
	if err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("GNU tar is required")
	}
}

func TestS07CpRLosesHardlinkMetadata(t *testing.T) {
	res := runScenario(t, "S07", "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore S07 content: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if res.MetaScore >= 1 {
		t.Fatalf("cp-r must lose hardlink metadata, meta=%v", res.MetaScore)
	}
	if !hasMeta(res.Diff.Meta, "work/spec/hard_a", "nlink 2->1") {
		t.Fatalf("expected a nlink defect for hard_a: %+v", res.Diff.Meta)
	}
}

func TestS07TarKeepsHardlinkMetadata(t *testing.T) {
	needGNUTar(t)
	res := runScenario(t, "S07", "tar")
	if res.Verdict != VerdictPass {
		t.Fatalf("tar must restore S07: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if res.MetaScore != 1 {
		t.Fatalf("tar must keep hardlinks and modes, meta=%v defects=%+v", res.MetaScore, res.Diff.Meta)
	}
}

func TestS09CpRPasses(t *testing.T) {
	res := runScenario(t, "S09", "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore S09: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if res.Recall != 1 || res.MetaScore != 1 {
		t.Fatalf("recall=%v meta=%v", res.Recall, res.MetaScore)
	}
	if !res.DamageDetected {
		t.Fatal("damage must be detected")
	}
}

func TestS09NoneFails(t *testing.T) {
	res := runScenario(t, "S09", "none")
	if res.Verdict != VerdictFail {
		t.Fatalf("without rollback FAIL is expected, got %s", res.Verdict)
	}
	if len(res.Diff.Missing) <= 1000 {
		t.Fatalf("expected a large missing set, got %d", len(res.Diff.Missing))
	}
}

func TestS06CpRDensifiesSparse(t *testing.T) {
	if os.Getenv("UNLUCKY_BIG_TESTS") != "1" {
		t.Skip("set UNLUCKY_BIG_TESTS=1")
	}
	res := runScenario(t, "S06", "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore S06 content: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if res.MetaScore >= 1 {
		t.Fatalf("cp-r must densify the sparse file, meta=%v", res.MetaScore)
	}
	if !hasMeta(res.Diff.Meta, "work/big/sparse.bin", "blocks") {
		t.Fatalf("expected a blocks defect for sparse.bin: %+v", res.Diff.Meta)
	}
}

func TestTarBaselineRejectsDocker(t *testing.T) {
	drv, err := sandbox.NewDocker(sandbox.DockerConfig{EvidenceDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), Config{
		Scenario: loadScenario(t, "S02"),
		Baseline: "tar",
		OutDir:   t.TempDir(),
		Driver:   drv,
	})
	if err == nil || !strings.Contains(err.Error(), "host-only") {
		t.Fatalf("tar baseline must be rejected for docker, got %v", err)
	}
}

// TestS05CpRLeavesHomeUntouched pins the S05 cp-r behavior: the host baseline
// only snapshots work, so the surviving home/.config directory is matched and
// its dropped subdirectory is missing. The reduced nlink of the directory
// itself must not turn into a metadata defect.
func TestS05CpRLeavesHomeUntouched(t *testing.T) {
	res := runScenario(t, "S05", "cp-r")
	if res.Verdict != VerdictFail {
		t.Fatalf("cp-r cannot restore home, FAIL expected, got %s", res.Verdict)
	}
	if res.Recall != 0.2 {
		t.Fatalf("recall=%v, want 0.2 (home/.config matched of 5 expected)", res.Recall)
	}
	if res.MetaScore != 1 {
		t.Fatalf("meta=%v, want 1: directory nlink is not a metadata defect (%+v)", res.MetaScore, res.Diff.Meta)
	}
	if len(res.Diff.Missing) != 4 {
		t.Fatalf("missing=%+v, want 4 entries", res.Diff.Missing)
	}
}

func inlineScenario(damage []scenario.DamageStep, restore []string) *scenario.Scenario {
	return &scenario.Scenario{
		ID:          "S90",
		Title:       "inline",
		Group:       "failure",
		SpecVersion: 1,
		Platforms:   []string{"linux"},
		Fixture:     "project_basic",
		Seed:        42,
		Damage:      damage,
		Restore:     scenario.Restore{Mode: "newest"},
		Expect:      scenario.Expect{Restore: restore},
	}
}

func runInline(t *testing.T, s *scenario.Scenario, baselineName string) *Result {
	t.Helper()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Config{
		Scenario: s,
		Baseline: baselineName,
		OutDir:   t.TempDir(),
		Timeout:  60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestParallelGroupJoinsBeforeCapture(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{{
		TimeoutS: 60,
		Parallel: [][]scenario.DamageStep{
			{{Shell: "rm -rf src"}},
			{{Shell: "rm -rf docs"}},
		},
	}}, []string{"work/src/**", "work/docs/**"})
	res := runInline(t, s, "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore both branches: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if len(res.Diff.Missing) != 0 || len(res.Diff.Changed) != 0 {
		t.Fatalf("both branches must be captured after the join: %+v", res.Diff)
	}
}

func TestParallelBranchErrorIsReported(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{{
		Parallel: [][]scenario.DamageStep{
			{{Shell: "rm -rf src"}},
			{{Shell: "exit 1"}},
		},
	}}, []string{"work/src/**"})
	res := runInline(t, s, "cp-r")
	if res.Verdict != VerdictError {
		t.Fatalf("a failing branch must be an ERROR, got %s", res.Verdict)
	}
	if !strings.Contains(res.Reason, "parallel[1]") {
		t.Fatalf("the branch index must be reported, got %q", res.Reason)
	}
}

func TestBarrierSteps(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{
		{Shell: "rm -rf src"},
		{Barrier: "signal s90-ready"},
		{Barrier: "wait s90-ready"},
		{Barrier: "wait-free s90-free"},
	}, []string{"work/src/**"})
	res := runInline(t, s, "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("barrier steps must pass: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
}

func TestBarrierStepFailsOnUnknownOp(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{{Barrier: "hold s90"}}, []string{"work/src/**"})
	if err := s.Validate(); err == nil {
		t.Fatal("an unknown barrier op must be rejected by validation")
	}
}

func TestScenarioS16CorruptedGit(t *testing.T) {
	needGit(t)
	for _, baselineName := range []string{"cp-r", "perfect"} {
		res := runScenario(t, "S16", baselineName)
		if res.Verdict != VerdictPass {
			t.Fatalf("%s must restore both repositories: verdict=%s reason=%s diff=%+v",
				baselineName, res.Verdict, res.Reason, res.Diff)
		}
		if res.Recall != 1 {
			t.Fatalf("%s: recall=%v", baselineName, res.Recall)
		}
	}
}

// The ~95% pressure is real only in the docker sandbox; on the local driver
// the 121 MB fill is a bounded large-file round-trip.
func TestScenarioS17DiskPressure(t *testing.T) {
	res := runScenario(t, "S17", "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore under disk pressure: verdict=%s reason=%s diff=%+v", res.Verdict, res.Reason, res.Diff)
	}
	if res.RecoveryPath != "" {
		t.Fatalf("a passing checkpoint must not carry a recovery path: %q", res.RecoveryPath)
	}

	none := runScenario(t, "S17", "none")
	if none.Verdict != VerdictFail {
		t.Fatalf("without rollback FAIL is expected, got %s", none.Verdict)
	}
	if len(none.Diff.Missing) == 0 {
		t.Fatal("the damaged files must be missing without a rollback")
	}
}

type failSnapshotDriver struct {
	sandbox.Driver
}

func (d failSnapshotDriver) Name() string { return "fail-snapshot" }

func (d failSnapshotDriver) CPSnapshot(ctx context.Context, timeout time.Duration) error {
	return fmt.Errorf("no space left on device")
}

func (d failSnapshotDriver) CPRestore(ctx context.Context, timeout time.Duration) error {
	return nil
}

func TestCheckpointSnapshotFailure(t *testing.T) {
	s := loadScenario(t, "S02")
	res, err := Run(context.Background(), Config{
		Scenario: s,
		Baseline: "cp-r",
		OutDir:   t.TempDir(),
		Driver:   failSnapshotDriver{sandbox.NewLocal()},
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("a failed snapshot must become an ERROR result, got run error %v", err)
	}
	if res.Verdict != VerdictError {
		t.Fatalf("a failed snapshot must be ERROR, got %s", res.Verdict)
	}
	if !strings.HasPrefix(res.Reason, "checkpoint:") {
		t.Fatalf("the reason must name the checkpoint step: %q", res.Reason)
	}
	if res.RecoveryPath == "" {
		t.Fatal("a failed checkpoint must carry a recovery path")
	}

	none, err := Run(context.Background(), Config{
		Scenario: loadScenario(t, "S02"),
		Baseline: "none",
		OutDir:   t.TempDir(),
		Driver:   failSnapshotDriver{sandbox.NewLocal()},
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("the none baseline must not use the snapshot: %v", err)
	}
	if none.Verdict != VerdictFail {
		t.Fatalf("without rollback FAIL is expected, got %s", none.Verdict)
	}
}

func TestPreCheckpointRunsBeforeSnapshot(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{{Shell: "rm -rf prec"}}, []string{"work/prec/**"})
	s.Checkpoint.Pre = []scenario.DamageStep{{Shell: "mkdir -p prec && printf x > prec/seed.txt"}}

	res := runInline(t, s, "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("the pre step must land in the snapshot: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if none := runInline(t, s, "none"); none.Verdict != VerdictFail {
		t.Fatalf("without rollback FAIL is expected, got %s", none.Verdict)
	}
}

func TestAggregateEqualVerdicts(t *testing.T) {
	runs := []*Result{{Verdict: VerdictPass}, {Verdict: VerdictPass}, {Verdict: VerdictPass}}
	res := Aggregate(runs)
	if res.Verdict != VerdictPass || res.Reason != "" {
		t.Fatalf("identical verdicts must pass through: %s %q", res.Verdict, res.Reason)
	}
	if Aggregate(nil) != nil {
		t.Fatal("an empty run list must aggregate to nil")
	}
	single := &Result{Verdict: VerdictFail}
	if Aggregate([]*Result{single}) != single {
		t.Fatal("a single run must be returned as is")
	}
}

func TestAggregateDivergenceIsFlaky(t *testing.T) {
	runs := []*Result{
		{Verdict: VerdictPass},
		{Verdict: VerdictFail},
		{Verdict: VerdictPass},
	}
	res := Aggregate(runs)
	if res.Verdict != VerdictFlaky {
		t.Fatalf("divergent verdicts must be FLAKY, got %s", res.Verdict)
	}
	if !strings.Contains(res.Reason, "PASS, FAIL, PASS") {
		t.Fatalf("the reason must list the verdicts in run order, got %q", res.Reason)
	}
}

func TestExecInterrupt(t *testing.T) {
	dir := t.TempDir()
	drv := sandbox.NewLocal()
	if err := drv.Prepare(context.Background(), sandbox.HostDirs{Root: dir, Work: dir, Home: dir}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, err := drv.ExecInterrupt(context.Background(), "sleep 5; echo late", 30*time.Second, 100*time.Millisecond)
	if !errors.Is(err, sandbox.ErrInterrupted) {
		t.Fatalf("the script must be killed mid-run, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the process group must be killed, took %s", time.Since(start))
	}
	if strings.Contains(string(out), "late") {
		t.Fatalf("the script must not complete: %q", out)
	}

	out, err = drv.ExecInterrupt(context.Background(), "echo done", 30*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("a script that finishes first must not be interrupted: %v", err)
	}
	if !strings.Contains(string(out), "done") {
		t.Fatalf("output is lost: %q", out)
	}
}

func TestRepeatExpandsIterationPlaceholder(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{
		{Shell: "printf 'run %s\n' {{i}} > rep-{{i}}.txt", Repeat: 3},
		{
			ToolCall: &scenario.ToolCall{
				Name: "append_line",
				Args: map[string]string{"cmd": "printf 'call %s\n' {{i}} >> tool-{{i}}.txt"},
			},
			Repeat: 2,
		},
		{Shell: "rm -rf docs"},
	}, []string{"work/docs/**"})
	res := runInline(t, s, "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore the tree: verdict=%s reason=%s", res.Verdict, res.Reason)
	}

	counts := map[string]int{}
	for _, step := range res.Steps {
		counts[step.Kind]++
		if step.Index < 0 || step.Index > 2 {
			t.Fatalf("step index %d is out of range", step.Index)
		}
	}
	if counts[string(scenario.StepShell)] != 4 {
		t.Fatalf("shell timings=%d, want 4: %+v", counts[string(scenario.StepShell)], res.Steps)
	}
	if counts[string(scenario.StepToolCall)] != 2 {
		t.Fatalf("toolcall timings=%d, want 2: %+v", counts[string(scenario.StepToolCall)], res.Steps)
	}

	paths := manifestPaths(t, res.Artifacts["damaged"])
	for _, want := range []string{
		"work/rep-0.txt", "work/rep-1.txt", "work/rep-2.txt",
		"work/tool-0.txt", "work/tool-1.txt",
	} {
		if !paths[want] {
			t.Fatalf("%s is missing from the damaged manifest", want)
		}
	}
	if paths["work/rep-{{i}}.txt"] || paths["work/tool-{{i}}.txt"] {
		t.Fatal("the placeholder must be expanded before execution")
	}
}

func manifestPaths(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines[1:] {
		var entry manifest.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Path != "" {
			out[entry.Path] = true
		}
	}
	return out
}

func TestCheckpointRepeatSeries(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{{Shell: "rm -rf src"}}, []string{"work/src/**"})
	s.Checkpoint.Repeat = 3
	res := runInline(t, s, "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore the tree: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	if len(res.CheckpointTimesMS) != 3 {
		t.Fatalf("checkpoint series=%v, want 3 values", res.CheckpointTimesMS)
	}
	if len(res.DiskSnapshotBytes) != 3 {
		t.Fatalf("snapshot series=%v, want 3 values", res.DiskSnapshotBytes)
	}
	for _, v := range res.DiskSnapshotBytes {
		if v <= 0 {
			t.Fatalf("a checkpoint must occupy disk: %v", res.DiskSnapshotBytes)
		}
	}
	if res.DiskWorkBytes <= 0 {
		t.Fatalf("work disk=%d, want > 0", res.DiskWorkBytes)
	}
	if res.PeakDiskBytes < res.DiskWorkBytes {
		t.Fatalf("peak disk=%d is below the work tree=%d", res.PeakDiskBytes, res.DiskWorkBytes)
	}
}

func TestCheckpointRepeatKeepsOneSlot(t *testing.T) {
	s := inlineScenario([]scenario.DamageStep{{Shell: "rm -rf src"}}, []string{"work/src/**"})
	s.Checkpoint.Repeat = 3
	res := runInline(t, s, "cp-r")
	if res.Verdict != VerdictPass {
		t.Fatalf("cp-r must restore the tree: verdict=%s reason=%s", res.Verdict, res.Reason)
	}
	first, last := res.DiskSnapshotBytes[0], res.DiskSnapshotBytes[len(res.DiskSnapshotBytes)-1]
	if last > first+first/10 {
		t.Fatalf("the snapshot slot grows across checkpoints: %v", res.DiskSnapshotBytes)
	}
}

func TestPercentile(t *testing.T) {
	if got := Percentile(nil, 50); got != 0 {
		t.Fatalf("an empty series must be 0, got %d", got)
	}
	values := []int64{50, 10, 40, 20, 30}
	if got := Percentile(values, 50); got != 30 {
		t.Fatalf("p50=%d, want 30", got)
	}
	if got := Percentile(values, 95); got != 50 {
		t.Fatalf("p95=%d, want 50", got)
	}
	if got := Percentile(values, 100); got != 50 {
		t.Fatalf("p100=%d, want 50", got)
	}
	if values[0] != 50 {
		t.Fatal("Percentile must not reorder the input")
	}
}
