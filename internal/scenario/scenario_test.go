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

func TestValidateCheckpointPre(t *testing.T) {
	ok := baseScenario(DamageStep{Shell: "rm -rf x"})

	ok.Checkpoint.Pre = []DamageStep{{Shell: "mkdir -p .fill"}}

	if err := ok.Validate(); err != nil {
		t.Fatalf("a shell pre step must pass: %v", err)
	}

	for _, step := range []DamageStep{
		{},
		{PostEdit: "true"},
		{Parallel: [][]DamageStep{{{Shell: "true"}}}},
	} {
		s := baseScenario(DamageStep{Shell: "rm -rf x"})
		s.Checkpoint.Pre = []DamageStep{step}

		err := s.Validate()
		if err == nil {
			t.Fatalf("pre step %+v must be rejected", step)
		}

		if !strings.Contains(err.Error(), "checkpoint.pre[0]") {
			t.Fatalf("the error must name the step, got: %v", err)
		}
	}

	nonShell := baseScenario(DamageStep{Shell: "rm -rf x"})
	nonShell.Checkpoint.Pre = []DamageStep{{Shell: "true"}, {PostEdit: "true"}}

	err := nonShell.Validate()
	if err == nil || !strings.Contains(err.Error(), "checkpoint.pre[1]: only shell steps are allowed before the checkpoint") {
		t.Fatalf("a non-shell pre step must be rejected, got: %v", err)
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

func TestValidateRepeat(t *testing.T) {
	ok := baseScenario(DamageStep{Shell: "echo {{i}}", Repeat: 3})

	ok.Checkpoint.Repeat = 200

	if err := ok.Validate(); err != nil {
		t.Fatalf("positive repeats must pass: %v", err)
	}

	bad := baseScenario(DamageStep{Shell: "true", Repeat: -1})
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "repeat must be >= 1") {
		t.Fatalf("a negative step repeat must be rejected, got: %v", err)
	}

	badCP := baseScenario(DamageStep{Shell: "true"})

	badCP.Checkpoint.Repeat = -1

	if err := badCP.Validate(); err == nil || !strings.Contains(err.Error(), "checkpoint.repeat") {
		t.Fatalf("a negative checkpoint repeat must be rejected, got: %v", err)
	}

	nested := baseScenario(DamageStep{Parallel: [][]DamageStep{{{Shell: "true", Repeat: -2}}}})
	if err := nested.Validate(); err == nil {
		t.Fatal("a negative repeat in a parallel branch must be rejected")
	}
}

func TestValidateParallelRepeat(t *testing.T) {
	group := baseScenario(DamageStep{
		Repeat:   2,
		Parallel: [][]DamageStep{{{Shell: "true"}}},
	})

	err := group.Validate()
	if err == nil || !strings.Contains(err.Error(), "repeat is supported only on shell/post_edit/toolcall steps") {
		t.Fatalf("a repeated parallel group must be rejected, got: %v", err)
	}

	branch := baseScenario(DamageStep{Parallel: [][]DamageStep{
		{{Shell: "true"}},
		{{Shell: "true", Repeat: 2}},
	}})

	err = branch.Validate()
	if err == nil || !strings.Contains(err.Error(), "parallel branch 1 step 0: repeat is supported only on shell/post_edit/toolcall steps") {
		t.Fatalf("a repeated nested step must be rejected, got: %v", err)
	}

	ok := baseScenario(DamageStep{Repeat: 1, Parallel: [][]DamageStep{{{Shell: "true", Repeat: 1}}}})
	if err := ok.Validate(); err != nil {
		t.Fatalf("repeat of 0/1 must stay valid in a parallel group: %v", err)
	}
}

func TestLoadCostScenarios(t *testing.T) {
	for _, id := range []string{"S19", "S20"} {
		s, err := Load(filepath.Join("..", "..", "scenarios", id+".yaml"))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}

		if s.Group != "cost" || s.Fixture != "big_tree" {
			t.Fatalf("%s: group=%q fixture=%q", id, s.Group, s.Fixture)
		}
	}
}
