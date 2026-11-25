// Package report renders a run report.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/saromanov/unlucky-agent/internal/runner"
)

// Schema is the report schema version.
const Schema = 1

// Run holds run metadata.
type Run struct {
	ID          string            `json:"id"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	Version     string            `json:"version"`
	GoVersion   string            `json:"go_version"`
	Platform    string            `json:"platform"`
	Baseline    string            `json:"baseline"`
	Fingerprint map[string]string `json:"fingerprint"`
}

// Report is a complete run report.
type Report struct {
	Schema  int             `json:"schema"`
	Run     Run             `json:"run"`
	Results []runner.Result `json:"results"`
}

// New builds a report from results.
func New(id, version, baselineName string, started time.Time, results []runner.Result) *Report {
	sort.Slice(results, func(i, j int) bool { return results[i].ScenarioID < results[j].ScenarioID })
	return &Report{
		Schema: Schema,
		Run: Run{
			ID:          id,
			StartedAt:   started.UTC(),
			FinishedAt:  time.Now().UTC(),
			Version:     version,
			GoVersion:   runtime.Version(),
			Platform:    runtime.GOOS + "/" + runtime.GOARCH,
			Baseline:    baselineName,
			Fingerprint: map[string]string{},
		},
		Results: results,
	}
}

// WriteJSON writes the report to a file.
func WriteJSON(path string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

const markdownTmpl = `# unlucky-agent: run report

- Run: {{.Run.ID}}
- Started: {{.Run.StartedAt.Format "2006-01-02T15:04:05Z"}}
- Finished: {{.Run.FinishedAt.Format "2006-01-02T15:04:05Z"}}
- Version: {{.Run.Version}}
- Platform: {{.Run.Platform}}
- Baseline: {{.Run.Baseline}}

## Matrix

| Scenario | Verdict | Recall | Meta | Clobbered | Reason | Notes |
|---|---|---|---|---|---|---|
{{range .Results}}| {{.ScenarioID}} | {{.Verdict}} | {{printf "%.3f" .Recall}} | {{printf "%.3f" .MetaScore}} | {{.Clobbered}} | {{.Reason}} | {{note .Notes}} |
{{end}}
{{if .MetaDefects}}## Metadata defects

Metadata defects are reported separately from content defects: a restore may
recover every byte and still lose hardlinks, permission bits or sparseness.

{{range .MetaDefects}}- {{.}}
{{end}}
{{end}}{{if .RecoveryPaths}}## Recovery paths

Every entry states what still can be recovered and how.

{{range .RecoveryPaths}}- {{.}}
{{end}}
{{end}}{{if .Cost}}## Cost

Checkpoint and tool-call percentiles cover the repeated calls of the run; the
none baseline is the added-latency reference. Durations below one millisecond
are printed in microseconds. Checkpoint and restore durations include the sync
that makes the copied state durable. Disk figures are allocated bytes.

| Scenario | Checkpoints | Checkpoint p50 | Checkpoint p95 | Restore | Tool calls | Tool p50 | Tool p95 | Work disk | Snapshot disk | Peak disk |
|---|---|---|---|---|---|---|---|---|---|---|
{{range .Cost}}| {{.ScenarioID}} | {{.Checkpoints}} | {{duration .CheckpointP50US}} | {{duration .CheckpointP95US}} | {{duration .RestoreUS}} | {{.ToolCalls}} | {{duration .ToolP50US}} | {{duration .ToolP95US}} | {{bytes .DiskWork}} | {{bytes .DiskSnapshot}} | {{bytes .PeakDisk}} |
{{end}}
{{end}}## Summary

{{.Summary}}
`

var mdTemplate = template.Must(template.New("md").Funcs(template.FuncMap{
	"note":     firstSentence,
	"bytes":    humanBytes,
	"duration": humanDuration,
}).Parse(markdownTmpl))

type mdData struct {
	*Report
	Summary       string
	MetaDefects   []string
	RecoveryPaths []string
	Cost          []CostRow
}

// CostRow is one line of the cost section. Durations are microseconds.
type CostRow struct {
	ScenarioID      string
	Checkpoints     int
	CheckpointP50US int64
	CheckpointP95US int64
	RestoreUS       int64
	ToolCalls       int
	ToolP50US       int64
	ToolP95US       int64
	DiskWork        int64
	DiskSnapshot    int64
	PeakDisk        int64
}

// WriteMarkdown writes a human-readable report.
func WriteMarkdown(w io.Writer, r *Report) error {
	data := mdData{
		Report:        r,
		Summary:       summarize(r),
		MetaDefects:   metaDefects(r),
		RecoveryPaths: recoveryPaths(r),
		Cost:          costRows(r),
	}
	return mdTemplate.Execute(w, data)
}

// costRows renders the timing series of a run, one row per scenario that has
// cost data. Results are already sorted by scenario id.
func costRows(r *Report) []CostRow {
	var out []CostRow
	for _, res := range r.Results {
		tool := toolCallTimes(res.Steps)
		if len(res.CheckpointTimesUS) == 0 && len(tool) == 0 {
			continue
		}
		out = append(out, CostRow{
			ScenarioID:      res.ScenarioID,
			Checkpoints:     len(res.CheckpointTimesUS),
			CheckpointP50US: runner.Percentile(res.CheckpointTimesUS, 50),
			CheckpointP95US: runner.Percentile(res.CheckpointTimesUS, 95),
			RestoreUS:       res.RestoreMS * 1000,
			ToolCalls:       len(tool),
			ToolP50US:       runner.Percentile(tool, 50),
			ToolP95US:       runner.Percentile(tool, 95),
			DiskWork:        res.DiskWorkBytes,
			DiskSnapshot:    maxSnapshot(res.DiskSnapshotBytes),
			PeakDisk:        res.PeakDiskBytes,
		})
	}
	return out
}

func toolCallTimes(steps []runner.StepTiming) []int64 {
	var out []int64
	for _, s := range steps {
		if s.Kind == "toolcall" {
			out = append(out, s.US)
		}
	}
	return out
}

func maxSnapshot(values []int64) int64 {
	var maxValue int64
	for _, v := range values {
		if v > maxValue {
			maxValue = v
		}
	}
	return maxValue
}

// humanDuration formats a microsecond duration: one decimal of a millisecond
// from 1 ms up, whole microseconds below.
func humanDuration(us int64) string {
	if us < 1000 {
		return fmt.Sprintf("%d µs", us)
	}
	return fmt.Sprintf("%.1f ms", float64(us)/1000)
}

// humanBytes formats an allocated byte count with binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(n)
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	return fmt.Sprintf("%.2f %s", value, units[idx])
}

// firstSentence shortens a note to its first clause for the matrix cell.
func firstSentence(note string) string {
	note = strings.TrimSpace(note)
	if i := strings.Index(note, ";"); i >= 0 {
		note = strings.TrimSpace(note[:i])
	}
	const limit = 120
	if r := []rune(note); len(r) > limit {
		note = string(r[:limit-3]) + "..."
	}
	return note
}

// metaDefects lists every metadata divergence of the run, one line each.
func metaDefects(r *Report) []string {
	var out []string
	for _, res := range r.Results {
		if res.Diff == nil {
			continue
		}
		for _, c := range res.Diff.Meta {
			out = append(out, fmt.Sprintf("%s %s: %s", res.ScenarioID, c.Path, c.Detail))
		}
	}
	return out
}

// recoveryPaths lists every failure that still leaves a way back.
func recoveryPaths(r *Report) []string {
	var out []string
	for _, res := range r.Results {
		if res.RecoveryPath == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s (%s): %s", res.ScenarioID, res.Verdict, res.RecoveryPath))
	}
	return out
}

func summarize(r *Report) string {
	counts := map[string]int{}
	for _, res := range r.Results {
		counts[res.Verdict]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}
