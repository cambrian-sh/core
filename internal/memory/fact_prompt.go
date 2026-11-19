package memory

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// ADR-0136 D4, as amended by A2: the fact-extraction prompt and its strict parser.
//
// SOURCE-AGNOSTIC (A2.2; decisions/ingestion-prompts-are-source-agnostic.md). The fixed
// text assumes no source shape: no source kind is named, the first person is resolved
// from data (a label the passage gives, else the source author), and the only example
// is a placeholder schema. Everything that differs per source arrives as DATA in the
// Source: block (domain.SourceMeta, from the ingest metadata).
// TestFactPrompt_SourceAgnostic holds the fixed text to that.
//
// PROMPT SAFETY. The prompt carries the source's own passage texts, its metadata block
// and the declared predicate vocabulary — nothing else: no other source, no tags, no
// principal beyond the recorded author. The text is DATA; the rules say so, and the
// parser never executes anything the model returns — it validates a closed JSON shape
// and drops every fact that does not cite a verbatim quote of the passage it names. A
// model talked into inventing facts by injected text can therefore only produce facts
// quoting that text, which point back at the very chunk that said it.

// factExtractPromptVersion identifies the extraction prompt; it is the first half of
// the extractor version on every extraction marker (A2.4: "<prompt id>/<generator
// model>"). It is derived from the library prompts' static hashes
// (prompts/fact_extract + prompts/fact_extract_input, ADR-0139 C4), so editing either
// file changes it and a stored corpus reads as pending for facts-backfill. Bump it
// by hand (a new prompt version) only when the passage splitter or the window packing
// changes.
//
// History: fact-extract-v3 (ADR-0136 A3.4, speaker-at-start markers) was the inline
// prompt this replaced; its rules are unchanged, the Source block, the preferred
// attribute names and the passages moved into tagged data in the user message, and
// the reply is a C1 response schema.
var factExtractPromptVersion = factExtractPrompts.version

// factExtractionInput is one extraction call: one window of one source.
type factExtractionInput struct {
	Source domain.SourceMeta
	// Passages are this window's passages, in source order; the prompt numbers them
	// from 1 and the model's "passage" field refers to that numbering.
	Passages []string
	// Speakers parallels Passages: the speaker at the start of each passage, from the
	// chunk's A3.3 speaker_at_start metadata; "" (or a short slice) = none known. It is
	// rendered beside the passage as data, never inside the text the quotes validate
	// against.
	Speakers []string
	// CanonicalPredicates are the declared predicate names (execution.facts.
	// single_attributes / multi_attributes), offered as preferred names so the same
	// attribute keys the same chain (A1.9 naming drift).
	CanonicalPredicates []string
	// Section and Sections place this window in the source (1-based); Sections <= 1
	// renders no section line.
	Section, Sections int
}

const (
	// maxFactPassageChars bounds one passage. A2.3: a longer chunk is SPLIT into
	// sub-passages (splitFactPassage), never cut, so this bound only guards the prompt
	// and the parser against a caller that bypassed the splitter.
	maxFactPassageChars = 6000
	maxFactFieldChars   = 300
	maxFactQuoteChars   = 400
	// factSectionReserve is the room the packer keeps for the section line, whose
	// numbers are known only after packing.
	factSectionReserve = 96
)

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// factSourceBlock renders A2.1's metadata, the content of the <source> region. Values
// are data: each is folded onto one line and clipped, and a closing tag inside one is
// neutralised, so a title cannot add a rule line or end the region.
func factSourceBlock(m domain.SourceMeta) string {
	line := func(s string) string {
		s = clipField(orUnknown(strings.Join(strings.Fields(s), " ")), maxFactFieldChars)
		return prompts.NeutraliseClosingTags(s, factDataTags...)
	}
	ref := "unknown"
	if !m.ReferenceTime.IsZero() {
		ref = m.ReferenceTime.UTC().Format(time.RFC3339)
	}
	qual := "basis: " + orUnknown(string(m.ReferenceBasis))
	if m.TimePrecision != "" {
		qual += ", precision: " + string(m.TimePrecision)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "- type: %s\n", line(m.SourceType))
	fmt.Fprintf(&sb, "- title: %s\n", line(m.Title))
	fmt.Fprintf(&sb, "- author: %s\n", line(m.Author))
	fmt.Fprintf(&sb, "- reference time: %s (%s)", ref, qual)
	return sb.String()
}

