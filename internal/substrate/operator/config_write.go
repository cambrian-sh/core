package operator

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// Config write effects. These strings cross the wire verbatim, so a console
// renders its copy from them.
const (
	// EffectLive — applied to the running kernel and stored.
	EffectLive = "live"
	// EffectRestartRequired — stored; the kernel reads this key only at boot.
	EffectRestartRequired = "restart_required"
	// EffectShadowed — stored, but a higher layer supplies the live value.
	EffectShadowed = "shadowed"
	// EffectRejected — not stored.
	EffectRejected = "rejected"
)

// ConfigWriter is the durable configuration write path (ADR-0101 D3).
//
// Deliberately separate from CommandEffects.SetRuntimeConfig, which stays what
// it has always been: the ADR-0054 automated-tuning seam — ephemeral, keyed by
// param name, driven by a tuner that must not leave permanent marks on a
// deployment. This port is a human saying "make it this, and keep it this way".
type ConfigWriter interface {
	// SetConfig applies and stores each key, returning one outcome per key.
	//
	// It returns outcomes rather than an error for per-key problems, because a
	// partial set can partly succeed and an all-or-nothing result would hide
	// which field did not take. An error is reserved for a failure of the whole
	// operation (the store is unreachable).
	SetConfig(values map[string]float64) ([]ConfigWriteOutcome, error)

	// DeleteConfig removes stored overrides, reverting each key to the layer
	// beneath the store.
	DeleteConfig(keys []string) ([]ConfigWriteOutcome, error)
}

// ConfigWriteOutcome is what happened to one key.
type ConfigWriteOutcome struct {
	Key    string
	Set    bool
	Effect string
	// ShadowedBy names the layer winning over the store — "env:CAMBRIAN_…".
	// Populated only when Effect == EffectShadowed, and it names the specific
	// variable: "something is pinning this" leaves an operator hunting.
	ShadowedBy string
	Error      string
}

// SecretWriter stores provider credentials (ADR-0101 D5). Write-only by
// construction: there is no read method on this interface, and the store's own
// decrypting method is unexported, so no operator RPC can reach a credential.
type SecretWriter interface {
	// SetGeneratorKey stores a credential for one generator. It returns the
	// outcome so a shadowing env var can be reported at write time, exactly as
	// for config: a key stored under an env var that overrides it changes nothing
	// until that variable is removed, and silence there is indistinguishable from
	// a broken save.
	SetGeneratorKey(generatorID, key string) (ConfigWriteOutcome, error)
	ClearGeneratorKey(generatorID string) error
}

// SetConfigWriter wires the durable config write path. nil ⇒ SetConfig and
// DeleteConfig return Unimplemented, which a console renders as "this kernel
// cannot persist settings" rather than offering a Save button that silently
// does nothing.
func (s *Service) SetConfigWriter(w ConfigWriter) { s.configWriter = w }

// SetSecretWriter wires the credential write path. nil ⇒ Unimplemented.
func (s *Service) SetSecretWriter(w SecretWriter) { s.secretWriter = w }

// HasConfigWriter reports whether durable config writes are available, so the
// composition root advertises the capability only when it can serve it.
func (s *Service) HasConfigWriter() bool { return s.configWriter != nil }

// SetConfig durably records configuration intent. Mutating: command_id + reason,
// audited, idempotent on command_id.
func (s *Service) SetConfig(ctx context.Context, req *pb.SetConfigOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.configWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no durable configuration store")
	}
	if len(req.GetValues()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "values is required")
	}

	var outcomes []ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"set_config", "config", configTargetID(req.GetValues()), summariseConfigWrite(req.GetValues()),
		func() error {
			var applyErr error
			outcomes, applyErr = s.configWriter.SetConfig(req.GetValues())
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp(outcomes),
	}, nil
}

// DeleteConfig removes stored overrides.
func (s *Service) DeleteConfig(ctx context.Context, req *pb.DeleteConfigOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.configWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no durable configuration store")
	}
	if len(req.GetKeys()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "keys is required")
	}

	var outcomes []ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"delete_config", "config", req.GetKeys()[0], joinKeys(req.GetKeys()),
		func() error {
			var applyErr error
			outcomes, applyErr = s.configWriter.DeleteConfig(req.GetKeys())
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp(outcomes),
	}, nil
}

// SetGeneratorKey stores a provider credential.
//
// The audit record names the generator and NOT the key — the whole point of a
// write-only credential is defeated if it lands in the audit log, which is the
// one store designed to be read and exported.
func (s *Service) SetGeneratorKey(ctx context.Context, req *pb.SetGeneratorKeyOpRequest) (*pb.SetConfigOpResponse, error) {
	if s.secretWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no credential store")
	}
	if req.GetGeneratorId() == "" {
		return nil, status.Error(codes.InvalidArgument, "generator_id is required")
	}
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required; use ClearGeneratorKey to remove one")
	}

	var outcome ConfigWriteOutcome
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"set_generator_key", "generator", req.GetGeneratorId(),
		"api key set for "+req.GetGeneratorId(), // never the key
		func() error {
			var applyErr error
			outcome, applyErr = s.secretWriter.SetGeneratorKey(req.GetGeneratorId(), req.GetKey())
			return applyErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SetConfigOpResponse{
		CommandId: ack.GetCommandId(),
		Deduped:   ack.GetDeduped(),
		Outcomes:  outcomesToOp([]ConfigWriteOutcome{outcome}),
	}, nil
}

// ClearGeneratorKey removes a stored credential.
func (s *Service) ClearGeneratorKey(ctx context.Context, req *pb.ClearGeneratorKeyOpRequest) (*pb.CommandAck, error) {
	if s.secretWriter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no credential store")
	}
	if req.GetGeneratorId() == "" {
		return nil, status.Error(codes.InvalidArgument, "generator_id is required")
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"clear_generator_key", "generator", req.GetGeneratorId(),
		"api key cleared for "+req.GetGeneratorId(),
		func() error { return s.secretWriter.ClearGeneratorKey(req.GetGeneratorId()) })
}

func outcomesToOp(in []ConfigWriteOutcome) []*pb.ConfigWriteOutcomeOp {
	out := make([]*pb.ConfigWriteOutcomeOp, 0, len(in))
	for _, o := range in {
		out = append(out, &pb.ConfigWriteOutcomeOp{
			Key:        o.Key,
			Stored:     o.Set,
			Effect:     o.Effect,
			ShadowedBy: o.ShadowedBy,
			Error:      o.Error,
		})
	}
	return out
}

// configTargetID picks a stable audit target for a multi-key write: the
// lexically first key. The full set is in the `after` summary; the target field
// exists so an audit query can filter, not to be exhaustive.
func configTargetID(values map[string]float64) string {
	first := ""
	for k := range values {
		if first == "" || k < first {
			first = k
		}
	}
	return first
}

// summariseConfigWrite renders the write for the audit record, sorted so two
// identical writes produce identical audit text and a diff of the log is
// readable.
func summariseConfigWrite(values map[string]float64) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strconv.FormatFloat(values[k], 'g', -1, 64))
	}
	return b.String()
}

func joinKeys(keys []string) string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return "cleared: " + strings.Join(sorted, ", ")
}
