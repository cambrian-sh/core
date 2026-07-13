package memory

import (
	"context"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// planIDFromSceneID inverts the scene id convention "scene-{planID}" (ADR-0049 D5).
func planIDFromSceneID(sceneID string) string { return strings.TrimPrefix(sceneID, "scene-") }

// resolveActionPath fetches the action records of one plan — the PATH taken in that
// situation — in chronological order. The plan_id stamp on each action (D11) makes this
// a metadata-containment lookup; the result is narrowed to actions and time-ordered.
func resolveActionPath(ctx context.Context, store domain.VectorStore, planID string) []domain.Document {
	if planID == "" || store == nil {
		return nil
	}
	docs, err := store.QueryByMetadata(ctx, map[string]string{"plan_id": planID}, 50)
	if err != nil {
		return nil
	}
	return filterActionsForPlan(docs)
}

// retrievePrecedents turns floor-gated scene hits into failure-weighted transitions by
// resolving each scene's action path. Shared by the planner push (Issue 013) and the
// agent pull (Issue 014) so both reason over identical precedent data.
func retrievePrecedents(ctx context.Context, store domain.VectorStore, scenes []domain.SearchResult) []domain.Precedent {
	precedents := make([]domain.Precedent, 0, len(scenes))
	for _, s := range scenes {
		planID := planIDFromSceneID(s.Document.ID)
		actions := resolveActionPath(ctx, store, planID)
		precedents = append(precedents, buildPrecedent(s, actions))
	}
	sortPrecedentsFailureFirst(precedents)
	return precedents
}

// ADR-0049 D11 — world-model precedents (transitions).
//
// A precedent answers "last time I was in a situation like this, what did I do and how
// did it turn out?". It is assembled deterministically from stored experience — a scene
// (the situation + outcome) plus its action path — and handed to the LLM to reason over.
// The retrieval is similarity-gated (below the floor → no precedent, never a fabricated
// analogy) and failure-weighted (negative precedents rank first, because a known failure
// under similar conditions is the most decision-relevant signal).

// buildPrecedent assembles one transition from a retrieved scene and its resolved action
// path. Pure: the situation is the scene's abstracted projection (falling back to its
// reconstruction text), the outcome is read off metadata, and the actions are the
// compact action lines in order. It never guesses — every field comes from stored data.
func buildPrecedent(scene domain.SearchResult, actions []domain.Document) domain.Precedent {
	doc := scene.Document
	situation := doc.Text
	if proj, ok := doc.Metadata["projection"].(string); ok && proj != "" {
		situation = proj
	}
	outcome, _ := doc.Metadata["outcome"].(string)
	if outcome == "" {
		outcome = "unknown"
	}
	lines := make([]string, 0, len(actions))
	for _, a := range actions {
		if t := strings.TrimSpace(a.Text); t != "" {
			lines = append(lines, t)
		}
	}
	return domain.Precedent{
		SceneID:    doc.ID,
		Situation:  situation,
		Outcome:    outcome,
		Success:    outcome == "success",
		Actions:    lines,
		Similarity: scene.Score,
	}
}

// sortPrecedentsFailureFirst orders precedents so the most decision-relevant come first:
// FAILURES ahead of successes (ADR-0049 D11 failure-weighting), then by descending
// similarity within each group. Pure; stable. Mutates the slice in place.
func sortPrecedentsFailureFirst(ps []domain.Precedent) {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Success != ps[j].Success {
			return !ps[i].Success // a failure (Success=false) sorts before a success
		}
		return ps[i].Similarity > ps[j].Similarity
	})
}

// precedentText renders a transition as a single compact line for the agent's recall
// lane (the planner uses a richer XML block). Pure.
func precedentText(p domain.Precedent) string {
	var sb strings.Builder
	sb.WriteString("SITUATION: ")
	sb.WriteString(p.Situation)
	sb.WriteString(" | OUTCOME: ")
	sb.WriteString(p.Outcome)
	if len(p.Actions) > 0 {
		sb.WriteString(" | ACTIONS: ")
		sb.WriteString(strings.Join(p.Actions, "; "))
	}
	return sb.String()
}

// filterActionsForPlan keeps only the action records of one plan, in chronological
// order. The provenance query (QueryByMetadata on plan_id) returns the scene and any
// other plan-tagged docs too; this narrows to the action path. Pure.
func filterActionsForPlan(docs []domain.Document) []domain.Document {
	out := make([]domain.Document, 0, len(docs))
	for _, d := range docs {
		if d.DocumentType == domain.DocTypeMnemonicAction {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, _ := out[i].Metadata["timestamp"].(string)
		tj, _ := out[j].Metadata["timestamp"].(string)
		return ti < tj // RFC3339 sorts lexicographically by time
	})
	return out
}
