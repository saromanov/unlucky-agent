// Package fixture builds deterministic working trees for scenarios.
package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// FixedMtime is the modification time of every fixture file.
var FixedMtime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type file struct {
	path string
	body string
	mode os.FileMode
}

// Known reports whether the fixture is registered.
func Known(name string) bool {
	switch name {
	case "project_basic", "project_git", "project_git_dirty", "project_git_shadow",
		"large_files", "special_files", "path_edges", "big_tree":
		return true
	}
	return false
}

// Build materializes the fixture into work and home.
func Build(name string, seed int64, work, home string) error {
	if !Known(name) {
		return fmt.Errorf("unknown fixture %q", name)
	}
	for _, d := range []string{work, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// Edge-case fixtures own their whole work tree and carry no project files.
	switch name {
	case "large_files":
		return buildLargeFiles(work, seed)
	case "special_files":
		return buildSpecialFiles(work)
	case "path_edges":
		return buildPathEdges(work)
	case "big_tree":
		return buildBigTree(work, seed)
	}

	files := []file{
		{"README.md", "# demo project\n\nunlucky-agent fixture.\n", 0o644},
		{"src/main.go", "package main\n\nfunc main() { println(\"demo\") }\n", 0o644},
		{"src/util.go", "package main\n\nfunc add(a, b int) int { return a + b }\n", 0o644},
		{"src/local.settings.json", "{\"debug\": true}\n", 0o600},
		{"docs/guide.md", "# guide\n", 0o644},
		{"scripts/run.sh", "#!/bin/sh\necho run\n", 0o755},
		{".env", "TOKEN=fixture\n", 0o600},
		{".gitignore", ".env\n*.local\n", 0o644},
	}
	homeFiles := []file{
		{".config/app/config.toml", "debug = true\n", 0o600},
		{".ssh/known_hosts", "example.com ssh-ed25519 AAAA\n", 0o600},
	}
	for _, f := range files {
		if err := write(work, f); err != nil {
			return err
		}
	}
	for _, f := range homeFiles {
		if err := write(home, f); err != nil {
			return err
		}
	}

	if name == "project_git" || name == "project_git_dirty" || name == "project_git_shadow" {
		if err := gitInit(work, home, seed); err != nil {
			return err
		}
	}
	if name == "project_git_shadow" {
		if err := buildGitShadow(work, home, files, seed); err != nil {
			return err
		}
	}
	if name == "project_git_dirty" {
		if err := gitDirty(work, home); err != nil {
			return err
		}
	}
	return nil
}

// buildGitShadow mirrors the project into work/.git-shadow with its own
// repository, so a shadow-based tool has a second copy to keep consistent.
func buildGitShadow(work, home string, files []file, seed int64) error {
	shadow := filepath.Join(work, ".git-shadow")
	for _, f := range files {
		if err := write(shadow, f); err != nil {
			return err
		}
	}
	return gitSeed(shadow, home, fmt.Sprintf("shadow seed=%d", seed))
}

func write(root string, f file) error {
	full := filepath.Join(root, filepath.FromSlash(f.path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, []byte(f.body), f.mode); err != nil {
		return err
	}
	// WriteFile applies umask, so the mode is set explicitly.
	if err := os.Chmod(full, f.mode); err != nil {
		return err
	}
	return os.Chtimes(full, FixedMtime, FixedMtime)
}

func gitInit(work, home string, seed int64) error {
	return gitSeed(work, home, fmt.Sprintf("fixture seed=%d", seed))
}

func gitSeed(dir, home, message string) error {
	gitconfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Fixture\n\temail = fixture@example.com\n"), 0o600); err != nil {
		return err
	}
	env := gitEnv(home)
	steps := [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "README.md", "src/main.go", "src/util.go", "docs/guide.md", "scripts/run.sh", ".gitignore"},
		{"commit", "-q", "-m", message},
	}
	for _, args := range steps {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
	}
	return nil
}

