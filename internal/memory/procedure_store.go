package memory

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Persistence and lifecycle for the ADR-0094 procedural tier.

// procedureDoc renders a Procedure as its stored document. Pure, so the storage shape
// is testable without a store or an embedder.
//
// The EMBEDDING SUBJECT is the trigger alone — you retrieve by SITUATION and act on
// steps. Embedding the steps would match routines to each other rather than to the
// problem in front of the planner, and it is the same reasoning ADR-0049 D7 used to
// exclude specific entity ids from a scene's similarity key.
func procedureDoc(p domain.Procedure, vec []float32) *domain.Document {
	stepsJSON, _ := json.Marshal(p.Steps)
	// Text is the RECONSTRUCTION face: readable, and enough to act on without a second
	// lookup. The retrieval face is the embedded trigger above.
	var sb strings.Builder
	sb.WriteString("procedure: ")
	sb.WriteString(p.Trigger)
	for i, s := range p.Steps {
		sb.WriteString("\n")
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString(". [")
		sb.WriteString(strings.Join(s.RequiredCapabilities, "+"))
		sb.WriteString("] ")
		sb.WriteString(s.Intent)
	}
	return &domain.Document{
		ID:           p.ID,
		DocumentType: domain.DocTypeMnemonicProcedure,
		Text:         sb.String(),
		Summary:      "procedure: " + p.Trigger,
		Embedding:    domain.Embedding{Vector: vec},
		// Confidence drives activation, so a well-corroborated routine outranks a
		// barely-seen one without a separate ranking term.
		ActivationStrength: procedureActivation(p),
		Metadata: map[string]interface{}{
			"trigger":             p.Trigger,
			"steps":               string(stepsJSON),
			"capability_shape":    p.CapabilitySignature(),
			"source_experiences":  p.SourceExperiences,
			"contributing_agents": p.ContributingAgents, // ATTRIBUTION ONLY — never read at routing time
			"tags":                p.Tags,
			"sample_count":        p.SampleCount,
			"confidence":          p.Confidence,
			"status":              string(p.Status),
			"superseded_by":       p.SupersededBy,
		},
	}
}

// procedureActivation maps corroboration onto the existing activation curve, capped so a
// procedure can never outrank on volume alone. A deprecated routine sinks to the floor
// rather than being deleted: it stops being retrieved, and stays as evidence that this
// way of working stopped working (ADR-0094 D4).
func procedureActivation(p domain.Procedure) float64 {
	if p.Status != domain.ProcedureActive {
		return 0.01
	}
	a := 0.2 + 0.1*float64(p.SampleCount)
	if a > 0.8 {
		a = 0.8
	}
	return a
}

// SaveProcedure embeds a procedure's trigger and persists it.
//
// Best-effort by design: a procedure is an ENRICHMENT distilled from episodes that are
// already durable, so failing to store one loses an optimisation, never a fact. That is
// also why ADR-0049 A2.5 could permit the batch pass at all — nothing load-bearing may
// depend on it.
func SaveProcedure(ctx context.Context, store domain.VectorStore, embedder domain.Embedder, p domain.Procedure) error {
	if store == nil || embedder == nil || p.ID == "" || p.Trigger == "" {
		return nil
	}
	vec, err := embedder.Embed(ctx, p.Trigger)
	if err != nil {
		slog.WarnContext(ctx, "procedure: embed failed; not stored", "id", p.ID, "err", err)
		return err
	}
	return store.Save(ctx, procedureDoc(p, vec))
}

