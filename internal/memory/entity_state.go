package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0049 D9 — the entity record as a deterministic, rebuildable cache.
//
// An entity's current state is NOT authored; it is FOLDED from the engagements that
// touched it. Each engagement observes a few fields (a delete observes exists=false; an
// overwrite observes a new content_ref). The materialized record is the field-level
// last-write-wins merge of every observation — and because the merge key is a monotonic
// ordinal, replaying the provenance in ANY order reproduces the same view exactly. No
// LLM, no guessing: the record reflects only what actions did.

// fieldValue is one materialized field plus the ordinal of the observation that set it.
// The ordinal — not arrival order — is the authoritative last-write-wins key, so the
// fold is order-independent (the rebuildable-cache guarantee).
type fieldValue struct {
	Value any       `json:"value"`
	Seq   uint64    `json:"seq"`
	At    time.Time `json:"at"`
}

// entityObservation is the field-level effect a single engagement had on one entity,
// derived deterministically from the tool verb + output. Seq totally orders observations.
type entityObservation struct {
	Seq    uint64
	At     time.Time
	Fields map[string]any
}

// actionVerb classifies a tool name into the state transition it performs, by mechanical
// substring match (the deterministic-safety exception to Zero-Hardcode — this is not
// agent routing, it is reading what a verb means). "" = no materialized effect.
func actionVerb(toolName string) string {
	n := strings.ToLower(toolName)
	switch {
	case containsAny(n, "delete", "remove", "unlink", "rmdir", "rm_"):
		return "delete"
	case containsAny(n, "write", "create", "overwrite", "save", "put", "update", "append", "edit", "mkdir", "mv_", "rename", "copy"):
		return "write"
	default:
		return "" // read/list/etc. — observes presence but mutates no field
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// contentFingerprint is a deterministic content hash used as a file entity's content_ref
// (a by-reference pointer — the ADR-0049 D6 baseline cid plays the same role in CAS).
// An overwrite changes the bytes → changes the fingerprint → supersedes the field.
func contentFingerprint(output []byte) string {
	if len(output) == 0 {
		return ""
	}
	sum := sha256.Sum256(output)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// observedFields is the deterministic field effect an engagement has on an entity of a
// given kind (ADR-0049 D9). A DELETE flips exists=false (detected by verb, so it holds
// even when the call was classified as a read). EVERY OTHER engagement — read or write —
// observes exists=true plus, for a content-bearing kind (file/api), the content baseline
// just seen (contentRef: a CAS cid, or a fingerprint when no ContentStore). A read is the
// richest baseline source — its output IS the content. Pure/deterministic.
func observedFields(kind, toolName, contentRef string) map[string]any {
	if actionVerb(toolName) == "delete" {
		return map[string]any{"exists": false}
	}
	f := map[string]any{"exists": true}
	if contentRef != "" && kind != "dir" { // a directory has no content body of its own
		f["content_ref"] = contentRef
	}
	return f
}

// mergeObservation applies one observation to a materialized field set, last-write-wins
// PER FIELD by ordinal: a field is overwritten only by a strictly-later observation;
// untouched fields are retained. Pure; order-independent (a later fold of an earlier
// observation is a no-op because its Seq loses). Returns a fresh map (no mutation).
func mergeObservation(cur map[string]fieldValue, obs entityObservation) map[string]fieldValue {
	out := make(map[string]fieldValue, len(cur)+len(obs.Fields))
	for k, v := range cur {
		out[k] = v
	}
	for k, val := range obs.Fields {
		if ex, ok := out[k]; ok && ex.Seq >= obs.Seq {
			continue // a later (or equal) observation already owns this field
		}
		out[k] = fieldValue{Value: val, Seq: obs.Seq, At: obs.At}
	}
	return out
}

// rebuildFields replays an entity's provenance into its materialized view. Deterministic
// and order-independent: it sorts by ordinal then folds, so reconstructing from stored
// scenes+actions reproduces the live record byte-for-byte (ADR-0049 D9). This is the
// reconstruction primitive Issue 012 reuses for "what's true now".
func rebuildFields(observations []entityObservation) map[string]fieldValue {
	ordered := make([]entityObservation, len(observations))
	copy(ordered, observations)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	fields := map[string]fieldValue{}
	for _, obs := range ordered {
		fields = mergeObservation(fields, obs)
	}
	return fields
}

// materializedValues projects the field set to a plain value map (drops the ordinal
// bookkeeping) — the view a reader consumes.
func materializedValues(fields map[string]fieldValue) map[string]any {
	out := make(map[string]any, len(fields))
	for k, fv := range fields {
		out[k] = fv.Value
	}
	return out
}

// materializedObservedAt is the entity-level valid-time: the most recent observation
// time across all materialized fields (ADR-0049 §A1.1). This is "when we last CONFIRMED
// this entity's state by looking" — the staleness signal ADR-0051 D3 (the Scout's
// trust-prior-vs-re-observe decision) reads. Distinct from the transaction-ordinal Seq
// (recording order): a re-read refreshes At even when the value is unchanged. Zero time
// for an empty/unknown entity (treated as maximally stale).
func materializedObservedAt(fields map[string]fieldValue) time.Time {
	var latest time.Time
	for _, fv := range fields {
		if fv.At.After(latest) {
			latest = fv.At
		}
	}
	return latest
}

// fieldDelta is one materialized field whose value changed between the stored view and a
// new observation — the unit of a drift signal (ADR-0049 §A1.2).
type fieldDelta struct {
	Field    string
	OldValue any
	NewValue any
}

// detectFieldDrift reports the pre-existing fields whose value an observation CHANGES
// (ADR-0049 §A1.2). Pure. A field absent from cur is first-touch *discovery*, not drift
// (excluded). A non-winning observation (Seq not strictly greater) changes nothing
// (excluded). The result is the raw material for a passive world_delta event — the caller
// decides whether to emit (it only does so for READ engagements: a write's change is
// intentional, a read discovering a difference means the world moved under us). Order is
// deterministic (sorted by field) so replay/tests are stable.
func detectFieldDrift(cur map[string]fieldValue, obs entityObservation) []fieldDelta {
	var deltas []fieldDelta
	for k, newVal := range obs.Fields {
		ex, ok := cur[k]
		if !ok || ex.Seq >= obs.Seq {
			continue // discovery (no prior field) or a losing observation — not drift
		}
		if !valuesEqual(ex.Value, newVal) {
			deltas = append(deltas, fieldDelta{Field: k, OldValue: ex.Value, NewValue: newVal})
		}
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].Field < deltas[j].Field })
	return deltas
}

// valuesEqual compares two materialized field values. Field values are JSONB-round-trip
// scalars (bool for exists, string for content_ref/endpoint), so DeepEqual is exact and
// stable across a store round-trip.
func valuesEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

// --- store round-trip codecs -------------------------------------------------------
// The materialized cache and provenance links ride in the entity doc's metadata as JSON
// strings, so they survive the JSONB round-trip cleanly (a nested map would come back as
// map[string]interface{} and lose the typed shape).

func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeEntityFields reads the materialized field set off a (possibly nil) stored entity.
func decodeEntityFields(doc *domain.Document) map[string]fieldValue {
	fields := map[string]fieldValue{}
	if doc == nil || doc.Metadata == nil {
		return fields
	}
	s, ok := doc.Metadata["fields"].(string)
	if !ok || s == "" {
		return fields
	}
	_ = json.Unmarshal([]byte(s), &fields)
	return fields
}

// decodeProvenance reads the provenance link list off a (possibly nil) stored entity.
func decodeProvenance(doc *domain.Document) []string {
	if doc == nil || doc.Metadata == nil {
		return nil
	}
	s, ok := doc.Metadata["provenance"].(string)
	if !ok || s == "" {
		return nil
	}
	var prov []string
	_ = json.Unmarshal([]byte(s), &prov)
	return prov
}

// appendProvenance adds an action link if non-empty and not already present (idempotent
// under replay, like the field merge).
func appendProvenance(prov []string, actionDocID string) []string {
	if actionDocID == "" {
		return prov
	}
	for _, p := range prov {
		if p == actionDocID {
			return prov
		}
	}
	return append(prov, actionDocID)
}
