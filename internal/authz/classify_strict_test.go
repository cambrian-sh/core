package authz_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ceilingAuthorizer stands in for a premium decision point with the ADR-0035 D8′-2
// write rule: the classification is the writer's ceiling, narrowed by the hint
// (ceiling ∩ hint; the whole ceiling for an empty hint), a hint tag outside the
// vocabulary is refused as coinage, provenance and owner tags are set aside, and
// the writer's provenance is stamped. It is the shape that silently dropped a
// requested tag before ENG-2115.
type ceilingAuthorizer struct {
	domain.AllowAllAuthorizer
	ceilings map[string][]string // writer id → write ceiling
	vocab    []string            // empty ⇒ no vocabulary check
	closed   []string            // closed tags (ADR-0091); an untagged write under a ceiling holding one is refused (ENG-2161)
}

func (c ceilingAuthorizer) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	if p.IsZero() {
		return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonNoPrincipal}
	}
	ceiling := c.ceilings[p.WriterID()]
	var req []string
	for _, t := range hint {
		if strings.HasPrefix(t, "provenance:") || domain.IsOwnerTag(t) {
			continue
		}
		if len(c.vocab) > 0 && !slices.Contains(c.vocab, t) {
			return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonForbiddenTag, Detail: "coinage; vocabulary is " + strings.Join(c.vocab, ",")}
		}
		req = append(req, t)
	}
	if len(req) == 0 {
		var closed []string
		for _, t := range ceiling {
			if slices.Contains(c.closed, t) {
				closed = append(closed, t)
			}
		}
		if len(closed) > 0 {
			return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonClassificationRequired,
				ClassificationChoice: &domain.WriteClassificationChoice{Ceiling: ceiling, Closed: closed}}
		}
	}
	out := []string{}
	for _, t := range ceiling {
		if len(req) == 0 || slices.Contains(req, t) {
			out = append(out, t)
		}
	}
	return domain.StampProvenanceSource(out, p.WriterID()), domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed}
}

func strictAuthorizer() ceilingAuthorizer {
	return ceilingAuthorizer{
		ceilings: map[string][]string{
			"finance_agent": {"confidential", "finance"},
		},
		vocab: []string{"confidential", "finance", "people", "internal_only"},
	}
}

func writerCtx(id string) context.Context {
	return domain.WithPrincipal(context.Background(), domain.AgentPrincipal(id))
}

func requireUnhonoured(t *testing.T, err error, wantTags ...string) *authz.UnhonouredClassificationError {
	t.Helper()
	if !errors.Is(err, authz.ErrWriteDenied) {
		t.Fatalf("want a refusal wrapping ErrWriteDenied, got %v", err)
	}
	var ue *authz.UnhonouredClassificationError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UnhonouredClassificationError, got %T: %v", err, err)
	}
	for _, w := range wantTags {
		if !slices.Contains(ue.Tags, w) || !strings.Contains(err.Error(), w) {
			t.Errorf("refusal must name %q, got tags %v / %q", w, ue.Tags, err.Error())
		}
	}
	return ue
}

// The readiness repro (ENG-2115): a writer with NO write ceiling asks for
// "confidential, finance". Before: stored with only its provenance stamp. Now: refused,
// naming both tags, and nothing reaches the store.
func TestClassifyStrict_NoCeilingWithRequestedTagsIsRefused(t *testing.T) {
	t.Parallel()
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, strictAuthorizer(), quietLogger())
	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"confidential", "finance"}}}
	err := w.Save(writerCtx("bench_harness"), doc)
	ue := requireUnhonoured(t, err, "confidential", "finance")
	if !strings.Contains(err.Error(), "bench_harness") {
		t.Errorf("refusal must name the writer, got %q", err.Error())
	}
	if ue.Reason == "" {
		t.Error("refusal must carry a reason")
	}
	if len(store.saved) != 0 {
		t.Fatalf("a refused write must not reach the store, got %d", len(store.saved))
	}
}

// A tag outside a writer's ceiling refuses the write — even when the other requested
// tags are inside it (no partial classification).
func TestClassifyStrict_TagOutsideCeilingIsRefused(t *testing.T) {
	t.Parallel()
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, strictAuthorizer(), quietLogger())
	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"finance", "people"}}}
	err := w.Save(writerCtx("finance_agent"), doc)
	ue := requireUnhonoured(t, err, "people")
	if slices.Contains(ue.Tags, "finance") {
		t.Errorf("only the unhonoured tags are named, got %v", ue.Tags)
	}
	if len(store.saved) != 0 {
		t.Fatal("a refused write must not reach the store")
	}
}

