package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0136 Amendment A2: the source-agnostic prompt (A2.2, A2.5), sectioned coverage of
// long sources (A2.3) and versioned, coverage-carrying markers (A2.4).

// sourceKindWords is the A2.2 guard vocabulary: source-kind words the FIXED prompt text
// must never contain; they may reach a prompt only as metadata values.
var sourceKindWords = []string{"conversation", "session", "chat", "user", "document", "pdf", "email", "turn"}

func assertSourceAgnostic(t *testing.T, name, text string) {
	t.Helper()
	for _, w := range sourceKindWords {
		if re := regexp.MustCompile(`(?i)\b` + w + `\b`); re.MatchString(text) {
			t.Errorf("%s: the fixed text names the source kind %q (%q); per-source words may only come from metadata",
				name, w, re.FindString(text))
		}
	}
}

func TestFactPrompt_SourceAgnostic(t *testing.T) {
	// Empty metadata and no passages: everything rendered is the fixed text.
	assertSourceAgnostic(t, "fact prompt", buildFactPrompt(factExtractionInput{}))
	// The section line of a multi-window source is fixed text too.
	assertSourceAgnostic(t, "fact prompt (section)", buildFactPrompt(factExtractionInput{Section: 2, Sections: 3}))
	// The rendered metadata labels, with every basis.
	for _, b := range []domain.ReferenceBasis{domain.ReferenceBasisEvent, domain.ReferenceBasisAuthored, domain.ReferenceBasisIngested} {
		assertSourceAgnostic(t, "source block "+string(b), factSourceBlock(domain.SourceMeta{ReferenceBasis: b, ReferenceTime: time.Unix(0, 0)}))
	}
	// The guard catches what it must (a word match, case-insensitive) and only that.
	for text, want := range map[string]bool{"the User said": true, "a Turn": true, "return the facts": false, "users": false} {
		hit := false
		for _, w := range sourceKindWords {
			if regexp.MustCompile(`(?i)\b` + w + `\b`).MatchString(text) {
				hit = true
			}
		}
		if hit != want {
			t.Errorf("guard on %q = %v, want %v", text, hit, want)
		}
	}
}

