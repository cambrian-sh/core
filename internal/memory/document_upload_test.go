package memory

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// upload is an operator upload as operatorIngestDoc builds it (ADR-0138 B1).
func upload(name, body, writer, session string, tags ...string) domain.ExternalDocument {
	return domain.ExternalDocument{
		SourceURI: "operator_ingest://" + name, SourceType: "operator_ingest", Title: name,
		Body: body, Author: writer, ThreadID: session, Tags: tags, UploadName: name,
	}
}

// B1.2: an upload is keyed by (writer, classification scope, normalised file name) —
// not by its session, not by its content, and never shared across writers.
func TestUploadID_IsKeyedByWriterScopeAndNormalisedName(t *testing.T) {
	t.Parallel()
	base := documentIdentityOf(upload("report.pdf", "v1", "op:a", "s1", "finance"))
	if !base.Upload || base.ContentAddressed {
		t.Fatalf("an upload is lineage-keyed: %+v", base)
	}
	same := []domain.ExternalDocument{
		upload("report.pdf", "v2 changed", "op:a", "s1", "finance"),    // content is not the key
		upload("report.pdf", "v1", "op:a", "s2", "finance"),            // the session is not the scope
		upload(" Report.PDF ", "v1", "op:a", "", "finance"),            // case, surrounding space
		upload(`C:\docs\report.pdf`, "v1", "op:a", "", "finance"),      // path stripped
		upload("report.pdf", "v1", "op:a", "", "finance", "finance"),   // duplicate tags
		upload("report.pdf", "v1", "op:a", "", "finance", "chunker:x"), // marker tags carry no scope
	}
	for i, d := range same {
		if got := documentIdentityOf(d).ID; got != base.ID {
			t.Errorf("case %d: %q, want %q", i, got, base.ID)
		}
	}
	if a, b := documentIdentityOf(upload("r.pdf", "x", "op:a", "", "hr", "finance")).ID,
		documentIdentityOf(upload("r.pdf", "x", "op:a", "", "finance", "hr")).ID; a != b {
		t.Errorf("scope order matters: %q vs %q", a, b)
	}
	differ := map[string]domain.ExternalDocument{
		"another scope":   upload("report.pdf", "v1", "op:a", "s1", "hr"),
		"no scope":        upload("report.pdf", "v1", "op:a", "s1"),
		"another writer":  upload("report.pdf", "v1", "op:b", "s1", "finance"),
		"keep-both name":  upload("report (2).pdf", "v1", "op:a", "s1", "finance"),
		"tag case counts": upload("report.pdf", "v1", "op:a", "s1", "Finance"),
	}
	for why, d := range differ {
		if got := documentIdentityOf(d).ID; got == base.ID {
			t.Errorf("%s: shares id %q", why, got)
		}
	}
	if got := normaliseUploadName("  My\tQ3   Report.PDF"); got != "my q3 report.pdf" {
		t.Errorf("normaliseUploadName = %q", got)
	}
	// NFD (macOS) and NFC spell one name.
	if normaliseUploadName("cafe\u0301.pdf") != normaliseUploadName("caf\u00e9.pdf") {
		t.Error("NFC normalisation")
	}
}

// The rule-U id of an untagged upload with an already-normal name IS the ADR-0138
// rule-3 id it had before B1, so those documents keep their lineage without a lookup.
// A caller-named source_document still wins, and a writer-less ingest is not an upload.
func TestUploadID_KeepsTheRule3IDAndYieldsToACallerNamedID(t *testing.T) {
	t.Parallel()
	d := upload("report.pdf", "v1", "op:a", "")
	before := d
	before.UploadName = ""
	if got, want := documentIdentityOf(d).ID, documentIdentityOf(before).ID; got != want {
		t.Errorf("untagged upload id %q, rule-3 id %q", got, want)
	}
	named := upload("report.pdf", "v1", "op:a", "s1", sourceDocumentMarker, "my-key")
	if id := documentIdentityOf(named); id.ID != "my-key" || id.Upload {
		t.Errorf("source_document must win: %+v", id)
	}
	if id := documentIdentityOf(upload("report.pdf", "v1", "", "s1", "finance")); id.Upload {
		t.Errorf("a writer-less ingest is not an upload: %+v", id)
	}
}

