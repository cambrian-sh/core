package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ProcedureInducer is the ADR-0094 D3 batch pass: read completed episodes, cluster them
// deterministically, and persist the routines that recur.
//
// It runs OFFLINE, which ADR-0049 §A2.5 now permits — and only under the constraints
// that permission carries. Nothing load-bearing may depend on it: the online path is
// fully correct with this pass never running, a missed pass degrades enrichment rather
// than breaking recall or planning, and it produces only abstractions, never repairs to
// primary records. That is the difference between this and the nightly consolidation
// ADR-0049 originally rejected, which the GRAPH depended on and which therefore broke
// the graph when it did not fire.
type ProcedureInducer struct {
	Store      domain.VectorStore
	Embedder   domain.Embedder
	Experience domain.ExperienceStore // may be nil ⇒ no provenance links
	// Naturalizer turns a candidate into human-readable step intents. Nil ⇒ the
	// deterministic fallback. See naturalize().
	Naturalizer ProcedureNaturalizer
	// MinSamples is the promotion threshold: below it a cluster is a coincidence.
	MinSamples int
}

// ProcedureNaturalizer writes readable step intents for a candidate routine. This is the
// ONLY place an LLM may enter the procedural tier, and it is deliberately the last
// stage: the deterministic pass has already decided that this IS a routine, so the model
// only phrases what was found. It never decides whether something is a procedure.
//
// One call per PROCEDURE — not per episode and not per chunk — so cost scales with the
// number of distinct routines, which saturates, rather than with traffic, which does
// not. That is what keeps the tier inside the kernel's "no LLM at chunk granularity"
// rule.
type ProcedureNaturalizer interface {
	Naturalize(ctx context.Context, trigger string, capabilities []string) ([]string, error)
}

// Induce runs one pass. Returns the number of procedures written.
func (p *ProcedureInducer) Induce(ctx context.Context, episodes []EpisodeShape) (int, error) {
	if p == nil || p.Store == nil || p.Embedder == nil {
		return 0, nil
	}
	minSamples := p.MinSamples
	if minSamples < 2 {
		minSamples = 2 // one occurrence is never a routine
	}
	candidates := InduceCandidates(episodes, minSamples)

	written := 0
	for _, c := range candidates {
		caps := strings.Split(c.Signature, ">")
		intents := p.naturalize(ctx, c.Trigger, caps)
		proc := c.ToProcedure(procedureID(c), intents)
		if err := SaveProcedure(ctx, p.Store, p.Embedder, proc); err != nil {
			// Best-effort: one unstorable routine must not abort the pass and lose the
			// rest. The pass is idempotent, so the next run retries it.
			slog.WarnContext(ctx, "procedure induction: save failed", "id", proc.ID, "err", err)
			continue
		}
		if p.Experience != nil {
			if err := p.Experience.LinkDerivation(ctx, proc.ID, proc.SourceExperiences); err != nil {
				// The routine is already durable; only its provenance edge is missing.
				// Logged rather than rolled back, because ADR-0095 D9's audit degrading
				// is strictly better than losing the artifact.
				slog.WarnContext(ctx, "procedure induction: provenance not linked",
					"id", proc.ID, "err", err)
			}
		}
		written++
	}
	return written, nil
}

// naturalize produces step intents, falling back to a deterministic rendering.
//
// FAIL-OPEN, matching every other optional LLM stage in the retrieval stack: a routine
// with mechanical intents ("perform: deploy") is still a correct, usable routine — the
// capabilities and their order carry the actual meaning. Failing the induction because a
// model was unreachable would make the tier depend on an oracle it does not need.
func (p *ProcedureInducer) naturalize(ctx context.Context, trigger string, caps []string) []string {
	if p.Naturalizer != nil {
		if intents, err := p.Naturalizer.Naturalize(ctx, trigger, caps); err == nil && len(intents) == len(caps) {
			return intents
		} else if err != nil {
			slog.DebugContext(ctx, "procedure induction: naturalizer unavailable; using deterministic intents",
				"err", err)
		}
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, "perform: "+strings.ReplaceAll(c, "+", " and "))
	}
	return out
}

// procedureID is derived from the cluster's SHAPE, not from a counter or a timestamp, so
// re-running the pass over the same corpus updates the same routine in place instead of
// minting a duplicate every night.
func procedureID(c ProcedureCandidate) string {
	sum := sha256.Sum256([]byte(c.Signature + "\x00" + normalizeTrigger(c.Trigger)))
	return "proc-" + hex.EncodeToString(sum[:8])
}

// EpisodesFromScenes reduces stored outcome records to the shape induction needs.
//
// Only what clustering uses is extracted — trigger, capability sequence, outcome,
// classification. The episode's prose never reaches the inducer, which is what keeps the
// pass deterministic and cheap.
func EpisodesFromScenes(docs []domain.Document) []EpisodeShape {
	out := make([]EpisodeShape, 0, len(docs))
	for _, d := range docs {
		if d.DocumentType != domain.DocTypeMnemonicScene {
			continue
		}
		trigger, _ := d.Metadata["projection"].(string)
		if trigger == "" {
			continue // no abstracted projection ⇒ nothing to match a situation on
		}
		outcome, _ := d.Metadata["outcome"].(string)
		out = append(out, EpisodeShape{
			ExperienceID: experienceIDOf(d),
			Trigger:      trigger,
			// scene_identity (shape + the concrete entities engaged) is what
			// distinguishes a RERUN from a VARIANT. The projection cannot: it renders
			// entities by kind and count, so three plans touching different files look
			// identical. Counting by projection would collapse genuine variants into
			// one observation and suppress every promotion. Falls back to the
			// experience id when absent, which preserves per-row counting for older
			// scenes rather than silently merging them.
			RawTrigger: sceneIdentityOf(d),
			Capabilities: capabilitiesOf(d),
			Succeeded:    outcome == "success",
			Tags:         docTags(d.Metadata),
		})
	}
	return out
}

func experienceIDOf(d domain.Document) string {
	if d.ExperienceID != "" {
		return d.ExperienceID
	}
	if v, ok := d.Metadata["plan_id"].(string); ok && v != "" {
		return "exp-" + v
	}
	return d.ID
}

// capabilitiesOf reads the ordered capability sequence an episode's steps required.
// Absent (the capability_contract arm is off) ⇒ no shape, and the episode is skipped by
// the inducer rather than grouped on situation alone, which would over-group.
func capabilitiesOf(d domain.Document) []string {
	raw, ok := d.Metadata["capabilities"].(string)
	if !ok || raw == "" {
		return nil
	}
	var caps []string
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return nil
	}
	return caps
}

// sceneIdentityOf reads the dedup identity a scene was stored with.
func sceneIdentityOf(d domain.Document) string {
	if v, ok := d.Metadata["scene_identity"].(string); ok && v != "" {
		return v
	}
	return ""
}