func TestScenePrompt_SourceAgnostic(t *testing.T) {
	sg := NewSceneGenerator(nil)
	p := sg.buildPrompt([]domain.ExternalDocument{{}})
	assertSourceAgnostic(t, "scene prompt", p)
	if strings.Contains(p, "social dynamics") {
		t.Fatal("the scene prompt still asks for a conversational dimension")
	}
	// Metadata is passed as data, the time with its basis.
	p = sg.buildPrompt([]domain.ExternalDocument{{SourceType: "pdf", Title: "Spec", Author: "acme",
		Timestamp: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), TimeSource: domain.TimeSourceSourceMetadata}})
	for _, want := range []string{"<SourceType>pdf</SourceType>", "<Title>Spec</Title>", "<Author>acme</Author>", `<Time basis="authored">2024-01-02T00:00:00Z</Time>`} {
		if !strings.Contains(p, want) {
			t.Errorf("scene prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestSourceMeta_ReferenceBasisFromIngestMetadata(t *testing.T) {
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	ing := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		src  domain.TimeSource
		t    time.Time
		want domain.ReferenceBasis
	}{
		{domain.TimeSourceExplicit, at, domain.ReferenceBasisEvent},
		{domain.TimeSourceSourceMetadata, at, domain.ReferenceBasisAuthored},
		{domain.TimeSourceInferred, at, domain.ReferenceBasisAuthored},
		{domain.TimeSourceIngestDefault, at, domain.ReferenceBasisIngested},
		{domain.TimeSourceClampedFuture, at, domain.ReferenceBasisIngested},
		{"", at, domain.ReferenceBasisIngested},
		{domain.TimeSourceExplicit, time.Time{}, domain.ReferenceBasisIngested},
	} {
		if got := domain.ReferenceBasisOf(c.src, c.t); got != c.want {
			t.Errorf("ReferenceBasisOf(%q, zero=%v) = %q, want %q", c.src, c.t.IsZero(), got, c.want)
		}
	}
	m := domain.SourceMetaOf(domain.ExternalDocument{SourceType: " pdf ", Title: "Manual", Timestamp: at,
		TimeSource: domain.TimeSourceExplicit, TimePrecision: domain.TimePrecisionDay}, ing, "agent-7")
	if m.SourceType != "pdf" || m.Title != "Manual" || m.Author != "agent-7" || !m.ReferenceTime.Equal(at) ||
		m.ReferenceBasis != domain.ReferenceBasisEvent || m.TimePrecision != domain.TimePrecisionDay {
		t.Fatalf("meta = %+v", m)
	}
	if m := domain.SourceMetaOf(domain.ExternalDocument{Author: "bob"}, ing, "agent-7"); m.Author != "bob" ||
		!m.ReferenceTime.Equal(ing) || m.ReferenceBasis != domain.ReferenceBasisIngested {
		t.Fatalf("undated meta = %+v", m)
	}
}

func TestFactPassages_SplitAtSentencesNeverCut(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 3*maxFactPassageChars; i++ {
		fmt.Fprintf(&sb, "Sentence number %d states one plain thing. ", i)
	}
	long := strings.TrimSpace(sb.String())
	pieces := splitFactPassage(long, maxFactPassageChars)
	if len(pieces) < 3 {
		t.Fatalf("pieces = %d, want >= 3", len(pieces))
	}
	for i, p := range pieces {
		if len(p) > maxFactPassageChars {
			t.Fatalf("piece %d is %d bytes > %d", i, len(p), maxFactPassageChars)
		}
		if !strings.HasSuffix(p, ".") && i < len(pieces)-1 {
			t.Errorf("piece %d does not end at a sentence boundary: …%q", i, p[len(p)-20:])
		}
		if !strings.Contains(long, p) {
			t.Fatalf("piece %d is not a span of the original", i)
		}
	}
	if got := strings.Join(pieces, " "); got != long {
		t.Fatal("the pieces do not cover the original text exactly (no silent cut)")
	}
	// No sentence boundary: whitespace; no whitespace at all: a rune boundary.
	words := strings.Repeat("word ", 3000)
	for _, p := range splitFactPassage(words, 1000) {
		if len(p) > 1000 || strings.HasSuffix(p, "wor") {
			t.Fatalf("whitespace split broke a word: %q", p[len(p)-10:])
		}
	}
	runes := strings.Repeat("é", 2000) // 2 bytes each
	total := 0
	for _, p := range splitFactPassage(runes, 1001) {
		if !strings.HasPrefix(p, "é") || len(p) > 1001 {
			t.Fatalf("rune split broke a character")
		}
		total += len(p)
	}
	if total != len(runes) {
		t.Fatalf("rune split lost bytes: %d of %d", total, len(runes))
	}
	if got := factPassages(factChunkDocs("short", long, "")); len(got) != 1+len(pieces) || got[0].Chunk != 0 || got[1].Chunk != 1 || got[len(got)-1].Chunk != 1 {
		t.Fatalf("passages = %d (%+v)", len(got), got[0])
	}
}

func TestFactWindows_PackInOrderWithinBudget(t *testing.T) {
	src := domain.SourceMeta{Title: "t"}
	passages := make([]string, 12)
	for i := range passages {
		passages[i] = fmt.Sprintf("passage %02d %s", i, strings.Repeat("x", 900))
	}
	head := len(factPromptHead(factExtractionInput{Source: src})) + factSectionReserve
	budget := head + 2000 // two passages per window
	ws := packFactWindows(passages, src, nil, budget)
	if len(ws) != 6 {
		t.Fatalf("windows = %+v, want 6 of 2", ws)
	}
	next := 0
	for i, w := range ws {
		if w.Start != next || w.End <= w.Start {
			t.Fatalf("window %d = %+v breaks order/coverage", i, w)
		}
		next = w.End
		p := buildFactPrompt(factExtractionInput{Source: src, Passages: passages[w.Start:w.End], Section: i + 1, Sections: len(ws)})
		if len(p) > budget {
			t.Fatalf("window %d renders %d chars > budget %d", i, len(p), budget)
		}
	}
	if next != len(passages) {
		t.Fatalf("windows cover %d of %d passages", next, len(passages))
	}
	if ws := packFactWindows(passages, src, nil, 10); len(ws) != len(passages) {
		t.Fatalf("a passage larger than the budget still gets its own window: %+v", ws)
	}
}

// windowGen answers each extraction window with one fact quoting the first passage
// it was sent, and counts the windows by their section line.
func windowGen() *scriptedGen {
	re := regexp.MustCompile(`(?m)^\[1\] (passage \d+)`)
	return &scriptedGen{answer: func(p string) (string, error) {
		if isAssessmentPrompt(p) {
			return `{"verdicts":[]}`, nil
		}
		m := re.FindStringSubmatch(p)
		if m == nil {
			return `{"facts":[]}`, nil
		}
		return `{"facts":[{"subject":"source","attribute":"mentions","value":"` + m[1] + `","passage":1,"quote":"` + m[1] + `"}]}`, nil
	}}
}

// A2.3 + A2.4: a source longer than max_calls_per_document windows is extracted up to
// the cap, recorded as a partial marker, resumed from the first uncovered window, then
// complete; a different extractor version makes it pending again.
func TestFactExtractor_SectionedPartialResumeAndVersion(t *testing.T) {
	sub := newMemSubstrate()
	gen := windowGen()
	src := domain.SourceMeta{Title: "Long source", ReferenceTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), ReferenceBasis: domain.ReferenceBasisAuthored}
	head := len(factPromptHead(factExtractionInput{Source: src})) + factSectionReserve
	texts := make([]string, 5)
	for i := range texts {
		texts[i] = fmt.Sprintf("passage %d says something. %s", i, strings.Repeat("y", 900))
	}
	cfg := FactExtractorConfig{MaxPromptChars: head + 1000, MaxCallsPerDocument: 2, GeneratorModel: "model-a"}
	e := newTestExtractor(gen, sub, cfg)
	docs := factChunkDocs(texts...)
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	ctx := context.Background()

	res, err := e.ExtractDocument(ctx, "doc-long", "", src, docs)
	if err != nil || res.Windows != 2 || !res.Partial || res.Stats.Inserted != 2 || gen.calls.Load() != 2 {
		t.Fatalf("run 1: %+v err %v calls %d", res, err, gen.calls.Load())
	}
	mk, _ := sub.marker("doc-long")
	if mk.ExtractorVersion != "fact-extract-v2/model-a" || mk.PassagesCovered != 2 || mk.PassagesTotal != 5 || mk.Items != 2 {
		t.Fatalf("partial marker = %+v", mk)
	}
	if done, _ := e.AlreadyExtracted(ctx, "doc-long", ids); done {
		t.Fatal("a partial marker must read as pending")
	}

	res, err = e.ExtractDocument(ctx, "doc-long", "", src, docs)
	if err != nil || res.Windows != 2 || !res.Partial || gen.calls.Load() != 4 {
		t.Fatalf("run 2 must resume at window 3: %+v err %v calls %d", res, err, gen.calls.Load())
	}
	res, err = e.ExtractDocument(ctx, "doc-long", "", src, docs)
	if err != nil || res.Windows != 1 || res.Partial || gen.calls.Load() != 5 {
		t.Fatalf("run 3 finishes the source: %+v err %v calls %d", res, err, gen.calls.Load())
	}
	mk, _ = sub.marker("doc-long")
	if mk.PassagesCovered != 5 || mk.PassagesTotal != 5 || mk.Items != 5 {
		t.Fatalf("complete marker = %+v", mk)
	}
	if done, _ := e.AlreadyExtracted(ctx, "doc-long", ids); !done {
		t.Fatal("a complete marker of the running version is done")
	}
	if res, _ := e.ExtractDocument(ctx, "doc-long", "", src, docs); !res.Skipped || gen.calls.Load() != 5 {
		t.Fatalf("run 4 must skip: %+v", res)
	}
	// Every window's fact landed on its own chunk (turn index = chunk index).
	for i := range texts {
		f, ok := sub.byValue(fmt.Sprintf("passage %d", i))
		if !ok || f.SourceChunkID != docs[i].ID || f.TurnIndex != i {
			t.Fatalf("passage %d fact = %+v ok=%v", i, f, ok)
		}
	}

	// A changed backbone is a new extractor: pending again, re-extracted from the start.
	cfg.GeneratorModel = "model-b"
	e2 := newTestExtractor(gen, sub, cfg)
	if done, _ := e2.AlreadyExtracted(ctx, "doc-long", ids); done {
		t.Fatal("a marker of another extractor version must read as pending")
	}
	if res, _ := e2.ExtractDocument(ctx, "doc-long", "", src, docs); res.Windows != 2 || res.Stats.Existing != 2 {
		t.Fatalf("new version starts at window 1 (same facts, idempotent ids): %+v", res)
	}
	// A legacy marker (written before 0024: no version) is pending.
	sub.clearMarkers()
	_ = sub.MarkExtracted(ctx, domain.ExtractionMarker{NamespaceID: "default", Extractor: domain.KindMemoryFact, DocumentID: "doc-long", ContentHash: factContentHash(docs), Items: 9})
	if done, _ := e2.AlreadyExtracted(ctx, "doc-long", ids); done {
		t.Fatal("a legacy marker must read as pending")
	}
}

