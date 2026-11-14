package memory

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

type captureSaveStore struct {
	fakeVectorStore
	savedDoc *domain.Document
	saveErr  error
}

func (c *captureSaveStore) Save(_ context.Context, doc *domain.Document) error {
	c.savedDoc = doc
	return c.saveErr
}

func newTestAgent(store domain.VectorStore) *Agent {
	mgr := NewMemoryManager(store, &fakeEmbedder{})
	return NewAgent(mgr, 3)
}

var errStoreFailed = func() error { return &storeFailedError{} }()

type storeFailedError struct{}

func (e *storeFailedError) Error() string { return "store write failed" }
