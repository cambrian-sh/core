// Package runtrace writes the decision-level run trace (ADR-0145 D10, ADR-0139 C6)
// as OpenTelemetry-GenAI-shaped spans, one JSON object per line, into a configured
// directory (execution.trace.dir). A harness copies the files into its run folder;
// nothing in the kernel reads them back.
package runtrace

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
)

// dedupedAttrs are large, mostly repeated inputs (the system prompt, the tool
// menu). Each is written in full the first time its content appears in a file and
// by hash afterwards: "<attr>_sha256" is always present, so every span still names
// exactly what it saw and a reader resolves the hash to the first full copy.
var dedupedAttrs = []string{domain.GenAISystemInstructions, domain.GenAIToolDefinitions}

// Writer is a domain.RunTracer appending spans to one JSONL file per process.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	buf      *bufio.Writer
	path     string
	maxChars int
	service  string
	seen     map[string]bool // dedupedAttrs content hashes already written in full
	broken   bool            // a write failed: stop trying, log once
}

var _ domain.RunTracer = (*Writer)(nil)

// Open creates dir and a new trace file "<service>-<UTC stamp>-<pid>.jsonl" in it.
// maxChars bounds every string attribute (and every string inside one); <= 0 is
// unbounded.
func Open(dir, service string, maxChars int) (*Writer, error) {
	if dir == "" {
		return nil, fmt.Errorf("runtrace: no directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("runtrace: create %s: %w", dir, err)
	}
	name := fmt.Sprintf("%s-%s-%d.jsonl", service, time.Now().UTC().Format("20060102T150405Z"), os.Getpid())
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("runtrace: open %s: %w", path, err)
	}
	return &Writer{f: f, buf: bufio.NewWriterSize(f, 64<<10), path: path, maxChars: maxChars,
		service: service, seen: map[string]bool{}}, nil
}

// Path is the file this writer appends to.
func (w *Writer) Path() string { return w.path }

// record is one line: an OTLP-like span flattened to JSON.
type record struct {
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	StartUnixNs  int64          `json:"start_time_unix_nano"`
	EndUnixNs    int64          `json:"end_time_unix_nano"`
	Attributes   map[string]any `json:"attributes"`
	Status       map[string]any `json:"status"`
	Resource     map[string]any `json:"resource"`
	SchemaFamily string         `json:"schema"`
}

// Span appends one span — or, with s.Phase == domain.TracePhaseStart, the start
// event of one: same span id as the finished span that follows, the request
// attributes, "cambrian.phase": "start" and end_time_unix_nano 0. Every line is
// flushed as it is written, so a follower of the file sees a call the moment it
// starts. Never fails the caller: an error is logged once and tracing stops for
// this writer.
func (w *Writer) Span(ctx context.Context, s domain.TraceSpan) {
	if w == nil {
		return
	}
	ctxAttrs := domain.TraceAttributesFromContext(ctx)
	attrs := make(map[string]any, len(s.Attributes)+len(ctxAttrs)+7)
	for k, v := range ctxAttrs {
		attrs[k] = v
	}
	for k, v := range s.Attributes {
		attrs[k] = v
	}
	if s.Phase != "" {
		attrs[domain.TraceAttrPhase] = s.Phase
	}
	session, _ := domain.SessionIDFromContext(ctx)
	if session != "" {
		attrs["cambrian.session.id"] = string(session)
	}
	if conv := domain.ConversationIDFromContext(ctx); conv != "" {
		if _, set := attrs[domain.GenAIConversationID]; !set {
			attrs[domain.GenAIConversationID] = conv
		}
	}
	if p := domain.PrincipalFromContext(ctx); p.ID != "" {
		attrs["cambrian.principal"] = string(p.Kind) + ":" + p.ID
	}
	if rp := domain.RunPrincipalFromContext(ctx); rp != "" {
		attrs["cambrian.run_principal"] = rp
	}
	kind := s.Kind
	if kind == "" {
		kind = "INTERNAL"
	}
	status := map[string]any{"code": "OK"}
	if s.Error != "" {
		status = map[string]any{"code": "ERROR", "message": s.Error}
	}
	end := s.End
	if end.IsZero() {
		end = time.Now()
	}
	start := s.Start
	if start.IsZero() {
		start = end
	}
	var endNs int64
	if s.Phase != domain.TracePhaseStart {
		endNs = end.UnixNano() // a start event has no end yet: 0
	}
	spanID := s.SpanID
	if spanID == "" {
		spanID = randomHex(8)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken {
		return
	}
	for _, k := range dedupedAttrs {
		v, ok := attrs[k]
		if !ok {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			delete(attrs, k)
			continue
		}
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		attrs[k+"_sha256"] = h
		if w.seen[k+h] {
			delete(attrs, k) // written in full earlier in this file
		} else {
			w.seen[k+h] = true
		}
	}
	rec := record{
		TraceID:     traceID(session),
		SpanID:      spanID,
		Name:        s.Name,
		Kind:        kind,
		StartUnixNs: start.UnixNano(),
		EndUnixNs:   endNs,
		Attributes:  bound(attrs, w.maxChars).(map[string]any),
		Status:      status,
		Resource:    map[string]any{"service.name": w.service},
		// The conventions are in development; name the family so a reader knows
		// which document the attribute names come from.
		SchemaFamily: "otel-semconv-genai",
	}
	line, err := json.Marshal(rec)
	if err == nil {
		line = append(line, '\n')
		if _, err = w.buf.Write(line); err == nil {
			err = w.buf.Flush() // a crash must not lose the decisive last spans
		}
	}
	if err != nil {
		w.broken = true
		slog.Warn("runtrace: write failed, tracing stopped for this process", "path", w.path, "err", err)
	}
}

// Close flushes and closes the file.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.buf.Flush() // best-effort: Close reports the file error below
	return w.f.Close()
}

// traceID is the OTel trace id: derived from the session so every span of one
// unit of work shares it across the kernel and agent files; random without one.
func traceID(session domain.SessionID) string {
	if session == "" {
		return randomHex(16)
	}
	sum := sha256.Sum256([]byte("cambrian-session:" + string(session)))
	return hex.EncodeToString(sum[:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail on supported platforms
	return hex.EncodeToString(b)
}

// bound truncates every string longer than max runes inside v (maps, slices,
// []byte). Non-string scalars pass through. max <= 0 leaves v unchanged.
func bound(v any, max int) any {
	if max <= 0 {
		return v
	}
	switch t := v.(type) {
	case string:
		return truncate(t, max)
	case []byte:
		return truncate(string(t), max)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = bound(x, max)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = bound(x, max)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = bound(x, max)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = truncate(x, max)
		}
		return out
	default:
		return v
	}
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + fmt.Sprintf("…[truncated %d of %d chars]", len(r)-max, len(r))
}
