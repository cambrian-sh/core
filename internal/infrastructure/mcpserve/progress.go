package mcpserve

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The MCP progress channel of the live step-output stream (ADR-0139 amendment
// 2026-11-06).
//
// The endpoint is stateless streamable HTTP (ADR-0126): there is no standalone
// server→client stream, but a notification sent IN THE CONTEXT OF a request rides
// that request's response stream (go-sdk StreamableHTTPOptions.Stateless). So live
// output can reach a caller only while one of its calls is open — which is why a
// task call that carries a progressToken is held open (bounded by the server's
// wait) and the task's step output is sent as notifications/progress on it.

// progressNotifier sends one notifications/progress for the current call.
type progressNotifier func(ctx context.Context, progress float64, message string) error

type progressKey struct{}

// withProgress attaches the call's notifier (invoke, when the request carries a
// progressToken).
func withProgress(ctx context.Context, n progressNotifier) context.Context {
	return context.WithValue(ctx, progressKey{}, n)
}

func progressFrom(ctx context.Context) progressNotifier {
	n, _ := ctx.Value(progressKey{}).(progressNotifier)
	return n
}

// Follow-loop tuning. Text deltas are coalesced so a token-level stream becomes a
// few notifications a second rather than one per token.
const (
	progressFlushEvery   = 250 * time.Millisecond
	progressMaxMessage   = 4096 // bytes of text per notification before an early flush
	progressBuffer       = 256  // chunks buffered between the stream and the call
	statusPollEvery      = 500 * time.Millisecond
	maxWaitSecondsAccept = 1 << 20 // guards the Duration conversion; MaxWait bounds it
)

// waitFor resolves how long a call holds open: an explicit wait_seconds (bounded by
// MaxWait), else DefaultWait when the call carries a progressToken, else 0.
func (o TaskOutput) waitFor(ctx context.Context, explicit *int) (time.Duration, error) {
	maxWait := o.MaxWait
	if maxWait <= 0 {
		maxWait = o.DefaultWait
	}
	if explicit != nil {
		if *explicit < 0 || *explicit > maxWaitSecondsAccept {
			return 0, fmt.Errorf("wait_seconds must be between 0 and the server's maximum")
		}
		return min(time.Duration(*explicit)*time.Second, maxWait), nil
	}
	if progressFrom(ctx) != nil {
		return min(o.DefaultWait, maxWait), nil
	}
	return 0, nil
}

// follow holds the call open until the task is terminal, wait elapses or the call
// is cancelled, sending the task's live step output as progress notifications
// when the call carries a progressToken. It runs on the handler's goroutine and
// starts none: the stream hands chunks to a bounded buffer (dropping, counted,
// when the caller is slower than the task — the result is still delivered whole by
// the status answer), and this loop drains it.
func (o TaskOutput) follow(ctx context.Context, lane TaskLane, taskID string, wait time.Duration) {
	notify := progressFrom(ctx)
	chunks := make(chan domain.StepOutputChunk, progressBuffer)
	var dropped atomic.Int64 // written from the stream's goroutine
	if notify != nil && o.Subscribe != nil {
		cancel := o.Subscribe(taskID, func(c domain.StepOutputChunk) {
			select {
			case chunks <- c:
			default:
				dropped.Add(1)
			}
		})
		defer cancel()
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	flushTick := time.NewTicker(progressFlushEvery)
	defer flushTick.Stop()
	pollTick := time.NewTicker(statusPollEvery)
	defer pollTick.Stop()

	var text strings.Builder
	sent := 0
	send := func(msg string) {
		if notify == nil || msg == "" {
			return
		}
		sent++
		// Best-effort: a client that went away ends the call through ctx.
		_ = notify(ctx, float64(sent), msg)
	}
	flush := func() {
		if text.Len() > 0 {
			send(text.String())
			text.Reset()
		}
		if n := dropped.Swap(0); n > 0 {
			send(fmt.Sprintf("[%d live output chunks dropped: the caller fell behind]", n))
		}
	}
	take := func(c domain.StepOutputChunk) {
		if c.Data == "" {
			return
		}
		if c.Kind != domain.StepOutputText {
			// Progress and status notes are their own messages.
			flush()
			send(c.Data)
			return
		}
		text.WriteString(c.Data)
		if text.Len() >= progressMaxMessage {
			flush()
		}
	}
	drain := func() {
		for {
			select {
			case c := <-chunks:
				take(c)
			default:
				flush()
				return
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			drain()
			return
		case c := <-chunks:
			take(c)
		case <-flushTick.C:
			flush()
		case <-pollTick.C:
			if view, ok := lane.Status(ctx, taskID); !ok || TaskTerminal(view.State) {
				drain()
				return
			}
		}
	}
}
