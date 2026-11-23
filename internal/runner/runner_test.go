package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
