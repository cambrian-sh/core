package llm_test

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"
)

func TestStreamChunk_New(t *testing.T) {
	chunk := domain.StreamChunk{
		Text:             "hello",
		IsFinal:          false,
		UsageTotalTokens: 0,
	}
	if chunk.Text != "hello" {
		t.Errorf("Text = %q, want %q", chunk.Text, "hello")
	}
	if chunk.IsFinal {
		t.Error("IsFinal = true, want false")
	}
	if chunk.UsageTotalTokens != 0 {
		t.Errorf("UsageTotalTokens = %d, want 0", chunk.UsageTotalTokens)
	}
}

func TestStreamChunk_Final(t *testing.T) {
	chunk := domain.StreamChunk{
		Text:                  "world",
		IsFinal:               true,
		UsagePromptTokens:     50,
		UsageCompletionTokens: 25,
		UsageTotalTokens:      75,
	}
	if !chunk.IsFinal {
		t.Error("IsFinal = false, want true")
	}
	if chunk.UsageTotalTokens != 75 {
		t.Errorf("UsageTotalTokens = %d, want 75", chunk.UsageTotalTokens)
	}
}

func TestReconcileTokens_WithUsageField(t *testing.T) {
	finalChunk := domain.StreamChunk{
		Text:                  "final text with usage",
		IsFinal:               true,
		UsagePromptTokens:     50,
		UsageCompletionTokens: 25,
		UsageTotalTokens:      75,
	}
	result := llm.ReconcileTokens(30, finalChunk, "full response text here")
	if result.PromptTokens != 50 {
		t.Errorf("PromptTokens = %d, want 50", result.PromptTokens)
	}
	if result.CompletionTokens != 25 {
		t.Errorf("CompletionTokens = %d, want 25", result.CompletionTokens)
	}
	if result.TotalTokens != 75 {
		t.Errorf("TotalTokens = %d, want 75", result.TotalTokens)
	}
}

func TestReconcileTokens_FallbackOnMissingUsage(t *testing.T) {
	finalChunk := domain.StreamChunk{
		Text:    "final text without usage field",
		IsFinal: true,
		// usage fields zero (provider omitted usage, e.g. Ollama streaming)
	}
	result := llm.ReconcileTokens(10, finalChunk, "full response text — quite long")
	if result.TotalTokens <= 0 {
		t.Errorf("TotalTokens = %d, want > 0 (estimated from fullText)", result.TotalTokens)
	}
}

func TestReconcileTokens_BudgetOverrun(t *testing.T) {
	finalChunk := domain.StreamChunk{
		IsFinal:               true,
		UsageTotalTokens:      115,
		UsagePromptTokens:     30,
		UsageCompletionTokens: 85,
	}
	result := llm.ReconcileTokens(90, finalChunk, "some text")
	overrun := result.TotalTokens > 100 // simulate budget limit of 100
	if !overrun {
		t.Errorf("TotalTokens = %d, want > 100 for BudgetOverrun=true", result.TotalTokens)
	}
}

func TestTokenizerInaccuracyCounter_InitialZero(t *testing.T) {
	llm.ResetTokenizerInaccuracyCounter()
	rate := llm.CheckTokenizerInaccuracy()
	if rate != 0 {
		t.Errorf("initial inaccuracy rate = %f, want 0", rate)
	}
}

func TestTokenizerInaccuracyCounter_RecordAndCheck(t *testing.T) {
	llm.ResetTokenizerInaccuracyCounter()

	llm.RecordTokenCall(false) // 0: no overrun
	llm.RecordTokenCall(false)
	llm.RecordTokenCall(false)
	llm.RecordTokenCall(true) // 1: overrun — 25%

	rate := llm.CheckTokenizerInaccuracy()
	if rate <= 0 || rate > 0.3 {
		t.Errorf("inaccuracy rate = %f, want > 0 and <= 0.3 (1 out of 4)", rate)
	}
}
