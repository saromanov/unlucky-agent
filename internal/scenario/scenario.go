package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var idPattern = regexp.MustCompile(`^S[0-9]{2}$`)

var groups = map[string]bool{
	"loss":      true,
	"meta":      true,
	"semantics": true,
	"failure":   true,
	"cost":      true,
	"safety":    true,
}

var tiers = map[string]bool{
	"git-shadow":  true,
	"fs-snapshot": true,
	"staging":     true,
	"baseline":    true,
}

var restoreModes = map[string]bool{
	"newest":        true,
	"at_checkpoint": true,
}

var requireLevels = map[string]bool{
	"":        true,
	"content": true,
	"meta":    true,
}

// StepKind is the kind of a damage step.
type StepKind string

// Step kinds.
const (
	StepShell    StepKind = "shell"
	StepPostEdit StepKind = "post_edit"
	StepToolCall StepKind = "toolcall"
	StepBarrier  StepKind = "barrier"
)

// ToolCall is an agent tool invocation.
type ToolCall struct {
	Name string            `yaml:"name"`
	Args map[string]string `yaml:"args"`
}

// DamageStep is a single damage step.
type DamageStep struct {
	Shell    string    `yaml:"shell"`
	PostEdit string    `yaml:"post_edit"`
	ToolCall *ToolCall `yaml:"toolcall"`
	Barrier  string    `yaml:"barrier"`
	TimeoutS int       `yaml:"timeout_s"`
}

// Kind returns the step kind. Exactly one field must be set.
func (s DamageStep) Kind() (StepKind, error) {
	set := 0
	kind := StepKind("")
	if s.Shell != "" {
		set++
		kind = StepShell
	}
	if s.PostEdit != "" {
		set++
		kind = StepPostEdit
	}
	if s.ToolCall != nil {
		set++
		kind = StepToolCall
	}
	if s.Barrier != "" {
		set++
		kind = StepBarrier
	}
	if set != 1 {
		return "", fmt.Errorf("a step must set exactly one of shell/post_edit/toolcall/barrier, got %d", set)
	}
	return kind, nil
}

// Scenario describes a single crash test.
type Scenario struct {
	ID          string       `yaml:"id"`
	Title       string       `yaml:"title"`
	Group       string       `yaml:"group"`
	SpecVersion int          `yaml:"spec_version"`
	Tiers       []string     `yaml:"tiers"`
	Platforms   []string     `yaml:"platforms"`
	Fixture     string       `yaml:"fixture"`
	Seed        int64        `yaml:"seed"`
	Checkpoint  Checkpoint   `yaml:"checkpoint"`
	Damage      []DamageStep `yaml:"damage"`
	Restore     Restore      `yaml:"restore"`
	Expect      Expect       `yaml:"expect"`
	Limits      Limits       `yaml:"limits"`
	Resources   Resources    `yaml:"resources"`
}

// Checkpoint defines the restore point requirements.
type Checkpoint struct {
	When string `yaml:"when"`
	Note string `yaml:"note"`
}

// Restore is the rollback mode.
type Restore struct {
	Mode string `yaml:"mode"`
}

// Expect defines scenario expectations.
type Expect struct {
	Restore  []string `yaml:"restore"`
	Preserve []string `yaml:"preserve"`
	Exclude  []string `yaml:"exclude"`
	Require  string   `yaml:"require"`
}

// Limits holds timeouts.
type Limits struct {
	TimeoutS int `yaml:"timeout_s"`
}

// Resources holds resource limits.
type Resources struct {
	DiskMB   int `yaml:"disk_mb"`
	MemoryMB int `yaml:"memory_mb"`
}

// Load reads a scenario with strict field checking.
func Load(path string) (*Scenario, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// LoadDir reads all *.yaml files and sorts them by ID.
func LoadDir(dir string) ([]*Scenario, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no *.yaml scenarios in %s", dir)
	}
	out := make([]*Scenario, 0, len(paths))
	for _, p := range paths {
		s, err := Load(p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Validate checks required fields and allowed values.
func (s *Scenario) Validate() error {
	var errs []string
	if !idPattern.MatchString(s.ID) {
		errs = append(errs, "id must look like S01..S99")
	}
	if strings.TrimSpace(s.Title) == "" {
		errs = append(errs, "title is empty")
	}
	if !groups[s.Group] {
		errs = append(errs, fmt.Sprintf("unknown group %q", s.Group))
	}
	if s.SpecVersion == 0 {
		errs = append(errs, "spec_version is not set")
	}
	for _, t := range s.Tiers {
		if !tiers[t] {
			errs = append(errs, fmt.Sprintf("unknown tier %q", t))
		}
	}
	if strings.TrimSpace(s.Fixture) == "" {
		errs = append(errs, "fixture is not set")
	}
	if len(s.Damage) == 0 {
		errs = append(errs, "damage is empty")
	}
	for i, step := range s.Damage {
		if _, err := step.Kind(); err != nil {
			errs = append(errs, fmt.Sprintf("damage[%d]: %v", i, err))
		}
	}
	if !restoreModes[s.Restore.Mode] {
		errs = append(errs, fmt.Sprintf("restore.mode %q is not allowed", s.Restore.Mode))
	}
	if !requireLevels[s.Expect.Require] {
		errs = append(errs, fmt.Sprintf("expect.require %q is not allowed", s.Expect.Require))
	}
	for _, g := range append(append([]string{}, s.Expect.Restore...), s.Expect.Exclude...) {
		if strings.Contains(g, "..") {
			errs = append(errs, fmt.Sprintf("glob %q contains ..", g))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid scenario:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
