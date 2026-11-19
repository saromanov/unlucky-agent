package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	res, err := Run(context.Background(), Config{
		Scenario: loadScenario(t, id),
		Baseline: baselineName,
		OutDir:   t.TempDir(),
		Timeout:  30 * time.Second,
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
