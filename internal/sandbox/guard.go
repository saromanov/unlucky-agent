package sandbox

import (
	"bufio"
	"fmt"
	"strings"
)

// Guard probe constants.
const (
	guardUIDWant  = "1000"
	guardHomeWant = "/sandbox/home"
)

// guardScript runs inside the sandbox before the first damage step and
// reports the raw facts: uid, HOME, container-root marker and the full
// mountinfo. The interpretation lives on the Go side so it stays testable.
const guardScript = `printf 'uid=%s\n' "$(id -u)"
printf 'home=%s\n' "$HOME"
if [ -e /.dockerenv ]; then printf 'dockerenv=yes\n'; else printf 'dockerenv=no\n'; fi
printf 'mountinfo-begin\n'
cat /proc/self/mountinfo
printf 'mountinfo-end\n'
`

// parseGuard interprets guardScript output and returns failed checks.
// Empty result means the guard passed.
func parseGuard(out string) []string {
	var (
		fails   []string
		uid     string
		home    string
		docker  string
		mounts  []string
		inMount bool
	)
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "uid="):
			uid = strings.TrimPrefix(line, "uid=")
		case strings.HasPrefix(line, "home="):
			home = strings.TrimPrefix(line, "home=")
		case strings.HasPrefix(line, "dockerenv="):
			docker = strings.TrimPrefix(line, "dockerenv=")
		case line == "mountinfo-begin":
			inMount = true
		case line == "mountinfo-end":
			inMount = false
		case inMount:
			mounts = append(mounts, line)
		}
	}
	if sc.Err() != nil {
		return append(fails, fmt.Sprintf("guard output unreadable: %v", sc.Err()))
	}
	if uid != guardUIDWant {
		fails = append(fails, fmt.Sprintf("uid is %q, want %s", uid, guardUIDWant))
	}
	if home != guardHomeWant {
		fails = append(fails, fmt.Sprintf("HOME is %q, want %q", home, guardHomeWant))
	}
	if docker != "yes" {
		fails = append(fails, "container root marker /.dockerenv not found")
	}
	fails = append(fails, checkMountinfo(mounts)...)
	return fails
}

// pseudoFilesystems never carry host data. Everything else is a disk-backed
// mount: the container root image, a bind of a host path or a named volume.
var pseudoFilesystems = map[string]bool{
	"overlay": true, "proc": true, "sysfs": true, "tmpfs": true,
	"cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true,
	"shm": true, "devtmpfs": true,
}

// allowedDiskMounts are the disk-backed mount points the layout permits.
// /sandbox/evidence is the only host bind in the sandbox layout. Docker also
// bind-mounts three single files (/etc/hosts, /etc/hostname, /etc/resolv.conf)
// into every container; they are runtime plumbing, not host trees.
var allowedDiskMounts = map[string]bool{
	"/sandbox/evidence": true,
	"/etc/hosts":        true,
	"/etc/hostname":     true,
	"/etc/resolv.conf":  true,
}

// incusGuardScript is the guardScript variant for incus: the container
// marker is a fuse.lxcfs mount instead of /.dockerenv.
const incusGuardScript = `printf 'uid=%s\n' "$(id -u)"
printf 'home=%s\n' "$HOME"
printf 'links=%s\n' "$(ls /sys/class/net | tr '\n' ' ')"
printf 'mountinfo-begin\n'
cat /proc/self/mountinfo
printf 'mountinfo-end\n'
`

// parseIncusGuard interprets incusGuardScript output and returns failed
// checks. Empty result means the guard passed.
func parseIncusGuard(out string) []string {
	var (
		fails     []string
		uid       string
		home      string
		links     string
		linksSeen bool
		mounts    []string
		inMount   bool
	)
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "uid="):
			uid = strings.TrimPrefix(line, "uid=")
		case strings.HasPrefix(line, "home="):
			home = strings.TrimPrefix(line, "home=")
		case strings.HasPrefix(line, "links="):
			links = strings.TrimPrefix(line, "links=")
			linksSeen = true
		case line == "mountinfo-begin":
			inMount = true
		case line == "mountinfo-end":
			inMount = false
		case inMount:
			mounts = append(mounts, line)
		}
	}
	if sc.Err() != nil {
		return append(fails, fmt.Sprintf("guard output unreadable: %v", sc.Err()))
	}
	if uid != guardUIDWant {
		fails = append(fails, fmt.Sprintf("uid is %q, want %s", uid, guardUIDWant))
	}
	if home != guardHomeWant {
		fails = append(fails, fmt.Sprintf("HOME is %q, want %q", home, guardHomeWant))
	}
	fails = append(fails, checkIncusLinks(links, linksSeen)...)
	fails = append(fails, checkIncusMountinfo(mounts)...)
	return fails
}

