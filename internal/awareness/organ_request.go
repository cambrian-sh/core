package awareness

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// The awareness organs' C1 requests (ADR-0139 D1/D2): planner, replan and
// clarifier each send one library prompt pair as
//
//	System         = [the slot-free stable prompt, cached]
//	Messages       = [one user turn: the _input prompt, data in tagged regions, task last]
//	ResponseSchema = the stable prompt's schema file
//
// and read the reply through decodeReply.

// planRequest renders one call of a prompt pair. Every value is untrusted data:
// each closing tag of the pair's data regions inside a value is neutralised and
// trailing newlines are trimmed, so a region always closes on its own line.
func planRequest(pair prompts.Pair, dataTags []string, values map[string]string, ttl domain.CacheTTL) (domain.GenerateRequest, error) {
	clean := make(map[string]string, len(values))
	for k, v := range values {
		clean[k] = prompts.NeutraliseClosingTags(strings.TrimRight(v, "\n"), dataTags...)
	}
	msg, err := pair.Message(clean)
	if err != nil {
		// A slot mismatch is a build defect (the golden tests render every
		// organ), not a model failure, so it is returned, never swallowed.
		return domain.GenerateRequest{}, fmt.Errorf("awareness: prompt %s: %w", pair.Input.ID, err)
	}
	return domain.GenerateRequest{
		System:         []domain.SystemBlock{{Text: pair.System.Body, Cache: true}},
		Messages:       []domain.ModelMessage{domain.UserMessage(msg)},
		ResponseSchema: &domain.ResponseSchema{Name: pair.System.ID, Schema: pair.Schema()},
		CacheTTL:       ttl,
	}, nil
}

// decodeReply decodes a schema-carrying reply into v.
//
// The provider layer validates (and once repairs) every reply whose generator did
// not report the schema Ignored, so such a reply is decoded as is: there is no
// JSON scraping left on this path. An unchecked reply — a generator that ignored
// the schema, or the legacy string path of domain.GenerateVia — gets the single
// tolerance the provider layer itself grants best-effort models: one surrounding
// markdown code fence. A reply wrapped in prose fails; guessing which braces were
// meant is how the old first-'{' scrapers decoded the wrong object.
func decodeReply(resp domain.GenerateResponse, v any) error {
	text := strings.TrimSpace(resp.Text)
	if resp.SchemaUnchecked() {
		text = domain.StripCodeFence(text)
	}
	if err := json.Unmarshal([]byte(text), v); err != nil {
		return fmt.Errorf("reply is not the requested JSON: %w", err)
	}
	return nil
}

// decodePlanReply is decodeReply for plans, with a plan-shaped error.
func decodePlanReply(resp domain.GenerateResponse, reply *planReply) error {
	if err := decodeReply(resp, reply); err != nil {
		return fmt.Errorf("%w | raw: %s", err, truncateRunes(resp.Text, 512))
	}
	return nil
}

// truncateRunes cuts s to at most n runes, never inside a UTF-8 sequence.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
