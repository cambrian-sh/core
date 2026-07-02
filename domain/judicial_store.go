package domain

import "context"

// JudicialStore saves verifier critiques as Judicial Records in the vector store.
type JudicialStore interface {
	Save(ctx context.Context, text string, embedding []float32, metadata map[string]any) error
}
