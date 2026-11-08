package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// Per-server health for the integrations surface
// (T-20261105-mcp-server-health-is-not-visible-on-the). Before this the
// connector's only output about a server was a slog line: an operator could not
// see which servers were up, retrying, or why one failed.

// Health states. Closed vocabulary, mirrored on IntegrationHealthOp.state.
const (
	HealthStopped  = "stopped"  // disabled, removed, or never started
	HealthStarting = "starting" // launching the process / handshaking
	HealthReady    = "ready"    // handshake done, tools published
	HealthError    = "error"    // the last attempt failed; retried with backoff
)

// ServerHealth is one server's live state. LastError is already redacted: no
// secret value injected into that server can appear in it.
type ServerHealth struct {
	ServerID      string
	State         string
	LastHandshake time.Time
	ToolCount     int
	LastError     string
	Restarts      int
	ChangedAt     time.Time
}

// SetHealthObserver installs the callback told about every health CHANGE (state,
// error, tool count or restart count). It is called synchronously from the
// connector's goroutines and must be prompt and non-blocking (AGENTS.md §3.3):
// the kernel's observer publishes to the in-memory event bus.
func (c *Connector) SetHealthObserver(fn func(ServerHealth)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observer = fn
}

// Health returns one server's health, or false when the connector has never
// been asked to run it.
func (c *Connector) Health(id string) (ServerHealth, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.health[id]
	if !ok {
		return ServerHealth{}, false
	}
	return *h, true
}

func (c *Connector) generation(id string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gens[id]
}

// setHealth applies mutate to one server's record when gen is still current,
// and notifies the observer (outside the lock) when anything changed.
func (c *Connector) setHealth(id string, gen uint64, mutate func(*ServerHealth)) {
	c.mu.Lock()
	if c.gens[id] != gen {
		c.mu.Unlock()
		return // a stale loop: its server was re-armed, stopped or removed
	}
	cur, ok := c.health[id]
	if !ok {
		cur = &ServerHealth{ServerID: id, State: HealthStopped}
		c.health[id] = cur
	}
	before := *cur
	mutate(cur)
	changed := !ok || cur.State != before.State || cur.LastError != before.LastError ||
		cur.ToolCount != before.ToolCount || cur.Restarts != before.Restarts
	if changed {
		cur.ChangedAt = time.Now()
	}
	snapshot := *cur
	obs := c.observer
	c.mu.Unlock()
	if changed && obs != nil {
		obs(snapshot)
	}
}

func (c *Connector) bumpRestarts(id string) {
	c.mu.Lock()
	if h, ok := c.health[id]; ok {
		h.Restarts++
	}
	c.mu.Unlock()
}