// An unknown (out-of-vocabulary) tag is refused, and the error names the writer's
// tags without leaking the vocabulary the decision point put in its detail.
func TestClassifyStrict_UnknownTagIsRefusedWithoutLeakingTheVocabulary(t *testing.T) {
	t.Parallel()
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, strictAuthorizer(), quietLogger())
	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"finance", "top_secret_coined"}}}
	err := w.Save(writerCtx("finance_agent"), doc)
	ue := requireUnhonoured(t, err, "top_secret_coined")
	if ue.Reason != string(domain.ReasonForbiddenTag) {
		t.Errorf("reason = %q, want the decision point's %q", ue.Reason, domain.ReasonForbiddenTag)
	}
	if strings.Contains(err.Error(), "internal_only") {
		t.Errorf("the refusal must not list the vocabulary: %q", err.Error())
	}
	if len(store.saved) != 0 {
		t.Fatal("a refused write must not reach the store")
	}
}

// A hint that names a subset of the ceiling is honoured in full (narrowing still
// works): the stored classification carries exactly the requested tags.
func TestClassifyStrict_NarrowingWithinTheCeilingIsHonoured(t *testing.T) {
	t.Parallel()
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, strictAuthorizer(), quietLogger())
	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"finance"}}}
	if err := w.Save(writerCtx("finance_agent"), doc); err != nil {
		t.Fatal(err)
	}
	got := tagsOf(store.saved[0])
	if !hasTag(got, "finance") || hasTag(got, "confidential") || !hasTag(got, "provenance:source=finance_agent") {
		t.Fatalf("stored %v, want finance + provenance only", got)
	}
}

// A writer that requests NO tags keeps today's documented default: the decision
// point's classification (the writer's ceiling; unclassified without one). The
// strict rule changes nothing for it (ENG-2107 owns the untagged-upload question).
func TestClassifyStrict_NoRequestedTagsKeepsTheDefault(t *testing.T) {
	t.Parallel()
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, strictAuthorizer(), quietLogger())
	if err := w.Save(writerCtx("finance_agent"), &domain.Document{}); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(store.saved[0]); !hasTag(got, "confidential") || !hasTag(got, "finance") {
		t.Fatalf("untagged write by a ceilinged writer gets the ceiling, got %v", got)
	}
	if err := w.Save(writerCtx("bench_harness"), &domain.Document{}); err != nil {
		t.Fatalf("untagged write by a writer with no ceiling stays allowed (unclassified), got %v", err)
	}
	if got := tagsOf(store.saved[1]); len(got) != 1 || got[0] != "provenance:source=bench_harness" {
		t.Fatalf("untagged write with no ceiling is unclassified (provenance only), got %v", got)
	}
}

// Provenance stamps and owner tags are kernel-stamped identity, not requests: a
// forged one in the hint is replaced/dropped by the decision point and does not
// refuse the write.
func TestClassifyStrict_KernelStampedTagsAreNotRequests(t *testing.T) {
	t.Parallel()
	got := authz.RequestedClassification([]string{"provenance:source=forged", "owner:alice", "finance", ""})
	if len(got) != 1 || got[0] != "finance" {
		t.Fatalf("RequestedClassification = %v, want [finance]", got)
	}
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, strictAuthorizer(), quietLogger())
	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"provenance:source=forged", "finance"}}}
	if err := w.Save(writerCtx("finance_agent"), doc); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(store.saved[0]); hasTag(got, "provenance:source=forged") {
		t.Fatalf("forged provenance survived: %v", got)
	}
}

// OSS (allow-all) returns the hint, so every requested tag is honoured: the strict
// rule never refuses an unscoped deployment's write.
func TestClassifyStrict_OSSIsUnaffected(t *testing.T) {
	t.Parallel()
	final, _, err := authz.ClassifyWriteStrict(writerCtx("anyone"), nil, domain.AgentPrincipal("anyone"), []string{"confidential"})
	if err != nil || !slices.Contains(final, "confidential") {
		t.Fatalf("OSS: final %v err %v", final, err)
	}
}

// An untagged write the decision point denies (no principal) stays the plain
// ErrWriteDenied — there are no tags to name.
func TestClassifyStrict_UntaggedDenialIsPlainWriteDenied(t *testing.T) {
	t.Parallel()
	_, _, err := authz.ClassifyWriteStrict(context.Background(), strictAuthorizer(), domain.PrincipalRef{}, nil)
	if !errors.Is(err, authz.ErrWriteDenied) {
		t.Fatalf("got %v", err)
	}
	var ue *authz.UnhonouredClassificationError
	if errors.As(err, &ue) {
		t.Fatal("an untagged denial names no tags")
	}
}