// ApplyOutcome folds one observation of a procedure being FOLLOWED back into it
// (ADR-0094 D4/D8).
//
// Confidence moves on a small consolidation rate, borrowed from the stability half of
// the CLS design in internal/centralexec/belief: a few bad runs must not
// catastrophically overwrite an established routine, and a few good ones must not
// enshrine a fluke. Without that damping this is the capability-erosion failure
// arXiv:2605.09315 measures, where continual self-adaptation degrades what already
// worked.
//
// Deprecation is a state transition, never a delete. A routine that stopped working is
// evidence in the same way a rejected benchmark arm is.
func ApplyOutcome(p domain.Procedure, succeeded bool, deprecateBelow float64) domain.Procedure {
	const rate = 0.15 // slow: stability over plasticity
	observed := 0.0
	if succeeded {
		observed = 1.0
	}
	if p.SampleCount == 0 {
		p.Confidence = observed
	} else {
		p.Confidence += rate * (observed - p.Confidence)
	}
	p.SampleCount++
	// Only demote a routine with enough evidence to have earned the judgement; a single
	// early failure is noise, not a verdict.
	if p.Status == domain.ProcedureActive && p.SampleCount >= 3 && p.Confidence < deprecateBelow {
		p.Status = domain.ProcedureDeprecated
	}
	return p
}

// Supersede links a replacement without deleting the original (ADR-0094 D4).
func Supersede(old domain.Procedure, replacementID string) domain.Procedure {
	old.Status = domain.ProcedureSuperseded
	old.SupersededBy = replacementID
	return old
}

// procedureFromDoc decodes a stored procedure. Pure; the inverse of procedureDoc.
func procedureFromDoc(d domain.Document) (domain.Procedure, bool) {
	if d.DocumentType != domain.DocTypeMnemonicProcedure {
		return domain.Procedure{}, false
	}
	p := domain.Procedure{ID: d.ID, Status: domain.ProcedureActive}
	if v, ok := d.Metadata["trigger"].(string); ok {
		p.Trigger = v
	}
	if raw, ok := d.Metadata["steps"].(string); ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.Steps)
	}
	if v, ok := d.Metadata["status"].(string); ok && v != "" {
		p.Status = domain.ProcedureStatus(v)
	}
	// JSONB round-trips numbers as float64; a typed assertion on int silently fails and
	// would reset the sample count to zero on every feedback cycle, quietly erasing the
	// corroboration this whole tier is built on.
	p.SampleCount = intFromMeta(d.Metadata["sample_count"])
	p.Confidence = floatFromMeta(d.Metadata["confidence"])
	p.SourceExperiences = stringsFromMeta(d.Metadata["source_experiences"])
	p.Tags = stringsFromMeta(d.Metadata["tags"])
	return p, true
}

func intFromMeta(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func floatFromMeta(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func stringsFromMeta(v interface{}) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []interface{}:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// FeedProcedureOutcome closes the ADR-0094 co-evolution loop: a routine that shaped a
// plan learns from how that plan turned out.
//
// This is the half of the loop that lives in the memory lane. Procedures shape plans
// (the D5 recall lane), and plan outcomes update procedures (here) — which is the
// dynamic arXiv:2607.01480 measures, where freezing either side costs more than 10%.
//
// Best-effort throughout: feedback is an improvement to an enrichment. A plan must never
// fail because the routine it consulted could not be updated afterwards.
func FeedProcedureOutcome(
	ctx context.Context,
	store domain.VectorStore,
	embedder domain.Embedder,
	procedureIDs []string,
	succeeded bool,
	deprecateBelow float64,
) {
	if store == nil || embedder == nil || len(procedureIDs) == 0 {
		return
	}
	for _, id := range procedureIDs {
		if id == "" {
			continue
		}
		// Kernel-internal read on the plan-completion path — no principal is asking, and
		// the routine ids come from the plan record, not from a query. Explicit bypass,
		// because the read chokepoint enforces by-identity reads (ADR-0095 D9) and would
		// otherwise report every routine as absent, so confidence would never move.
		// The write below is unchanged: it goes through the store's own write chokepoint.
		doc, err := store.GetByID(domain.WithScope(ctx, domain.ScopeSystem), id)
		if err != nil || doc == nil {
			slog.DebugContext(ctx, "procedure feedback: routine not found", "id", id, "err", err)
			continue
		}
		p, ok := procedureFromDoc(*doc)
		if !ok {
			continue
		}
		updated := ApplyOutcome(p, succeeded, deprecateBelow)
		if err := SaveProcedure(ctx, store, embedder, updated); err != nil {
			slog.WarnContext(ctx, "procedure feedback: could not persist update", "id", id, "err", err)
		}
	}
}
