package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
