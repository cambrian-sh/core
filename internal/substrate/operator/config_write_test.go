package operator

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
)

type stubConfigWriter struct {
	outs []ConfigWriteOutcome
	err  error
	// gotValues records what reached the writer, so a test can prove the handler
	// passed the request through rather than inventing a response.
	gotValues map[string]float64
	gotKeys   []string
}

func (s *stubConfigWriter) SetConfig(v map[string]float64) ([]ConfigWriteOutcome, error) {
	s.gotValues = v
	return s.outs, s.err
}
func (s *stubConfigWriter) DeleteConfig(k []string) ([]ConfigWriteOutcome, error) {
	s.gotKeys = k
	return s.outs, s.err
}

type stubSecretWriter struct {
	out    ConfigWriteOutcome
	gotKey string
}

func (s *stubSecretWriter) SetGeneratorKey(id, key string) (ConfigWriteOutcome, error) {
	s.gotKey = key
	return s.out, nil
}
func (s *stubSecretWriter) ClearGeneratorKey(string) error { return nil }

// newWriteService builds a Service with the write paths wired and a real
// in-memory audit store, so the audit assertions exercise the production
// recording path rather than a stub that could diverge from it.
func newWriteService(cw ConfigWriter, sw SecretWriter) *Service {
	// A real Spool too: runMutation emits an AuditEvent onto the feed, so a nil
	// feed would make every mutating test panic on a path production never takes.
	s := &Service{audit: NewInMemoryAuditStore(), feed: NewSpool(SpoolConfig{})}
	if cw != nil {
		s.SetConfigWriter(cw)
	}
	if sw != nil {
		s.SetSecretWriter(sw)
	}
	return s
}

// An unwired write path must answer Unimplemented, so a console renders a
// read-only form rather than a Save button that silently discards the change.
func TestConfigWrite_UnwiredReturnsUnimplemented(t *testing.T) {
	s := &Service{}
	ctx := context.Background()

	if _, err := s.SetConfig(ctx, &pb.SetConfigOpRequest{
		CommandId: "c", Reason: "r", Values: map[string]float64{"a": 1},
	}); codeOf(err) != codes.Unimplemented {
		t.Errorf("SetConfig: code = %v, want Unimplemented", codeOf(err))
	}
	if _, err := s.DeleteConfig(ctx, &pb.DeleteConfigOpRequest{
		CommandId: "c", Reason: "r", Keys: []string{"a"},
	}); codeOf(err) != codes.Unimplemented {
		t.Errorf("DeleteConfig: code = %v, want Unimplemented", codeOf(err))
	}
	if _, err := s.SetGeneratorKey(ctx, &pb.SetGeneratorKeyOpRequest{
		CommandId: "c", Reason: "r", GeneratorId: "g", Key: "k",
	}); codeOf(err) != codes.Unimplemented {
		t.Errorf("SetGeneratorKey: code = %v, want Unimplemented", codeOf(err))
	}
}

