// Package manifest records a file-level description of a tree as JSON Lines.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// Schema is the manifest schema version.
	Schema = 1
	// HashAlgorithm is the content hash algorithm.
	HashAlgorithm = "sha256"
)

// Header is the first line of a manifest file.
type Header struct {
	Manifest int    `json:"manifest"`
	Schema   int    `json:"schema"`
	Root     string `json:"root"`
	Platform string `json:"platform"`
	Hash     string `json:"hash"`
	Xattr    bool   `json:"xattr"`
}

// Entry describes a single tree element.
type Entry struct {
	Root    string            `json:"root"`
	Path    string            `json:"path"`
	Type    string            `json:"type"`
	Mode    uint32            `json:"mode"`
	UID     uint32            `json:"uid"`
	GID     uint32            `json:"gid"`
	Size    int64             `json:"size"`
	Blocks  int64             `json:"blocks"`
	NLink   uint64            `json:"nlink"`
	Hash    string            `json:"hash,omitempty"`
	MtimeNS int64             `json:"mtime_ns"`
	Link    string            `json:"link,omitempty"`
	Xattr   map[string]string `json:"xattr,omitempty"`
}

// Stats reports capture totals.
type Stats struct {
	Entries int
	Bytes   int64
	Xattr   bool
}

// Root is one capture root. The root name is part of the recorded path: "work/src/main.go".
type Root struct {
	Name string
	Dir  string
}

// Capture walks roots without following symlinks and writes a manifest to w.
// Entries are sorted by path, so the output is deterministic.
func Capture(roots []Root, w io.Writer) (Stats, error) {
	if len(roots) == 0 {
		return Stats{}, errors.New("manifest: no roots given")
	}

	absRoots := make([]Root, 0, len(roots))
	names := make([]string, 0, len(roots))
	xattrOK := true

	for _, r := range roots {
		abs, err := filepath.Abs(r.Dir)
		if err != nil {
			return Stats{}, err
		}

		info, err := os.Stat(abs)
		if err != nil {
			return Stats{}, err
		}

		if !info.IsDir() {
			return Stats{}, fmt.Errorf("manifest: %s is not a directory", abs)
		}

		if !probeXattr(abs) {
			xattrOK = false
		}

		absRoots = append(absRoots, Root{Name: r.Name, Dir: abs})
		names = append(names, r.Name)
	}

	hdr := Header{
		Manifest: 1,
		Schema:   Schema,
		Root:     strings.Join(names, ","),
		Platform: runtime.GOOS,
		Hash:     HashAlgorithm,
		Xattr:    xattrOK,
	}

	var stats Stats

	stats.Xattr = xattrOK

	sorter := newEntrySorter(SortChunkEntries)
	defer sorter.cleanup()

	for _, r := range absRoots {
		abs := r.Dir

		err := filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			if p == abs {
				return nil
			}

			rel, err := filepath.Rel(abs, p)
			if err != nil {
				return err
			}

			e, err := entryFor(r.Name, filepath.ToSlash(filepath.Join(r.Name, rel)), p, xattrOK)
			if err != nil {
				return err
			}

			if err := sorter.add(e); err != nil {
				return err
			}

			stats.Entries++
			stats.Bytes += e.Size

			return nil
		})
		if err != nil {
			return Stats{}, err
		}
	}

	if err := sorter.write(w, hdr); err != nil {
		return Stats{}, err
	}

	return stats, nil
}

func entryFor(rootName, rel, full string, xattrOK bool) (Entry, error) {
	info, err := os.Lstat(full)
	if err != nil {
		return Entry{}, err
	}

	e := Entry{
		Root:    rootName,
		Path:    rel,
		Type:    typeName(info.Mode()),
		Mode:    uint32(info.Mode().Perm()),
		Size:    info.Size(),
		MtimeNS: info.ModTime().UnixNano(),
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.UID = st.Uid
		e.GID = st.Gid
		e.Blocks = st.Blocks
		e.NLink = statValue(st.Nlink)
		e.Mode = st.Mode & 0o7777
	}

	switch {
	case info.Mode().IsRegular():
		h, err := hashFile(full)
		if err != nil {
			return Entry{}, err
		}

		e.Hash = h
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return Entry{}, err
		}

		e.Link = target
		e.Size = 0
	}

	if xattrOK && info.Mode()&os.ModeSymlink == 0 {
		attrs, err := readXattr(full)
		if err == nil && len(attrs) > 0 {
			e.Xattr = attrs
		}
	}

	return e, nil
}

func statValue[T ~uint32 | ~uint64](v T) uint64 { return uint64(v) }

func typeName(m os.FileMode) string {
	switch {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "dir"
	case m&os.ModeSymlink != 0:
		return "symlink"
	case m&os.ModeNamedPipe != 0:
		return "fifo"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeCharDevice != 0:
		return "chardev"
	case m&os.ModeDevice != 0:
		return "blockdev"
	default:
		return "other"
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}

	defer f.Close()

	h := sha256.New()

	buf := make([]byte, 1<<20)

	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func probeXattr(path string) bool {
	_, err := unix.Listxattr(path, nil)
	if err == nil {
		return true
	}

	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		return false
	}
	// EBADF/ENODATA and similar errors do not mean missing support.
	return true
}

func readXattr(path string) (map[string]string, error) {
	sz, err := unix.Listxattr(path, nil)
	if err != nil {
		return nil, err
	}

	if sz == 0 {
		return nil, nil
	}

	buf := make([]byte, sz)

	n, err := unix.Listxattr(path, buf)
	if err != nil {
		return nil, err
	}

	names := strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00")

	out := make(map[string]string, len(names))

	for _, name := range names {
		if name == "" {
			continue
		}

		val, err := getXattr(path, name)
		if err != nil {
			continue
		}

		out[name] = val
	}

	if len(out) == 0 {
		return nil, nil
	}

	return out, nil
}

func getXattr(path, name string) (string, error) {
	sz, err := unix.Getxattr(path, name, nil)
	if err != nil {
		return "", err
	}

	buf := make([]byte, sz)

	n, err := unix.Getxattr(path, name, buf)
	if err != nil {
		return "", err
	}

	return string(buf[:n]), nil
}
