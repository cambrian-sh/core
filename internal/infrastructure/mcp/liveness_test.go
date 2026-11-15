package mcp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// T-20261104-mcp-health-ping-drops-pdf-reader-and-fetch: health pings must
// carry the SEP-2575 _meta a 2026-07-28 server requires, and a server that
// answers a ping with an error — or stays silent once — must not be churned.

// pingMiddleware wraps only the "ping" method; everything else passes.
type pingMiddleware func(ctx context.Context, req mcpsdk.Request) (mcpsdk.Result, error, bool)

// pingServer starts an in-memory server with one tool. onPing intercepts
// "ping" on the SERVER (it sees only requests the SDK routes to handlers — a
// stateless-protocol ping is answered MethodNotFound before that); onSend
// intercepts "ping" on the kernel CLIENT, where silence can be simulated
// whatever the negotiated protocol.
func pingServer(t *testing.T, ctx context.Context, onPing, onSend pingMiddleware) (*mcpsdk.ClientSession, *mcpsdk.ServerSession) {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "ping-srv", Version: "1.0.0"}, nil)
	type noArgs struct{}
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "noop"}, func(context.Context, *mcpsdk.CallToolRequest, noArgs) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	})
	server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "ping" && onPing != nil {
				if res, err, handled := onPing(ctx, req); handled {
					return res, err
				}
			}
			return next(ctx, method, req)
		}
	})
	ct, st := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(kernelClientInfo, nil)
	client.AddSendingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "ping" && onSend != nil {
				if res, err, handled := onSend(ctx, req); handled {
					return res, err
				}
			}
			return next(ctx, method, req)
		}
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs, ss
}

// strictPing rejects a ping whose _meta lacks the SEP-2575 fields, the way
// mcp-server-fetch and pdf-reader did in the 2026-11-04 boot log.
func strictPing(rejected *atomic.Int32) pingMiddleware {
	return func(_ context.Context, req mcpsdk.Request) (mcpsdk.Result, error, bool) {
		p, _ := req.GetParams().(*mcpsdk.PingParams)
		var meta map[string]any
		if p != nil {
			meta = p.Meta
		}
		_, hasVer := meta[mcpsdk.MetaKeyProtocolVersion].(string)
		_, hasCaps := meta[mcpsdk.MetaKeyClientCapabilities].(map[string]any)
		if !hasVer || !hasCaps {
			rejected.Add(1)
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams,
				Message: "params._meta must be an object carrying the required io.modelcontextprotocol/protocolVersion"}, true
		}
		return nil, nil, false
	}
}

func TestPingParams_SatisfyAStatelessProtocolServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rejected atomic.Int32
	cs, _ := pingServer(t, ctx, strictPing(&rejected), nil)

	if v := cs.InitializeResult().ProtocolVersion; v < protocolStateless {
		t.Fatalf("negotiated %q; this test needs a >= %s session to exercise the bug", v, protocolStateless)
	}
	// The SDK's own bare ping is what the strict server rejects (the bug)...
	if err := cs.Ping(ctx, nil); err == nil || rejected.Load() != 1 {
		t.Fatalf("a bare ping should be rejected for missing _meta (err=%v rejected=%d) — the reproduction is broken", err, rejected.Load())
	}
	// ...the kernel's params are well-formed: the strict check passes. (A
	// go-sdk server then answers MethodNotFound — ping is not a method of the
	// stateless protocol — which is an answer, hence alive.)
	p := pingParams(cs)
	for _, k := range []string{mcpsdk.MetaKeyProtocolVersion, mcpsdk.MetaKeyClientInfo, mcpsdk.MetaKeyClientCapabilities} {
		if _, ok := p.Meta[k]; !ok {
			t.Errorf("ping _meta lacks %s", k)
		}
	}
	err := cs.Ping(ctx, p)
	if rejected.Load() != 1 {
		t.Fatalf("the strict server rejected the kernel's ping params: %v", err)
	}
	if got := classifyPing(ctx, err); got != alive {
		t.Fatalf("classifyPing(%v) = %v, want alive", err, got)
	}
}

func TestClassifyPing(t *testing.T) {
	live := context.Background()
	expired, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want liveness
	}{
		{"answered", live, nil, alive},
		{"method not found is an answer", live, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "no ping"}, alive},
		{"timeout", expired, context.DeadlineExceeded, unresponsive},
		{"closed", live, mcpsdk.ErrConnectionClosed, gone},
	}
	for _, c := range cases {
		if got := classifyPing(c.ctx, c.err); got != c.want {
			t.Errorf("%s: classifyPing = %v, want %v", c.name, got, c.want)
		}
	}
}

