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

| Scenario | Verdict | Recall | Meta | Clobbered | Reason |
|---|---|---|---|---|---|
{{range .Results}}| {{.ScenarioID}} | {{.Verdict}} | {{printf "%.3f" .Recall}} | {{printf "%.3f" .MetaScore}} | {{.Clobbered}} | {{.Reason}} |
{{end}}
{{if .MetaDefects}}## Metadata defects

Metadata defects are reported separately from content defects: a restore may
recover every byte and still lose hardlinks, permission bits or sparseness.

{{range .MetaDefects}}- {{.}}
{{end}}
{{end}}## Summary

{{.Summary}}
`

var mdTemplate = template.Must(template.New("md").Parse(markdownTmpl))

type mdData struct {
	*Report
	Summary     string
	MetaDefects []string
}

// WriteMarkdown writes a human-readable report.
func WriteMarkdown(w io.Writer, r *Report) error {
	return mdTemplate.Execute(w, mdData{Report: r, Summary: summarize(r), MetaDefects: metaDefects(r)})
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
