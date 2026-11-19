// Command unlucky-agent is a reproducible crash-test bench for rollback tools.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saromanov/unlucky-agent/internal/baseline"
	"github.com/saromanov/unlucky-agent/internal/manifest"
	"github.com/saromanov/unlucky-agent/internal/oracle"
	"github.com/saromanov/unlucky-agent/internal/report"
	"github.com/saromanov/unlucky-agent/internal/runner"
	"github.com/saromanov/unlucky-agent/internal/scenario"
)

// version is injected at build time via -ldflags.
var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "unlucky-agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no command given")
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "scenarios":
		return cmdScenarios(args[1:])
	case "manifest":
		return cmdManifest(args[1:])
	case "diff":
		return cmdDiff(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "baselines":
		fmt.Println(strings.Join(baseline.Names(), "\n"))
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `unlucky-agent is a crash-test bench for rollback tools

  unlucky-agent scenarios list|validate [-dir scenarios]
  unlucky-agent baselines
  unlucky-agent manifest -root DIR [-name work] -o out.jsonl
  unlucky-agent diff -base a.jsonl -observed b.jsonl [-exclude glob]... [-require meta]
  unlucky-agent run -scenario scenarios/S02.yaml -baseline cp-r -out out/
  unlucky-agent version
`)
}

func cmdScenarios(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("scenarios: expected subcommand list or validate")
	}
	fs := flag.NewFlagSet("scenarios", flag.ExitOnError)
	dir := fs.String("dir", "scenarios", "scenario directory")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	list, err := scenario.LoadDir(*dir)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		for _, s := range list {
			fmt.Printf("%s  %-10s %-14s %s\n", s.ID, s.Group, s.Tiers, s.Title)
		}
		fmt.Printf("total: %d\n", len(list))
		return nil
	case "validate":
		for _, s := range list {
			fmt.Printf("%s ok\n", s.ID)
		}
		fmt.Printf("valid scenarios: %d\n", len(list))
		return nil
	default:
		return fmt.Errorf("scenarios: unknown subcommand %q", args[0])
	}
}

func cmdManifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	root := fs.String("root", "", "tree root")
	name := fs.String("name", "work", "root name in the manifest")
	out := fs.String("o", "", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" || *out == "" {
		return fmt.Errorf("manifest: -root and -o are required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	tmp := *out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	stats, err := manifest.Capture([]manifest.Root{{Name: *name, Dir: *root}}, f)
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, *out); err != nil {
		return err
	}
	fmt.Printf("%s: %d entries, %d bytes\n", *out, stats.Entries, stats.Bytes)
	return nil
}

type globList []string

func (g *globList) String() string { return strings.Join(*g, ",") }
func (g *globList) Set(v string) error {
	*g = append(*g, v)
	return nil
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	base := fs.String("base", "", "expected-state manifest")
	obs := fs.String("observed", "", "observed-state manifest")
	require := fs.String("require", "content", "content or meta")
	var exclude globList
	fs.Var(&exclude, "exclude", "exclusion glob (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" || *obs == "" {
		return fmt.Errorf("diff: -base and -observed are required")
	}
	pol := oracle.Policy{Exclude: exclude, RequireMeta: *require == "meta"}
	res, err := oracle.DiffFiles(*base, *obs, pol)
	if err != nil {
		return err
	}
	fmt.Printf("expected=%d matched=%d recall=%.3f meta=%.3f\n",
		res.Expected, res.Matched, res.Recall(), res.MetaScore())
	printChanges("missing", res.Missing)
	printChanges("changed", res.Changed)
	printChanges("meta", res.Meta)
	printChanges("added", res.Added)
	if res.Pass() {
		fmt.Println("verdict: PASS")
		return nil
	}
	fmt.Println("verdict: FAIL")
	return fmt.Errorf("hard gates not passed")
}

func printChanges(kind string, changes []oracle.Change) {
	for _, c := range changes {
		fmt.Printf("%-8s %s %s\n", kind, c.Path, c.Detail)
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	scenarioPath := fs.String("scenario", "", "scenario file")
	baselineName := fs.String("baseline", "none", "baseline: none or cp-r")
	outDir := fs.String("out", "out", "output directory")
	restoreCmd := fs.String("restore", "", "restore command; overrides the baseline")
	keep := fs.Bool("keep", false, "keep the temporary directory")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-step timeout")
	all := fs.Bool("all", false, "run every scenario from -dir")
	dir := fs.String("dir", "scenarios", "scenario directory for -all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	var scenarios []*scenario.Scenario
	switch {
	case *all:
		list, err := scenario.LoadDir(*dir)
		if err != nil {
			return err
		}
		scenarios = list
	case *scenarioPath != "":
		s, err := scenario.Load(*scenarioPath)
		if err != nil {
			return err
		}
		scenarios = []*scenario.Scenario{s}
	default:
		return fmt.Errorf("run: -scenario or -all is required")
	}

	started := time.Now()
	results := make([]runner.Result, 0, len(scenarios))
	for _, s := range scenarios {
		res, err := runner.Run(context.Background(), runner.Config{
			Scenario:   s,
			Baseline:   *baselineName,
			RestoreCmd: *restoreCmd,
			OutDir:     *outDir,
			Keep:       *keep,
			Timeout:    *timeout,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", s.ID, err)
		}
		fmt.Printf("%s %-4s recall=%.3f meta=%.3f clobbered=%d %s\n",
			res.ScenarioID, res.Verdict, res.Recall, res.MetaScore, res.Clobbered, res.Reason)
		results = append(results, *res)
	}

	rep := report.New(fmt.Sprintf("run-%d", started.Unix()), version, *baselineName, started, results)
	jsonPath := filepath.Join(*outDir, "report.json")
	if err := report.WriteJSON(jsonPath, rep); err != nil {
		return err
	}
	mdPath := filepath.Join(*outDir, "report.md")
	mf, err := os.Create(mdPath)
	if err != nil {
		return err
	}
	defer mf.Close()
	if err := report.WriteMarkdown(mf, rep); err != nil {
		return err
	}
	fmt.Printf("report: %s, %s\n", jsonPath, mdPath)

	for _, r := range results {
		if r.Verdict == runner.VerdictError {
			return fmt.Errorf("run %s finished with ERROR: %s", r.ScenarioID, r.Reason)
		}
	}
	return nil
}
