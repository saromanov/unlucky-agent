// Package baseline implements reference rollback strategies.
package baseline

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Names lists the supported baselines.
func Names() []string { return []string{"none", "cp-r"} }

// Snapshot copies the whole tree.
func Snapshot(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return copyTree(src, dst)
}

// RestoreExact restores the tree from the snapshot and removes extra files.
// This is the obvious baseline that clobbers edits made after the snapshot.
func RestoreExact(snapshot, dst string) error {
	if _, err := os.Stat(snapshot); err != nil {
		return fmt.Errorf("baseline: snapshot: %w", err)
	}
	if err := removeExtras(snapshot, dst); err != nil {
		return err
	}
	return copyTree(snapshot, dst)
}

func removeExtras(snapshot, dst string) error {
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

func copyTree(src, dst string) error {
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
