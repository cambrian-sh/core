package memory

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// memSubstrate is an in-memory knowledge substrate for the fact layer's unit tests: it
// implements the raw domain.FactSubstrateStore and domain.FactItemReader and derives every
// chain with the REAL authority (domain.ValidTimeSupersessionAuthority) from items +
// stored assessments + profiles — the same inputs the postgres store gathers — and
// applies the same tag predicate on reads (TagPredicate.Allows on the item
// classification). The tests wrap it in the real authz decorators.
type memSubstrate struct {
	mu       sync.Mutex
	items    map[domain.KnowledgeItemID]domain.KnowledgeItem
	srcRef   map[string]domain.KnowledgeItemID
	asms     map[string]domain.Assessment
	profiles map[string]domain.PredicateProfile
	markers  map[string]domain.ExtractionMarker
	journal  map[string]domain.ExtractionWindow
	embs     map[domain.KnowledgeItemID][]float32
	puts     atomic.Int32 // inserted items
	asmPuts  atomic.Int32 // inserted assessments
	rederive atomic.Int32 // RederivePredicate calls
}

var (
	_ domain.FactSubstrateStore = (*memSubstrate)(nil)
	_ domain.FactItemReader     = (*memSubstrate)(nil)
	_ domain.FactStagedReader   = (*memSubstrate)(nil)
)

func newMemSubstrate() *memSubstrate {
	return &memSubstrate{
		items: map[domain.KnowledgeItemID]domain.KnowledgeItem{}, srcRef: map[string]domain.KnowledgeItemID{},
		asms: map[string]domain.Assessment{}, profiles: map[string]domain.PredicateProfile{},
		markers: map[string]domain.ExtractionMarker{}, embs: map[domain.KnowledgeItemID][]float32{},
		journal: map[string]domain.ExtractionWindow{},
	}
}

var vtsAuthority = domain.ValidTimeSupersessionAuthority{}

func (m *memSubstrate) PutItem(_ context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := it.Kind + "|" + it.EntityID + "|" + it.SourceRef
	if id, ok := m.srcRef[key]; ok && it.SourceRef != "" {
		return id, false, nil
	}
	m.items[it.ID] = it
	m.srcRef[key] = it.ID
	m.puts.Add(1)
	return it.ID, true, nil
}

func (m *memSubstrate) GetItem(_ context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	return &it, nil
}

func (m *memSubstrate) CurrentResolutions(context.Context, string, string, string) ([]domain.Resolution, error) {
	return nil, nil
}

func (m *memSubstrate) EraseItems(context.Context, string, string, domain.KnowledgeErasure) (int, error) {
	return 0, nil
}

func (m *memSubstrate) GetAssessment(_ context.Context, id string) (*domain.Assessment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.asms[id]
	if !ok {
		return nil, nil
	}
	return &a, nil
}

func (m *memSubstrate) PutAssessment(_ context.Context, a domain.Assessment) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.asms[a.ID]; ok {
		return false, nil
	}
	m.asms[a.ID] = a
	m.asmPuts.Add(1)
	return true, nil
}

func (m *memSubstrate) PredicateProfile(_ context.Context, _, kind, pred string) (domain.PredicateProfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.profiles[pred]; ok {
		return p, nil
	}
	return domain.PredicateProfile{Kind: kind, Predicate: pred, Cardinality: domain.CardinalityUnknown, Source: domain.ProfileSourceLearned}, nil
}

func (m *memSubstrate) VoteProfile(ctx context.Context, ns, kind, pred string, v domain.ProfileVote, minVotes int) (bool, error) {
	cur, _ := m.PredicateProfile(ctx, ns, kind, pred)
	next, changed := domain.ApplyProfileVote(cur, v, minVotes)
	if !changed {
		return false, nil
	}
	m.mu.Lock()
	m.profiles[pred] = next
	m.mu.Unlock()
	return next.Cardinality != cur.Cardinality, nil
}

