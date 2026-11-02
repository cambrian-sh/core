package network

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261102-kg-extractor-cold-start-drops-the-first-kg: the dispatcher must
// report a failed extraction as an error (distinct from an empty result) and
// retry an unreachable agent a bounded number of times.

// kgFakeCaller fails its first failFirst calls with failErr, then replies with
// reply (raw payload bytes). It records each call's request.
type kgFakeCaller struct {
	mu        sync.Mutex
	failFirst int
	failErr   error
	reply     []byte
	calls     int
	requests  []kgRequest
}

func (c *kgFakeCaller) CallAgent(_ context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	var req kgRequest
	_ = json.Unmarshal(h.Payload.Data, &req) // the test inspects what it can decode
	c.requests = append(c.requests, req)
	if c.calls <= c.failFirst {
		return nil, c.failErr
	}
	return &domain.Handoff{FromAgent: agentID, Payload: &domain.Payload{Data: c.reply}}, nil
}

func (c *kgFakeCaller) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var errBoot = errors.New("getOrDialClient: boot agent kg_extractor_agent: agent kg_extractor_agent: socket did not appear within 10s")

// recordSleeps is the backoff seam: it records each wait and returns at once.
func recordSleeps(d *KgExtractorDispatcher) *[]time.Duration {
	var waits []time.Duration
	d.sleep = func(ctx context.Context, dur time.Duration) error {
		waits = append(waits, dur)
		return ctx.Err()
	}
	return &waits
}

func TestKgExtractorDispatch_EmptyResultIsNotAnError(t *testing.T) {
	c := &kgFakeCaller{reply: []byte(`{"triplets":[[],[{"h":"luna","r":"is","t":"cat"}]]}`)}
	d := &KgExtractorDispatcher{Caller: c, Attempts: 3}
	out, err := d.ExtractBatch(context.Background(), []string{"hello", "Luna is a cat"}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("successful reply returned error: %v", err)
	}
	if len(out) != 2 || len(out[0]) != 0 || len(out[1]) != 1 || out[1][0].Weight != 1.0 {
		t.Fatalf("out = %+v", out)
	}
}

func TestKgExtractorDispatch_UnreachableIsAnError(t *testing.T) {
	c := &kgFakeCaller{failFirst: 1 << 30, failErr: errBoot}
	d := &KgExtractorDispatcher{Caller: c, Attempts: 1}
	out, err := d.ExtractBatch(context.Background(), []string{"x"}, []string{"a"})
	if err == nil || out != nil {
		t.Fatalf("unreachable agent: out=%v err=%v, want nil and an error", out, err)
	}
	if !errors.Is(err, domain.ErrTripletExtractorUnavailable) || !errors.Is(err, errBoot) {
		t.Fatalf("error must wrap ErrTripletExtractorUnavailable and the cause: %v", err)
	}
}

// Cold start: the agent fails to boot twice, then serves. Three attempts get
// the batch through, with Backoff then 2*Backoff between them.
func TestKgExtractorDispatch_ColdStartRetrySucceeds(t *testing.T) {
	c := &kgFakeCaller{failFirst: 2, failErr: errBoot, reply: []byte(`{"triplets":[[{"h":"a","r":"b","t":"c"}]]}`)}
	d := &KgExtractorDispatcher{Caller: c, Attempts: 3, Backoff: 100 * time.Millisecond}
	waits := recordSleeps(d)
	out, err := d.ExtractBatch(context.Background(), []string{"x"}, []string{"a"})
	if err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if len(out) != 1 || len(out[0]) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if c.count() != 3 {
		t.Fatalf("calls = %d, want 3", c.count())
	}
	if len(*waits) != 2 || (*waits)[0] != 100*time.Millisecond || (*waits)[1] != 200*time.Millisecond {
		t.Fatalf("backoff = %v, want [100ms 200ms]", *waits)
	}
}

// Bounded: a permanently unreachable agent is called exactly Attempts times.
func TestKgExtractorDispatch_RetriesBounded(t *testing.T) {
	c := &kgFakeCaller{failFirst: 1 << 30, failErr: errBoot}
	d := &KgExtractorDispatcher{Caller: c, Attempts: 4, Backoff: time.Millisecond}
	recordSleeps(d)
	if _, err := d.ExtractBatch(context.Background(), []string{"x"}, []string{"a"}); !errors.Is(err, domain.ErrTripletExtractorUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if c.count() != 4 {
		t.Fatalf("calls = %d, want exactly 4", c.count())
	}
}

// The real backoff wait honours ctx: a cancelled batch stops retrying.
func TestKgExtractorDispatch_CtxEndsRetries(t *testing.T) {
	c := &kgFakeCaller{failFirst: 1 << 30, failErr: errBoot}
	d := &KgExtractorDispatcher{Caller: c, Attempts: 5, Backoff: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.ExtractBatch(ctx, []string{"x"}, []string{"a"})
	if !errors.Is(err, domain.ErrTripletExtractorUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second || c.count() != 1 {
		t.Fatalf("ctx did not end the retries: calls=%d elapsed=%s", c.count(), time.Since(start))
	}
}

// An unusable reply is an error but not "unavailable", and is not retried.
func TestKgExtractorDispatch_BadReplyNotRetried(t *testing.T) {
	for _, reply := range [][]byte{[]byte(`not json`), nil} {
		c := &kgFakeCaller{reply: reply}
		d := &KgExtractorDispatcher{Caller: c, Attempts: 3}
		recordSleeps(d)
		_, err := d.ExtractBatch(context.Background(), []string{"x"}, []string{"a"})
		if err == nil || errors.Is(err, domain.ErrTripletExtractorUnavailable) {
			t.Fatalf("reply %q: err = %v, want a non-unavailable error", reply, err)
		}
		if c.count() != 1 {
			t.Fatalf("reply %q: calls = %d, want 1", reply, c.count())
		}
	}
}

func TestKgExtractorDispatch_NoCallerIsUnavailable(t *testing.T) {
	var d *KgExtractorDispatcher
	if _, err := d.ExtractBatch(context.Background(), []string{"x"}, nil); !errors.Is(err, domain.ErrTripletExtractorUnavailable) {
		t.Fatalf("nil dispatcher: err = %v", err)
	}
	if out, err := (&KgExtractorDispatcher{}).ExtractBatch(context.Background(), nil, nil); err != nil || len(out) != 0 {
		t.Fatalf("empty batch must succeed without a call: %v %v", out, err)
	}
}

// Ready is an empty-batch round trip with the same retry.
func TestKgExtractorDispatch_ReadyProbe(t *testing.T) {
	c := &kgFakeCaller{failFirst: 1, failErr: errBoot, reply: []byte(`{"triplets":[]}`)}
	d := &KgExtractorDispatcher{Caller: c, Attempts: 2}
	recordSleeps(d)
	var _ domain.ReadinessProber = d
	if err := d.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if c.count() != 2 || len(c.requests[1].Texts) != 0 {
		t.Fatalf("probe calls=%d requests=%+v", c.count(), c.requests)
	}
	down := &KgExtractorDispatcher{Caller: &kgFakeCaller{failFirst: 1 << 30, failErr: errBoot}, Attempts: 2}
	recordSleeps(down)
	if err := down.Ready(context.Background()); !errors.Is(err, errBoot) {
		t.Fatalf("Ready on a down agent = %v, want the boot cause", err)
	}
}
