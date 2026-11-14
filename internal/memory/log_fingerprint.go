package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"
)

// TextLogAttrs is how a person's text (a recall query, a sub-question) appears in
// INFO/WARN logs: its length in runes and a 12-hex SHA-256 fingerprint under
// "<name>_len" / "<name>_sha256", never the text itself
// (T-20261103-kernel-logs-print-raw-query-text-and-leaf). orchestrator.log and the
// operator log ring sit outside every erasure and retention path (ADR-0137), so
// content written there could not be forgotten. The fingerprint still correlates a
// log line with the decision record (emitDecision hashes the same way) and with
// another log line about the same text; the raw text is logged only at DEBUG, by
// the call site, for an operator who turns debug logging on deliberately.
func TextLogAttrs(name, text string) []any {
	sum := sha256.Sum256([]byte(text))
	return []any{name + "_len", utf8.RuneCountInString(text), name + "_sha256", hex.EncodeToString(sum[:6])}
}