func (m *memSubstrate) SeedDeclaredProfiles(ctx context.Context, ns, kind string, declared map[string]string, _ int, setBy string) ([]string, error) {
	var changed []string
	for p, c := range declared {
		cur, _ := m.PredicateProfile(ctx, ns, kind, p)
		if cur.Source == domain.ProfileSourceDeclared && cur.Cardinality == c {
			continue
		}
		next := cur
		next.Source, next.Cardinality, next.SetBy = domain.ProfileSourceDeclared, c, setBy
		m.mu.Lock()
		m.profiles[p] = next
		m.mu.Unlock()
		if cur.Cardinality != c {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// RederivePredicate is a no-op count: the fake derives on every read.
func (m *memSubstrate) RederivePredicate(context.Context, string, string, string, int) (int, error) {
	m.rederive.Add(1)
	return 0, nil
}

func (m *memSubstrate) PutItemEmbeddings(_ context.Context, embs []domain.ItemEmbedding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range embs {
		m.embs[e.ItemID] = e.Vector
	}
	return nil
}

func (m *memSubstrate) ExtractionMarker(_ context.Context, ns, ex, doc, hash string) (*domain.ExtractionMarker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mk, ok := m.markers[ns+"|"+ex+"|"+doc+"|"+hash]
	if !ok {
		return nil, nil
	}
	return &mk, nil
}

// MarkExtracted upserts, as the store does.
func (m *memSubstrate) MarkExtracted(_ context.Context, mk domain.ExtractionMarker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markers[mk.NamespaceID+"|"+mk.Extractor+"|"+mk.DocumentID+"|"+mk.ContentHash] = mk
	return nil
}

// marker returns the (single) marker of document doc, for assertions.
func (m *memSubstrate) marker(doc string) (domain.ExtractionMarker, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mk := range m.markers {
		if mk.DocumentID == doc {
			return mk, true
		}
	}
	return domain.ExtractionMarker{}, false
}

func (m *memSubstrate) clearMarkers() {
	m.mu.Lock()
	m.markers = map[string]domain.ExtractionMarker{}
	m.mu.Unlock()
}

func visibleTo(it domain.KnowledgeItem, scope *domain.TagPredicate) bool {
	return scope == nil || scope.Bypass || scope.Allows(it.Classification)
}

// derive is the fake's deriveKeys for one key, under scope.
func (m *memSubstrate) derive(entity, actor string, scope *domain.TagPredicate) []domain.Fact {
	return m.deriveHiding(entity, actor, scope, nil)
}

// deriveHiding is derive with the hidden items absent (SimilarFactsHiding).
func (m *memSubstrate) deriveHiding(entity, actor string, scope *domain.TagPredicate, hidden map[domain.KnowledgeItemID]bool) []domain.Fact {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []domain.KnowledgeItem
	ids := map[domain.KnowledgeItemID]bool{}
	for _, it := range m.items {
		if it.EntityID == entity && it.AssertedBy == actor && visibleTo(it, scope) && !hidden[it.ID] {
			items = append(items, it)
			ids[it.ID] = true
		}
	}
	if len(items) == 0 {
		return nil
	}
	in := domain.ResolutionInputs{Cardinality: domain.CardinalityUnknown}
	if p, ok := m.profiles[vtsAuthority.ProfilePredicate(items)]; ok {
		in.Cardinality = p.Cardinality
	}
	rel := map[domain.KnowledgeItemID]bool{}
	asmIDs := make([]string, 0, len(m.asms))
	for id := range m.asms {
		asmIDs = append(asmIDs, id)
	}
	sort.Strings(asmIDs)
	for _, aid := range asmIDs {
		a := m.asms[aid]
		cites := false
		for _, id := range a.InputItemIDs {
			cites = cites || ids[id]
		}
		if !cites {
			continue
		}
		in.Assessments = append(in.Assessments, a)
		for _, v := range a.Verdicts {
			for _, side := range []domain.KnowledgeItemID{v.A, v.B} {
				if r, ok := m.items[side]; ok && !ids[side] && !rel[side] && visibleTo(r, scope) && !hidden[side] {
					rel[side] = true
					in.Related = append(in.Related, r)
				}
			}
		}
	}
	out := vtsAuthority.ResolveWith(items, in)
	byID := map[domain.KnowledgeItemID]domain.KnowledgeItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	var facts []domain.Fact
	for _, iv := range out.Chain {
		it := byID[iv.ItemID]
		if domain.IsFactErasedSentinel(it) {
			continue
		}
		f := domain.FactFromItem(it).ApplyInterval(iv)
		f.Cardinality = domain.FactCardinality(in.Cardinality)
		facts = append(facts, f)
	}
	return facts
}

func (m *memSubstrate) viewOf(id domain.KnowledgeItemID, scope *domain.TagPredicate) (domain.Fact, bool) {
	m.mu.Lock()
	it, ok := m.items[id]
	m.mu.Unlock()
	if !ok {
		return domain.Fact{}, false
	}
	for _, f := range m.derive(it.EntityID, it.AssertedBy, scope) {
		if f.ID == string(id) {
			return f, true
		}
	}
	return domain.Fact{}, false
}

func (m *memSubstrate) SearchFacts(_ context.Context, _ []float32, opts domain.FactSearchOptions) ([]domain.FactHit, error) {
	m.mu.Lock()
	var ids []domain.KnowledgeItemID
	for id, it := range m.items {
		if visibleTo(it, opts.Scope) && !domain.IsFactErasedSentinel(it) {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	inDocs := map[string]bool{} // ADR-0136 A4: SourceDocumentIDs restricts, like the SQL
	for _, d := range opts.SourceDocumentIDs {
		inDocs[d] = true
	}
	var out []domain.FactHit
	for _, id := range ids {
		f, ok := m.viewOf(id, opts.Scope)
		if !ok {
			continue
		}
		if len(inDocs) > 0 && !inDocs[f.SourceDocumentID] {
			continue
		}
		h := domain.FactHit{Fact: f, Score: 0.9}
		if f.SupersededBy != "" {
			if s, ok := m.viewOf(domain.KnowledgeItemID(f.SupersededBy), opts.Scope); ok {
				h.SupersededByValue = s.Value
			}
			h.SupersededAt = f.ValidTo
		}
		out = append(out, h)
		if opts.TopK > 0 && len(out) >= opts.TopK {
			break
		}
	}
	return out, nil
}

func (m *memSubstrate) FactChain(_ context.Context, f domain.Fact, scope *domain.TagPredicate) ([]domain.Fact, error) {
	return m.derive(f.EntityID, f.Actor, scope), nil
}

func (m *memSubstrate) SimilarFacts(ctx context.Context, f domain.Fact, vec []float32, k int, scope *domain.TagPredicate) ([]domain.Fact, error) {
	return m.SimilarFactsHiding(ctx, f, vec, k, scope, nil)
}

// SimilarFactsHiding mirrors the postgres read: rank (here by id), LIMIT k, then the
// derived views, dropping an item whose derived interval is empty — all with the hidden
// items absent.
func (m *memSubstrate) SimilarFactsHiding(_ context.Context, f domain.Fact, _ []float32, k int, scope *domain.TagPredicate, hidden []domain.KnowledgeItemID) ([]domain.Fact, error) {
	hide := map[domain.KnowledgeItemID]bool{}
	for _, id := range hidden {
		hide[id] = true
	}
	m.mu.Lock()
	var ids []domain.KnowledgeItemID
	for id, it := range m.items {
		v := domain.FactFromItem(it)
		if it.EntityID != f.EntityID && v.ScopeKey == f.ScopeKey && v.SubjectKey == f.SubjectKey &&
			!it.Negation && visibleTo(it, scope) && !domain.IsFactErasedSentinel(it) && !hide[id] {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > k {
		ids = ids[:k]
	}
	var out []domain.Fact
	for _, id := range ids {
		m.mu.Lock()
		it := m.items[id]
		m.mu.Unlock()
		for _, v := range m.deriveHiding(it.EntityID, it.AssertedBy, scope, hide) {
			if v.ID == string(id) && !v.ValidEmpty {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

// FactsByID implements domain.FactStagedReader: items as stored, with their embedding.
func (m *memSubstrate) FactsByID(_ context.Context, ids []domain.KnowledgeItemID, scope *domain.TagPredicate) ([]domain.Fact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Fact
	for _, id := range ids {
		it, ok := m.items[id]
		if !ok || !visibleTo(it, scope) {
			continue
		}
		f := domain.FactFromItem(it)
		f.Embedding = m.embs[id]
		out = append(out, f)
	}
	return out, nil
}

func journalKey(ns, ex, doc, hash string) string { return ns + "|" + ex + "|" + doc + "|" + hash }

func (m *memSubstrate) PutExtractionWindow(_ context.Context, w domain.ExtractionWindow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal[journalKey(w.NamespaceID, w.Extractor, w.DocumentID, w.ContentHash)+"|"+w.ExtractorVersion+"|"+strconv.Itoa(w.PassageStart)] = w
	return nil
}

func (m *memSubstrate) ExtractionWindows(_ context.Context, ns, ex, doc, hash, version string) ([]domain.ExtractionWindow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ExtractionWindow
	for _, w := range m.journal {
		if journalKey(w.NamespaceID, w.Extractor, w.DocumentID, w.ContentHash) == journalKey(ns, ex, doc, hash) && w.ExtractorVersion == version {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PassageStart < out[j].PassageStart })
	return out, nil
}

func (m *memSubstrate) DeleteExtractionWindows(_ context.Context, ns, ex, doc, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, w := range m.journal {
		if journalKey(w.NamespaceID, w.Extractor, w.DocumentID, w.ContentHash) == journalKey(ns, ex, doc, hash) {
			delete(m.journal, k)
		}
	}
	return nil
}

// byValue returns the derived view of the item with this value (any key).
func (m *memSubstrate) byValue(value string) (domain.Fact, bool) {
	m.mu.Lock()
	var id domain.KnowledgeItemID
	for iid, it := range m.items {
		if domain.FactFromItem(it).Value == value {
			id = iid
		}
	}
	m.mu.Unlock()
	if id == "" {
		return domain.Fact{}, false
	}
	return m.viewOf(id, nil)
}

// seed writes facts as items directly (the lane tests' fixture), with an embedding.
func (m *memSubstrate) seed(facts ...domain.Fact) {
	for _, f := range facts {
		it := f.ToItem("default")
		_, _, _ = m.PutItem(context.Background(), it)
	}
}
