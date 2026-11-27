package oracle

import (
	"reflect"
	"sort"
	"testing"

	"github.com/saromanov/unlucky-agent/internal/manifest"
)

func entry(path, kind string) manifest.Entry {
	return manifest.Entry{
		Root:   "work",
		Path:   path,
		Type:   kind,
		Mode:   0o644,
		UID:    1000,
		GID:    1000,
		Size:   4096,
		Blocks: 8,
		NLink:  1,
		Hash:   "hash-" + path,
	}
}

func diffCaseMatrix() []struct {
	name string
	base []manifest.Entry
	obs  []manifest.Entry
	pol  Policy
} {
	dirA := entry("work/dir/a.txt", "file")
	dirB := entry("work/dir/b.txt", "file")
	dirC := entry("work/dir/c.txt", "file")
	dirD := entry("work/dir/d", "dir")
	dirE := entry("work/dir/e.txt", "file")
	dirF := entry("work/dir/f.txt", "file")
	gitFile := entry("work/.git/index", "file")

	changedB := dirB
	changedB.Hash = "different"
	metaC := dirC
	metaC.Mode = 0o600
	nlinkD := dirD
	nlinkD.NLink = 3

	addedNew := entry("work/dir/new.txt", "file")
	addedExtra := entry("work/dir/zzz.txt", "file")
	addedGit := entry("work/.git/refs", "file")

	pol := Policy{
		Exclude:    []string{"work/.git/**"},
		AllowAdded: []string{"work/dir/new.txt"},
	}
	expectedOnly := Policy{Expected: []string{"work/dir/**"}}

	base := []manifest.Entry{dirA, dirB, dirC, dirD, dirE, dirF, gitFile}
	obs := []manifest.Entry{
		dirA, changedB, metaC, nlinkD, dirF, gitFile, addedNew, addedExtra, addedGit,
	}

	shuffledBase := []manifest.Entry{dirF, dirA, gitFile, dirD, dirC, dirE, dirB}
	shuffledObs := []manifest.Entry{addedGit, metaC, dirF, addedExtra, dirA, nlinkD, addedNew, changedB, gitFile}

	return []struct {
		name string
		base []manifest.Entry
		obs  []manifest.Entry
		pol  Policy
	}{
		{"empty", nil, nil, Policy{}},
		{"observed-only", nil, obs[:5], pol},
		{"base-only", base, nil, pol},
		{"baseline", base, base, pol},
		{"added-missing-changed-meta", base, obs, pol},
		{"expected-policy", base, obs, expectedOnly},
		{"shuffled-both", shuffledBase, shuffledObs, pol},
		{"shuffled-base-only", shuffledBase, obs, pol},
		{"shuffled-observed-only", base, shuffledObs, pol},
	}
}

func runDiffMatrix(t *testing.T) {
	t.Helper()

	for _, c := range diffCaseMatrix() {
		t.Run(c.name, func(t *testing.T) {
			basePath := writeManifest(t, c.base...)
			obsPath := writeManifest(t, c.obs...)

			got, err := DiffFiles(basePath, obsPath, c.pol)
			if err != nil {
				t.Fatal(err)
			}

			want := Diff(sortedCopy(c.base), sortedCopy(c.obs), c.pol)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("streaming result differs from the in-memory diff:\ngot  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestDiffFilesMatchesDiff(t *testing.T) {
	runDiffMatrix(t)
}

func TestDiffFilesMatchesDiffSmallChunks(t *testing.T) {
	saved := manifest.SortChunkEntries

	manifest.SortChunkEntries = 3

	defer func() { manifest.SortChunkEntries = saved }()

	runDiffMatrix(t)
}

func TestDiffFilesExternalSortFallback(t *testing.T) {
	base := []manifest.Entry{
		entry("work/dir/c.txt", "file"),
		entry("work/dir/a.txt", "file"),
		entry("work/dir/b.txt", "file"),
	}
	obs := []manifest.Entry{
		entry("work/dir/b.txt", "file"),
		entry("work/dir/a.txt", "file"),
		entry("work/dir/z.txt", "file"),
	}

	saved := manifest.SortChunkEntries

	manifest.SortChunkEntries = 2

	defer func() { manifest.SortChunkEntries = saved }()

	got, err := DiffFiles(writeManifest(t, base...), writeManifest(t, obs...), Policy{})
	if err != nil {
		t.Fatal(err)
	}

	want := Diff(sortedCopy(base), sortedCopy(obs), Policy{})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback result differs from the in-memory diff:\ngot  %+v\nwant %+v", got, want)
	}
}

func sortedCopy(entries []manifest.Entry) []manifest.Entry {
	out := make([]manifest.Entry, len(entries))
	copy(out, entries)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	return out
}
