package oracle

import (
	"path"
	"path/filepath"
	"strings"
)

// Policy defines manifest comparison rules.
type Policy struct {
	Exclude     []string `json:"exclude"`
	Preserve    []string `json:"preserve"`
	AllowAdded  []string `json:"allow_added"`
	Expected    []string `json:"expected"`
	RequireMeta bool     `json:"require_meta"`
}

// Excluded reports whether the path is excluded from comparison.
func (p Policy) Excluded(rel string) bool {
	for _, g := range p.Exclude {
		if Match(g, rel) {
			return true
		}
	}
	return false
}

// Preserved reports whether the path is an edit made after T.
func (p Policy) Preserved(rel string) bool {
	for _, g := range p.Preserve {
		if Match(g, rel) {
			return true
		}
	}
	return false
}

// Wants reports whether the path is expected after restore.
func (p Policy) Wants(rel string) bool {
	if len(p.Expected) == 0 {
		return true
	}
	for _, g := range p.Expected {
		if Match(g, rel) {
			return true
		}
	}
	return false
}

// AddedAllowed reports whether a new path is allowed.
func (p Policy) AddedAllowed(rel string) bool {
	for _, g := range p.AllowAdded {
		if Match(g, rel) {
			return true
		}
	}
	return false
}

// Match matches a path against a glob. ** matches any number of segments.
func Match(pattern, p string) bool {
	ps := split(pattern)
	xs := split(p)
	return matchSegments(ps, xs)
}

func split(s string) []string {
	s = strings.Trim(filepath.ToSlash(s), "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

func matchSegments(ps, xs []string) bool {
	if len(ps) == 0 {
		return len(xs) == 0
	}
	if ps[0] == "**" {
		for i := 0; i <= len(xs); i++ {
			if matchSegments(ps[1:], xs[i:]) {
				return true
			}
		}
		return false
	}
	if len(xs) == 0 {
		return false
	}
	ok, err := path.Match(ps[0], xs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(ps[1:], xs[1:])
}
