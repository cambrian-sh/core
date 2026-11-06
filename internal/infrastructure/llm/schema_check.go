package llm

// Exported entry points to the ADR-0139 C1 response-schema validator, for
// callers outside the Provider layer that hold a final answer and its schema —
// the D12 subagent result (internal/substrate/network/subagent.go), whose
// output_schema has "the same shape and strict semantics as
// GenerateStreamRequest.response_schema". One validator, one compile cache.

// CompileResponseSchema reports whether raw is a JSON Schema the validator
// accepts.
func CompileResponseSchema(raw []byte) error {
	_, err := compileSchema(raw)
	return err
}

// ValidateJSONReply checks text against schema and returns the normalised JSON
// (surrounding whitespace and one code fence stripped, nothing hunted for).
func ValidateJSONReply(schema []byte, text string) (string, error) {
	return validateReply(schema, text)
}