// gitDirty applies uncommitted edits to a git fixture: one staged edit, one
// staged new file and one unstaged edit.
func gitDirty(work, home string) error {
	edits := []file{
		// Unstaged edit: never added, so `git reset --hard` reverts it.
		{"src/util.go", "package main\n\nfunc add(a, b int) int { return a + b }\n\n// local uncommitted edit\n", 0o644},
		// Staged edit: both the index and the worktree differ from HEAD.
		{"src/main.go", "package main\n\nfunc main() { println(\"demo, edited\") }\n", 0o644},
		// Staged new file: `git reset --hard` deletes it.
		{"src/extra.go", "package main\n\n// staged new file, never committed\n", 0o644},
	}
	for _, f := range edits {
		if err := write(work, f); err != nil {
			return err
		}
	}

	args := []string{"add", "src/main.go", "src/extra.go"}
	cmd := exec.Command("git", args...)
	cmd.Dir = work
	cmd.Env = gitEnv(home)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}

// gitEnv returns the environment shared by git calls: a fixed identity, fixed
// dates and a home-local config, so the fixture stays deterministic.
func gitEnv(home string) []string {
	return append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		"TZ=UTC",
	)
}

// Sizes of the large_files fixture.
const (
	largeFileSize  = int64(2 << 30) // 2 GiB
	denseBufLen    = 1 << 20        // 1 MiB write buffer
	sparseBlockLen = 4 << 10        // 4 KiB data islands
)

// buildLargeFiles creates one dense 2 GiB file and one 2 GiB sparse file.
func buildLargeFiles(work string, seed int64) error {
	dir := filepath.Join(work, "big")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeDenseFile(filepath.Join(dir, "regular.bin"), seed); err != nil {
		return err
	}
	if err := writeSparseFile(filepath.Join(dir, "sparse.bin"), seed); err != nil {
		return err
	}
	return fixDirTimes(dir)
}

// writeDenseFile writes a deterministic 2 GiB file: a repeating seed-derived
// buffer with the buffer offset in its first eight bytes.
func writeDenseFile(path string, seed int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, denseBufLen)
	sum := sha256.Sum256([]byte(fmt.Sprintf("large_files:%d", seed)))
	for i := 0; i < len(buf); i += len(sum) {
		copy(buf[i:], sum[:])
	}
	for off := int64(0); off < largeFileSize; off += denseBufLen {
		binary.LittleEndian.PutUint64(buf[:8], uint64(off))
		n := int64(denseBufLen)
		if rest := largeFileSize - off; rest < n {
			n = rest
		}
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, FixedMtime, FixedMtime)
}

// writeSparseFile writes two data islands and truncates the file to 2 GiB, so
// the blocks in between stay holes.
func writeSparseFile(path string, seed int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	block := make([]byte, sparseBlockLen)
	sum := sha256.Sum256([]byte(fmt.Sprintf("large_files:sparse:%d", seed)))
	for i := 0; i < len(block); i += len(sum) {
		copy(block[i:], sum[:])
	}
	for _, off := range []int64{0, largeFileSize / 2} {
		if _, err := f.WriteAt(block, off); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Truncate(largeFileSize); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, FixedMtime, FixedMtime)
}

