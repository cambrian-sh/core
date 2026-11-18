package forget

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2109: replies redacted in the transcript are tombstoned, but they are not
// records the person asked to erase — the answer counts them apart and says so.
func TestReceiptResultTellsRedactedRepliesApart(t *testing.T) {
	b, err := receiptResult(domain.ErasureReceipt{RequestID: "erase:abc", Status: domain.ErasureStatusComplete,
		Targets: 3, RedactedMessages: []string{"msg_1", "msg_2"}})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out["erased"] != true || out["erased_count"] != float64(1) {
		t.Errorf("erased/erased_count = %v/%v, want true/1", out["erased"], out["erased_count"])
	}
	if ids, _ := out["redacted_messages"].([]any); len(ids) != 2 {
		t.Errorf("redacted_messages = %v", out["redacted_messages"])
	}
	tell, _ := out["tell_the_person"].(string)
	if !strings.Contains(tell, "Erased 1 record(s)") || !strings.Contains(tell, "2 earlier chat repl(ies)") ||
		!strings.Contains(tell, domain.RedactedReplyText) {
		t.Errorf("tell_the_person = %q", tell)
	}

	// No redaction: the answer is as before.
	b, _ = receiptResult(domain.ErasureReceipt{RequestID: "erase:def", Targets: 2})
	out = map[string]any{}
	_ = json.Unmarshal(b, &out)
	if _, has := out["redacted_messages"]; has || out["erased_count"] != float64(2) {
		t.Errorf("no redaction must not list messages: %v", out)
	}
}
