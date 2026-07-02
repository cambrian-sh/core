package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func newWatchConfigTestAdapter(t *testing.T) (*BBoltAdapter, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wc_test.db")
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	_ = db.Update(func(tx *bbolt.Tx) error {
		_, _ = tx.CreateBucketIfNotExists(watchConfigBucket)
		return nil
	})
	adapter := &BBoltAdapter{db: db}
	return adapter, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// Cycle 1 — WriteWatchConfig + ReadWatchConfig round-trip all fields.
func TestBBoltAdapter_WatchConfig_RoundTrip(t *testing.T) {
	adapter, cleanup := newWatchConfigTestAdapter(t)
	defer cleanup()

	rec := WatchConfigRecord{
		ID:                 "wc-1",
		Name:               "gold price alert",
		Description:        "Fires when gold > 5000",
		SourceType:         "daemon",
		SourceStreamID:     "gold_tracker",
		Condition:          "price > 5000",
		ConditionType:      "deterministic",
		ActionType:         "dispatch_agent",
		ActionTargetType:   "capability",
		ActionTarget:       "analyst",
		ActionPayload:      "{{price}}",
		Active:             true,
		ResponseMode:       "sync",
		DaemonParams:       map[string]any{"timeout_ms": float64(3000)},
		MaxConcurrentPlans: 2,
	}

	if err := adapter.WriteWatchConfig(rec); err != nil {
		t.Fatalf("WriteWatchConfig: %v", err)
	}

	got, err := adapter.ReadWatchConfig("wc-1")
	if err != nil {
		t.Fatalf("ReadWatchConfig: %v", err)
	}

	if got.Name != "gold price alert" {
		t.Errorf("Name: want %q, got %q", "gold price alert", got.Name)
	}
	if got.ConditionType != "deterministic" {
		t.Errorf("ConditionType: want %q, got %q", "deterministic", got.ConditionType)
	}
	if got.ResponseMode != "sync" {
		t.Errorf("ResponseMode: want %q, got %q", "sync", got.ResponseMode)
	}
	if got.MaxConcurrentPlans != 2 {
		t.Errorf("MaxConcurrentPlans: want 2, got %d", got.MaxConcurrentPlans)
	}
	if v, ok := got.DaemonParams["timeout_ms"]; !ok || v != float64(3000) {
		t.Errorf("DaemonParams[timeout_ms]: want 3000.0, got %v", got.DaemonParams["timeout_ms"])
	}
}

// Cycle 2 — ReadAllWatchConfigs returns all persisted records.
func TestBBoltAdapter_WatchConfig_ReadAll(t *testing.T) {
	adapter, cleanup := newWatchConfigTestAdapter(t)
	defer cleanup()

	recs := []WatchConfigRecord{
		{ID: "wc-a", Name: "config-a", Active: true},
		{ID: "wc-b", Name: "config-b", Active: false},
		{ID: "wc-c", Name: "config-c", Active: true},
	}
	for _, r := range recs {
		if err := adapter.WriteWatchConfig(r); err != nil {
			t.Fatalf("WriteWatchConfig %s: %v", r.ID, err)
		}
	}

	all, err := adapter.ReadAllWatchConfigs()
	if err != nil {
		t.Fatalf("ReadAllWatchConfigs: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ReadAllWatchConfigs: want 3 records, got %d", len(all))
	}
}

// Cycle 3 — DeleteWatchConfig removes the record; subsequent read returns not-found.
func TestBBoltAdapter_WatchConfig_Delete(t *testing.T) {
	adapter, cleanup := newWatchConfigTestAdapter(t)
	defer cleanup()

	_ = adapter.WriteWatchConfig(WatchConfigRecord{ID: "wc-del", Name: "to delete"})

	if err := adapter.DeleteWatchConfig("wc-del"); err != nil {
		t.Fatalf("DeleteWatchConfig: %v", err)
	}

	got, err := adapter.ReadWatchConfig("wc-del")
	if err == nil {
		t.Error("expected error reading deleted WatchConfig")
	}
	if got != nil {
		t.Errorf("expected nil record after deletion, got %+v", got)
	}
}

// Cycle 4 — SetWatchConfigActive toggles only Active; other fields unchanged.
func TestBBoltAdapter_WatchConfig_SetActive(t *testing.T) {
	adapter, cleanup := newWatchConfigTestAdapter(t)
	defer cleanup()

	_ = adapter.WriteWatchConfig(WatchConfigRecord{
		ID:     "wc-toggle",
		Name:   "important rule",
		Active: true,
	})

	if err := adapter.SetWatchConfigActive("wc-toggle", false); err != nil {
		t.Fatalf("SetWatchConfigActive: %v", err)
	}

	got, err := adapter.ReadWatchConfig("wc-toggle")
	if err != nil {
		t.Fatalf("ReadWatchConfig after SetActive: %v", err)
	}
	if got.Active {
		t.Error("Active: want false after SetWatchConfigActive(false)")
	}
	if got.Name != "important rule" {
		t.Errorf("Name should be unchanged: want %q, got %q", "important rule", got.Name)
	}
}

// Cycle 5 — DeleteWatchConfig returns error for non-existent ID.
func TestBBoltAdapter_WatchConfig_DeleteNotFound(t *testing.T) {
	adapter, cleanup := newWatchConfigTestAdapter(t)
	defer cleanup()

	if err := adapter.DeleteWatchConfig("does-not-exist"); err == nil {
		t.Error("expected error when deleting non-existent WatchConfig")
	}
}
