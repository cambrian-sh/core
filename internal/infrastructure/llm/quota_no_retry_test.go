package llm

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type countingRT struct {
	calls int
	body  string
	code  int
}

func (rt *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	return &http.Response{
		StatusCode: rt.code,
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func newReq(t *testing.T) *http.Request {
	t.Helper()
	body := []byte(`{"model":"x"}`)
	req, err := http.NewRequest(http.MethodPost, "https://provider.example/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return req
}

// The defect: a quota refusal the provider delivered in 0.56s was retried five times with
// backoff and then surfaced as a context deadline with no explanation, so chat looked broken.
// A weekly quota cannot clear inside a retry ladder — retrying it only converts a fast, clear
// refusal into a slow, silent one.
func TestQuotaRefusalIsNotRetried(t *testing.T) {
	rt := &countingRT{
		code: http.StatusTooManyRequests,
		body: `{"type":"error","error":{"type":"GoUsageLimitError","message":"Weekly usage limit reached. Resets in 2 days."}}`,
	}
	tr := &rateLimitRetryTransport{base: rt}

	resp, err := tr.RoundTrip(newReq(t))
	if err != nil {
		t.Fatalf("the response must be handed back, not turned into an error: %v", err)
	}
	if rt.calls != 1 {
		t.Fatalf("quota refusal was retried %d times; it must be attempted exactly once", rt.calls)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status must survive for the caller's health path, got %d", resp.StatusCode)
	}
	// The body must still be readable — the sniff must restore what it consumed, or a
	// fast failure becomes an EMPTY one and the caller cannot say why.
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "Weekly usage limit reached") {
		t.Fatalf("body was consumed by the quota sniff, caller got: %q", string(got))
	}
}

// The counter-case, and the reason the marker list is specific: an ordinary rate limit IS
// transient and must still ride the backoff ladder. A loose match would make the kernel give
// up on weather it used to survive.
func TestOrdinaryRateLimitStillRetries(t *testing.T) {
	rt := &countingRT{
		code: http.StatusTooManyRequests,
		body: `{"error":{"message":"Rate limit reached for requests. Please try again in 1s."}}`,
	}
	tr := &rateLimitRetryTransport{base: rt}

	if _, err := tr.RoundTrip(newReq(t)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.calls != llmMaxRateLimitRetries+1 {
		t.Fatalf("a transient rate limit must exhaust the ladder (%d attempts), got %d",
			llmMaxRateLimitRetries+1, rt.calls)
	}
}

func TestQuotaExhaustedRestoresBodyWhenItDoesNotMatch(t *testing.T) {
	const body = `{"error":{"message":"Rate limit reached"}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	detail, exhausted := quotaExhausted(resp)
	if exhausted || detail != "" {
		t.Fatalf("a plain rate limit must not be classified as quota: %q", detail)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Fatalf("body not restored on the non-matching path: %q", string(got))
	}
}
