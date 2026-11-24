package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/saromanov/unlucky-agent/internal/runner"
)

func renderMarkdown(t *testing.T, results []runner.Result) string {
	t.Helper()
	r := New("run", "test", "cp-r", time.Now(), results)
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRecoveryPathsSection(t *testing.T) {
	out := renderMarkdown(t, []runner.Result{{
		ScenarioID:   "S17",
		Verdict:      runner.VerdictError,
		Reason:       "checkpoint: no space left on device",
		RecoveryPath: "the checkpoint failed; no restore point exists",
	}})
	if !strings.Contains(out, "## Recovery paths") {
		t.Fatalf("the section must be rendered:\n%s", out)
	}
	if !strings.Contains(out, "- S17 (ERROR): the checkpoint failed; no restore point exists") {
		t.Fatalf("the entry must carry the verdict and the path:\n%s", out)
	}
	if strings.Index(out, "## Recovery paths") > strings.Index(out, "## Summary") {
		t.Fatalf("the section must precede the summary:\n%s", out)
	}
}

func TestRecoveryPathsSectionOmitted(t *testing.T) {
	out := renderMarkdown(t, []runner.Result{{ScenarioID: "S02", Verdict: runner.VerdictPass}})
	if strings.Contains(out, "Recovery paths") {
		t.Fatalf("a clean report must not render the section:\n%s", out)
	}
}
