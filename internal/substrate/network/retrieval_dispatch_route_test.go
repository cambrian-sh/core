package network

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// replyCaller plays the retrieval_agent with a fixed decompose response and
// records the request it was sent.
type replyCaller struct {
	reply string
	reqs  *[]string
}

func (c replyCaller) CallAgent(_ context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	*c.reqs = append(*c.reqs, string(h.Payload.Data))
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(c.reply)}}, nil
}

// ADR-0135 D1: the route rides the decompose op's response; an older agent that
// sends none yields an empty route (⇒ chain), and Decompose keeps its contract.
func TestRetrievalDispatch_DecomposeRoutedCarriesRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, reply, wantRoute string
		wantSubqs              []string
	}{
		{"facets", `{"route":"facets","sub_questions":["a?","b?"],"refs":["a","b"]}`, "facets", []string{"a?", "b?"}},
		{"route-less agent", `{"sub_questions":["a?"],"refs":[""]}`, "", []string{"a?"}},
		{"direct", `{"route":"direct","sub_questions":["q?"],"refs":[""]}`, "direct", []string{"q?"}},
	}
	for _, c := range cases {
		var reqs []string
		d := &RetrievalDispatcher{Caller: replyCaller{c.reply, &reqs}}
		plan, err := d.DecomposeRouted(context.Background(), "q?")
		if err != nil || plan.Route != c.wantRoute || !reflect.DeepEqual(plan.SubQuestions, c.wantSubqs) {
			t.Errorf("%s: DecomposeRouted = %+v, %v", c.name, plan, err)
		}
		subqs, _, err := d.Decompose(context.Background(), "q?")
		if err != nil || !reflect.DeepEqual(subqs, c.wantSubqs) {
			t.Errorf("%s: Decompose = %v, %v", c.name, subqs, err)
		}
		// The routed prompt is asked for only by DecomposeRouted; Decompose sends
		// exactly today's request.
		if len(reqs) != 2 || reqs[0] != `{"op":"decompose","query":"q?","routes":true}` || reqs[1] != `{"op":"decompose","query":"q?"}` {
			t.Errorf("%s: requests sent %q", c.name, reqs)
		}
	}
}
