package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAllRepoScenarios(t *testing.T) {
	list, err := LoadDir(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 4 {
		t.Fatalf("expected at least 4 scenarios, got %d", len(list))
	}
	seen := map[string]bool{}
	for _, s := range list {
		if seen[s.ID] {
			t.Fatalf("duplicate id %s", s.ID)
		}
		seen[s.ID] = true
		if len(s.Damage) == 0 {
			t.Fatalf("%s: empty damage", s.ID)
		}
	}
}

func TestRejectUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "S99.yaml")
	body := `
id: S99
title: "x"
group: loss
spec_version: 1
fixture: project_basic
seed: 1
restore: {mode: newest}
damage: [{shell: "true"}]
unknown_field: 1
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("the error must mention the field, got: %v", err)
	}
}

func TestValidateRejectsBadStep(t *testing.T) {
	s := &Scenario{
		ID:          "S01",
		Title:       "x",
		Group:       "loss",
		SpecVersion: 1,
		Fixture:     "project_basic",
		Damage: []DamageStep{{
			Shell:   "rm -rf x",
			Barrier: "b",
		}},
		Restore: Restore{Mode: "newest"},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("a step with two kinds must be rejected")
	}
}

func TestValidateRejectsBadGlob(t *testing.T) {
	s := &Scenario{
		ID:          "S01",
		Title:       "x",
		Group:       "loss",
		SpecVersion: 1,
		Fixture:     "project_basic",
		Damage:      []DamageStep{{Shell: "true"}},
		Restore:     Restore{Mode: "newest"},
		Expect:      Expect{Restore: []string{"../etc/**"}},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("a glob containing .. must be rejected")
	}
}

func baseScenario(steps ...DamageStep) *Scenario {
	return &Scenario{
		ID:          "S01",
		Title:       "x",
		Group:       "loss",
		SpecVersion: 1,
		Fixture:     "project_basic",
		Damage:      steps,
		Restore:     Restore{Mode: "newest"},
	}
}

func TestValidateParallel(t *testing.T) {
	ok := baseScenario(DamageStep{Parallel: [][]DamageStep{
		{{Shell: "rm -rf src"}},
		{{Barrier: "wait-held w.lock"}, {Shell: "rm -rf docs"}},
	}})
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid parallel group must pass: %v", err)
	}
	if kind, err := ok.Damage[0].Kind(); err != nil || kind != StepParallel {
		t.Fatalf("kind=%q err=%v, want %q", kind, err, StepParallel)
	}

	nested := baseScenario(DamageStep{Parallel: [][]DamageStep{{
		{Parallel: [][]DamageStep{{{Shell: "true"}}}},
	}}})
	if err := nested.Validate(); err == nil {
		t.Fatal("a nested parallel group must be rejected")
	}

	empty := baseScenario(DamageStep{Parallel: [][]DamageStep{{}}})
	if err := empty.Validate(); err == nil {
		t.Fatal("an empty parallel branch must be rejected")
	}

	twoKinds := baseScenario(DamageStep{Parallel: [][]DamageStep{
		{{Shell: "true"}},
		{{Shell: "true", PostEdit: "true"}},
	}})
	if err := twoKinds.Validate(); err == nil {
		t.Fatal("a branch step with two kinds must be rejected")
	}

	both := baseScenario(DamageStep{Shell: "true", Parallel: [][]DamageStep{{{Shell: "true"}}}})
	if err := both.Validate(); err == nil {
		t.Fatal("a step that sets shell and parallel must be rejected")
	}
}

func TestValidateBarrierOp(t *testing.T) {
	for _, spec := range []string{
		"signal a",
		"wait a",
		"wait-held s14-write.lock",
		"wait-free w",
	} {
		s := baseScenario(DamageStep{Barrier: spec})
		if err := s.Validate(); err != nil {
			t.Fatalf("%q must be accepted: %v", spec, err)
		}
	}
	for _, spec := range []string{
		"",
		"signal",
		"signal ",
		"signal a b",
		"hold a",
		"signal ../a",
		"signal a;rm",
		"signal  a",
		"signal\ta",
	} {
		s := baseScenario(DamageStep{Barrier: spec})
		if spec == "" {
			continue
		}
		if err := s.Validate(); err == nil {
			t.Fatalf("%q must be rejected", spec)
		}
	}
}

func TestValidateInterrupt(t *testing.T) {
	s := baseScenario(DamageStep{Shell: "rm -rf x"})
	s.Restore.Interrupt = &InterruptSpec{KillAfterMS: 300}
	if err := s.Validate(); err != nil {
		t.Fatalf("a positive kill_after_ms must pass: %v", err)
	}

	s.Restore.Interrupt = &InterruptSpec{}
	if err := s.Validate(); err == nil {
		t.Fatal("kill_after_ms <= 0 must be rejected")
	}

	s.Restore.Interrupt = nil
	s.Restore.ConcurrentWrite = "mkdir -p live; exec 9>live/w.lock; flock 9"
	if err := s.Validate(); err == nil {
		t.Fatal("concurrent_write without a .barriers/ lock must be rejected")
	}

	s.Restore.ConcurrentWrite = "exec 9>.barriers/writer.lock; flock 9"
	if err := s.Validate(); err != nil {
		t.Fatalf("a cooperating writer must pass: %v", err)
	}
}

func TestParseBarrier(t *testing.T) {
	op, name, err := ParseBarrier("wait-held w.lock")
	if err != nil || op != BarrierWaitHeld || name != "w.lock" {
		t.Fatalf("op=%q name=%q err=%v", op, name, err)
	}
	if _, _, err := ParseBarrier("wait"); err == nil {
		t.Fatal("a barrier without a name must be rejected")
	}
}
