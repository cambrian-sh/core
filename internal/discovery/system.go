package discovery

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// SystemSource deterministically observes the local system (ADR-0078 D2): OS/arch and the
// host's network interfaces + their non-loopback addresses. Local-only, read-only, no
// egress — so it carries none of the SSRF surface the http source guards against. It
// answers "system"-kind targets (selected when the request references network/system
// state); the always-on OS path facts (home/desktop/cwd) stay in the dispatcher's EnvFacts.
type SystemSource struct{}

func (s *SystemSource) Kind() string { return "system" }

func (s *SystemSource) Probe(_ context.Context, target domain.DiscoveryTarget) ([]domain.DiscoveredEntity, error) {
	ents := []domain.DiscoveredEntity{{
		Kind: "service", ID: "system:runtime", Exists: true,
		Summary: fmt.Sprintf("os=%s arch=%s cpus=%d", runtime.GOOS, runtime.GOARCH, runtime.NumCPU()),
	}}

	ifaces, err := net.Interfaces()
	if err != nil {
		return ents, nil // OS facts still useful; interface enumeration is best-effort
	}
	var addrs []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		aa, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range aa {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil || ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			addrs = append(addrs, fmt.Sprintf("%s=%s", ifc.Name, ip.String()))
		}
	}
	sort.Strings(addrs)
	summary := "no active non-loopback interfaces"
	if len(addrs) > 0 {
		summary = "interfaces: " + strings.Join(addrs, ", ")
	}
	ents = append(ents, domain.DiscoveredEntity{
		Kind: "service", ID: "system:network", Exists: true, Summary: summary,
	})
	return ents, nil
}
