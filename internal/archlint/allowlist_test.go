package archlint

// The explicit exceptions to the architecture rules (ENG-1484). Every entry
// is either a known leak tracked by an open ticket, which deletes the entry
// when it is fixed, or a site an ADR or ticket justifies, and its reason says
// which. An entry that stops matching fails the test, so these lists only
// shrink. Keys are printed by a failing run ("allowlist key: …").

// allowImports: rule 1, import layering.
var allowImports = map[string]string{
	// Known leak (ENG-1104 family): the MCP-serve adapter's core tools call the
	// memory service directly instead of a domain port.
	"internal/infrastructure/mcpserve/coretools.go -> github.com/cambrian-sh/core/internal/memory": "known leak, ENG-1104: declare the port in domain/ and drop this entry",
}

// allowConstructors: rule 2, restricted memory-stack constructors. Empty: the
// raw adapter is opened only by app/boot_infrastructure.go and cited cmd/ tools.
var allowConstructors = map[string]string{}

// allowDomainCtx: rule 3, ctx-first I/O methods on domain ports.
var allowDomainCtx = map[string]string{
	"domain.ToolArgValidator.ValidateArgs": "pure argument validation against a schema, no I/O (ENG-1466 re-verification)",
}

// allowBackground: rule 4, context.Background()/TODO() in app/, domain/ and
// internal/. Keys are "<file> <func> #<k-th call in that func>".
var allowBackground = map[string]string{
	// Shutdown: the parent ctx is already cancelled; a bounded fresh deadline is
	// how a graceful stop gets time to finish (AGENTS.md §2.7 names no other).
	"app/agentcard_endpoint.go startAgentCards #1":       "graceful HTTP shutdown deadline after the parent ctx ended (ENG-1467 review)",
	"app/app.go Run #1":                                  "graceful kernel shutdown deadline after the parent ctx ended (ENG-1467 review)",
	"app/start_services.go serveGRPC #1":                 "graceful listener shutdown deadline after the parent ctx ended (ENG-1467 review)",
	"app/start_services.go startIngestionHTTP #1":        "graceful server shutdown deadline after the parent ctx ended (ENG-1467 review)",
	"internal/sandbox/docker.go containerHandle.kill #1": "container kill/cleanup must run after the caller's ctx is gone, bounded at 15 s (ADR-0157, ENG-2031)",
	"internal/sandbox/docker.go egressSidecar.stop #1":   "the egress forwarder sidecar is removed with its server, after the caller's ctx is gone, bounded at 15 s (ENG-688)",

	// Kernel-owned work: no request exists whose ctx could ride along.
	"app/boot_tools.go kernelBuild.buildMCPConnector #1":                              "MCP server health transitions come from the connector's own loops (ENG-1466)",
	"internal/metabolism/agentmgr/crash_detection.go AgentManager.scheduleRestart #1": "kernel-owned daemon auto-restart after a crash (ENG-1467)",
	"internal/metabolism/agentmgr/stale_recycle.go AgentManager.recycleDaemon #1":     "kernel-owned re-boot of a stale pool worker after a layer change, like scheduleRestart (ADR-0157 D7.10.2, ENG-2011)",
	"internal/metabolism/agentmgr/crash_detection.go AgentManager.publishEvent #1":    "daemon lifecycle events are kernel-owned (ENG-1466)",
	"internal/ingress/consent_bridge.go NewConsentBridge #1":                          "the bridge's own post ctx until Start hands it the kernel ctx (ENG-1467 review)",
	"internal/ingress/consent_bridge.go ConsentBridge.post #1":                        "nil-ctx guard on a best-effort delivery path (ENG-1467 review)",
}

// allowClockIDs: rule 5, clock-derived identifiers (AGENTS.md §2.6). Keys are
// "<file> <func> <target> #<k-th hit for that target in that func>".
var allowClockIDs = map[string]string{
	// Known defects, each tracked by its ticket, which deletes the entry. (The
	// receipt QueryID entry left with ENG-2068: the id is now derived from the
	// request, domain.DerivedQueryCorrelation.)
}
