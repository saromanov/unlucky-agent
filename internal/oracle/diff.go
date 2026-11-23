package oracle

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

// Change kinds.
const (
	KindMissing   = "missing"
	KindChanged   = "changed"
	KindMeta      = "meta"
	KindAdded     = "added"
	KindClobbered = "clobbered"
)

// BlocksTolerance is the allowed st_blocks drift, in 512-byte blocks.
// It is 32 KiB: enough to ignore filesystem allocation noise, small enough to
// catch a densified multi-gigabyte sparse file.
const BlocksTolerance = 64

// Change is a single divergence.
type Change struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Result is the comparison outcome.
type Result struct {
	Missing   []Change `json:"missing"`
	Changed   []Change `json:"changed"`
	Meta      []Change `json:"meta"`
	Added     []Change `json:"added"`
	Clobbered []Change `json:"clobbered"`
	Expected  int      `json:"expected"`
	Matched   int      `json:"matched"`
	Skipped   int      `json:"skipped"`
}

// Recall is the fraction of expected paths that were restored.
func (r *Result) Recall() float64 {
	if r.Expected == 0 {
		return 1
	}
	return float64(r.Matched) / float64(r.Expected)
}

// MetaScore is the fraction of paths with matching metadata.
func (r *Result) MetaScore() float64 {
	if r.Expected == 0 {
		return 1
	}
	return float64(r.Expected-len(r.Meta)) / float64(r.Expected)
}

// Pass reports whether all hard gates hold.
func (r *Result) Pass() bool {
	return len(r.Missing) == 0 && len(r.Changed) == 0 && len(r.Clobbered) == 0
}

// DiffFiles compares the expected manifest with the observed one.
func DiffFiles(expectedPath, observedPath string, pol Policy) (*Result, error) {
	base, err := loadEntries(expectedPath)
	if err != nil {
		return nil, fmt.Errorf("expected manifest: %w", err)
	}
	obs, err := loadEntries(observedPath)
	if err != nil {
		return nil, fmt.Errorf("observed manifest: %w", err)
	}
	return Diff(base, obs, pol), nil
}

// Diff compares two entry sets.
func Diff(base, obs []manifest.Entry, pol Policy) *Result {
	index := make(map[string]manifest.Entry, len(obs))
	for _, e := range obs {
		index[e.Path] = e
	}

	res := &Result{}
	for _, b := range base {
		if pol.Excluded(b.Path) {
			res.Skipped++
			continue
		}
		if !pol.Wants(b.Path) {
			continue
		}
		res.Expected++

		o, ok := index[b.Path]
		if !ok {
			res.Missing = append(res.Missing, Change{Path: b.Path, Kind: KindMissing, Detail: b.Type})
			continue
		}
		if b.Type != o.Type || b.Hash != o.Hash || b.Link != o.Link {
			res.Changed = append(res.Changed, Change{
				Path:   b.Path,
				Kind:   KindChanged,
				Detail: fmt.Sprintf("type %s->%s hash %s->%s", b.Type, o.Type, short(b.Hash), short(o.Hash)),
			})
			continue
		}
		detail := ""
		if b.Mode != o.Mode || b.UID != o.UID || b.GID != o.GID || !xattrEqual(b.Xattr, o.Xattr) {
			detail = fmt.Sprintf("mode %04o->%04o uid %d->%d gid %d->%d", b.Mode, o.Mode, b.UID, o.UID, b.GID, o.GID)
		}
		if b.NLink != o.NLink && b.Type != "dir" {
			// A directory nlink is 2 plus its subdirectory count, so it drops
			// whenever a child directory is missing. That loss is already a
			// missing defect; only file nlink carries hardlink information.
			detail = appendDetail(detail, fmt.Sprintf("nlink %d->%d", b.NLink, o.NLink))
		}
		if blocksDrift(b.Blocks, o.Blocks) {
			detail = appendDetail(detail, fmt.Sprintf("blocks %d->%d", b.Blocks, o.Blocks))
		}
		if detail != "" {
			res.Meta = append(res.Meta, Change{Path: b.Path, Kind: KindMeta, Detail: detail})
			continue
		}
		res.Matched++
	}

	known := make(map[string]struct{}, len(base))
	for _, b := range base {
		known[b.Path] = struct{}{}
	}
	for _, o := range obs {
		if pol.Excluded(o.Path) {
			continue
		}
		if _, ok := known[o.Path]; ok {
			continue
		}
		if pol.AddedAllowed(o.Path) {
			continue
		}
		res.Added = append(res.Added, Change{Path: o.Path, Kind: KindAdded, Detail: o.Type})
	}
	return res
}

// Clobber compares post-T edits with the restored state and returns
// paths whose content was destroyed by the rollback.
func Clobber(postEditPath, restoredPath string, pol Policy) ([]Change, error) {
	post, err := loadEntries(postEditPath)
	if err != nil {
		return nil, err
	}
	restored, err := loadEntries(restoredPath)
	if err != nil {
		return nil, err
	}
	index := make(map[string]manifest.Entry, len(restored))
	for _, e := range restored {
		index[e.Path] = e
	}

	var out []Change
	for _, p := range post {
		if pol.Excluded(p.Path) || !pol.Preserved(p.Path) {
			continue
		}
		o, ok := index[p.Path]
		switch {
		case !ok:
			out = append(out, Change{Path: p.Path, Kind: KindClobbered, Detail: "removed by rollback"})
		case o.Hash != p.Hash || o.Type != p.Type:
			out = append(out, Change{Path: p.Path, Kind: KindClobbered, Detail: "rollback overwrote the post-T edit"})
		}
	}
	return out, nil
}

// LoadEntries reads a whole manifest.
func LoadEntries(path string) ([]manifest.Entry, error) { return loadEntries(path) }

func loadEntries(path string) ([]manifest.Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	var hdr manifest.Header
	if err := dec.Decode(&hdr); err != nil {
		return nil, fmt.Errorf("%s: header: %w", path, err)
	}
	var out []manifest.Entry
	for {
		var e manifest.Entry
		err := dec.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, e)
	}
	return out, nil
}

func xattrEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// appendDetail joins metadata defect fragments with a single space.
func appendDetail(detail, part string) string {
	if detail == "" {
		return part
	}
	return detail + " " + part
}

// blocksDrift reports whether the allocated block counts diverge beyond the
// noise tolerance.
func blocksDrift(base, observed int64) bool {
	diff := observed - base
	if diff < 0 {
		diff = -diff
	}
	return diff > BlocksTolerance
}
