package oracle

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"work/src/**", "work/src/main.go", true},
		{"work/src/**", "work/src/a/b/c.go", true},
		{"work/src/**", "work/docs/guide.md", false},
		{"work/*.md", "work/README.md", true},
		{"work/*.md", "work/docs/README.md", false},
		{"**/.env", "work/.env", true},
		{"**/.env", "work/src/.env", true},
		{"work/.git/logs/**", "work/.git/logs/HEAD", true},
		{"work/.git/logs/**", "work/.git/index", false},
		{"work/**/*.go", "work/src/util.go", true},
		{"work/**/*.go", "work/util.go", true},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.path); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestPolicyExcluded(t *testing.T) {
	p := Policy{Exclude: []string{"work/.git/index", "work/.git/logs/**"}}
	if !p.Excluded("work/.git/index") {
		t.Error("work/.git/index must be excluded")
	}
	if p.Excluded("work/src/main.go") {
		t.Error("work/src/main.go must not be excluded")
	}
}