// redact removes every secret value from s. Values shorter than four
// characters are left alone: replacing "1" or "ab" everywhere would mangle the
// message and protect nothing a guess would not reveal.
func redact(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

func newProbeConnector() *Connector {
	return &Connector{
		client:   mcpsdk.NewClient(&mcpsdk.Implementation{Name: "cambrian-probe", Version: "1.0.0"}, nil),
		sessions: map[string]*mcpsdk.ClientSession{},
		watches:  map[string]context.CancelFunc{},
		health:   map[string]*ServerHealth{},
		gens:     map[string]uint64{},
		configs:  map[string]ServerConfig{},
		users:    newUserPool(),
	}
}

// ToolReport is one advertised tool with the policy the kernel would enforce.
type ToolReport struct {
	Name        string
	Description string
	Dangerous   bool
	Effects     []string
	// PolicySource is "tool" (a per-tool entry), "default" (the server default)
	// or "fail_closed" (neither: dangerous, read+write+egress).
	PolicySource string
}

// TestReport is what TestServer learned.
type TestReport struct {
	OK           bool
	LatencyMs    int64
	Err          string
	UsedLive     bool
	Tools        []ToolReport
	ProbeTool    string
	ProbeOK      bool
	ProbeOutput  string
	ProbeSkipped string
}

// maxProbeOutput bounds the probe tool's text echoed to the console.
const maxProbeOutput = 2000

// TestServer handshakes a server and lists its tools with their effective
// policy. It uses the LIVE session when the server is ready (so testing a
// running integration never spawns a second copy) and otherwise launches it
// once, ephemerally, with the same config and secrets a real start would use.
//
// When probeTool is named it is called once — but only when its effective
// policy is non-dangerous and limited to read/egress. A probe is an operator
// action outside ToolExecutor (no grants, no approval prompt), so it must never
// be able to reach a tool that writes; the policy check is what makes the
// catalog's "harmless" claim enforceable rather than trusted.
func (c *Connector) TestServer(ctx context.Context, s ServerConfig, probeTool string, probeArgs map[string]any) TestReport {
	start := time.Now()
	var rep TestReport
	var secrets []string

	sess, live := c.Session(s.ID)
	if live {
		if h, ok := c.Health(s.ID); !ok || h.State != HealthReady {
			live = false
		}
	}
	if live {
		rep.UsedLive = true
		// The live session's secrets are not at hand; resolve them the same way
		// a connect would, purely for redaction.
		_, secrets = s.explicitEnv()
		if s.AuthType != "" && s.AuthType != "none" {
			secrets = appendSecret(secrets, tokenFor(s.ID, s.AuthTokenEnv))
		}
	} else {
		tmp := newProbeConnector()
		var err error
		sess, secrets, err = tmp.connect(ctx, s)
		if err != nil {
			rep.Err = redact(err.Error(), secrets)
			rep.LatencyMs = time.Since(start).Milliseconds()
			return rep
		}
		defer func() { _ = sess.Close() }()
	}

	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		rep.Err = "connected, but listing tools failed: " + redact(err.Error(), secrets)
		rep.LatencyMs = time.Since(start).Milliseconds()
		return rep
	}
	rep.OK = true
	var probePolicy *ToolReport
	for _, t := range res.Tools {
		tr := DescribeToolPolicy(s, t.Name)
		tr.Description = t.Description
		rep.Tools = append(rep.Tools, tr)
		if t.Name == probeTool {
			cp := tr
			probePolicy = &cp
		}
	}
	rep.LatencyMs = time.Since(start).Milliseconds()

	if probeTool == "" {
		return rep
	}
	switch {
	case probePolicy == nil:
		rep.ProbeSkipped = "the server does not advertise the probe tool " + probeTool
	case !harmless(*probePolicy):
		rep.ProbeSkipped = "the probe tool's policy is not read-only (dangerous or writes); it is never called from a test"
	default:
		rep.ProbeTool = probeTool
		cres, cerr := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: probeTool, Arguments: probeArgs})
		switch {
		case cerr != nil:
			rep.ProbeOutput = redact(cerr.Error(), secrets)
		case cres.IsError:
			rep.ProbeOutput = redact(resultText(cres), secrets)
		default:
			rep.ProbeOK = true
			rep.ProbeOutput = redact(resultText(cres), secrets)
		}
		if len(rep.ProbeOutput) > maxProbeOutput {
			rep.ProbeOutput = rep.ProbeOutput[:maxProbeOutput] + "…"
		}
	}
	return rep
}

// harmless: not dangerous, and no effect beyond read / egress.
func harmless(t ToolReport) bool {
	if t.Dangerous {
		return false
	}
	for _, e := range t.Effects {
		if e != string(domain.EffectRead) && e != string(domain.EffectEgress) {
			return false
		}
	}
	return true
}

// DescribeToolPolicy reports the policy a server's tool resolves to — the same
// resolution discover applies (policyFor), with inferred effects filled in the
// way the registry infers them for a policy that declares none.
func DescribeToolPolicy(s ServerConfig, toolName string) ToolReport {
	src := "fail_closed"
	if _, ok := s.Tools[toolName]; ok {
		src = "tool"
	} else if s.DefaultPolicy != nil {
		src = "default"
	}
	p := policyFor(toolName, s.Tools, s.DefaultPolicy)
	st := domain.SystemTool{Name: toolName}
	applyPolicy(&st, p, s.DefaultClassificationTags)
	effects := st.Effects
	if len(effects) == 0 {
		effects = domain.InferEffects(st)
	}
	out := ToolReport{Name: toolName, Dangerous: st.Dangerous, PolicySource: src}
	for _, e := range effects {
		out.Effects = append(out.Effects, string(e))
	}
	return out
}

// resultText renders a tool result's text content (or its structured JSON).
func resultText(r *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(*mcpsdk.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 && r.StructuredContent != nil {
		if js, err := json.Marshal(r.StructuredContent); err == nil {
			return string(js)
		}
	}
	return b.String()
}
