package kernel

import (
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory"
)

// MemoryBackend is every capability of the raw memory adapter that the memory stack
// composes, as typed fields (T-20261113-replace-optional-interface-probing-in-the,
// ENG-1504). NewMemoryStack used to take one broad domain.VectorStore and discover
// the rest with runtime type assertions — vec.(domain.ExperienceStore),
// vec.(domain.GraphStore), vec.(domain.ChunkTripletsStore), vec.(domain.DocumentStore)
// and, in app/app.go, any(vec).(memory.LexicalSearcher). When the concrete adapter
// stopped matching one of them, a feature (spreading activation, Hebbian
// reinforcement, KG triplets, hybrid retrieval, the ADR-0093 document row) switched
// off with a Warn line or nothing at all.
//
// Production builds it with NewMemoryBackend from the concrete adapter, so a missing
// capability is a COMPILE error there. Every field is the RAW tier-0 adapter
// (AGENTS.md §2.1): NewMemoryStack wraps Vector in the enforcing read/write tiers and
// Documents in the enforcing document store; the other fields are handed only to the
// kernel-internal components that need the concrete adapter (each use is justified
// where NewMemoryStack makes it, AGENTS.md §2.2).
//
// A struct literal with nil fields is how test fakes build a partial backend; the
// absent capabilities are then logged at Error by NewMemoryStack and their features
// stay off — the same behaviour as before, but no longer silent.
type MemoryBackend struct {
	// Vector is the chunk store (tier 0). Required.
	Vector domain.VectorStore
	// Graph backs the ADR-0017 spreading engine and ADR-0049 D10 Hebbian reinforcement.
	Graph domain.GraphStore
	// Experience persists the ADR-0095 episode parent rows (a kernel-owned write).
	Experience domain.ExperienceStore
	// ChunkTriplets backs the ADR-0053 batched (h, r, t) extractor and KG²RAG.
	ChunkTriplets domain.ChunkTripletsStore
	// Lexical is the sparse half of ADR-0054 hybrid retrieval.
	Lexical memory.LexicalSearcher
	// Documents is the ADR-0093 source-document row store (wrapped, never handed over).
	Documents domain.DocumentStore
	// Eraser is the RAW ADR-0137 memory eraser (postgres.PgErasureStore): one transaction
	// that deletes documents, experiences, chunks, substrate items and evidence and leaves
	// a content-free receipt. It is not bound by NewMemoryBackend because it is built over
	// the knowledge store and document lifecycle, not the vector adapter: app.go sets it.
	// Optional (nil = no erasure; not reported by Missing): the maintenance commands and
	// test fakes leave it unset. NewMemoryStack hands it only to the S3 enforcing
	// decorator (authz.EnforcingEraser) and exposes only that (MemoryStack.Eraser).
	Eraser domain.MemoryEraser
}

// NewMemoryBackend binds every capability to the one concrete Postgres adapter. It
// cannot omit a field: if *postgres.PgVectorAdapter stops implementing one of the
// ports, this function stops compiling. A nil adapter is a composition bug and
// panics here, at boot, rather than as a nil dereference on the first query.
func NewMemoryBackend(a *postgres.PgVectorAdapter) MemoryBackend {
	if a == nil {
		panic("kernel.NewMemoryBackend: nil *postgres.PgVectorAdapter")
	}
	return MemoryBackend{
		Vector:        a,
		Graph:         a,
		Experience:    a,
		ChunkTriplets: a,
		Lexical:       a,
		Documents:     a,
	}
}

// Missing names the capabilities this backend lacks (only a hand-built partial
// backend, a test fake, can lack any).
func (b MemoryBackend) Missing() []string {
	var out []string
	if b.Vector == nil {
		out = append(out, "Vector")
	}
	if b.Graph == nil {
		out = append(out, "Graph")
	}
	if b.Experience == nil {
		out = append(out, "Experience")
	}
	if b.ChunkTriplets == nil {
		out = append(out, "ChunkTriplets")
	}
	if b.Lexical == nil {
		out = append(out, "Lexical")
	}
	if b.Documents == nil {
		out = append(out, "Documents")
	}
	return out
}

// reportMissing logs, at Error, every capability the backend lacks and the feature
// that therefore stays off. A production backend (NewMemoryBackend) lacks none.
func (b MemoryBackend) reportMissing() {
	missing := b.Missing()
	if len(missing) == 0 {
		return
	}
	slog.Error("memory backend lacks required capabilities; their features are OFF "+
		"(Graph: spreading/Hebbian/KG triplets; Experience: episode parents; ChunkTriplets: KG triplets/KG²RAG; "+
		"Lexical: hybrid retrieval; Documents: ADR-0093 document rows)",
		"missing", strings.Join(missing, ","))
}
