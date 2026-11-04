package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// persistChunks stores doc under its resolved identity without tracking the fact lane —
// the shape most ingest tests need (ProcessSync and the batch worker resolve the id
// themselves, ADR-0138 D1.4).
func (im *IngestionManager) persistChunks(ctx context.Context, doc domain.ExternalDocument, chunks []domain.Chunk,
	entityID, scene string, trackKG bool) (persistOutcome, error) {
	return im.persistChunksTracked(ctx, doc, im.resolveDocumentID(ctx, doc), chunks, entityID, scene, trackKG, false)
}

// T-20261103-binary-uploads-with-a-tag-or-session-collapse (RED before ADR-0138): two
// different PDFs uploaded with the same tag, or in the same session, carry their bytes in
// Data with an empty Body, and used to resolve to ONE document id — the second upload
// overwrote the first one's positional chunks.
func TestDocumentID_BinaryUploadsWithOneTagOrSessionDiffer(t *testing.T) {
	t.Parallel()
	pdf := func(data string, tags []string, thread string) domain.ExternalDocument {
		return domain.ExternalDocument{SourceURI: "operator_ingest://x.pdf", SourceType: "pdf",
			Tags: tags, ThreadID: thread, Data: []byte(data), Author: "operator:alice"}
	}
	for _, tc := range []struct {
		name   string
		tags   []string
		thread string
	}{
		{"tag", []string{"finance"}, ""},
		{"session", nil, "sess-1"},
		{"tag+context-note", []string{"finance"}, ""},
		{"no identity", nil, ""},
	} {
		a, b := pdf("%PDF-1.7 first", tc.tags, tc.thread), pdf("%PDF-1.7 second", tc.tags, tc.thread)
		if tc.name == "tag+context-note" {
			a.Body, b.Body = "## Context\n\nQ3", "## Context\n\nQ3"
		}
		if tc.name == "no identity" {
			a.SourceURI, b.SourceURI = "operator_ingest://", "operator_ingest://"
		}
		if legacyExternalDocumentID(a) != legacyExternalDocumentID(b) {
			t.Errorf("%s: precondition: the legacy ids were expected to collide", tc.name)
		}
		if ia, ib := externalDocumentID(a), externalDocumentID(b); ia == ib {
			t.Errorf("%s: two different binaries share id %q", tc.name, ia)
		}
		again := a
		again.Data = []byte("%PDF-1.7 first")
		if externalDocumentID(a) != externalDocumentID(again) {
			t.Errorf("%s: the same binary must resolve to the same id (idempotent re-ingest)", tc.name)
		}
	}
	// The content digest stored on the document differs too (ADR-0138 D1.3).
	if revisionDigest(pdf("a", nil, "")) == revisionDigest(pdf("b", nil, "")) {
		t.Error("revision digests of different binaries are equal")
	}
	// Body and Data are length-delimited: moving bytes between them changes the digest.
	x := domain.ExternalDocument{Body: "ab", Data: []byte("c")}
	y := domain.ExternalDocument{Body: "a", Data: []byte("bc")}
	if revisionDigest(x) == revisionDigest(y) {
		t.Error("Body/Data boundary is not part of the digest")
	}
}

// T-20261103-ingesting-the-same-content-under-a-different (RED before ADR-0138): the same
// text ingested by two principals under one tag, thread or source used to resolve to one
// id, so the second writer's upsert took the first writer's chunks over.
func TestDocumentID_WriterSeparatesEveryRuleButACallerNamedID(t *testing.T) {
	t.Parallel()
	base := func(author string) []domain.ExternalDocument {
		return []domain.ExternalDocument{
			{Body: "same text", Tags: []string{"longmemeval"}, Author: author},
			{Body: "same text", ThreadID: "longmemeval:q1:s1", SourceURI: "longmemeval/q1/s1", Author: author},
			{Body: "same text", SourceURI: "/data/notes.md", Author: author},
			{Body: "same text", Author: author},
		}
	}
	a, b := base("lme-ns1-q1"), base("lme-ns2-q1")
	for i := range a {
		if legacyExternalDocumentID(a[i]) != legacyExternalDocumentID(b[i]) {
			t.Errorf("rule %d: precondition: legacy ids were expected to collide", i)
		}
		if externalDocumentID(a[i]) == externalDocumentID(b[i]) {
			t.Errorf("rule %d: two writers share id %q", i, externalDocumentID(a[i]))
		}
		if externalDocumentID(a[i]) != externalDocumentID(base("lme-ns1-q1")[i]) {
			t.Errorf("rule %d: not deterministic for one writer", i)
		}
	}
	// Readable prefixes survive: the group, thread or source is still the id's head.
	if id := externalDocumentID(a[1]); !strings.HasPrefix(id, "longmemeval:q1:s1:") {
		t.Errorf("thread id lost its prefix: %q", id)
	}
	if id := externalDocumentID(a[2]); !strings.HasPrefix(id, "/data/notes.md#w=") {
		t.Errorf("named-source id: %q", id)
	}
	// Rule 1 stays verbatim (a downstream contract); the lifecycle refuses a second writer.
	named := func(author string) domain.ExternalDocument {
		return domain.ExternalDocument{Body: "x", Tags: []string{"musique", sourceDocumentMarker, "musique:p3"}, Author: author}
	}
	if externalDocumentID(named("a")) != "musique:p3" || externalDocumentID(named("b")) != "musique:p3" {
		t.Error("a caller-named id must stay verbatim for every writer")
	}
}

