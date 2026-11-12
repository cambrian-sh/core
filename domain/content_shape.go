package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Content shaping: the one policy for how much of a tool result (or an upstream
// step's result) reaches a model's prompt, and in what form (owner decision
// 2026-11-12, T-20261112-worker-resolve-cid-inline-has-no-size-cap-a and
// T-20261112-summarise-a-document-step-gets-only-the-first; sizes are
// execution.content).
//
//   - Binary blocks of an MCP content array (an image, audio, an embedded blob)
//     are bytes a text model cannot read: they become a one-line ledger reference
//     ("[image/png, 241 KB -> ledger led_…]"); text blocks stay. In p06 a 600-char
//     stdout travelled as a 300k-char result because the code sandbox also returned
//     its chart as a base64 image block.
//   - A result over the inline threshold travels as its ledger reference WITH a
//     summary (OffloadSummary), never as a bare id the model can only re-open whole.
//   - Long text is paged by TextPageStarts, the same pages ledger_get serves, so a
//     page number named anywhere (an offload summary, a shortened dependency's
//     outline) is the page ledger_get returns.

// TextPageStarts returns the byte offsets at which each page of text starts, pages
// being at most size bytes and cut on rune boundaries. Empty text has no pages.
func TextPageStarts(text string, size int) []int {
	if size <= 0 {
		size = len(text)
	}
	var starts []int
	for at := 0; at < len(text); {
		starts = append(starts, at)
		cut := at + min(size, len(text)-at)
		for cut < len(text) && cut > at && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == at { // a page smaller than one rune: take the rune whole
			_, n := utf8.DecodeRuneInString(text[at:])
			cut = at + n
		}
		at = cut
	}
	return starts
}

// PageOfOffset is the 1-based page (of TextPageStarts) holding byte offset off.
func PageOfOffset(starts []int, off int) int {
	return sort.Search(len(starts), func(i int) bool { return starts[i] > off })
}

// mcpBlock is one block of an MCP tool result's content array.
type mcpBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
	Data     string          `json:"data,omitempty"`
	Resource json.RawMessage `json:"resource,omitempty"`
}

// mcpContent decodes an MCP content array: the bare array, or an object whose
// "content" is one. ok=false for anything else.
func mcpContent(body []byte) (blocks []json.RawMessage, wrapped map[string]json.RawMessage, ok bool) {
	t := strings.TrimSpace(string(body))
	if strings.HasPrefix(t, "[") {
		if json.Unmarshal(body, &blocks) != nil || !allTyped(blocks) {
			return nil, nil, false
		}
		return blocks, nil, true
	}
	if strings.HasPrefix(t, "{") {
		if json.Unmarshal(body, &wrapped) != nil {
			return nil, nil, false
		}
		c, has := wrapped["content"]
		if !has || json.Unmarshal(c, &blocks) != nil || !allTyped(blocks) {
			return nil, nil, false
		}
		return blocks, wrapped, true
	}
	return nil, nil, false
}

func allTyped(blocks []json.RawMessage) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, raw := range blocks {
		var b mcpBlock
		if json.Unmarshal(raw, &b) != nil || b.Type == "" {
			return false
		}
	}
	return true
}

// BinaryBlock is a binary block of an MCP content array, decoded.
type BinaryBlock struct {
	Kind        string // image, audio, resource
	ContentType string
	Bytes       []byte
}

// binaryOf returns the decoded bytes of a binary block, ok=false for a text block
// or a block whose data is not base64.
func binaryOf(raw json.RawMessage) (BinaryBlock, bool) {
	var b mcpBlock
	if json.Unmarshal(raw, &b) != nil {
		return BinaryBlock{}, false
	}
	data, mime := b.Data, b.MimeType
	if b.Type == "resource" && len(b.Resource) > 0 {
		var r struct {
			Blob     string `json:"blob"`
			MimeType string `json:"mimeType"`
		}
		if json.Unmarshal(b.Resource, &r) != nil || r.Blob == "" {
			return BinaryBlock{}, false
		}
		data, mime = r.Blob, r.MimeType
	} else if b.Type != "image" && b.Type != "audio" {
		return BinaryBlock{}, false
	}
	if data == "" {
		return BinaryBlock{}, false
	}
	raw2, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		if raw2, err = base64.RawStdEncoding.DecodeString(data); err != nil {
			return BinaryBlock{}, false
		}
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return BinaryBlock{Kind: b.Type, ContentType: mime, Bytes: raw2}, true
}

