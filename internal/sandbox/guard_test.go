package sandbox

import (
	"strings"
	"testing"
)

const guardHappy = `uid=1000
home=/sandbox/home
dockerenv=yes
mountinfo-begin
36 25 0:31 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/abc
37 36 0:32 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
38 36 0:33 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
39 36 0:34 / /sandbox/evidence rw,relatime - ext4 /dev/nvme0n1p3 rw
40 36 0:35 / /etc/hosts rw,relatime - ext4 /dev/nvme0n1p3 rw
mountinfo-end
`

func TestParseGuard(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantSub string
	}{
		{name: "happy", out: guardHappy},
		{
			name:    "root uid",
			out:     strings.Replace(guardHappy, "uid=1000", "uid=0", 1),
			wantSub: "uid is",
		},
		{
			name:    "host home",
			out:     strings.Replace(guardHappy, "home=/sandbox/home", "home=/root", 1),
			wantSub: "HOME",
		},
		{
			name:    "no container marker",
			out:     strings.Replace(guardHappy, "dockerenv=yes", "dockerenv=no", 1),
			wantSub: "/.dockerenv",
		},
		{
			name: "host bind",
			out: strings.Replace(guardHappy, "mountinfo-end",
				"41 36 0:36 / /hijack rw,relatime - ext4 /dev/sdb1 rw\nmountinfo-end", 1),
			wantSub: "disk-backed mount",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fails := parseGuard(tt.out)
			if tt.wantSub == "" {
				if len(fails) != 0 {
					t.Fatalf("guard must pass, got %v", fails)
				}

				return
			}

			if len(fails) == 0 {
				t.Fatalf("guard must fail, want %q", tt.wantSub)
			}

			joined := strings.Join(fails, "; ")
			if !strings.Contains(joined, tt.wantSub) {
				t.Fatalf("fail mismatch: got %q, want substring %q", joined, tt.wantSub)
			}
		})
	}
}

func TestCheckMountinfo(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  int
	}{
		{
			name: "pseudo filesystems are skipped",
			lines: []string{
				"36 25 0:31 / / rw - overlay overlay rw",
				"37 36 0:32 / /dev/shm rw - tmpfs shm rw",
				"38 36 0:33 / /proc rw - proc proc rw",
			},
		},
		{
			name:  "evidence bind is allowed",
			lines: []string{"39 36 0:34 / /sandbox/evidence rw - ext4 /dev/sda1 rw"},
		},
		{
			name:  "docker single-file binds are allowed",
			lines: []string{"40 36 0:35 / /etc/resolv.conf rw - ext4 /dev/sda1 rw"},
		},
		{
			name:  "disk mount outside the layout",
			lines: []string{"41 36 0:36 / /var rw - xfs /dev/sdb1 rw"},
			want:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fails := checkMountinfo(tt.lines)
			if len(fails) != tt.want {
				t.Fatalf("got %d fails (%v), want %d", len(fails), fails, tt.want)
			}
		})
	}
}
