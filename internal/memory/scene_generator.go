package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

const sceneGenMaxTokensPerDoc = 80
const sceneGenTokenThreshold = 6000

// SceneGenerator generates episodic scene descriptions for batches of
// ExternalDocument using a batched LLM call (ADR-0028).
type SceneGenerator struct {
	gen domain.Generator
}

// NewSceneGenerator constructs a SceneGenerator backed by the given LLM generator.
func NewSceneGenerator(gen domain.Generator) *SceneGenerator {
	return &SceneGenerator{gen: gen}
}

// Generate produces one scene string per document in batch.
// If the LLM call fails or returns malformed JSON, deterministic fallback
// placeholder strings are returned so ingestion never blocks on LLM availability.
func (sg *SceneGenerator) Generate(ctx context.Context, batch []domain.ExternalDocument) ([]string, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	// Dynamic split: if estimated prompt exceeds token threshold, halve the batch.
	if len(batch)*sceneGenMaxTokensPerDoc > sceneGenTokenThreshold {
		mid := len(batch) / 2
		left, _ := sg.Generate(ctx, batch[:mid])
		right, _ := sg.Generate(ctx, batch[mid:])
		return append(left, right...), nil
	}

	prompt := sg.buildPrompt(batch)
	resp, err := sg.gen.Generate(ctx, prompt)
	if err != nil {
		slog.Warn("SceneGenerator: LLM call failed, using fallback placeholders", "err", err)
		return sg.fallbacks(batch), nil
	}

	var scenes []string
	if jsonErr := json.Unmarshal([]byte(resp), &scenes); jsonErr != nil || len(scenes) != len(batch) {
		slog.Warn("SceneGenerator: malformed LLM response, using fallback placeholders", "err", jsonErr)
		return sg.fallbacks(batch), nil
	}

	return scenes, nil
}

// buildPrompt renders the scene prompt. Its fixed text is source-agnostic (ADR-0136 A2.5;
// decisions/ingestion-prompts-are-source-agnostic.md): it names no source kind and asks
// for no source-specific dimension; every per-source detail — type, title, author, time
// and what that time is (its reference basis) — arrives as data in the context block.
// TestScenePrompt_SourceAgnostic holds the fixed text to that.
func (sg *SceneGenerator) buildPrompt(batch []domain.ExternalDocument) string {
	var contextBlocks string
	for i, doc := range batch {
		contextBlocks += fmt.Sprintf(
			"<Source index=%d>\n  <SourceType>%s</SourceType>\n  <Title>%s</Title>\n  <Author>%s</Author>\n  <Time basis=%q>%s</Time>\n  <BodyPreview>%s</BodyPreview>\n</Source>\n",
			i, doc.SourceType, doc.Title, doc.Author,
			string(domain.ReferenceBasisOf(doc.TimeSource, doc.Timestamp)),
			doc.Timestamp.Format("2006-01-02T15:04:05Z"),
			buildBodyPreview(doc.Body, previewMaxChars),
		)
	}
	schema := fmt.Sprintf(`{"type":"array","items":{"type":"string","maxLength":300},"minItems":%d,"maxItems":%d}`,
		len(batch), len(batch))

	return domain.PromptBuild(
		domain.PromptSystem(
			"You are Cambrian's episodic context extractor.",
			"Generate a concise SCENE description of the context the information appeared in: who produced it or is involved, where and when it originated, and why it exists, as far as the metadata and the preview show.",
			"Max 2 sentences. No explanation. No bullet points. Do not invent context the metadata and the preview do not support.",
			"SCENE answers: 'In what context did this information appear?' not 'What does it say?'.",
			fmt.Sprintf("Return a JSON array of exactly %d scene strings, one per source, in the order given.", len(batch)),
		),
		domain.PromptContext(contextBlocks),
		domain.PromptTask("Generate one SCENE per source in the order given."),
		domain.PromptOutputSchemaJSON(schema),
	)
}

func (sg *SceneGenerator) fallbacks(batch []domain.ExternalDocument) []string {
	out := make([]string, len(batch))
	for i, doc := range batch {
		out[i] = fmt.Sprintf(
			"External %s document '%s' from %s by %s, received %s (scene generation pending)",
			doc.SourceType, doc.Title, doc.SourceURI, doc.Author,
			doc.Timestamp.Format("2006-01-02"),
		)
	}
	return out
}
