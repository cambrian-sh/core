package chat

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type ctxKey struct{}

// T-20261107-chat-agent-tells-the-user-it-has-no-tool-for: every turn carries the
// MCP systems the planner's agents can call as "work_systems", from the wired
// source and under the turn's own context; a customer-facing turn is not shown the
// deployment's internal systems; no source (or an empty summary) adds no key.
func TestTurn_CarriesWorkSystemsFromTheWiredSource(t *testing.T) {
	const summary = "- emp: crm_search, mail_list"
	for _, tc := range []struct {
		name    string
		profile domain.ConversationProfile
		source  func(context.Context) string
		want    string
		present bool
	}{
		{"employee", domain.ProfileEmployee, func(context.Context) string { return summary }, summary, true},
		{"operator", domain.ProfileOperator, func(context.Context) string { return summary }, summary, true},
		{"customer is not shown internal systems", domain.ProfileCustomer, func(context.Context) string { return summary }, "", false},
		{"empty summary adds no key", domain.ProfileEmployee, func(context.Context) string { return "  " }, "", false},
		{"no source", domain.ProfileEmployee, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, pool := setup(t, tc.profile)
			var sawCtx bool
			if tc.source != nil {
				src := tc.source
				svc.SetWorkSystems(func(ctx context.Context) string {
					sawCtx = ctx.Value(ctxKey{}) == "turn"
					return src(ctx)
				})
			}
			ctx := context.WithValue(context.Background(), ctxKey{}, "turn")
			if _, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "what's in my CRM?"}); err != nil {
				t.Fatalf("Turn: %v", err)
			}
			hs := pool.all()
			if len(hs) != 1 {
				t.Fatalf("dispatches = %d", len(hs))
			}
			got, ok := hs[0].Payload.Metadata["work_systems"]
			if ok != tc.present || got != tc.want {
				t.Errorf("work_systems = %q (present %v), want %q (present %v)", got, ok, tc.want, tc.present)
			}
			if tc.present && !sawCtx {
				t.Error("the summary was not built under the turn's context")
			}
		})
	}
}
