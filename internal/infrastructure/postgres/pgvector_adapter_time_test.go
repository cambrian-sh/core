package postgres

import (
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// TestCreatedAtDefaultNow verifies REQ-DATA-1: when a Document is saved with a
// zero-value CreatedAt, PostgreSQL's DEFAULT NOW() fires and the stored
// timestamp is recent. When CreatedAt is explicitly set, the explicit value is
// preserved.
func TestCreatedAtDefaultNow(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := t.Context()

	// Case 1: zero-value CreatedAt — DEFAULT NOW() must fire.
	zeroDoc := &domain.Document{
		ID:           "test-created-at-zero",
		Text:         "zero created_at test",
		DocumentType: domain.DocTypeMnemonicFact,
		Metadata:     map[string]interface{}{"test": true},
	}
	if err := adapter.Save(ctx, zeroDoc); err != nil {
		t.Fatalf("Save with zero CreatedAt failed: %v", err)
	}

	got, err := adapter.GetByID(ctx, zeroDoc.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID after zero-CreatedAt save: got=%v err=%v", got, err)
	}
	delta := time.Since(got.CreatedAt)
	if delta < 0 || delta > 5*time.Second {
		t.Errorf("expected DEFAULT NOW() to fire; got CreatedAt=%v (delta=%v)", got.CreatedAt, delta)
	}

	// Case 2: explicit CreatedAt — must be preserved.
	explicit := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	explicitDoc := &domain.Document{
		ID:           "test-created-at-explicit",
		Text:         "explicit created_at test",
		DocumentType: domain.DocTypeMnemonicFact,
		CreatedAt:    explicit,
		Metadata:     map[string]interface{}{"test": true},
	}
	if err := adapter.Save(ctx, explicitDoc); err != nil {
		t.Fatalf("Save with explicit CreatedAt failed: %v", err)
	}

	got2, err := adapter.GetByID(ctx, explicitDoc.ID)
	if err != nil || got2 == nil {
		t.Fatalf("GetByID after explicit-CreatedAt save: got=%v err=%v", got2, err)
	}
	// Allow 1 second tolerance for DB timestamp precision.
	if diff := got2.CreatedAt.Sub(explicit); diff < -time.Second || diff > time.Second {
		t.Errorf("explicit CreatedAt not preserved: stored=%v want=%v", got2.CreatedAt, explicit)
	}
}