// watched arms the health loop over an already-connected session and marks it
// ready, as ConnectAll would. The config's transport is unusable on purpose: a
// drop shows up as error health (the reconnect cannot succeed).
func watched(t *testing.T, ctx context.Context, cs *mcpsdk.ClientSession, pingWait time.Duration) (*Connector, *recSink, *healthLog) {
	t.Helper()
	conn := NewConnector(CredentialSources{})
	conn.pingWait = pingWait
	hl := &healthLog{}
	conn.SetHealthObserver(hl.add)
	sink := newRecSink()
	const id = "srv"
	sink.SetServerTools(ctx, id, []domain.SystemTool{{Name: "mcp:srv/noop"}})
	conn.mu.Lock()
	conn.sessions[id] = cs
	conn.mu.Unlock()
	conn.armWatch(ctx, ServerConfig{ID: id, Transport: "in-memory"}, sink, 10*time.Millisecond)
	conn.setHealth(id, conn.generation(id), func(h *ServerHealth) { h.State, h.ToolCount = HealthReady, 1 })
	return conn, sink, hl
}

func stateOf(conn *Connector) string {
	h, _ := conn.Health("srv")
	return h.State
}

// A server answering ping with MethodNotFound is alive: the health loop keeps
// it ready and never withdraws its tools.
func TestWatch_PingErrorResponseIsLiveness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var pings atomic.Int32
	cs, _ := pingServer(t, ctx, nil, func(context.Context, mcpsdk.Request) (mcpsdk.Result, error, bool) {
		pings.Add(1)
		return nil, nil, false // the stateless go-sdk server answers MethodNotFound
	})
	conn, sink, hl := watched(t, ctx, cs, time.Second)
	waitFor(t, "several pings", func() bool { return pings.Load() >= 8 })
	if s := stateOf(conn); s != HealthReady {
		t.Fatalf("state = %s, want ready; transitions %v", s, hl.states("srv"))
	}
	if sink.count("srv") != 1 {
		t.Fatal("tools were withdrawn from a server that answered every ping")
	}
	cancel()
}

// The strict server sees only well-formed pings from the loop: no rejection,
// no flap.
func TestWatch_StrictServerStaysReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rejected, pings atomic.Int32
	cs, _ := pingServer(t, ctx, strictPing(&rejected), func(context.Context, mcpsdk.Request) (mcpsdk.Result, error, bool) {
		pings.Add(1)
		return nil, nil, false
	})
	conn, sink, hl := watched(t, ctx, cs, time.Second)
	waitFor(t, "several pings", func() bool { return pings.Load() >= 8 })
	if rejected.Load() != 0 || stateOf(conn) != HealthReady || sink.count("srv") != 1 {
		t.Fatalf("rejected=%d state=%s tools=%d transitions=%v", rejected.Load(), stateOf(conn), sink.count("srv"), hl.states("srv"))
	}
	cancel()
}

// One unanswered ping (a serial server busy with a long call) is tolerated;
// pingTimeoutsBeforeDrop in a row drop the server.
func TestWatch_SilenceHysteresis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var pings atomic.Int32
	var silentFrom atomic.Int32
	silentFrom.Store(1 << 30)
	cs, _ := pingServer(t, ctx, nil, func(c context.Context, _ mcpsdk.Request) (mcpsdk.Result, error, bool) {
		n := pings.Add(1)
		// Phase 1: every other ping goes unanswered — never two in a row.
		// Phase 2 (n >= silentFrom): every ping goes unanswered.
		if n%2 == 0 || n >= silentFrom.Load() {
			select {
			case <-c.Done():
			case <-release:
			}
			return nil, c.Err(), true
		}
		return nil, nil, false
	})
	conn, sink, hl := watched(t, ctx, cs, 30*time.Millisecond)
	waitFor(t, "alternating pings", func() bool { return pings.Load() >= 8 })
	if stateOf(conn) != HealthReady || sink.count("srv") != 1 {
		t.Fatalf("isolated silences dropped the server: state=%s transitions=%v", stateOf(conn), hl.states("srv"))
	}
	silentFrom.Store(pings.Load())
	waitFor(t, "drop after consecutive silences", func() bool { return stateOf(conn) == HealthError })
	if sink.count("srv") != 0 {
		t.Fatal("a dropped server's tools must be withdrawn")
	}
	cancel()
}

// A closed connection is definitive: dropped on the first failed ping.
func TestWatch_ClosedConnectionDropsAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs, ss := pingServer(t, ctx, nil, nil)
	conn, sink, _ := watched(t, ctx, cs, time.Second)
	_ = ss.Close()
	waitFor(t, "drop", func() bool { return stateOf(conn) == HealthError })
	if sink.count("srv") != 0 {
		t.Fatal("tools of a closed server must be withdrawn")
	}
	cancel()
}