// ReferenceBinaryBlocks replaces every binary block of an MCP content array with a
// text block naming it: "[image/png, 241 KB -> ledger led_…]" when ref returns an
// id, "[image/png, 241 KB, not kept: <why>]" when it returns a reason instead.
// Text blocks are kept as they are. A result that is not an MCP content array, or
// has no binary block, is returned unchanged with changed=false.
func ReferenceBinaryBlocks(result []byte, ref func(BinaryBlock) (id, why string)) (out []byte, changed bool) {
	blocks, wrapped, ok := mcpContent(result)
	if !ok {
		return result, false
	}
	for i, raw := range blocks {
		bb, isBin := binaryOf(raw)
		if !isBin {
			continue
		}
		id, why := ref(bb)
		var text string
		if id != "" {
			text = fmt.Sprintf("[%s %s, %s -> ledger %s]", bb.ContentType, bb.Kind, HumanBytes(len(bb.Bytes)), id)
		} else {
			text = fmt.Sprintf("[%s %s, %s, not kept: %s]", bb.ContentType, bb.Kind, HumanBytes(len(bb.Bytes)), why)
		}
		blocks[i], _ = marshalPlain(map[string]string{"type": "text", "text": text})
		changed = true
	}
	if !changed {
		return result, false
	}
	var b []byte
	var err error
	if wrapped != nil {
		wrapped["content"], _ = marshalPlain(blocks)
		b, err = marshalPlain(wrapped)
	} else {
		b, err = marshalPlain(blocks)
	}
	if err != nil {
		return result, false
	}
	return b, true
}

// marshalPlain is json.Marshal without HTML escaping, so a reference reads
// "-> ledger led_…" to the model, not with the arrow escaped as >.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// HumanBytes renders a size the way a model reads it: "812 B", "241 KB", "3.4 MB".
func HumanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", (n+512)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// ResultText is the readable text of a tool result: the text blocks of an MCP
// content array joined by blank lines (a binary block is already a one-line
// reference by then), a JSON string unquoted, anything else as it is.
func ResultText(body []byte) string {
	if blocks, _, ok := mcpContent(body); ok {
		var parts []string
		for _, raw := range blocks {
			var b mcpBlock
			if json.Unmarshal(raw, &b) == nil && b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	var s string
	if json.Unmarshal(body, &s) == nil {
		return s
	}
	return string(body)
}

// ResultShape describes a result's structure in a few words, for an offload
// summary: "MCP content array: 3 blocks (text, text, text)", "JSON object with
// keys a, b, c", "JSON list of 120 items", "text, 812 lines".
func ResultShape(body []byte) string {
	if blocks, _, ok := mcpContent(body); ok {
		kinds := make([]string, 0, len(blocks))
		for _, raw := range blocks {
			var b mcpBlock
			_ = json.Unmarshal(raw, &b)
			kinds = append(kinds, b.Type)
		}
		return fmt.Sprintf("MCP content array: %d blocks (%s)", len(blocks), strings.Join(kinds, ", "))
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(keys) > 12 {
				keys = append(keys[:12], fmt.Sprintf("… %d more", len(t)-12))
			}
			return "JSON object with keys " + strings.Join(keys, ", ")
		case []any:
			return fmt.Sprintf("JSON list of %d items", len(t))
		case string:
			return fmt.Sprintf("text, %d lines", strings.Count(t, "\n")+1)
		default:
			return "JSON value"
		}
	}
	return fmt.Sprintf("text, %d lines", strings.Count(string(body), "\n")+1)
}

// OffloadSummaryKey is the single key of the summary object an offloaded result
// carries on the wire (ExecuteToolResponse.result_json beside result_cid).
const OffloadSummaryKey = "_offloaded"

// OffloadSummary is what a model is told about a result that travels as its ledger
// reference: never a bare id (owner decision A2, 2026-11-12).
type OffloadSummary struct {
	Ref         string `json:"ref"`
	Tool        string `json:"tool,omitempty"`
	SizeBytes   int    `json:"size_bytes"`
	ContentType string `json:"content_type"`
	Shape       string `json:"shape"`
	Preview     string `json:"preview"`
	Pages       int    `json:"pages"`
	Read        string `json:"read"`
}

// NewOffloadSummary summarises an offloaded result. The preview is of its readable
// text (ResultText), so a JSON-escaped MCP array previews as what it says.
func NewOffloadSummary(ref, tool string, body []byte, previewChars, pageBytes int) OffloadSummary {
	ct := "text/plain"
	if json.Valid(body) {
		ct = "application/json"
	}
	text := ResultText(body)
	pages := len(TextPageStarts(string(body), pageBytes))
	return OffloadSummary{
		Ref: ref, Tool: tool, SizeBytes: len(body), ContentType: ct, Shape: ResultShape(body),
		Preview: RunePrefix(text, previewChars), Pages: pages,
		Read: fmt.Sprintf("ledger_get id='%s' page=1..%d; search inside it: ledger_get id='%s' find='<text>'", ref, max(pages, 1), ref),
	}
}

// JSON renders the wire form: {"_offloaded": {...}}.
func (s OffloadSummary) JSON() []byte {
	b, _ := json.Marshal(map[string]OffloadSummary{OffloadSummaryKey: s})
	return b
}

// RunePrefix is at most n bytes of s, cut on a rune boundary.
func RunePrefix(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// RuneSuffix is at most n bytes of the end of s, starting on a rune boundary.
func RuneSuffix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