// factDataTags are the data regions of fact_extract_input; a closing tag of any of
// them inside a value is neutralised before rendering (prompts SPEC.md §3.1).
var factDataTags = []string{"source", "preferred_attributes", "passages"}

// factSourceRegion is the <source> content of one window: the metadata block plus, for
// a multi-window source, the section line.
func factSourceRegion(in factExtractionInput) string {
	s := factSourceBlock(in.Source)
	if in.Sections > 1 {
		s += fmt.Sprintf("\n- section: %d of %d (the passages below are one consecutive part of the source)", in.Section, in.Sections)
	}
	return s
}

// buildFactRequest renders one extraction call over in.Passages — all of them: the
// window packer (packFactWindows) already sized them to the budget. The rules are the
// cached system block; the window is the user message, in tagged data regions
// (ADR-0139 D2); the reply shape is the response schema (C1).
//
// The passages are the ingested document's text (an upload, a webhook, a poller body):
// the whole numbered list goes inside ONE ADR-0063 payload fence marked
// provenance="ingested_document" (ENG-94), so the fence tells the model it is data, and
// neither a forged fence line nor a fake </passages> can end the region (the list is
// one JSON-encoded, HTML-escaped line between nonce lines). One fence per window, not
// per passage: a conversation's short turns would otherwise pay the fence header each
// and need more windows (more calls) for the same source. A quote the model copies
// from the encoded line decodes back to the original text when its JSON reply is
// parsed, so the verbatim quote check still holds. The prompt files are unchanged, so
// the extractor version is too: a stored corpus is not re-extracted for this.
func buildFactRequest(in factExtractionInput) domain.GenerateRequest {
	var passages strings.Builder
	for i, p := range in.Passages {
		passages.WriteString(factPassageEntry(i, p, speakerAt(in.Speakers, i)))
	}
	return factExtractPrompts.mustRequest(map[string]string{
		"source":               factSourceRegion(in),
		"preferred_attributes": strings.Join(in.CanonicalPredicates, ", "),
		"passages": domain.FenceUntrusted(domain.DataProvenance{Class: domain.ProvenanceIngestedDocument, Source: domain.SourceIngested},
			strings.TrimRight(passages.String(), "\n")),
	})
}

// factEntryChars is what one passage entry adds to a rendered request: its length once
// JSON-encoded inside the window's fence (the encoding is per character, so entry
// sizes add up).
func factEntryChars(i int, p, speaker string) int {
	return domain.PayloadFenceEncodedLen(factPassageEntry(i, p, speaker))
}

// factWindowOverhead is the size of a window's request with no passage: the system
// block, the schema, the message frame, the source block and the (empty) passages
// fence, plus room for the section line, whose numbers are known only after packing.
func factWindowOverhead(src domain.SourceMeta, canonical []string) int {
	return requestChars(buildFactRequest(factExtractionInput{Source: src, CanonicalPredicates: canonical})) + factSectionReserve
}

// factPassageEntry renders one numbered passage. A known speaker at its start (A3.4)
// is rendered as a marker BEFORE the text — data, folded onto one line and clipped like
// a Source value, so a label cannot add a line of its own. The entry is fenced with the
// rest of the window (buildFactRequest).
func factPassageEntry(i int, p, speaker string) string {
	if len(p) > maxFactPassageChars {
		p = p[:maxFactPassageChars]
	}
	if speaker = clipField(strings.Join(strings.Fields(speaker), " "), maxFactFieldChars); speaker != "" {
		return fmt.Sprintf("[%d] (speaker at start: %s) %s\n", i+1, speaker, p)
	}
	return fmt.Sprintf("[%d] %s\n", i+1, p)
}

// speakerAt is speakers[i], or "" past its end.
func speakerAt(speakers []string, i int) string {
	if i < len(speakers) {
		return speakers[i]
	}
	return ""
}

// factWindow is a half-open range [Start, End) of a source's passages sent in one call.
type factWindow struct{ Start, End int }