// checkIncusLinks requires the container network namespace to expose the
// loopback interface only.
func checkIncusLinks(links string, seen bool) []string {
	if !seen {
		return []string{"network interface list missing"}
	}
	var fails []string
	for _, name := range strings.Fields(links) {
		if name != "lo" {
			fails = append(fails, fmt.Sprintf("network interface %q present, want lo only", name))
		}
	}
	return fails
}

// incusPseudoFilesystems are the virtual filesystems used by incus that never
// carry host data. lxcfs backs the container-visible /proc and /sys entries.
var incusPseudoFilesystems = map[string]bool{
	"fuse.lxcfs":  true,
	"fusectl":     true,
	"binfmt_misc": true,
}

// incusAllowedDiskMounts are the disk-backed mount points the incus layout
// permits. The container rootfs lives on a host storage pool by definition,
// so "/" is allowed.
var incusAllowedDiskMounts = map[string]bool{
	"":                  true,
	"/":                 true,
	"/sandbox/evidence": true,
	"/etc/hosts":        true,
	"/etc/hostname":     true,
	"/etc/resolv.conf":  true,
}

// incusTmpfsPaths must be mounted as tmpfs inside the container.
var incusTmpfsPaths = []string{containerWork, containerHome, containerScratch}

// incusTmpfsMounts are the only mount points the incus layout may back with
// tmpfs: the sandbox trees plus the runtime paths the image itself needs.
var incusTmpfsMounts = map[string]bool{
	containerWork:    true,
	containerHome:    true,
	containerScratch: true,
	"/dev":           true,
	"/dev/shm":       true,
	"/run":           true,
	"/run/lock":      true,
}

// checkIncusMountinfo flags disk-backed mounts outside the incus allowed set
// and the missing lxcfs marker, and requires the work, home and scratch trees
// to be tmpfs.
func checkIncusMountinfo(mounts []string) []string {
	var (
		fails []string
		lxcfs bool
		types = map[string][]string{}
	)
	for _, line := range mounts {
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		rest := strings.Fields(line[sep+3:])
		if len(rest) == 0 {
			continue
		}
		mountPoint := fields[4]
		fstype := rest[0]
		if fstype == "fuse.lxcfs" {
			lxcfs = true
		}
		types[mountPoint] = append(types[mountPoint], fstype)
		if fstype == "tmpfs" && !incusTmpfsMounts[mountPoint] {
			fails = append(fails, fmt.Sprintf("tmpfs mount %q outside sandbox layout", mountPoint))
		}
		if pseudoFilesystems[fstype] || incusPseudoFilesystems[fstype] {
			continue
		}
		if !incusAllowedDiskMounts[mountPoint] && !isIncusTmpfsPath(mountPoint) {
			fails = append(fails, fmt.Sprintf("disk-backed mount %q (fstype %s) outside /sandbox/evidence", mountPoint, fstype))
		}
	}
	if !lxcfs {
		fails = append(fails, "container root marker fuse.lxcfs not found")
	}
	for _, p := range incusTmpfsPaths {
		got, ok := types[p]
		if !ok {
			fails = append(fails, fmt.Sprintf("sandbox path %q is not mounted, want tmpfs", p))
			continue
		}
		if !hasFilesystem(got, "tmpfs") {
			fails = append(fails, fmt.Sprintf("sandbox path %q is %s, want tmpfs", p, got[0]))
		}
	}
	return fails
}

func isIncusTmpfsPath(mountPoint string) bool {
	for _, p := range incusTmpfsPaths {
		if mountPoint == p {
			return true
		}
	}
	return false
}

func hasFilesystem(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

// checkMountinfo flags disk-backed mounts outside the allowed set. A host
// path mounted anywhere else (for example /home or /root) is a jailbreak
// and must refuse the run.
func checkMountinfo(mounts []string) []string {
	var fails []string
	for _, line := range mounts {
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := fields[4]
		fstype := strings.Fields(line[sep+3:])[0]
		if pseudoFilesystems[fstype] {
			continue
		}
		if !allowedDiskMounts[mountPoint] {
			fails = append(fails, fmt.Sprintf("disk-backed mount %q (fstype %s) outside /sandbox/evidence", mountPoint, fstype))
		}
	}
	return fails
}
