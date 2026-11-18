package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// ErrHandshakeTimeout is returned when a server did not complete the MCP
// connect handshake (transport connect + initialize) within the bound.
var ErrHandshakeTimeout = errors.New("MCP handshake timed out")

// defaultHandshakeTimeout bounds one MCP connect handshake (and one boot
// tools/list) when mcp.handshake_timeout_ms is unset (ENG-2003). 60 s is the
// bound the per-user dial already used; a stdio server spawning a process
// takes a few seconds, a remote one well under one.
const defaultHandshakeTimeout = 60 * time.Second

func (c *Connector) handshakeTimeout() time.Duration {
	if c.handshakeWait > 0 {
		return c.handshakeWait
	}
	return defaultHandshakeTimeout
}

// connectSession runs the SDK connect with a deadline on the HANDSHAKE only
// (ENG-1528). Before, the shared connect had no deadline at all — an http/sse
// server that accepted the connection and never answered initialize hung the
// synchronous boot ConnectAll, and so the kernel boot — while the per-user
// dial bounded it with context.WithTimeout, whose deferred cancel killed every
// legacy-SSE session the moment the dial returned: go-sdk's
// SSEClientTransport issues its long-lived GET (which carries every response)
// with the connect ctx, unlike StreamableClientTransport, which detaches.
//
// So the session context is a plain cancellable child of parent, and the
// bound is a timer that cancels it only while the handshake runs; on success
// the timer is stopped and the context lives until the session ends (a
// watcher on Wait cancels it then, so nothing is left behind). parent keeps
// its role: the shared path passes the kernel/watch ctx (the stream ends with
// the watch), the per-user path a WithoutCancel ctx (pooled past the call).
//
// The bound is the limit plus go-sdk's own tail: a cancelled call first sends
// notifications/cancelled under the SDK's 5 s notify timeout, which a silent
// Streamable-HTTP server swallows too (server/discover, then the initialize
// fallback — so at most limit + ~10 s; a silent SSE server fails at the limit).
//
// Rejected: http.Client.Timeout (it would cut the SSE GET stream and long tool
// calls); a WithTimeout around Connect (the per-user bug above); wrapping the
// Transport so the Connection's Close cancels the context — Client.Connect
// type-asserts unexported optional interfaces on the connection (the
// streamable session-id/protocol-version hook), which a wrapper would hide.
func (c *Connector) connectSession(parent context.Context, t mcpsdk.Transport) (*mcpsdk.ClientSession, error) {
	ctx, cancel := context.WithCancel(parent)
	limit := c.handshakeTimeout()
	timer := time.AfterFunc(limit, cancel)
	sess, err := c.client.Connect(ctx, t, nil)
	fired := !timer.Stop()
	if err != nil {
		cancel()
		if fired && parent.Err() == nil {
			return nil, fmt.Errorf("%w after %s (no answer to initialize): %v", ErrHandshakeTimeout, limit, err)
		}
		return nil, err
	}
	if fired {
		// The timer fired between Connect returning and Stop: the session's
		// stream context is already cancelled, so the session is unusable.
		_ = sess.Close()
		cancel()
		return nil, fmt.Errorf("%w after %s", ErrHandshakeTimeout, limit)
	}
	go func() {
		_ = sess.Wait() // returns when the session closes, from either side
		cancel()
	}()
	return sess, nil
}

// boundedDiscover is discover under the same bound: a server that answers
// initialize but never answers tools/list must not hang the boot ConnectAll
// or a watch loop either. tools/list is plain request/response, so a context
// deadline is the right tool here.
func (c *Connector) boundedDiscover(ctx context.Context, s ServerConfig, sess *mcpsdk.ClientSession) ([]domain.SystemTool, error) {
	dctx, cancel := context.WithTimeout(ctx, c.handshakeTimeout())
	defer cancel()
	return discover(dctx, s, sess)
}