// B1.2 through the real ingest: a changed re-upload in ANOTHER session resolves to the
// same id with a new digest, so the existing ADR-0138 D3 ReviseDocument (the one
// retirement path) is what replaces the old version.
func TestProcessSync_ChangedUploadIsANewVersionOfTheSameDocument(t *testing.T) {
	t.Parallel()
	lc := &fakeLifecycle{}
	im := newLifecycleIngestManager(t, &captureAllStore{}, lc, IngestionConfig{})
	v1 := upload("report.md", "first version text", "op:a", "s1", "finance")
	v2 := upload("Report.md", "second version text", "op:a", "s2", "finance")
	for _, d := range []domain.ExternalDocument{v1, v2} {
		if _, err := im.ProcessSync(context.Background(), d); err != nil {
			t.Fatalf("ProcessSync: %v", err)
		}
	}
	if len(lc.revised) != 2 || lc.revised[0].ID != lc.revised[1].ID {
		t.Fatalf("both uploads must revise one id: %+v", lc.revised)
	}
	if lc.revised[0].ContentDigest == lc.revised[1].ContentDigest {
		t.Error("the versions must carry different digests")
	}
	if len(lc.assocs) != 2 || lc.assocs[1].SessionID != "s2" || !reflect.DeepEqual(lc.assocs[1].Scope, []string{"finance"}) {
		t.Errorf("associations: %+v", lc.assocs)
	}
}

// B1.1: an exact duplicate of a stored document of the same writer — in another
// session, under another tag, under another name — writes nothing (no revision, no
// chunk), records the association, and reports "duplicate of <id>".
func TestProcessSync_ExactDuplicateUploadWritesNothing(t *testing.T) {
	t.Parallel()
	d := upload("copy of notes.md", "the same bytes", "op:a", "s9", "hr")
	lc := &fakeLifecycle{byContent: map[string]domain.DocumentRecord{
		"op:a|" + revisionDigest(d): {ID: "operator_ingest://notes.md#w=x", Version: 2},
	}}
	store := &captureAllStore{}
	im := newLifecycleIngestManager(t, store, lc, IngestionConfig{})
	out := &domain.IngestOutcome{}
	got, err := im.ProcessSync(domain.WithIngestOutcome(context.Background(), out), d)
	if err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if got != "operator_ingest://notes.md#w=x" || out.DuplicateOf != got || out.Version != 2 {
		t.Errorf("returned %q, outcome %+v", got, out)
	}
	if len(store.snapshot()) != 0 || len(lc.revised) != 0 {
		t.Errorf("a duplicate wrote %d chunk(s), %d revision(s)", len(store.snapshot()), len(lc.revised))
	}
	if len(lc.assocs) != 1 || lc.assocs[0].DocumentID != got || lc.assocs[0].SessionID != "s9" ||
		lc.assocs[0].UploadName != "copy of notes.md" || !reflect.DeepEqual(lc.assocs[0].Scope, []string{"hr"}) {
		t.Errorf("association: %+v", lc.assocs)
	}

	// Another writer's identical bytes are not a duplicate of op:a's document.
	other := d
	other.Author = "op:b"
	out2 := &domain.IngestOutcome{}
	if _, err := im.ProcessSync(domain.WithIngestOutcome(context.Background(), out2), other); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if out2.DuplicateOf != "" || len(lc.revised) != 1 || out2.Version != 1 {
		t.Errorf("another writer: outcome %+v, revisions %d", out2, len(lc.revised))
	}
}

// Only operator uploads are deduplicated: a repeated thread turn or an agent-plane
// ingest never asks, so a "thanks" in two threads stays two turns.
func TestProcessSync_NonUploadsAreNeverDeduplicated(t *testing.T) {
	t.Parallel()
	turn := domain.ExternalDocument{Body: "thanks", ThreadID: "t2", SourceURI: "ingest_memory://t2", Author: "agent:a"}
	lc := &fakeLifecycle{byContent: map[string]domain.DocumentRecord{"agent:a|" + revisionDigest(turn): {ID: "t1:x"}}}
	im := newLifecycleIngestManager(t, &captureAllStore{}, lc, IngestionConfig{})
	if _, err := im.ProcessSync(context.Background(), turn); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if lc.contentHits != 0 || len(lc.revised) != 1 || len(lc.assocs) != 0 {
		t.Errorf("lookups %d, revisions %d, associations %d", lc.contentHits, len(lc.revised), len(lc.assocs))
	}
}

