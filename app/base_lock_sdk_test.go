package app

import (
	"strconv"
	"strings"
	"testing"

	core "github.com/cambrian-sh/core"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// minChannelSDK is the oldest published cambrian-agent-sdk the embedded base
// lock may pin. 0.1.6 is the first that ships cambrian_agent_sdk.channel, the
// kernel-socket channel every sandboxed agent dials (ADR-0157 S2/S5); 0.1.5 did
// not, so a release-built env booted no sandboxed Python agent
// (T-20261118-release-base-lock-pins-the-published-agent-sdk). 0.1.7 is the
// first whose skill/tool menus and artifact calls present the task lease; on
// 0.1.6 an authenticated kernel refuses the reasoning loop's skill menu
// no_lease every turn (T-20261118-release-agent-sdk-0-1-7-with-the-lease-fixes).
// 0.1.8 is the first that sends agent heartbeats (ENG-1348): on 0.1.7 a
// sandboxed agent sends none, so the kernel never tracks its liveness and a
// hung pool worker is never reclaimed
// (T-20261118-pin-the-published-agent-sdk-0-1-8-in-agents).
// scripts/gen_agent_requirements.py --check verifies the wheel itself (network);
// this is the offline floor that keeps the embedded lock from regressing.
var minChannelSDK = [3]int{0, 1, 8}

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
				t.Fatalf("base lock pins cambrian-agent-sdk==%s, older than %d.%d.%d: sandboxed agents "+
					"would lack the kernel channel, the task lease on menus and artifacts, or heartbeats",
					ver, minChannelSDK[0], minChannelSDK[1], minChannelSDK[2])
			}
			return
		}
	}
}