// packFactWindows packs passages IN ORDER into windows whose rendered request stays
// within budget characters (A2.3). Every window holds at least one passage, so a
// passage larger than the budget still gets a call of its own. budget <= 0 = one window.
// speakers parallels passages (the A3.4 marker counts toward the budget).
func packFactWindows(passages, speakers []string, src domain.SourceMeta, canonical []string, budget int) []factWindow {
	if len(passages) == 0 {
		return nil
	}
	if budget <= 0 {
		return []factWindow{{0, len(passages)}}
	}
	head := factWindowOverhead(src, canonical)
	var out []factWindow
	start, size := 0, head
	for i, p := range passages {
		n := factEntryChars(i-start, p, speakerAt(speakers, i))
		if i > start && size+n > budget {
			out = append(out, factWindow{start, i})
			start, size = i, head
			n = factEntryChars(0, p, speakerAt(speakers, i))
		}
		size += n
	}
	return append(out, factWindow{start, len(passages)})
}

// factPassage is one extractor passage — a chunk's text, or one sub-passage of a chunk
// longer than maxFactPassageChars — with the index of its chunk and the speaker known to
// be speaking at its start ("" = none known).
type factPassage struct {
	Chunk   int
	Text    string
	Speaker string
}

// factPassages splits the chunks into the extractor's passages, in order. The split is
// deterministic, so a resumed run counts the same passages (A2.4 coverage).
//
// A3.4 speakers come from the STORED chunk metadata (domain.MetaSpeakerAtStart /
// MetaSpeakers), which live ingest and facts-backfill both hand over, so both render
// the same input. The chunk's first passage takes its speaker at start. A later
// sub-passage of a split chunk starts at an offset the chunk metadata does not label,
// so it is labelled only when the whole chunk has a single speaker; otherwise it is
// left unlabelled (unknown is stated as unknown, never guessed).
func factPassages(chunks []*domain.Document) []factPassage {
	var out []factPassage
	for i, c := range chunks {
		atStart, speakers := chunkSpeakerMeta(c.Metadata)
		for k, t := range splitFactPassage(c.Text, maxFactPassageChars) {
			sp := atStart
			if k > 0 && !(len(speakers) == 1 && speakers[0] == atStart) {
				sp = ""
			}
			out = append(out, factPassage{Chunk: i, Text: t, Speaker: sp})
		}
	}
	return out
}

// chunkSpeakerMeta reads a chunk's A3.3 speaker labels from its metadata, as written at
// ingest ([]string) or as read back from JSONB ([]any).
func chunkSpeakerMeta(meta map[string]any) (atStart string, speakers []string) {
	atStart, _ = meta[domain.MetaSpeakerAtStart].(string)
	switch v := meta[domain.MetaSpeakers].(type) {
	case []string:
		speakers = v
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				speakers = append(speakers, s)
			}
		}
	}
	return strings.TrimSpace(atStart), speakers
}

