package util

import (
	"log/slog"
	"testing"
)

// Proves the wiring, not the ring: after InitLogger, the process-wide logger
// really does feed the process-wide window.
func TestInitLogger_InstallsAReachableRing(t *testing.T) {
	if _, err := InitLogger(LogModeHeadless, t.TempDir()); err != nil {
		t.Fatalf("init: %v", err)
	}
	ring := DefaultLogRing()
	if ring == nil {
		t.Fatal("InitLogger left no reachable window")
	}
	before := ring.Stats().LastSeq
	slog.Info("ADR-0074: plugin registered", "plugin", "authz")

	recs := ring.Since(before, 0)
	if len(recs) != 1 {
		t.Fatalf("the default logger did not reach the ring: %d records", len(recs))
	}
	if recs[0].Component != "ADR-0074" || recs[0].Attrs["plugin"] != "authz" {
		t.Fatalf("record not retained faithfully: %+v", recs[0])
	}
}