// Every chokepoint that classifies on a writer's behalf applies the same rule: the
// artifact path (UploadArtifact, ledger) and the knowledge-item path (kernel facts,
// the plugin KnowledgeStore seam — ingress evidence → knowledge).
func TestClassifyStrict_ArtifactAndKnowledgeChokepoints(t *testing.T) {
	t.Parallel()
	a := strictAuthorizer()
	if _, err := authz.ClassifyArtifactWrite(writerCtx("bench_harness"), a, domain.AgentPrincipal("bench_harness"), []string{"confidential"}); err == nil {
		t.Fatal("artifact write with an unhonourable tag must be refused")
	} else {
		requireUnhonoured(t, err, "confidential")
	}
	if tags, err := authz.ClassifyArtifactWrite(writerCtx("finance_agent"), a, domain.AgentPrincipal("finance_agent"), []string{"finance"}); err != nil || !slices.Contains(tags, "finance") {
		t.Fatalf("honourable artifact write: %v %v", tags, err)
	}

	ks := authz.NewEnforcingKnowledgeStore(knowledgePutRecorder{}, a, quietLogger())
	_, _, err := ks.PutItem(writerCtx("bench_harness"), domain.KnowledgeItem{ID: "k1", Kind: "observation", Classification: []string{"finance"}})
	requireUnhonoured(t, err, "finance")
}

type knowledgePutRecorder struct{ domain.KnowledgeStore }

func (knowledgePutRecorder) PutItem(_ context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	return it.ID, true, nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ENG-2161 (owner decision 2026-11-19): an UNTAGGED write whose writer's ceiling holds
// closed tags is refused with a *ClassificationRequiredError naming the closed tags
// and the ceiling to choose from; it still wraps ErrWriteDenied (InvalidArgument at
// the RPC planes).
func TestClassifyWriteStrict_UntaggedUnderClosedCeilingIsRefusedNamingTheChoice(t *testing.T) {
	a := strictAuthorizer()
	a.ceilings["operator"] = []string{"company_wide", "finance", "people"}
	a.vocab = append(a.vocab, "company_wide")
	a.closed = []string{"finance", "people"}

	for _, hint := range [][]string{nil, {"provenance:source=forged"}, {domain.OwnerTag("emp-1")}} {
		_, dec, err := authz.ClassifyWriteStrict(writerCtx("operator"), a, domain.AgentPrincipal("operator"), hint)
		if !errors.Is(err, authz.ErrWriteDenied) {
			t.Fatalf("hint %v: want a refusal wrapping ErrWriteDenied, got %v", hint, err)
		}
		var cre *authz.ClassificationRequiredError
		if !errors.As(err, &cre) {
			t.Fatalf("hint %v: want *ClassificationRequiredError, got %T: %v", hint, err, err)
		}
		if !slices.Equal(cre.Closed, []string{"finance", "people"}) || !slices.Equal(cre.Ceiling, []string{"company_wide", "finance", "people"}) {
			t.Errorf("hint %v: closed=%v ceiling=%v", hint, cre.Closed, cre.Ceiling)
		}
		for _, w := range []string{"finance", "people", "company_wide", "nothing was stored"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("hint %v: message must name %q: %q", hint, w, err.Error())
			}
		}
		if dec.Reason != domain.ReasonClassificationRequired {
			t.Errorf("hint %v: decision reason %q", hint, dec.Reason)
		}
	}

	// Choosing from the ceiling — an open tag alone included — is honoured.
	final, _, err := authz.ClassifyWriteStrict(writerCtx("operator"), a, domain.AgentPrincipal("operator"), []string{"company_wide"})
	if err != nil || !slices.Contains(final, "company_wide") || slices.Contains(final, "finance") {
		t.Fatalf("a chosen open tag: final=%v err=%v", final, err)
	}
}

// ENG-2161: a writer whose ceiling holds only OPEN tags keeps the ADR-0035 C2 default
// — an untagged write takes the whole ceiling.
func TestClassifyWriteStrict_UntaggedUnderOpenCeilingTakesTheCeiling(t *testing.T) {
	a := strictAuthorizer()
	a.closed = []string{"people"} // closed in the deployment, but not in this writer's ceiling
	final, _, err := authz.ClassifyWriteStrict(writerCtx("finance_agent"), a, domain.AgentPrincipal("finance_agent"), nil)
	if err != nil {
		t.Fatalf("untagged write under an open-only ceiling must proceed: %v", err)
	}
	if !slices.Contains(final, "confidential") || !slices.Contains(final, "finance") {
		t.Fatalf("final %v, want the whole ceiling", final)
	}
}
