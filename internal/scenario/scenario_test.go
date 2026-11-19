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
