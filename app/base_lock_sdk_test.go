package app

import (
	"strconv"
	"strings"
	"testing"

	core "github.com/cambrian-sh/core"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// minChannelSDK is the first published cambrian-agent-sdk that ships
// cambrian_agent_sdk.channel, the kernel-socket channel every sandboxed agent
// dials (ADR-0157 S2/S5). 0.1.5 did not, so a release-built env booted no
// sandboxed Python agent (T-20261118-release-base-lock-pins-the-published-agent-sdk).
// scripts/gen_agent_requirements.py --check verifies the wheel itself (network);
// this is the offline floor that keeps the embedded lock from regressing.
var minChannelSDK = [3]int{0, 1, 6}

func TestEmbeddedBaseLockPinsAnSDKWithTheChannel(t *testing.T) {
	b, err := core.AgentsFS.ReadFile(baseLockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := pyenv.ParseLock(b)
	if err != nil {
		t.Fatalf("embedded base lock: %v", err)
	}
	var ver string
	for _, r := range lock.Requirements {
		if pyenv.NormalizeName(r.Name) == "cambrian-agent-sdk" {
			ver = r.Version
		}
	}
	if ver == "" {
		t.Fatal("the base lock must pin cambrian-agent-sdk")
	}
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) != 3 {
		t.Fatalf("cambrian-agent-sdk==%s: want a MAJOR.MINOR.PATCH release", ver)
	}
	var got [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("cambrian-agent-sdk==%s: want a plain release version: %v", ver, err)
		}
		got[i] = n
	}
	for i := range got {
		if got[i] != minChannelSDK[i] {
			if got[i] < minChannelSDK[i] {
				t.Fatalf("base lock pins cambrian-agent-sdk==%s, older than %d.%d.%d, the first release "+
					"with cambrian_agent_sdk.channel: sandboxed agents could not reach the kernel",
					ver, minChannelSDK[0], minChannelSDK[1], minChannelSDK[2])
			}
			return
		}
	}
}
