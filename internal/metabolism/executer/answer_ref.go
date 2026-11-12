package executer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// applyAnswerRef resolves a worker's answer_ref (domain.HandoffKeyAnswerRef): the
// step's output becomes the readable text of the referenced tool result, read
// through the ENFORCED ledger reader. The reference must be a tool_result entry of
// THIS run (its ledger run id, generation included) and THIS step — a worker can
// hand on what its own calls returned, never another step's or another run's
// entry. A refused reference fails the step deterministically with the reason (the
// same reference is refused again), instead of letting an empty or one-line answer
// through as the step's output. No reference: resp unchanged.
func (d *DAGExecutor) applyAnswerRef(ctx context.Context, stepIndex int, resp *domain.Handoff) (*domain.Handoff, error) {
	if resp == nil || resp.Context == nil {
		return resp, nil
	}
	ref := strings.TrimSpace(resp.Context[domain.HandoffKeyAnswerRef])
	if ref == "" {
		return resp, nil
	}
	delete(resp.Context, domain.HandoffKeyAnswerRef)
	refuse := func(why string) (*domain.Handoff, error) {
		slog.Warn("answer_ref refused", "step", stepIndex, "ref", ref, "why", why)
		return nil, domain.NewDeterministicError(fmt.Sprintf("step %d: answer_ref %s refused: %s", stepIndex, ref, why))
	}
	if !domain.IsLedgerID(ref) {
		return refuse("not a ledger id (led_…)")
	}
	if d.LedgerReader == nil {
		return refuse("the run ledger is off (execution.ledger.enabled)")
	}
	scope := d.LedgerReadScope
	if scope == nil {
		scope = &domain.TagPredicate{Bypass: true}
	}
	e, err := d.LedgerReader.Get(domain.WithScope(ctx, scope), ref)
	switch {
	case err != nil:
		return refuse("ledger read failed: " + err.Error())
	case e == nil:
		return refuse("no such entry")
	case e.Kind != domain.LedgerToolResult:
		return refuse("it is a " + string(e.Kind) + ", not a tool result")
	case e.RunID != d.ledgerProvenance(ctx).RunID || e.StepIndex != stepIndex:
		return refuse("it is not a result of this step's own tool calls")
	case e.Truncated:
		return refuse(fmt.Sprintf("only the first %d of %d bytes were kept; answer in your own words", len(e.Body), e.SizeBytes))
	}
	text := domain.ResultText(e.Body)
	if strings.TrimSpace(text) == "" {
		return refuse("the tool result has no text")
	}
	out := *resp
	payload := domain.Payload{Type: "text"}
	if resp.Payload != nil {
		payload = *resp.Payload
	}
	payload.Data = []byte(text)
	out.Payload = &payload
	slog.Info("answer_ref: step output is a tool result of its own", "step", stepIndex, "ref", ref, "tool", e.ToolName, "bytes", len(text))
	return &out, nil
}
