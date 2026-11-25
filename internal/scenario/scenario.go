package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/saromanov/unlucky-agent/internal/shim"
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
	StepParallel StepKind = "parallel"
)

// Barrier operations.
const (
	BarrierSignal   = "signal"
	BarrierWait     = "wait"
	BarrierWaitHeld = "wait-held"
	BarrierWaitFree = "wait-free"
)

// ParseBarrier splits a barrier step value into an operation and a name.
func ParseBarrier(spec string) (string, string, error) {
	parts := strings.Split(spec, " ")
	if len(parts) != 2 || parts[1] == "" {
		return "", "", fmt.Errorf("barrier %q must be \"<op> <name>\"", spec)
	}
	op, name := parts[0], parts[1]
	switch op {
	case BarrierSignal, BarrierWait, BarrierWaitHeld, BarrierWaitFree:
	default:
		return "", "", fmt.Errorf("unknown barrier op %q", op)
	}
	if !shim.ValidName(name) {
		return "", "", fmt.Errorf("invalid barrier name %q", name)
	}
	return op, name, nil
}

// ToolCall is an agent tool invocation.
type ToolCall struct {
	Name string            `yaml:"name"`
	Args map[string]string `yaml:"args"`
}

// DamageStep is a single damage step.
type DamageStep struct {
	Shell    string         `yaml:"shell"`
	PostEdit string         `yaml:"post_edit"`
	ToolCall *ToolCall      `yaml:"toolcall"`
	Barrier  string         `yaml:"barrier"`
	Parallel [][]DamageStep `yaml:"parallel"`
	TimeoutS int            `yaml:"timeout_s"`
	Repeat   int            `yaml:"repeat"`
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
	if len(s.Parallel) > 0 {
		set++
		kind = StepParallel
	}
	if set != 1 {
		return "", fmt.Errorf("a step must set exactly one of shell/post_edit/toolcall/barrier/parallel, got %d", set)
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
	When   string       `yaml:"when"`
	Note   string       `yaml:"note"`
	Pre    []DamageStep `yaml:"pre"`
	Repeat int          `yaml:"repeat"`
}

// Restore is the rollback mode.
type Restore struct {
	Mode            string         `yaml:"mode"`
	ConcurrentWrite string         `yaml:"concurrent_write"`
	Interrupt       *InterruptSpec `yaml:"interrupt"`
}

// InterruptSpec asks the runner to SIGKILL the restore mid-run.
type InterruptSpec struct {
	KillAfterMS int `yaml:"kill_after_ms"`
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
		if err := validateStep(step); err != nil {
			errs = append(errs, fmt.Sprintf("damage[%d]: %v", i, err))
		}
	}
	if s.Checkpoint.Repeat < 0 {
		errs = append(errs, "checkpoint.repeat must be >= 1")
	}
	for i, step := range s.Checkpoint.Pre {
		kind, err := step.Kind()
		if err != nil {
			errs = append(errs, fmt.Sprintf("checkpoint.pre[%d]: %v", i, err))
			continue
		}
		if kind != StepShell {
			errs = append(errs, fmt.Sprintf("checkpoint.pre[%d]: only shell steps are allowed before the checkpoint", i))
		}
	}
	if !restoreModes[s.Restore.Mode] {
		errs = append(errs, fmt.Sprintf("restore.mode %q is not allowed", s.Restore.Mode))
	}
	if spec := s.Restore.Interrupt; spec != nil && spec.KillAfterMS <= 0 {
		errs = append(errs, "restore.interrupt.kill_after_ms must be > 0")
	}
	if s.Restore.ConcurrentWrite != "" && !strings.Contains(s.Restore.ConcurrentWrite, shim.Dir()+"/") {
		errs = append(errs, fmt.Sprintf("restore.concurrent_write must hold a %s/<name> lock", shim.Dir()))
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

// validateStep checks one damage step, including nested parallel branches.
func validateStep(step DamageStep) error {
	kind, err := step.Kind()
	if err != nil {
		return err
	}
	if step.Repeat < 0 {
		return fmt.Errorf("repeat must be >= 1")
	}
	if kind == StepBarrier {
		if _, _, err := ParseBarrier(step.Barrier); err != nil {
			return err
		}
	}
	if kind != StepParallel {
		return nil
	}
	for j, branch := range step.Parallel {
		if len(branch) == 0 {
			return fmt.Errorf("parallel branch %d is empty", j)
		}
		for k, nested := range branch {
			if len(nested.Parallel) > 0 {
				return fmt.Errorf("parallel branch %d step %d: nested parallel is not allowed", j, k)
			}
			if _, err := nested.Kind(); err != nil {
				return fmt.Errorf("parallel branch %d step %d: %w", j, k, err)
			}
			if nested.Repeat < 0 {
				return fmt.Errorf("parallel branch %d step %d: repeat must be >= 1", j, k)
			}
			if nested.Barrier != "" {
				if _, _, err := ParseBarrier(nested.Barrier); err != nil {
					return fmt.Errorf("parallel branch %d step %d: %w", j, k, err)
				}
			}
		}
	}
	return nil
}