// A2.3: the per-source fact budget stops extraction, and a source whose budget is spent
// is not pending (nothing more could be kept under this configuration).
func TestFactExtractor_MaxFactsPerSourceStopsAndIsDone(t *testing.T) {
	sub := newMemSubstrate()
	gen := windowGen()
	src := domain.SourceMeta{}
	head := len(factPromptHead(factExtractionInput{Source: src})) + factSectionReserve
	texts := make([]string, 4)
	for i := range texts {
		texts[i] = fmt.Sprintf("passage %d. %s", i, strings.Repeat("z", 900))
	}
	e := newTestExtractor(gen, sub, FactExtractorConfig{MaxPromptChars: head + 1000, MaxFactsPerSource: 2})
	docs := factChunkDocs(texts...)
	res, err := e.ExtractDocument(context.Background(), "doc-b", "", src, docs)
	if err != nil || res.Windows != 2 || !res.Partial || gen.calls.Load() != 2 {
		t.Fatalf("budget: %+v err %v calls %d", res, err, gen.calls.Load())
	}
	ids := []string{docs[0].ID, docs[1].ID, docs[2].ID, docs[3].ID}
	if done, _ := e.AlreadyExtracted(context.Background(), "doc-b", ids); !done {
		t.Fatal("a source whose fact budget is spent is not pending")
	}
}