// A writer-less text ingest (kernel-internal, connectors that stamp no Author) keeps its
// exact legacy id: ADR-0138 changes nothing for it but the bare-scheme fix.
func TestDocumentID_WriterlessTextKeepsTheLegacyID(t *testing.T) {
	t.Parallel()
	for _, d := range []domain.ExternalDocument{
		{Body: "b", Tags: []string{"g"}},
		{Body: "b", ThreadID: "t"},
		{Body: "b", SourceURI: "/x/y.md"},
		{Body: "b"},
		{Body: "b", Tags: []string{sourceDocumentMarker, "named"}},
	} {
		if got, want := externalDocumentID(d), legacyExternalDocumentID(d); got != want {
			t.Errorf("%+v: v2 %q != legacy %q", d, got, want)
		}
	}
}

// The doors synthesise "<scheme>://" + session id; with no session the bare scheme used
// to be the id of every such ingest by every caller (rule 3).
func TestDocumentID_BareSchemeURIIsNotASource(t *testing.T) {
	t.Parallel()
	a := domain.ExternalDocument{Body: "first", SourceURI: "ingest_memory://"}
	b := domain.ExternalDocument{Body: "second", SourceURI: "ingest_memory://"}
	if legacyExternalDocumentID(a) != "ingest_memory://" {
		t.Fatalf("precondition: legacy id %q", legacyExternalDocumentID(a))
	}
	if externalDocumentID(a) == externalDocumentID(b) {
		t.Errorf("two different bodies under a bare scheme share id %q", externalDocumentID(a))
	}
	if !namesSource("operator_ingest://report.pdf") || namesSource("ingest_memory:///") || !namesSource("notes.md") {
		t.Error("namesSource")
	}
}

// fakeLifecycle is an in-memory domain.DocumentLifecycleStore that records the order of
// its calls against the chunk store's saves.
type fakeLifecycle struct {
	mu      sync.Mutex
	records map[string]domain.DocumentRecord
	revised []domain.DocumentRevision
	refuse  bool
	events  *[]string
}

func (f *fakeLifecycle) DocumentRecord(_ context.Context, id string) (domain.DocumentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[id]
	r.ID, r.Exists = id, ok
	return r, nil
}

func (f *fakeLifecycle) ReviseDocument(_ context.Context, rev domain.DocumentRevision) (domain.DocumentRevisionOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revised = append(f.revised, rev)
	if f.events != nil {
		*f.events = append(*f.events, "revise:"+rev.ID)
	}
	if f.refuse {
		return domain.DocumentRevisionOutcome{}, fmt.Errorf("revise: %w", domain.ErrDocumentOwnedByAnotherWriter)
	}
	return domain.DocumentRevisionOutcome{Version: 1}, nil
}

type orderedStore struct {
	captureAllStore
	events *[]string
}

func (o *orderedStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	*o.events = append(*o.events, "save")
	return o.captureAllStore.SaveBatch(ctx, docs)
}

