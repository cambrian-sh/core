package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Health-ping liveness (ADR-0043 D8;
// T-20261104-mcp-health-ping-drops-pdf-reader-and-fetch).
//
// Two defects made working servers flap error -> starting -> ready on every
// 30 s health tick:
//
//  1. The go-sdk (v1.7.0, and v1.8.0 unchanged) stamps the per-request _meta
//     of the stateless 2026-07-28 protocol (SEP-2575: protocolVersion,
//     clientInfo, clientCapabilities) onto ListTools, CallTool and the other
//     typed calls — but NOT onto Ping. A server that negotiated 2026-07-28
//     (mcp-server-fetch, pdf-reader) rejects that bare ping with InvalidParams.
//     pingParams supplies the fields the SDK leaves out.
//  2. The watch loop treated EVERY ping error as death. A JSON-RPC error
//     RESPONSE is the opposite of death: the server read the request and
//     answered. A server that legitimately does not implement ping
//     (MethodNotFound) or is strict about its params is alive; dropping its
//     tools churned a healthy integration. Only a closed connection (the
//     process exited, the HTTP session is gone) or a server that stays silent
//     is a liveness failure.
//
// The second fix is load-bearing, not a fallback: in the stateless protocol
// `ping` is not a method at all (the go-sdk server answers a well-formed
// stateless ping with MethodNotFound, "not supported in the new protocol").
// For those sessions the liveness signal is therefore "the server process read
// a request and answered it within the timeout" — a valid signal for an stdio
// process and an HTTP endpoint alike — and the _meta from fix 1 keeps the
// request well-formed, so a server that does still serve ping answers it
// cleanly. A list call would not do instead: the SDK may serve tools/list from
// its TTL cache (SEP-2549) without a round trip, reporting a dead server alive.

// protocolStateless is the first protocol version whose requests must carry
// their own _meta (SEP-2575). Kept as a string compare, like the SDK's own
// usesNewProtocol: protocol versions are ISO dates.
const protocolStateless = "2026-07-28"

// kernelClientInfo is the Implementation every kernel MCP client declares at
// initialize, and restates in a stateless ping's _meta.
var kernelClientInfo = &mcpsdk.Implementation{Name: "cambrian", Version: "1.0.0"}

// clientCapabilitiesMeta is the wire form of the capabilities the kernel's
// client declares. The connector constructs its client with nil options, so
// the SDK's default applies: roots with listChanged (go-sdk Client.capabilities
// — no sampling or elicitation handler is installed). The SDK's own converter
// to this per-request form is unexported, hence restated here; a test pins it
// against a server that validates it.
func clientCapabilitiesMeta() map[string]any {
	return map[string]any{"roots": map[string]any{"listChanged": true}}
}

// pingParams returns the params a health ping must carry on this session: nil
// for a session on a pre-2026-07-28 protocol (a bare ping is correct there),
// and the SEP-2575 _meta triple for a stateless session.
func pingParams(sess *mcpsdk.ClientSession) *mcpsdk.PingParams {
	res := sess.InitializeResult()
	if res == nil || res.ProtocolVersion < protocolStateless {
		return nil
	}
	return &mcpsdk.PingParams{Meta: mcpsdk.Meta{
		mcpsdk.MetaKeyProtocolVersion:    res.ProtocolVersion,
		mcpsdk.MetaKeyClientInfo:         kernelClientInfo,
		mcpsdk.MetaKeyClientCapabilities: clientCapabilitiesMeta(),
	}}
}

// liveness is what one health ping proved.
type liveness int

const (
	// alive: the server answered — with a result or with a JSON-RPC error.
	alive liveness = iota
	// unresponsive: no answer within the ping timeout, connection still open. A
	// server that handles requests one at a time cannot answer while a long
	// tool call is running, so one silence is not death (see pingTimeoutsBeforeDrop).
	unresponsive
	// gone: the connection is closed or failed at the transport; definitive.
	gone
)

// pingTimeoutsBeforeDrop is the number of consecutive unanswered pings after
// which a server is declared dead. Hysteresis is applied to silence ONLY, and
// is justified there: many stdio servers serve requests serially (a playwright
// navigation, a markitdown conversion, a slow fetch hold the pipe), and dropping
// the session on the first missed ping would also abort the tool call that
// delayed it. A closed connection is dropped at once — there is nothing to wait for.
const pingTimeoutsBeforeDrop = 2

// defaultPingTimeout bounds one health ping when mcp.ping_timeout_ms is unset
// (ENG-2003).
const defaultPingTimeout = 10 * time.Second

func (c *Connector) pingTimeout() time.Duration {
	if c.pingWait > 0 {
		return c.pingWait
	}
	return defaultPingTimeout
}

// classifyPing maps a ping error to what it proves about the server. pctx is
// the ping's own deadline context, used to tell a ping timeout from transport
// failure.
func classifyPing(pctx context.Context, err error) liveness {
	if err == nil {
		return alive
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		return alive
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(pctx.Err(), context.DeadlineExceeded) {
		return unresponsive
	}
	return gone
}