// buildSpecialFiles creates permission, link, fifo and socket edge cases.
func buildSpecialFiles(work string) error {
	dir := filepath.Join(work, "spec")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []file{
		{"spec/ro.txt", "read only\n", 0o400},
		{"spec/rwx.sh", "#!/bin/sh\necho run\n", 0o755},
		{"spec/suid.sh", "#!/bin/sh\necho suid\n", 0o755},
		{"spec/hard_a", "hardlink target\n", 0o644},
	} {
		if err := write(work, f); err != nil {
			return err
		}
	}
	// os.FileMode carries the setuid bit as ModeSetuid, not as 0o4000.
	if err := os.Chmod(filepath.Join(dir, "suid.sh"), 0o755|os.ModeSetuid); err != nil {
		return err
	}
	// hard_b shares the inode with hard_a; losing it means losing nlink.
	if err := os.Link(filepath.Join(dir, "hard_a"), filepath.Join(dir, "hard_b")); err != nil {
		return err
	}

	for _, l := range []struct{ name, target string }{
		{"link_ok", "rwx.sh"},
		{"link_broken", "missing/target"},
		{"cyc_a", "cyc_b"},
		{"cyc_b", "cyc_a"},
	} {
		p := filepath.Join(dir, l.name)
		if err := os.Symlink(l.target, p); err != nil {
			return err
		}
		if err := chtimesNoFollow(p); err != nil {
			return err
		}
	}

	fifo := filepath.Join(dir, "fifo.pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return err
	}
	if err := os.Chtimes(fifo, FixedMtime, FixedMtime); err != nil {
		return err
	}

	sock := filepath.Join(dir, "sock.unix")
	if err := bindUnixSocket(sock); err != nil {
		return err
	}
	if err := os.Chtimes(sock, FixedMtime, FixedMtime); err != nil {
		return err
	}

	// The setgid directory is chmodded after MkdirAll, since the mode must
	// survive as an explicit permission bit.
	sgid := filepath.Join(dir, "sgid")
	if err := os.MkdirAll(sgid, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(sgid, 0o755|os.ModeSetgid); err != nil {
		return err
	}
	return fixDirTimes(dir)
}

// bindUnixSocket creates a bound AF_UNIX socket file without listening, so the
// node stays on disk.
func bindUnixSocket(path string) error {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("bind %s: %w", path, err)
	}
	return syscall.Close(fd)
}

// chtimesNoFollow sets the symlink timestamps themselves, not the target.
func chtimesNoFollow(path string) error {
	ts := []unix.Timespec{
		unix.NsecToTimespec(FixedMtime.UnixNano()),
		unix.NsecToTimespec(FixedMtime.UnixNano()),
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}

// Path edge fixture parameters.
const (
	manyFiles      = 50000
	deepNameLen    = 200
	deepPathBudget = 3990 // absolute path budget: below PATH_MAX with staging slack
)

// buildPathEdges creates path-name, deep-path and file-count edge cases.
func buildPathEdges(work string) error {
	dir := filepath.Join(work, "edge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	names := []string{
		"space name.txt",
		"tab\tname.txt",
		"line\nbreak.txt",
		"single'quote.txt",
		"double\"quote.txt",
		`back\slash.txt`,
		"-leading-dash.txt",
		"--spacer.txt",
		"日本語.txt",
		"caf\u00e9-NFC.txt",  // NFC: U+00E9
		"cafe\u0301-NFD.txt", // NFD: e + U+0301
	}
	for _, n := range names {
		if err := write(work, file{"edge/" + n, n + "\n", 0o644}); err != nil {
			return err
		}
	}

	if err := buildDeepChain(dir); err != nil {
		return err
	}

	many := filepath.Join(dir, "many")
	if err := os.MkdirAll(many, 0o755); err != nil {
		return err
	}
	for i := 0; i < manyFiles; i++ {
		p := filepath.Join(many, fmt.Sprintf("f%06d.txt", i))
		if err := os.WriteFile(p, []byte(strconv.Itoa(i)+"\n"), 0o644); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o644); err != nil {
			return err
		}
		if err := os.Chtimes(p, FixedMtime, FixedMtime); err != nil {
			return err
		}
	}
	return fixDirTimes(dir)
}

// buildDeepChain nests ~200-byte directory names so the final file path is
// close to PATH_MAX but still below it.
func buildDeepChain(root string) error {
	dir := root
	for level := 0; ; level++ {
		if deepPathBudget-len(dir)-1 < deepNameLen+8 {
			break
		}
		dir = filepath.Join(dir, deepDirName(level))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if level > 64 {
			return fmt.Errorf("deep chain does not fit the path budget")
		}
	}
	nameLen := deepPathBudget - len(dir) - 1
	if nameLen > deepNameLen {
		nameLen = deepNameLen
	}
	if nameLen < 8 {
		return fmt.Errorf("deep chain leaves no room for a file name")
	}
	return writePath(filepath.Join(dir, "f"+strings.Repeat("y", nameLen-1)))
}

