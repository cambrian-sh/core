package network

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A structural guard, in the spirit of check-no-premium.sh: some rules are cheaper to
// enforce over the source than to re-derive from behaviour.
//
// The defect: both lease-minting paths in server.go set
// StepAllocation.Winner = "llm:ollama:qwen3:8b" whenever an Ollama router was present,
// on the stated reason that StreamChunks needed a non-empty Winner. It did not — the
// gateway's configured-default fallback (SetDefaultModelID, covered by
// gateway_default_model_test.go) had covered that case since it was added. So the
// literal did not fill a gap; it OVERRODE the operator's configured default with a
// model named in Go, on every Ollama-equipped deployment. "Which model runs my agents"
// was unanswerable from configuration, and the bypass control arm silently streamed
// through a different model than the arm it is the control for.
//
// Model choice belongs to configuration. A literal here is how it stops belonging there.
func TestServerMintsNoHardcodedModelID(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	// Match a model id literal, not the word in prose: the explanatory comments above
	// each call site deliberately NAME the removed literal, and a guard that fires on
	// its own rationale would force the reason to be deleted to stay green.
	lit := regexp.MustCompile(`AgentDefinition\{ID:\s*"llm:`)
	if loc := lit.FindIndex(src); loc != nil {
		line := 1 + strings.Count(string(src[:loc[0]]), "\n")
		t.Errorf("server.go:%d allocates a model by hardcoded id. Leave StepAllocation "+
			"empty and let the gateway resolve the configured default "+
			"(SetDefaultModelID), or bind the Dispatcher's computed StepAllocation — "+
			"see the Known Gaps entry on per-step model allocation.", line)
	}
}

// Asserts the premise, so the guard above cannot pass because the pattern is wrong.
func TestNoHardcodedModelGuardActuallyMatches(t *testing.T) {
	lit := regexp.MustCompile(`AgentDefinition\{ID:\s*"llm:`)
	sample := `sa.Winner = domain.AgentDefinition{ID: "llm:ollama:qwen3:8b"}`
	if !lit.MatchString(sample) {
		t.Fatal("the guard pattern no longer matches the exact literal it exists to catch")
	}
	if lit.MatchString(`// the hardcoded llm:ollama:qwen3:8b removed here`) {
		t.Fatal("the guard fires on prose; it would force the rationale comments to be deleted")
	}
}
