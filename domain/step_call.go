package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// StepCall is what the kernel knows, from the caller's lease, about the plan step
// a tool call is made for (ADR-0145 D9/D14): the run, the step, and the request
// facts its guards need (StepGuard). Set by the ExecuteTool RPC from the lease
// binding (never from a header or the arguments) and read by the ToolExecutor's
// guards. There is no per-step tool permission: ADR-0145 D14 removed the
// effect-filtered menu (security is the person's access and approvals).
type StepCall struct {
	RunID     RunID
	StepIndex int
	Guard     StepGuard
}

type stepCallKey struct{}

// WithStepCall carries the step facts on ctx.
func WithStepCall(ctx context.Context, s StepCall) context.Context {
	return context.WithValue(ctx, stepCallKey{}, s)
}

// StepCallFrom returns the step facts on ctx; ok is false outside a plan step.
func StepCallFrom(ctx context.Context) (StepCall, bool) {
	s, ok := ctx.Value(stepCallKey{}).(StepCall)
	return s, ok && s.RunID != "" && s.StepIndex >= 0
}

// IdempotencyKey is the D9 key of a side-effecting call: the hash of the run (the
// plan), the step and the tool with its canonical arguments.
func IdempotencyKey(run RunID, step int, tool string, args []byte) string {
	h := sha256.New()
	for _, p := range []string{string(run), strconv.Itoa(step), tool, string(CanonicalJSON(args))} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IdempotencyCache holds the results of side-effecting calls by idempotency key
// (ADR-0145 D9): a retry of the same call (same run, step, tool and canonical
// arguments) gets the stored result instead of running again, and a duplicate
// arriving while the first is in flight waits for it. Only a call that ran and
// succeeded is stored; a refused or failed call may be retried. In memory, for
// the TTL — and, when built with NewDurableIdempotencyCache, ALSO in the durable
// action record (ADR-0154 stage 2, ENG-1428): every call is recorded under the
// same key (proposed → dispatched, then accepted with its response on success,
// or left dispatched with the error), so a retry after a kernel restart is
// answered from the record instead of running the write again. Without a store
// the behaviour is exactly the in-memory one (ADR-0154 D6).
type IdempotencyCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	done map[string]idemEntry
	busy map[string]chan struct{}
	// durable is the ADR-0154 action record; nil ⇒ in memory only.
	durable ActionRecordStore
}

type idemEntry struct {
	resp ToolCallResponse
	at   time.Time
}

// NewIdempotencyCache makes a cache keeping results for ttl (<= 0: 24 hours).
func NewIdempotencyCache(ttl time.Duration) *IdempotencyCache {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &IdempotencyCache{ttl: ttl, now: time.Now, done: map[string]idemEntry{}, busy: map[string]chan struct{}{}}
}

// NewDurableIdempotencyCache is NewIdempotencyCache backed by the durable action
// record (ADR-0154). store nil ⇒ NewIdempotencyCache.
func NewDurableIdempotencyCache(ttl time.Duration, store ActionRecordStore) *IdempotencyCache {
	c := NewIdempotencyCache(ttl)
	c.durable = store
	return c
}

// Begin is BeginAction with no record metadata beyond the key.
func (c *IdempotencyCache) Begin(ctx context.Context, key string) (stored ToolCallResponse, found bool, finish func(resp ToolCallResponse, ok bool)) {
	return c.BeginAction(ctx, DurableAction{Key: key})
}

// BeginAction claims meta.Key. It returns the stored response (found=true) when
// the call already ran; otherwise the caller runs it and MUST call finish
// exactly once with the outcome (stored only when ok). A concurrent caller of
// the same key waits for the first (or for ctx). meta's other fields (tool, run,
// step, principal, session, argument hash) describe the durable record.
func (c *IdempotencyCache) BeginAction(ctx context.Context, meta DurableAction) (stored ToolCallResponse, found bool, finish func(resp ToolCallResponse, ok bool)) {
	key := meta.Key
	for {
		c.mu.Lock()
		c.sweepLocked()
		if e, ok := c.done[key]; ok {
			c.mu.Unlock()
			return e.resp, true, nil
		}
		wait, inFlight := c.busy[key]
		if !inFlight {
			ch := make(chan struct{})
			c.busy[key] = ch
			c.mu.Unlock()
			release := func(resp ToolCallResponse, ok bool) {
				c.mu.Lock()
				if ok {
					c.done[key] = idemEntry{resp: resp, at: c.now()}
				}
				delete(c.busy, key)
				c.mu.Unlock()
				close(ch)
			}
			if c.durable == nil {
				return ToolCallResponse{}, false, release
			}
			// Durable: a call the record shows as accepted (from before a restart)
			// is answered from it; otherwise the call is recorded as dispatched
			// before it runs. Record I/O fails OPEN — durability is an addition,
			// and a database hiccup must not block the write (it is logged).
			if rec, err := c.durable.Get(ctx, key); err == nil && rec.Result != nil &&
				(rec.State == ActionStateAccepted || rec.State == ActionStateConfirmed) {
				release(*rec.Result, true)
				return *rec.Result, true, nil
			} else if err != nil && !errors.Is(err, ErrActionRecordNotFound) {
				slog.WarnContext(ctx, "ADR-0154: action record unreadable; the call runs without its durable record", "key", key, "err", err)
			}
			meta.State = ActionStateProposed
			if _, _, err := c.durable.Record(ctx, meta); err != nil {
				slog.WarnContext(ctx, "ADR-0154: action record not written", "key", key, "err", err)
			} else if _, err := c.durable.Advance(ctx, key, ActionStateDispatched, ActionUpdate{}); err != nil {
				slog.WarnContext(ctx, "ADR-0154: action record not marked dispatched", "key", key, "err", err)
			}
			return ToolCallResponse{}, false, func(resp ToolCallResponse, ok bool) {
				dctx := context.WithoutCancel(ctx)
				var err error
				if ok {
					r := resp
					_, err = c.durable.Advance(dctx, key, ActionStateAccepted, ActionUpdate{Result: &r})
				} else {
					// Failed, refused or ambiguous: the record stays dispatched with
					// the error, so a retry re-dispatches it (ADR-0154 D3).
					msg := resp.Error
					if msg == "" {
						msg = resp.DenyReason
					}
					_, err = c.durable.Advance(dctx, key, ActionStateDispatched, ActionUpdate{Error: msg})
				}
				if err != nil {
					slog.WarnContext(dctx, "ADR-0154: action record outcome not written", "key", key, "ok", ok, "err", err)
				}
				release(resp, ok)
			}
		}
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ToolCallResponse{Error: "cancelled while an identical call was in flight: " + ctx.Err().Error()}, true, nil
		}
	}
}

func (c *IdempotencyCache) sweepLocked() {
	cut := c.now().Add(-c.ttl)
	for k, e := range c.done {
		if e.at.Before(cut) {
			delete(c.done, k)
		}
	}
}

// ToolArgValidator validates (and, where unambiguous, coerces) a tool call's
// arguments against the tool's JSON Schema at the dispatch boundary (ADR-0145 D9).
// It returns the arguments to run with, or an error written for the model to
// correct. Implemented in internal/toolargs.
type ToolArgValidator interface {
	ValidateArgs(tool string, schema, args []byte) ([]byte, error)
}
