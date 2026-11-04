package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// The fact extractor's LLM call discipline (ADR-0136 D4, T-20261104-fact-extractor-
// fails-permanently-on-some): a tolerant read of the answer, a bounded re-ask when it
// still cannot be read, a timeout that grows with the window it is given, and a failure
// log that names the kind of failure. The 2026-11-04 v3 backfill on claude-haiku-4-5
// showed both classes this answers: a valid object followed by a closing code fence
// (`invalid character '`' after top-level value`; 6 of 7 passed on a plain re-ask), and
// full 48k-character windows running past a flat 120 s call timeout even alone.

// errFactResponseUnparseable marks every answer that could not be read (FactErrorKind
// "parse"); the specific errors below wrap it.
var errFactResponseUnparseable = errors.New("unparseable model response")

// errFactResponseNotJSON is a response with no JSON object holding the expected key.
var errFactResponseNotJSON = fmt.Errorf("fact extraction: response holds no JSON object (%w)", errFactResponseUnparseable)

// maxJSONObjectStarts bounds how many '{' positions decodeKeyedJSONObject tries, so a
// long prose answer full of braces costs a bounded amount of decoding.
const maxJSONObjectStarts = 64

// decodeKeyedJSONObject returns the first top-level JSON object in resp that decodes
// and holds key. It strips a leading and a trailing code fence, then hands each '{' to
// encoding/json's Decoder, which reads exactly one value and ignores whatever follows
// it (a closing fence, a remark) — no hand-rolled brace matching
// (decisions/use-parser-libraries-not-hand-rolled.md). Requiring key matters: without
// it, a truncated answer whose outer object does not decode would yield its first
// inner fact object and read as a successful empty extraction.
func decodeKeyedJSONObject(resp, key string) (json.RawMessage, error) {
	s := strings.TrimSpace(resp)
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:] // the fence line, with its info string ("```json")
		} else {
			s = strings.TrimLeft(s, "`")
		}
	}
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	var lastErr error
	for i, tries := 0, 0; tries < maxJSONObjectStarts && i < len(s); tries++ {
		k := strings.IndexByte(s[i:], '{')
		if k < 0 {
			break
		}
		i += k
		var obj map[string]json.RawMessage
		if err := json.NewDecoder(strings.NewReader(s[i:])).Decode(&obj); err != nil {
			lastErr = err
		} else if v, ok := obj[key]; ok {
			return v, nil
		}
		i++
	}
	if lastErr != nil {
		return nil, fmt.Errorf("fact extraction: response is not the expected JSON: %w (%w)", lastErr, errFactResponseUnparseable)
	}
	return nil, errFactResponseNotJSON
}

// Error kinds of a failed fact call, as logged (err_kind) and counted by the backfill.
const (
	FactErrKindParse   = "parse"
	FactErrKindTimeout = "timeout"
	FactErrKindOther   = "other"
)

// FactErrorKind classifies an extraction error: an unreadable answer, a call that ran
// out of time, or anything else (transport, store). "" for nil.
func FactErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errFactResponseUnparseable):
		return FactErrKindParse
	case errors.Is(err, context.DeadlineExceeded):
		return FactErrKindTimeout
	default:
		return FactErrKindOther
	}
}

// callTimeout is one call's bound for a prompt of promptChars characters:
// llm_timeout_ms + llm_timeout_per_kchar_ms per 1000 characters, capped at
// llm_timeout_max_ms. The answer to a fuller window is longer (more facts, each with a
// verbatim quote), so a flat bound either kills full windows or idles on small ones; a
// smaller max_prompt_chars default was rejected because it changes window packing,
// resume coverage and the call count of every long source (ticket log, 2026-11-04).
func (e *FactExtractor) callTimeout(promptChars int) time.Duration {
	t := e.cfg.LLMTimeout + time.Duration(int64(e.cfg.LLMTimeoutPerKChar)*int64(promptChars)/1000)
	if t > e.cfg.LLMTimeoutMax {
		t = e.cfg.LLMTimeoutMax
	}
	return t
}

// extractCall is one window's extraction: the call, the parse, and up to
// cfg.ParseRetries re-asks of the SAME prompt when the answer cannot be read. The
// re-ask carries no reminder text: a plain retry fixed 6 of 7 such failures, and an
// identical prompt keeps the extractor version (fact-extract-v3) exact. A timeout or a
// transport error is not retried here (the next backfill run retries the document);
// every failed call is logged with its kind.
func (e *FactExtractor) extractCall(ctx context.Context, documentID string, w int, prompt string, passages []string, maxFacts int, res *FactDocumentResult) ([]extractedFact, factParseStats, error) {
	timeout := e.callTimeout(len(prompt))
	for attempt := 0; ; attempt++ {
		llmCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := callTripletsLLM(llmCtx, e.gen, prompt)
		if err == nil && llmCtx.Err() != nil {
			// A streamed answer cut off by the deadline arrives as partial text with no
			// error; it is a timeout, not an unreadable answer.
			err = llmCtx.Err()
		}
		cancel()
		e.llmCalls.Add(1)
		res.LLMCalls++
		if attempt > 0 {
			e.parseRetries.Add(1)
			res.ParseRetries++
		}
		if err != nil {
			kind := FactErrorKind(err)
			if kind == FactErrKindTimeout {
				e.timeouts.Add(1)
			}
			slog.WarnContext(ctx, "FactExtractor: extraction call failed", "document_id", documentID, "window", w+1,
				"attempt", attempt+1, "err_kind", kind, "timeout", timeout.String(), "prompt_chars", len(prompt), "err", err)
			return nil, factParseStats{}, err
		}
		parsed, pst, perr := parseFactResponse(resp, passages, maxFacts)
		if perr == nil {
			if attempt > 0 {
				e.parseRepaired.Add(1)
				res.ParseRepaired++
			}
			return parsed, pst, nil
		}
		e.parseFailures.Add(1)
		retry := attempt < e.cfg.ParseRetries && ctx.Err() == nil
		head, tail := redactedResponseEnds(resp)
		slog.WarnContext(ctx, "FactExtractor: extraction answer unparseable", "document_id", documentID, "window", w+1,
			"attempt", attempt+1, "retrying", retry, "err_kind", FactErrorKind(perr), "response_chars", len(resp),
			"response_head", head, "response_tail", tail, "err", perr)
		if !retry {
			return nil, factParseStats{}, perr
		}
	}
}

// redactedResponseEnds returns the first 96 and last 48 characters of an answer for a
// failure log, with the CONTENT of every JSON string literal masked to "…" and
// whitespace collapsed. What a parse failure needs to show is the answer's shape (a
// fence, a preamble, a truncation, a trailer); the string values are extracted user
// text and stay out of the log.
func redactedResponseEnds(resp string) (head, tail string) {
	var b strings.Builder
	inStr, esc, space := false, false, false
	for _, r := range resp {
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == '"':
				inStr = false
				b.WriteString(`…"`)
			}
			continue
		case r == '"':
			inStr = true
			b.WriteByte('"')
		case r == ' ' || r == '\n' || r == '\r' || r == '\t':
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		default:
			b.WriteRune(r)
		}
		space = false
	}
	if inStr {
		b.WriteString("…") // an unterminated string: a truncated answer
	}
	rs := []rune(b.String())
	const nHead, nTail = 96, 48
	if len(rs) <= nHead+nTail {
		return string(rs), ""
	}
	return string(rs[:nHead]), string(rs[len(rs)-nTail:])
}
