package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// EvidenceNotifier wraps the ledger writer the ToolExecutor records through and,
// after an external MCP tool result is recorded, publishes a
// domain.ToolEvidenceRecordedEvent: a content digest plus the ledger entry id as
// the pointer (ENG-715, owner decision 2026-11-15). It never changes what is
// written and never fails the write — a provenance lane must not be able to
// stall or fail a tool call.
type EvidenceNotifier struct {
	inner domain.LedgerWriter
	bus   domain.EventBus
	now   func() time.Time
}

// NewEvidenceNotifier wraps inner. A nil bus returns inner unchanged.
func NewEvidenceNotifier(inner domain.LedgerWriter, bus domain.EventBus) domain.LedgerWriter {
	if inner == nil || bus == nil {
		return inner
	}
	return &EvidenceNotifier{inner: inner, bus: bus, now: time.Now}
}

// Record writes through and then publishes for an external MCP tool result.
func (n *EvidenceNotifier) Record(ctx context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	ent, err := n.inner.Record(ctx, rec)
	if err != nil || rec.Kind != domain.LedgerToolResult || ent.ID == "" {
		return ent, err
	}
	server, ok := domain.ExternalMCPServer(rec.ToolName)
	if !ok {
		return ent, nil
	}
	_ = n.bus.Publish(ctx, domain.ToolEvidenceRecordedEvent{ // best-effort: observation never fails the call
		LedgerID:       ent.ID,
		ToolName:       rec.ToolName,
		ServerID:       server,
		ArgsDigest:     digest(domain.CanonicalJSON(rec.ArgsJSON)),
		ContentDigest:  digest(rec.Body),
		ConversationID: rec.Provenance.ConversationID,
		TurnID:         rec.Provenance.TurnID,
		RunID:          rec.Provenance.RunID,
		StepIndex:      rec.Provenance.StepIndex,
		Writer:         rec.Writer.ID,
		Owner:          rec.Provenance.Owner,
		At:             n.now().UTC(),
	})
	return ent, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