// deepDirName returns a unique 200-byte directory name.
func deepDirName(level int) string {
	stem := fmt.Sprintf("d%04d-", level)
	return stem + strings.Repeat("x", deepNameLen-len(stem))
}

// writePath creates an empty deterministic regular file.
func writePath(path string) error {
	if err := os.WriteFile(path, []byte("deep\n"), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, FixedMtime, FixedMtime)
}

// big_tree fixture parameters: 50,000 files of 25,600 bytes each, which is
// 1.28e9 bytes of content in total (about 1.19 GiB).
const (
	bigTreeFiles    = 50000
	bigTreeFileSize = 25600
	bigTreeFanout   = 10
	bigTreeLeafDirs = bigTreeFiles / bigTreeFanout
	bigTreeBufLen   = 4096
)

// bigTreeRadix holds the branching factor of each directory level below tree.
var bigTreeRadix = [7]int{2, 2, 2, 5, 5, 5, 5}

// bigTreeDir returns the work-relative leaf directory of a leaf index. The
// leaf index is split into seven mixed-radix digits over bigTreeRadix, most
// significant first, so level <n> holds d<(leaf/divisor) % radix>.
func bigTreeDir(leaf int) string {
	div := 1
	for _, r := range bigTreeRadix {
		div *= r
	}
	parts := make([]string, 0, len(bigTreeRadix)+1)
	parts = append(parts, "tree")
	for _, r := range bigTreeRadix {
		div /= r
		parts = append(parts, fmt.Sprintf("d%d", (leaf/div)%r))
	}
	return strings.Join(parts, "/")
}

// bigTreePath returns the work-relative path of file index i:
// tree/d<b>/d<b>/d<b>/d<b>/d<b>/d<b>/d<b>/f<i%10>.txt. Every file sits in a
// directory eight components below work, so the shell can rebuild the path
// from the index alone.
func bigTreePath(i int) string {
	name := fmt.Sprintf("f%d.txt", i%bigTreeFanout)
	return bigTreeDir(i/bigTreeFanout) + "/" + name
}

// buildBigTree creates the 50,000-file tree used by the cost scenarios. The
// content of a file is a sha256-derived block repeated to bigTreeFileSize, so
// the tree is deterministic for a seed and the write path reuses one buffer.
func buildBigTree(work string, seed int64) error {
	root := filepath.Join(work, "tree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	buf := make([]byte, bigTreeBufLen)
	for leaf := 0; leaf < bigTreeLeafDirs; leaf++ {
		dir := filepath.Join(work, filepath.FromSlash(bigTreeDir(leaf)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for k := 0; k < bigTreeFanout; k++ {
			full := filepath.Join(dir, fmt.Sprintf("f%d.txt", k))
			if err := writeBigTreeFile(full, seed, leaf*bigTreeFanout+k, buf); err != nil {
				return err
			}
		}
	}
	return fixDirTimes(root)
}

// writeBigTreeFile writes one deterministic file, streaming the shared buffer.
func writeBigTreeFile(path string, seed int64, index int, buf []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("big_tree:%d:%d", seed, index)))
	for off := 0; off < len(buf); off += len(sum) {
		copy(buf[off:], sum[:])
	}
	for written := 0; written < bigTreeFileSize; {
		n := bigTreeFileSize - written
		if n > len(buf) {
			n = len(buf)
		}
		if _, err := f.Write(buf[:n]); err != nil {
			_ = f.Close()
			return err
		}
		written += n
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, FixedMtime, FixedMtime)
}

// fixDirTimes pins directory timestamps after the children are in place.
func fixDirTimes(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		return os.Chtimes(p, FixedMtime, FixedMtime)
	})
}
