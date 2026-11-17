package config

import (
	"strings"
	"testing"
)

// ADR-0157 D5.2: execution.ledger.artifacts caps — defaults, the vault ceiling
// as a hard upper bound, and validation.
func TestLedgerArtifactsConfig_DefaultsCeilingAndValidation(t *testing.T) {
	var c LedgerArtifactsConfig
	if c.FileCap() != ArtifactVaultCeiling || c.CollectBytesCap() != 256<<20 || c.CollectFilesCap() != 64 {
		t.Fatalf("defaults: %d %d %d", c.FileCap(), c.CollectBytesCap(), c.CollectFilesCap())
	}
	c = LedgerArtifactsConfig{MaxFileBytes: 1024, MaxCollectBytes: 4096, MaxCollectFiles: 3}
	if c.FileCap() != 1024 || c.CollectBytesCap() != 4096 || c.CollectFilesCap() != 3 {
		t.Fatalf("set values not read: %+v", c)
	}
	if errs := (LedgerConfig{Artifacts: c}).validate(); len(errs) != 0 {
		t.Fatalf("valid caps refused: %v", errs)
	}
	over := LedgerConfig{Artifacts: LedgerArtifactsConfig{MaxFileBytes: ArtifactVaultCeiling + 1}}
	if errs := over.validate(); len(errs) != 1 || !strings.Contains(errs[0], "vault ceiling") {
		t.Fatalf("a cap above the vault ceiling: %v", errs)
	}
	if over.Artifacts.FileCap() != ArtifactVaultCeiling {
		t.Fatalf("FileCap above the ceiling = %d", over.Artifacts.FileCap())
	}
	neg := LedgerConfig{Artifacts: LedgerArtifactsConfig{MaxCollectBytes: -1, MaxCollectFiles: -1}}
	if errs := neg.validate(); len(errs) != 2 {
		t.Fatalf("negative caps: %v", errs)
	}
}