// Mutating RPCs require command_id + reason, like every other one on this plane.
// Without them the audit log records an action nobody can attribute or explain.
func TestConfigWrite_RequiresCommandIDAndReason(t *testing.T) {
	s := newWriteService(&stubConfigWriter{}, nil)

	if _, err := s.SetConfig(context.Background(), &pb.SetConfigOpRequest{
		Values: map[string]float64{"execution.ewma_alpha": 0.5},
	}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}

func TestConfigWrite_EmptyRequestIsRejected(t *testing.T) {
	s := newWriteService(&stubConfigWriter{}, nil)
	ctx := context.Background()

	if _, err := s.SetConfig(ctx, &pb.SetConfigOpRequest{CommandId: "c", Reason: "r"}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("SetConfig with no values: code = %v, want InvalidArgument", codeOf(err))
	}
	if _, err := s.DeleteConfig(ctx, &pb.DeleteConfigOpRequest{CommandId: "c", Reason: "r"}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("DeleteConfig with no keys: code = %v, want InvalidArgument", codeOf(err))
	}
}

// Per-key outcomes must reach the wire intact. Collapsing them into one ok/fail
// would hide which field did not take, which is the state an operator most needs.
func TestSetConfig_OutcomesReachTheWire(t *testing.T) {
	w := &stubConfigWriter{outs: []ConfigWriteOutcome{
		{Key: "execution.blend_weight_cosine", Set: true, Effect: EffectLive},
		{Key: "execution.ewma_alpha", Set: true, Effect: EffectShadowed, ShadowedBy: "env:CAMBRIAN_EXECUTION__EWMA_ALPHA"},
		{Key: "execution.bogus", Effect: EffectRejected, Error: "unknown configuration key on this kernel"},
	}}
	s := newWriteService(w, nil)

	resp, err := s.SetConfig(context.Background(), &pb.SetConfigOpRequest{
		CommandId: "c1", Reason: "tuning retrieval",
		Values: map[string]float64{"execution.blend_weight_cosine": 0.6},
	})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if len(resp.GetOutcomes()) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(resp.GetOutcomes()))
	}

	byKey := map[string]*pb.ConfigWriteOutcomeOp{}
	for _, o := range resp.GetOutcomes() {
		byKey[o.GetKey()] = o
	}
	if got := byKey["execution.ewma_alpha"]; got.GetEffect() != EffectShadowed ||
		got.GetShadowedBy() != "env:CAMBRIAN_EXECUTION__EWMA_ALPHA" || !got.GetStored() {
		t.Fatalf("shadowed outcome lost detail: %+v", got)
	}
	if got := byKey["execution.bogus"]; got.GetStored() || got.GetError() == "" {
		t.Fatalf("rejected outcome lost detail: %+v", got)
	}
	// The handler must pass the request through, not synthesise a reply.
	if w.gotValues["execution.blend_weight_cosine"] != 0.6 {
		t.Fatalf("writer received %+v", w.gotValues)
	}
}

// The credential must not land in the audit record. The audit log is the one
// store designed to be read and exported, so a key that reaches it is a key that
// has left the boundary — which defeats the whole point of a write-only field.
func TestSetGeneratorKey_KeyNeverReachesTheAuditLog(t *testing.T) {
	const secret = "sk-live-SUPERSECRET-9999"
	s := newWriteService(nil, &stubSecretWriter{out: ConfigWriteOutcome{
		Key: "generator:gpt:api_key", Set: true, Effect: EffectLive,
	}})
	audit := s.audit.(*InMemoryAuditStore)

	if _, err := s.SetGeneratorKey(context.Background(), &pb.SetGeneratorKeyOpRequest{
		CommandId: "c1", Reason: "rotating the key", GeneratorId: "gpt", Key: secret,
	}); err != nil {
		t.Fatalf("SetGeneratorKey: %v", err)
	}

	if len(audit.entries) == 0 {
		t.Fatal("nothing was audited — a credential write must be recorded")
	}
	for _, e := range audit.entries {
		blob := e.Actor + e.ActionType + e.TargetType + e.TargetID + e.After + e.Reason
		if strings.Contains(blob, secret) {
			t.Fatalf("the API key appears in the audit record: %q", e.After)
		}
	}
}

func TestSetGeneratorKey_EmptyKeyIsRejected(t *testing.T) {
	s := newWriteService(nil, &stubSecretWriter{})

	// An empty key must not silently CLEAR the credential — that is a different
	// operation with a different audit meaning, and it has its own RPC.
	if _, err := s.SetGeneratorKey(context.Background(), &pb.SetGeneratorKeyOpRequest{
		CommandId: "c", Reason: "r", GeneratorId: "gpt",
	}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}

func TestHasConfigWriter_TracksWiring(t *testing.T) {
	if (&Service{}).HasConfigWriter() {
		t.Fatal("bare service reports a config writer")
	}
	if !newWriteService(&stubConfigWriter{}, nil).HasConfigWriter() {
		t.Fatal("wired service reports no config writer")
	}
}