// A2.3: an oversized chunk is split, and a quote is validated against the sub-passage
// actually sent; the fact keys on the chunk.
func TestFactExtractor_OversizedChunkSplitQuotesTheSubPassage(t *testing.T) {
	sub := newMemSubstrate()
	tail := "The pump was replaced by model K9 in March."
	long := strings.Repeat("Filler sentence without facts. ", 400) + tail // > maxFactPassageChars
	// The model names the passage that holds the quote; passage 1 does not, so a quote
	// checked against the WHOLE chunk (or the wrong sub-passage) would be accepted.
	var sentPassages int
	gen := &scriptedGen{answer: func(p string) (string, error) {
		n := 0
		for _, line := range strings.Split(p, "\n") {
			if m := regexp.MustCompile(`^\[(\d+)\] `).FindStringSubmatch(line); m != nil {
				sentPassages++
				if strings.Contains(line, tail) {
					fmt.Sscanf(m[1], "%d", &n)
				}
			}
		}
		if n == 0 {
			return `{"facts":[]}`, nil
		}
		return fmt.Sprintf(`{"facts":[`+
			`{"subject":"pump","attribute":"model","value":"K9","supersedes_prior":true,"passage":%d,"quote":"replaced by model K9"},`+
			`{"subject":"pump","attribute":"model","value":"K8","passage":1,"quote":"replaced by model K9"}]}`, n), nil
	}}
	e := newTestExtractor(gen, sub, FactExtractorConfig{Supersession: true})
	// Basis ingested: the cue gives no lower bound (A2.2 rule 5).
	src := domain.SourceMeta{ReferenceTime: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), ReferenceBasis: domain.ReferenceBasisIngested}
	res, err := e.ExtractDocument(context.Background(), "doc-p", "", src, factChunkDocs(long))
	if err != nil || res.Stats.Inserted != 1 {
		t.Fatalf("res %+v err %v", res, err)
	}
	f, ok := sub.byValue("K9")
	if !ok || f.SourceChunkID != "chunk-a" || f.TurnIndex != 0 {
		t.Fatalf("fact = %+v", f)
	}
	if _, wrong := sub.byValue("K8"); wrong || sentPassages < 2 {
		t.Fatalf("a quote must be validated against the sub-passage it names (sent %d passages)", sentPassages)
	}
	// The STORED statement (not the derived view, which orders an undated fact by its
	// assertion time) carries no start: an ingest clock does not bound a cued fact.
	sub.mu.Lock()
	stored := domain.FactFromItem(sub.items[domain.KnowledgeItemID(f.ID)])
	sub.mu.Unlock()
	if stored.ValidFrom != nil || stored.ValidFromSource != domain.ValidFromUnknown {
		t.Fatalf("an ingest clock must not bound a cued fact: %+v", stored)
	}
	mk, _ := sub.marker("doc-p")
	if mk.PassagesTotal < 2 || mk.PassagesCovered != mk.PassagesTotal {
		t.Fatalf("the long chunk must be covered as several passages: %+v", mk)
	}
}

func TestFactExtractor_EstimateAndBound(t *testing.T) {
	e := newTestExtractor(&scriptedGen{}, newMemSubstrate(), FactExtractorConfig{MaxPromptChars: 48000, MaxCallsPerDocument: 8, Supersession: true, Assessments: true})
	if got := e.MaxLLMCallsPerDocument(); got != 16 {
		t.Fatalf("bound = %d, want 8 windows x 2", got)
	}
	if got := e.EstimateLLMCalls(10_000); got != 2 {
		t.Fatalf("a 10k-char source is one window (+1 assessment): %d", got)
	}
	if got := e.EstimateLLMCalls(900_000); got != 16 {
		t.Fatalf("a 900k-char source is capped at 8 windows: %d", got)
	}
}
