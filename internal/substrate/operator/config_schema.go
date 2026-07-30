package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// ConfigSchemaReporter is the read half of the runtime-config surface (ADR-0101
// D7). `SetRuntimeConfig` has been able to WRITE a tunable since contract 0047;
// nothing could read one back, so the Dispatch & retrieval form could show only
// the kernel's documented defaults, labelled as a guess.
//
// Scoped deliberately to the NUMERIC tuning surface, mirroring
// SetRuntimeConfigRequest.params (map<string,double>) so read and write agree on
// shape and booleans travel as 1/0 in both directions. The store holds
// non-numeric config too — generators, MCP servers, the embedder — but those get
// typed accessors (ListGenerators, ListMCPServers) rather than being forced
// through a value union that would serve neither job well.
type ConfigSchemaReporter interface {
	// SchemaJSON is a JSON Schema (Draft 2020-12) describing the editable
	// tunables: types, ranges and descriptions.
	SchemaJSON() (schema, version, hash string)
	// EditableKeys are tunables the operator plane may write. KernelOnlyKeys are
	// reported so the form can render them read-only rather than omitting them —
	// a field an operator cannot find is indistinguishable from one that does not
	// exist.
	EditableKeys() []string
	KernelOnlyKeys() []string
	// CurrentValues are the live values, keyed identically to the write path.
	CurrentValues() map[string]float64
	// ValueSource maps each key to the layer supplying it: "default",
	// "tuning.json", "store", "env:CAMBRIAN_…" and so on.
	//
	// This is the field that closes the worst configuration bug there is — you
	// change a value, it saves, and nothing happens because something upstream
	// pins it. Without it the form can only disclaim.
	ValueSource() map[string]string
}

// SetConfigSchemaReporter wires the runtime-config read surface. nil ⇒
// GetConfigSchema returns Unimplemented, which the console renders as "this
// kernel cannot tell me its current values" rather than showing defaults as if
// they were live.
func (s *Service) SetConfigSchemaReporter(r ConfigSchemaReporter) { s.configSchema = r }

// GetConfigSchema returns the editable tunables, their live values, and where
// each value came from. Read RPC (no command_id).
func (s *Service) GetConfigSchema(_ context.Context, _ *pb.GetConfigSchemaOpRequest) (*pb.ConfigSchemaOp, error) {
	if s.configSchema == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not report its configuration schema")
	}
	schema, version, hash := s.configSchema.SchemaJSON()
	return &pb.ConfigSchemaOp{
		SchemaVersion:  version,
		SchemaJson:     schema,
		SchemaHash:     hash,
		EditableKeys:   s.configSchema.EditableKeys(),
		KernelOnlyKeys: s.configSchema.KernelOnlyKeys(),
		CurrentValues:  s.configSchema.CurrentValues(),
		ValueSource:    s.configSchema.ValueSource(),
	}, nil
}

// HasConfigSchema reports whether the runtime-config read surface is wired, so
// the composition root can advertise "config-schema" only when it can be served.
func (s *Service) HasConfigSchema() bool { return s.configSchema != nil }