// splitFactPassage splits text longer than max into consecutive pieces of at most max
// bytes, each ending at the last sentence boundary in its second half (".", "!" or "?"
// followed by whitespace, or a line break), else at the last whitespace, else at a
// UTF-8 rune boundary. Pieces are trimmed spans of the original text, so a quote of one
// piece is a quote of the chunk. Empty text yields no piece.
func splitFactPassage(text string, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if max <= 0 || len(text) <= max {
		return []string{text}
	}
	var out []string
	for len(text) > max {
		cut := factCutPoint(text, max)
		if piece := strings.TrimSpace(text[:cut]); piece != "" {
			out = append(out, piece)
		}
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// factCutPoint is where splitFactPassage cuts text (len(text) > max): 0 < cut <= max.
func factCutPoint(text string, max int) int {
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
	for i := max - 1; i >= max/2; i-- {
		switch text[i] {
		case '\n':
			return i + 1
		case '.', '!', '?':
			if isSpace(text[i+1]) {
				return i + 1
			}
		}
	}
	for i := max; i > 0; i-- {
		if isSpace(text[i]) {
			return i
		}
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut == 0 {
		return max
	}
	return cut
}

// rawExtractedFact is the closed JSON shape the model must return.
type rawExtractedFact struct {
	Subject   string  `json:"subject"`
	Attribute string  `json:"attribute"`
	Value     any     `json:"value"`
	ValidFrom *string `json:"valid_from"`
	ValidTo   *string `json:"valid_to"`
	Polarity  string  `json:"polarity"`
	Passage   int     `json:"passage"`
	Quote     string  `json:"quote"`
	// SupersedesPrior accepts a JSON bool or the strings "true"/"false" (A1.2 rule 1).
	SupersedesPrior any    `json:"supersedes_prior"`
	Speaker         string `json:"speaker"`
}

// extractedFact is one validated fact, positioned on its passage (0-based).
type extractedFact struct {
	Subject, Attribute, Value string
	ValidFrom, ValidTo        *time.Time
	Polarity                  domain.FactPolarity
	Passage                   int
	Quote                     string
	SupersedesPrior           bool
	Speaker                   string
}

// factParseStats says what the parser refused, by reason.
type factParseStats struct {
	Returned      int
	Kept          int
	BadShape      int // missing subject/attribute/value, or out-of-range passage
	QuoteNotFound int // the quote is not a verbatim span of the passage
	BadDate       int // an unparseable date (the fact is kept, the date dropped)
	Duplicate     int
	OverCap       int
}

// parseFactReply validates a C1 reply. A reply the provider layer checked against the
// response schema (and repaired once) is decoded as the JSON it is; only a reply that
// bypassed that check (resp.SchemaUnchecked: a generator that ignores response_schema,
// or the legacy string port) is read tolerantly (decodeKeyedJSONObject).
func parseFactReply(resp domain.GenerateResponse, passages []string, maxFacts int) ([]extractedFact, factParseStats, error) {
	return parseFacts(resp.Text, resp.SchemaUnchecked(), passages, maxFacts)
}

// parseFactResponse is the tolerant read of a text answer: strict on shape and
// grounding and lenient only on wrapping — a code fence or prose around the one JSON
// object, or after it, is ignored (decodeKeyedJSONObject).
func parseFactResponse(resp string, passages []string, maxFacts int) ([]extractedFact, factParseStats, error) {
	return parseFacts(resp, true, passages, maxFacts)
}

// replyField returns the value of key in the reply's top-level JSON object: exactly
// that object when the reply is schema-checked, the first decodable object holding key
// when it is not (tolerant).
func replyField(text, key string, tolerant bool) (json.RawMessage, error) {
	if tolerant {
		return decodeKeyedJSONObject(text, key)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return nil, fmt.Errorf("fact extraction: response is not the expected JSON: %w (%w)", err, errFactResponseUnparseable)
	}
	v, ok := obj[key]
	if !ok {
		return nil, errFactResponseNotJSON
	}
	return v, nil
}

// parseFacts validates the model's answer against the passages it was given: grounding
// (a verbatim quote of the passage it names) is checked here whatever the wrapping.
func parseFacts(resp string, tolerant bool, passages []string, maxFacts int) ([]extractedFact, factParseStats, error) {
	var st factParseStats
	raw, err := replyField(resp, "facts", tolerant)
	if err != nil {
		return nil, st, err
	}
	var payload struct {
		Facts []rawExtractedFact
	}
	if err := json.Unmarshal(raw, &payload.Facts); err != nil {
		return nil, st, fmt.Errorf("fact extraction: response is not the expected JSON: %w (%w)", err, errFactResponseUnparseable)
	}
	st.Returned = len(payload.Facts)
	normPassages := make([]string, len(passages))
	for i, p := range passages {
		if len(p) > maxFactPassageChars {
			p = p[:maxFactPassageChars]
		}
		normPassages[i] = normalizeQuoteText(p)
	}
	seen := map[string]bool{}
	var out []extractedFact
	for _, r := range payload.Facts {
		subj := clipField(r.Subject, maxFactFieldChars)
		attr := clipField(r.Attribute, maxFactFieldChars)
		val := clipField(valueString(r.Value), maxFactFieldChars)
		idx := r.Passage - 1
		if domain.NormalizeFactText(subj) == "" || domain.NormalizeFactAttribute(attr) == "" ||
			domain.NormalizeFactText(val) == "" || idx < 0 || idx >= len(passages) {
			st.BadShape++
			continue
		}
		quote := clipField(r.Quote, maxFactQuoteChars)
		if nq := normalizeQuoteText(quote); nq == "" || !strings.Contains(normPassages[idx], nq) {
			st.QuoteNotFound++
			continue
		}
		from, okFrom := parseFactDate(r.ValidFrom)
		to, okTo := parseFactDate(r.ValidTo)
		if !okFrom || !okTo {
			st.BadDate++
		}
		if from != nil && to != nil && !to.After(*from) {
			to = nil
		}
		pol := domain.FactPositive
		if strings.EqualFold(strings.TrimSpace(r.Polarity), string(domain.FactNegative)) {
			pol = domain.FactNegative
		}
		key := fmt.Sprintf("%d|%s|%s|%s", idx, domain.NormalizeFactText(subj), domain.NormalizeFactAttribute(attr), domain.NormalizeFactText(val))
		if seen[key] {
			st.Duplicate++
			continue
		}
		if maxFacts > 0 && len(out) >= maxFacts {
			st.OverCap++
			continue
		}
		seen[key] = true
		out = append(out, extractedFact{
			Subject: subj, Attribute: attr, Value: val,
			ValidFrom: from, ValidTo: to, Polarity: pol, Passage: idx, Quote: quote,
			SupersedesPrior: truthy(r.SupersedesPrior), Speaker: clipField(r.Speaker, 64),
		})
	}
	st.Kept = len(out)
	return out, st, nil
}

func clipField(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// valueString accepts a JSON string, number or bool as a value (models emit counts as
// numbers: "planted": 12).
func valueString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

// normalizeQuoteText is the grounding comparison form: lower case, whitespace
// collapsed, typographic quotes and dashes folded, so a model's harmless re-spacing does
// not reject a real quote while a paraphrase still does.
func normalizeQuoteText(s string) string {
	r := strings.NewReplacer("’", "'", "‘", "'", "“", "\"", "”", "\"", "–", "-", "—", "-")
	return strings.Join(strings.Fields(strings.ToLower(r.Replace(s))), " ")
}

// parseFactDate reads the date forms the prompt asks for. null/""/"unknown" ⇒ (nil,
// true). An unparseable or implausible date ⇒ (nil, false): the fact keeps no date
// rather than a wrong one.
func parseFactDate(s *string) (*time.Time, bool) {
	if s == nil {
		return nil, true
	}
	v := strings.TrimSpace(*s)
	switch strings.ToLower(v) {
	case "", "null", "none", "unknown", "n/a":
		return nil, true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, v); err == nil {
			if t.Year() < 1800 || t.Year() > 2300 {
				return nil, false
			}
			u := t.UTC()
			return &u, true
		}
	}
	return nil, false
}

// truthy reads a JSON bool or a "true"/"false" string; anything else is false.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	}
	return false
}

// buildAssessmentRequest renders A1.2 rule 3's one batched judge call per session: for
// each new fact, its candidates (same-key items of an unknown-cardinality predicate and
// same-subject items under other predicates); the model gives ONE verdict per pair from
// a closed vocabulary. Labels (N1, C1.2) stand in for ids, and the reply names each pair
// by them, so no answer is read by position. Dates are shown but the model is told it
// does not decide direction — the dates do (the valid_time_supersession authority). The
// rendering is deterministic for a given (sorted) item list, because its hash is part of
// the assessment's keyed id. The rules are the cached system block, the facts tagged
// data in the user message (ADR-0139 D2).
func buildAssessmentRequest(items []contradictionItem) domain.GenerateRequest {
	var sb strings.Builder
	for i, it := range items {
		fmt.Fprintf(&sb, "N%d: %s\n", i+1, describeFact(it.New))
		for j, c := range it.Candidates {
			fmt.Fprintf(&sb, "  C%d.%d: %s\n", i+1, j+1, describeFact(c))
		}
	}
	return factAssessPrompts.mustRequest(map[string]string{
		"facts": strings.TrimRight(sb.String(), "\n"),
	})
}

type contradictionItem struct {
	New        domain.Fact
	Candidates []domain.Fact
}

func describeFact(f domain.Fact) string {
	s := prompts.NeutraliseClosingTags(f.Subject+" | "+f.Attribute+" | "+f.Value, "facts")
	if f.ValidFrom != nil {
		s += " (from " + f.ValidFrom.UTC().Format("2006-01-02") + ")"
	}
	if f.Polarity == domain.FactNegative {
		s += " [negated]"
	}
	return s
}

var assessmentVerdicts = map[string]string{
	domain.VerdictContradictory: domain.VerdictContradictory,
	domain.VerdictConsistent:    domain.VerdictConsistent,
	domain.VerdictSamePredicate: domain.VerdictSamePredicate,
	domain.VerdictUncertain:     domain.VerdictUncertain,
	domain.VerdictRefused:       domain.VerdictRefused,
}

// assessmentReplyVerdict is one entry of the judge's reply (prompts/fact_assess/
// verdicts.schema.json): a pair named by its labels, never by position.
type assessmentReplyVerdict struct {
	New       string `json:"new"`
	Candidate string `json:"candidate"`
	Verdict   string `json:"verdict"`
}

// parseAssessmentReply reads a C1 reply: decoded as is when schema-checked, tolerantly
// otherwise (see parseFactReply).
func parseAssessmentReply(resp domain.GenerateResponse, items []contradictionItem) ([]domain.AssessmentVerdict, error) {
	return parseAssessment(resp.Text, resp.SchemaUnchecked(), items)
}

// parseAssessmentResponse is the tolerant read of a text answer.
func parseAssessmentResponse(resp string, items []contradictionItem) ([]domain.AssessmentVerdict, error) {
	return parseAssessment(resp, true, items)
}

// parseAssessment maps the model's labels back to item-id pairs with a verdict
// from the closed vocabulary (an unknown word reads as uncertain). Unknown labels are
// ignored; a pair the model left out has no verdict (the authority then decides it by
// cardinality and cues alone).
func parseAssessment(resp string, tolerant bool, items []contradictionItem) ([]domain.AssessmentVerdict, error) {
	raw, err := replyField(resp, "verdicts", tolerant)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Verdicts []assessmentReplyVerdict
	}
	if err := json.Unmarshal(raw, &payload.Verdicts); err != nil {
		return nil, fmt.Errorf("fact assessment: response is not the expected JSON: %w (%w)", err, errFactResponseUnparseable)
	}
	var out []domain.AssessmentVerdict
	seen := map[[2]int]bool{}
	for _, v := range payload.Verdicts {
		var ni, a, b int
		if _, err := fmt.Sscanf(strings.TrimSpace(v.New), "N%d", &ni); err != nil || ni < 1 || ni > len(items) {
			continue
		}
		if _, err := fmt.Sscanf(strings.TrimSpace(v.Candidate), "C%d.%d", &a, &b); err != nil || a != ni || b < 1 || b > len(items[ni-1].Candidates) {
			continue
		}
		p := [2]int{ni - 1, b - 1}
		if seen[p] {
			continue
		}
		seen[p] = true
		verdict, ok := assessmentVerdicts[strings.ToLower(strings.TrimSpace(v.Verdict))]
		if !ok {
			verdict = domain.VerdictUncertain
		}
		it := items[ni-1]
		out = append(out, domain.AssessmentVerdict{
			A: domain.KnowledgeItemID(it.New.ID), B: domain.KnowledgeItemID(it.Candidates[b-1].ID), Verdict: verdict,
		})
	}
	return out, nil
}

// refusedVerdicts is the stored answer of a judge whose output could not be read: every
// pair refused, so a replay agrees with it instead of asking again.
func refusedVerdicts(items []contradictionItem) []domain.AssessmentVerdict {
	var out []domain.AssessmentVerdict
	for _, it := range items {
		for _, c := range it.Candidates {
			out = append(out, domain.AssessmentVerdict{A: domain.KnowledgeItemID(it.New.ID), B: domain.KnowledgeItemID(c.ID), Verdict: domain.VerdictRefused})
		}
	}
	return out
}
