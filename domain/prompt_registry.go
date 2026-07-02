package domain

// PromptEntry records a single versioned prompt in the compile-time catalog.
// Subsystem packages register their entries via init() functions so no
// circular imports are needed.
type PromptEntry struct {
	ID      string // human-readable slug: "planner.plan", "memory.tier2_score"
	Version string // semver of the last commit that changed the prompt text
	Hash    string // PromptHashOf(static text) — written to audit records
	Schema  string // JSON Schema or enum/string/integer contract declaration
}

// PromptRegistry is the compile-time catalog of every registered LLM prompt.
// It is populated by init() functions in subsystem packages.
// Key: Hash (8-char SHA-256 prefix). Value: entry metadata.
//
// Usage — subsystem init:
//
//	func init() {
//	    domain.PromptRegistry[myPromptHash] = domain.PromptEntry{
//	        ID: "subsystem.prompt_name", Version: "1.0.0",
//	        Hash: myPromptHash, Schema: myOutputSchema,
//	    }
//	}
//
// Usage — writing to an audit record:
//
//	planEvent.PlannerPromptVersion = domain.PromptRegistry[domain.PromptHashOf(staticText)].Hash
var PromptRegistry = map[string]PromptEntry{}
