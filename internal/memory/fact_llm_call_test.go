package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// T-20261104-fact-extractor-fails-permanently-on-some: the tolerant read, the bounded
// re-ask, the scaled timeout and the failure classification (fact_llm_call.go).

const oneFact = `{"facts":[{"subject":"user","attribute":"home_city","value":"Paris","passage":1,"quote":"I live in Paris"}]}`

func TestFactParse_ToleratesFencesAndTrailers(t *testing.T) {
	passages := []string{"I live in Paris these days."}
	for name, resp := range map[string]string{
		"bare":                  oneFact,
		"fenced":                "```json\n" + oneFact + "\n```",
		"trailing fence only":   oneFact + "\n```", // the 2026-11-04 failure: '`' after top-level value
		"trailer with a brace":  oneFact + "\n```\nNote: one fact {from passage 1}.",
		"prose before":          "Here are the facts:\n" + oneFact,
		"brace in prose before": "Output shape {facts: [...]} follows.\n" + oneFact,
	} {
		got, _, err := parseFactResponse(resp, passages, 10)
		if err != nil || len(got) != 1 || got[0].Value != "Paris" {
			t.Errorf("%s: got %+v err %v, want the one fact", name, got, err)
		}
	}
	// A truncated answer must fail, not read as an empty extraction through its first
	// inner fact object.
	truncated := `{"facts":[{"subject":"user","attribute":"home_city","value":"Paris","passage":1,"quote":"I live in Paris"},{"subject":"us`
	if got, _, err := parseFactResponse(truncated, passages, 10); err == nil || FactErrorKind(err) != FactErrKindParse {
		t.Fatalf("truncated: got %+v err %v, want a parse error", got, err)
	}
	if _, _, err := parseFactResponse("no json here", passages, 10); !errors.Is(err, errFactResponseNotJSON) || FactErrorKind(err) != FactErrKindParse {
		t.Fatalf("prose: err %v, want errFactResponseNotJSON (kind parse)", err)
	}
	if _, _, err := parseFactResponse(`{"other":1}`, passages, 10); !errors.Is(err, errFactResponseNotJSON) {
		t.Fatalf("object without the facts key: err %v, want errFactResponseNotJSON", err)
	}
	if got, _, err := parseFactResponse(`{"facts":[]}`, passages, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty facts: %v %v", got, err)
	}
	// The assessment answer reads the same way.
	if _, err := parseAssessmentResponse(`{"verdicts":[]}`+"\n```", nil); err != nil {
		t.Fatalf("assessment with a trailing fence: %v", err)
	}
}

