package evidence

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Test-only exports for the external evidence_test package, which exists so a
// test can drive the consumer over the REAL Postgres store: package postgres
// imports internal/memory, which imports this package, so an internal test
// importing postgres is an import cycle.

// DrainUntilIdle runs one wake-up of the consumer's loop.
func DrainUntilIdle(c *Consumer, ctx context.Context) { c.drainUntilIdle(ctx) }

// NewTestBlobs is the in-memory content store the internal tests use.
func NewTestBlobs() domain.ContentStore { return newFakeBlobs() }

// ExactJitter makes the outbox backoff its upper bound for the test.
func ExactJitter(t *testing.T) { exactJitter(t) }