// An earlier LINEAGE id of the same file (rule 3: no tag, no session) is grandfathered
// for this writer only; a content-addressed earlier id (tag or session) never is.
func TestResolveUploadID_GrandfathersOnlyALineageKeyedPriorID(t *testing.T) {
	t.Parallel()
	d := upload("Report.PDF", "v2", "op:a", "")
	before := d
	before.UploadName = ""
	rule3 := documentIdentityOf(before).ID
	if rule3 == documentIdentityOf(d).ID {
		t.Fatal("precondition: a non-normal name changes the id")
	}
	mine := &fakeLifecycle{records: map[string]domain.DocumentRecord{rule3: {WrittenBy: "op:a"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, mine, IngestionConfig{}).resolveDocumentID(context.Background(), d).ID; got != rule3 {
		t.Errorf("same writer: want %q, got %q", rule3, got)
	}
	theirs := &fakeLifecycle{records: map[string]domain.DocumentRecord{rule3: {WrittenBy: "op:b"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, theirs, IngestionConfig{}).resolveDocumentID(context.Background(), d).ID; got != documentIdentityOf(d).ID {
		t.Errorf("another writer's row must not be reused, got %q", got)
	}
	tagged := upload("report.pdf", "v2", "op:a", "s1", "finance")
	tb := tagged
	tb.UploadName = ""
	content := documentIdentityOf(tb).ID
	lc := &fakeLifecycle{records: map[string]domain.DocumentRecord{content: {WrittenBy: "op:a"}}}
	if got := newLifecycleIngestManager(t, &captureAllStore{}, lc, IngestionConfig{}).resolveDocumentID(context.Background(), tagged).ID; got == content {
		t.Errorf("a content-addressed prior id was reused: %q", got)
	}
}

// B1.3: the console's "Replace?" lookup resolves through the ingest's own code, finds
// only the caller's document, and finds nothing under the legacy scheme.
func TestMatchUpload_FindsOnlyTheCallersStoredDocument(t *testing.T) {
	t.Parallel()
	d := upload("report.pdf", "", "op:a", "", "finance")
	id := documentIdentityOf(d).ID
	lc := &fakeLifecycle{records: map[string]domain.DocumentRecord{id: {WrittenBy: "op:a", Title: "report.pdf", Version: 3}}}
	im := newLifecycleIngestManager(t, &captureAllStore{}, lc, IngestionConfig{})
	rec, err := im.MatchUpload(context.Background(), d)
	if err != nil || !rec.Exists || rec.ID != id || rec.Version != 3 {
		t.Fatalf("match: %+v, %v", rec, err)
	}
	if rec, _ := im.MatchUpload(context.Background(), upload("report.pdf", "", "op:a", "", "hr")); rec.Exists {
		t.Error("another scope matched")
	}
	if rec, _ := im.MatchUpload(context.Background(), upload("report.pdf", "", "op:b", "", "finance")); rec.Exists {
		t.Error("another writer matched")
	}
	legacy := newLifecycleIngestManager(t, &captureAllStore{}, lc, IngestionConfig{LegacyDocumentIdentity: true})
	if rec, _ := legacy.MatchUpload(context.Background(), d); rec.Exists {
		t.Error("legacy scheme matched")
	}
}

// document_identity = legacy reproduces the old behaviour exactly: the old id, no
// duplicate lookup, no association.
func TestLegacyScheme_UploadsKeepTheOldBehaviour(t *testing.T) {
	t.Parallel()
	d := upload("report.md", "body", "op:a", "s1", "finance")
	lc := &fakeLifecycle{byContent: map[string]domain.DocumentRecord{"op:a|" + revisionDigest(d): {ID: "x"}}}
	im := newLifecycleIngestManager(t, &captureAllStore{}, lc, IngestionConfig{LegacyDocumentIdentity: true})
	if _, err := im.ProcessSync(context.Background(), d); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if lc.contentHits != 0 || len(lc.assocs) != 0 {
		t.Errorf("legacy looked up content %d time(s), recorded %d association(s)", lc.contentHits, len(lc.assocs))
	}
	if len(lc.revised) != 1 || lc.revised[0].ID != legacyExternalDocumentID(d) || strings.HasPrefix(lc.revised[0].ID, uploadIDScheme) {
		t.Errorf("legacy id: %+v", lc.revised)
	}
}