func TestFactExtractor_ParseRetryRepairsAndIsCounted(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	sub := newMemSubstrate()
	gen := &scriptedGen{}
	gen.answer = func(string) (string, error) {
		if gen.calls.Load() == 1 {
			return "Sorry, here is a summary instead.", nil
		}
		return `{"facts":[{"subject":"user","attribute":"home_city","value":"Paris","passage":1,"quote":"I live in Paris"}]}`, nil
	}
	e := newTestExtractor(gen, sub, FactExtractorConfig{ParseRetries: 1})
	res, err := e.ExtractDocument(context.Background(), "doc-1", "", evt(time.Now()), factChunkDocs("I live in Paris now."))
	if err != nil {
		t.Fatal(err)
	}
	if gen.calls.Load() != 2 || res.LLMCalls != 2 || res.Windows != 1 || res.ParseRetries != 1 || res.ParseRepaired != 1 || res.Stats.Inserted != 1 {
		t.Fatalf("calls %d res %+v, want one window repaired by one re-ask", gen.calls.Load(), res)
	}
	if s := e.Stats(); s.ParseFailures != 1 || s.ParseRetries != 1 || s.ParseRepaired != 1 || s.LLMCalls != 2 {
		t.Fatalf("stats %+v", s)
	}
	// The re-ask is the identical prompt: the extractor version stays exact.
	if e.MaxLLMCallsPerDocument() != 0 || e.EstimateLLMCalls(100) != 1 {
		t.Fatalf("bounds: max %d estimate %d", e.MaxLLMCallsPerDocument(), e.EstimateLLMCalls(100))
	}

	// Retries exhausted: the document fails as "parse" after 1+N calls.
	bad := &scriptedGen{answer: func(string) (string, error) { return "```json\n{\"facts\": [", nil }}
	e = newTestExtractor(bad, newMemSubstrate(), FactExtractorConfig{ParseRetries: 2, MaxCallsPerDocument: 4})
	res, err = e.ExtractDocument(context.Background(), "doc-2", "", evt(time.Now()), factChunkDocs("text"))
	if FactErrorKind(err) != FactErrKindParse || bad.calls.Load() != 3 || res.ParseRetries != 2 || res.ParseRepaired != 0 {
		t.Fatalf("err %v calls %d res %+v, want parse failure after 3 calls", err, bad.calls.Load(), res)
	}
	if e.MaxLLMCallsPerDocument() != 4*3 {
		t.Fatalf("MaxLLMCallsPerDocument = %d, want windows x (1 + parse_retries)", e.MaxLLMCallsPerDocument())
	}

	// A timeout is not re-asked and is classified as such.
	slow := &scriptedGen{block: make(chan struct{}), answer: func(string) (string, error) { return oneFact, nil }}
	e = newTestExtractor(slow, newMemSubstrate(), FactExtractorConfig{ParseRetries: 1, LLMTimeout: 20 * time.Millisecond})
	_, err = e.ExtractDocument(context.Background(), "doc-3", "", evt(time.Now()), factChunkDocs("text"))
	if FactErrorKind(err) != FactErrKindTimeout || slow.calls.Load() != 1 || e.Stats().Timeouts != 1 {
		t.Fatalf("err %v calls %d stats %+v, want one timed-out call", err, slow.calls.Load(), e.Stats())
	}
	if FactErrorKind(errors.New("boom")) != FactErrKindOther || FactErrorKind(nil) != "" {
		t.Fatal("kinds")
	}
}

func TestFactExtractor_CallTimeoutScalesAndIsBounded(t *testing.T) {
	e := newTestExtractor(&scriptedGen{}, newMemSubstrate(), FactExtractorConfig{
		LLMTimeout: 120 * time.Second, LLMTimeoutPerKChar: 2500 * time.Millisecond, LLMTimeoutMax: 300 * time.Second,
	})
	for chars, want := range map[int]time.Duration{
		0: 120 * time.Second, 10000: 145 * time.Second, 48000: 240 * time.Second, 200000: 300 * time.Second,
	} {
		if got := e.callTimeout(chars); got != want {
			t.Errorf("callTimeout(%d) = %v, want %v", chars, got, want)
		}
	}
	// No cap configured (or one below the base): a flat base timeout, never unbounded.
	flat := newTestExtractor(&scriptedGen{}, newMemSubstrate(), FactExtractorConfig{LLMTimeout: time.Minute, LLMTimeoutPerKChar: time.Second})
	if got := flat.callTimeout(48000); got != time.Minute {
		t.Fatalf("flat callTimeout = %v, want 1m", got)
	}
}

func TestRedactedResponseEnds_MasksStringContent(t *testing.T) {
	resp := "Here you go:\n```json\n{\"facts\":[{\"subject\":\"user\",\"value\":\"my secret \\\"pin\\\" 4321\"}]}\n```"
	head, tail := redactedResponseEnds(resp)
	all := head + tail
	if strings.Contains(all, "secret") || strings.Contains(all, "4321") || strings.Contains(all, "user") {
		t.Fatalf("string content leaked: %q", all)
	}
	if !strings.Contains(all, "```json {") || !strings.Contains(all, "}]} ```") {
		t.Fatalf("shape lost: %q", all)
	}
	long := strings.Repeat("x", 500) + `{"facts":[{"quote":"unterminated`
	h, tl := redactedResponseEnds(long)
	if len([]rune(h)) != 96 || len([]rune(tl)) != 48 || strings.Contains(tl, "unterminated") || !strings.HasSuffix(tl, `"…`) {
		t.Fatalf("head %q tail %q", h, tl)
	}
}
