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

func TestCostSection(t *testing.T) {
	out := renderMarkdown(t, []runner.Result{{
		ScenarioID:        "S19",
		Verdict:           runner.VerdictPass,
		RestoreMS:         1234,
		CheckpointTimesUS: []int64{10000, 20000, 30000, 40000, 50000},
		DiskSnapshotBytes: []int64{1 << 20, 2 << 20},
		DiskWorkBytes:     1 << 30,
		PeakDiskBytes:     2 << 30,
		Steps: []runner.StepTiming{
			{Index: 0, Kind: "toolcall", US: 700},
			{Index: 0, Kind: "toolcall", US: 900},
			{Index: 1, Kind: "shell", US: 100},
		},
	}})
	if !strings.Contains(out, "## Cost") {
		t.Fatalf("the cost section must be rendered:\n%s", out)
	}

	want := "| S19 | 5 | 30.0 ms | 50.0 ms | 1234.0 ms | 2 | 700 µs | 900 µs | 1.00 GiB | 2.00 MiB | 2.00 GiB |"
	if !strings.Contains(out, want) {
		t.Fatalf("the cost row must carry the series, want %q:\n%s", want, out)
	}

	if strings.Index(out, "## Cost") > strings.Index(out, "## Summary") {
		t.Fatalf("the cost section must precede the summary:\n%s", out)
	}
}

func TestCostSectionOmitted(t *testing.T) {
	out := renderMarkdown(t, []runner.Result{{
		ScenarioID: "S02",
		Verdict:    runner.VerdictPass,
		RestoreMS:  10,
	}})
	if strings.Contains(out, "## Cost") {
		t.Fatalf("a scenario without cost data must not render the section:\n%s", out)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[int64]string{
		0:        "0 µs",
		999:      "999 µs",
		1000:     "1.0 ms",
		1250:     "1.2 ms",
		15769000: "15769.0 ms",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Fatalf("humanDuration(%d)=%q, want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		512:         "512 B",
		1536:        "1.50 KiB",
		1 << 20:     "1.00 MiB",
		3 << 30 / 2: "1.50 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d)=%q, want %q", in, got, want)
		}
	}
}
