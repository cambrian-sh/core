package sandbox

import (
	"slices"
	"testing"
)

// ENG-2217: every directory the workdir env points into (home, temp, the npm
// and uv caches, uv's tool dir) is an exec tmpfs owned by the server's uid,
// because the bind-mounted workdir is not writable by that uid (EIO on Docker
// Desktop for Windows) and /tmp is noexec.
func TestDockerScratchMounts_DefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	if got := dockerScratchDirs(p); !slices.Equal(got, []string{"cache", "home", "tmp", "uv-tools"}) {
		t.Fatalf("scratch dirs = %v", got)
	}
	low := dockerScratchMounts(p, TierPolicy{LowPrivilege: true, MemoryMB: 2048})
	want := "rw,exec,nosuid,nodev,mode=0700,size=2048m,uid=65534,gid=65534"
	if len(low) != 8 || low[0] != "--tmpfs" || low[1] != "/work/cache:"+want || low[7] != "/work/uv-tools:"+want {
		t.Fatalf("low-privilege mounts = %v", low)
	}
	root := dockerScratchMounts(p, TierPolicy{})
	if root[1] != "/work/cache:rw,exec,nosuid,nodev,mode=0700,size=1024m" {
		t.Fatalf("no memory limit, image user: %v", root)
	}
}

func TestDockerScratchDirs_TopLevelOnlyAndSafe(t *testing.T) {
	p := &Policy{WorkdirEnv: map[string]string{"A": "x/y", "B": "/x/z/", "C": "..", "D": ""}, ConfinedEnv: map[string]string{"E": "w"}}
	if got := dockerScratchDirs(p); !slices.Equal(got, []string{"w", "x"}) {
		t.Fatalf("dirs = %v", got)
	}
}