func newLifecycleIngestManager(t *testing.T, store domain.VectorStore, lc domain.DocumentLifecycleStore, cfg IngestionConfig) *IngestionManager {
	t.Helper()
	agent := &Agent{Manager: &MemoryManager{Store: store, Embedder: &mockEmbedder{vec: []float32{0.1, 0.2}}}, pendingCap: 256}
	reg, err := NewRegistry(defaultChunkers(), config.ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	im := NewIngestionManagerWithRegistry(nil, &mockEmbedder{vec: []float32{0.1, 0.2}}, agent, reg, cfg)
	im.SetDocumentLifecycle(lc)
	return im
}

// ADR-0138 D2: a stored legacy id written by THIS writer is reused (no duplicate, no
// re-key); one written by another writer is not (that is the collision being fixed); and
// a binary's content-addressed legacy id is never reused (it is the id that collided).
func TestResolveDocumentID_GrandfathersOnlyTheSameWritersLegacyRow(t *testing.T) {
	t.Parallel()
	doc := domain.ExternalDocument{Body: "same text", ThreadID: "thr", Author: "agent:a"}
	legacy := legacyExternalDocumentID(doc)
	v2 := externalDocumentID(doc)

	mine := &fakeLifecycle{records: map[string]domain.DocumentRecord{legacy: {WrittenBy: "agent:a"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, mine, IngestionConfig{}).resolveDocumentID(context.Background(), doc).ID; got != legacy {
		t.Errorf("same writer: want grandfathered %q, got %q", legacy, got)
	}
	theirs := &fakeLifecycle{records: map[string]domain.DocumentRecord{legacy: {WrittenBy: "agent:b"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, theirs, IngestionConfig{}).resolveDocumentID(context.Background(), doc).ID; got != v2 {
		t.Errorf("other writer: want v2 %q, got %q", v2, got)
	}
	both := &fakeLifecycle{records: map[string]domain.DocumentRecord{legacy: {WrittenBy: "agent:a"}, v2: {WrittenBy: "agent:a"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, both, IngestionConfig{}).resolveDocumentID(context.Background(), doc).ID; got != v2 {
		t.Errorf("v2 row present: want %q, got %q", v2, got)
	}
	bin := domain.ExternalDocument{ThreadID: "thr", Data: []byte("%PDF"), Author: "agent:a"}
	binLC := &fakeLifecycle{records: map[string]domain.DocumentRecord{legacyExternalDocumentID(bin): {WrittenBy: "agent:a"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, binLC, IngestionConfig{}).resolveDocumentID(context.Background(), bin).ID; got != externalDocumentID(bin) {
		t.Errorf("binary: the colliding legacy id must not be reused, got %q", got)
	}
	// A named source (rule 3) is lineage-keyed: its legacy id is grandfathered too.
	file := domain.ExternalDocument{Body: "v1", SourceURI: "/data/notes.md", Author: "agent:a"}
	fileLC := &fakeLifecycle{records: map[string]domain.DocumentRecord{"/data/notes.md": {WrittenBy: "agent:a"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, fileLC, IngestionConfig{}).resolveDocumentID(context.Background(), file).ID; got != "/data/notes.md" {
		t.Errorf("named source: want grandfathered uri, got %q", got)
	}
}

// execution.ingestion.document_identity = legacy is the pre-ADR function, byte for byte.
func TestResolveDocumentID_LegacySchemeIsTheOldFunction(t *testing.T) {
	t.Parallel()
	im := newLifecycleIngestManager(t, &captureAllStore{}, &fakeLifecycle{}, IngestionConfig{LegacyDocumentIdentity: true})
	for _, d := range []domain.ExternalDocument{
		{Body: "b", Tags: []string{"g"}, Author: "w"},
		{ThreadID: "t", Data: []byte("pdf"), Author: "w"},
		{Body: "b", SourceURI: "ingest_memory://", Author: "w"},
	} {
		if got, want := im.resolveDocumentID(context.Background(), d).ID, legacyExternalDocumentID(d); got != want {
			t.Errorf("legacy scheme: %q != %q", got, want)
		}
	}
}

// ADR-0138 D3: the identity claim (and any retirement) runs BEFORE the chunk save, with
// the writer and the revision digest; a refused claim writes nothing and surfaces as
// ErrDocumentOwnedByAnotherWriter.
func TestPersist_RevisesBeforeTheChunkSaveAndRefusalWritesNothing(t *testing.T) {
	t.Parallel()
	var events []string
	lc := &fakeLifecycle{events: &events}
	store := &orderedStore{events: &events}
	im := newLifecycleIngestManager(t, store, lc, IngestionConfig{})
	doc := domain.ExternalDocument{Body: "v2 text", Title: "t", Tags: []string{sourceDocumentMarker, "named-doc"}, Author: "agent:a"}
	chunks := []domain.Chunk{{Body: "v2 text"}}
	if _, err := im.persistChunks(context.Background(), doc, chunks, "e1", "", false); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if len(events) < 2 || events[0] != "revise:named-doc" || events[1] != "save" {
		t.Fatalf("want revise then save, got %v", events)
	}
	if r := lc.revised[0]; r.Writer != "agent:a" || r.ContentDigest != revisionDigest(doc) || r.Text != "v2 text" || r.HasData {
		t.Errorf("revision: %+v", r)
	}

	events = nil
	refusing := &fakeLifecycle{refuse: true, events: &events}
	store2 := &orderedStore{events: &events}
	im2 := newLifecycleIngestManager(t, store2, refusing, IngestionConfig{})
	_, err := im2.persistChunks(context.Background(), doc, chunks, "e1", "", false)
	if !errors.Is(err, domain.ErrDocumentOwnedByAnotherWriter) {
		t.Fatalf("want ErrDocumentOwnedByAnotherWriter, got %v", err)
	}
	if len(store2.snapshot()) != 0 {
		t.Errorf("a refused ingest wrote %d chunk(s)", len(store2.snapshot()))
	}
}

// ProcessSync resolves the id once: the chunks, the entity stub's document_id and the
// revision all name the same id.
func TestProcessSync_OneResolvedIDForEveryStep(t *testing.T) {
	t.Parallel()
	store := &captureAllStore{}
	lc := &fakeLifecycle{}
	im := newLifecycleIngestManager(t, store, lc, IngestionConfig{})
	doc := domain.ExternalDocument{Body: "hello world", ThreadID: "thr", SourceURI: "ingest_memory://thr", Author: "agent:a"}
	if _, err := im.ProcessSync(context.Background(), doc); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	want := externalDocumentID(doc)
	if len(lc.revised) != 1 || lc.revised[0].ID != want {
		t.Fatalf("revised: %+v, want id %q", lc.revised, want)
	}
	for _, d := range store.snapshot() {
		if got, _ := d.Metadata["document_id"].(string); got != want {
			t.Errorf("row %s: document_id %q, want %q", d.ID, got, want)
		}
	}
}
