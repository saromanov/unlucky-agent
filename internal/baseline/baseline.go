// Package baseline implements reference rollback strategies.
package baseline

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Names lists the supported baselines.
func Names() []string { return []string{"none", "cp-r", "perfect"} }

// Snapshot copies the whole tree.
func Snapshot(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyTree(src, dst, nil)
}

// PerfectSnapshot snapshots both the work and the home tree at the checkpoint.
func PerfectSnapshot(work, home, snapWork, snapHome string) error {
	if err := Snapshot(work, snapWork); err != nil {
		return err
	}
	return Snapshot(home, snapHome)
}

// PerfectRestore restores both trees byte-for-byte and keeps post-T edits.
// Preserved paths are manifest-style paths rooted at "work/" or "home/".
// Entries with any other prefix are ignored for both trees.
func PerfectRestore(snapWork, snapHome, work, home string, preserved []string) error {
	keepWork := make(keepSet)
	keepHome := make(keepSet)
	for _, p := range preserved {
		if rel, ok := strings.CutPrefix(p, "work/"); ok {
			addKeep(keepWork, rel)
			continue
		}
		if rel, ok := strings.CutPrefix(p, "home/"); ok {
			addKeep(keepHome, rel)
		}
	}
	trees := []struct {
		snapshot string
		dst      string
		keep     keepSet
	}{
		{snapWork, work, keepWork},
		{snapHome, home, keepHome},
	}
	for _, t := range trees {
		if _, err := os.Stat(t.snapshot); err != nil {
			return fmt.Errorf("baseline: snapshot: %w", err)
		}
		if err := removeExtras(t.snapshot, t.dst, t.keep); err != nil {
			return err
		}
		if err := copyTree(t.snapshot, t.dst, t.keep); err != nil {
			return err
		}
	}
	return nil
}

// RestoreExact restores the tree from the snapshot and removes extra files.
// This is the obvious baseline that clobbers edits made after the snapshot.
func RestoreExact(snapshot, dst string) error {
	if _, err := os.Stat(snapshot); err != nil {
		return fmt.Errorf("baseline: snapshot: %w", err)
	}
	if err := removeExtras(snapshot, dst, nil); err != nil {
		return err
	}
	return copyTree(snapshot, dst, nil)
}

// keepSet holds tree-relative paths a restore must not delete or overwrite.
type keepSet map[string]struct{}

func addKeep(set keepSet, rel string) {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || rel == "" {
		return
	}
	set[rel] = struct{}{}
}

// contains reports whether rel is preserved exactly.
func (k keepSet) contains(rel string) bool {
	_, ok := k[rel]
	return ok
}

// protects reports whether rel is preserved or is an ancestor of a preserved
// path: a post-edit file may live in a directory the snapshot does not have.
func (k keepSet) protects(rel string) bool {
	if len(k) == 0 {
		return false
	}
	if k.contains(rel) {
		return true
	}
	prefix := rel + string(filepath.Separator)
	for p := range k {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func removeExtras(snapshot, dst string, keep keepSet) error {
	var extras []string
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if keep.protects(rel) {
			return nil
		}
		if _, err := os.Lstat(filepath.Join(snapshot, rel)); err != nil {
			if os.IsNotExist(err) {
				extras = append(extras, p)
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range extras {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string, keep keepSet) error {
	type dirMeta struct {
		path string
		mode os.FileMode
		at   time.Time
	}
	var dirs []dirMeta

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			// Directory modes are applied after copying, otherwise read-only
			// directories (for example .git/objects) block writes.
			if err := os.Chmod(target, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{path: target, mode: info.Mode().Perm(), at: info.ModTime()})
			return nil
		case keep.contains(rel):
			// A preserved post-T edit must survive the restore untouched.
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			if err := copyFile(p, target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		default:
			return fmt.Errorf("baseline: unsupported file type %s", p)
		}
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
		if err := os.Chtimes(dirs[i].path, dirs[i].at, dirs[i].at); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// An existing file may be read-only (git objects), so it is removed first.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
