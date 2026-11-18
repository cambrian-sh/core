package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ArtifactVault provides Content-Addressable Storage for agent-produced artifacts.
// Files are stored at data/vault/{sha256_hash}. Deduplication by hash.
type ArtifactVault struct {
	VaultPath        string
	MaxFileSizeBytes int64
}

func NewArtifactVault(vaultPath string) *ArtifactVault {
	return &ArtifactVault{
		VaultPath:        vaultPath,
		MaxFileSizeBytes: 100 * 1024 * 1024,
	}
}

// Store saves content with SHA-256 content-addressing. Returns the hex hash.
// If content with the same hash already exists, it is NOT overwritten (dedup).
// Returns an error if content exceeds MaxFileSizeBytes.
func (v *ArtifactVault) Store(content []byte) (string, error) {
	if v.MaxFileSizeBytes > 0 && int64(len(content)) > v.MaxFileSizeBytes {
		return "", fmt.Errorf("artifact size %d exceeds limit %d", len(content), v.MaxFileSizeBytes)
	}
	hash := sha256Hex(content)
	filePath := filepath.Join(v.VaultPath, hash)
	if _, err := os.Stat(filePath); err == nil {
		return hash, nil // already exists
	}
	if err := os.MkdirAll(v.VaultPath, 0755); err != nil {
		return "", fmt.Errorf("create vault dir: %w", err)
	}
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return "", fmt.Errorf("write artifact: %w", err)
	}
	return hash, nil
}

// Load reads content by hash from the vault.
func (v *ArtifactVault) Load(hash string) ([]byte, error) {
	filePath := filepath.Join(v.VaultPath, hash)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Delete removes the bytes stored under hash. A missing file is not an error. The
// vault is content-addressed and shared (two ledger entries — two owners' uploads
// of the same bytes — can name one body), so the caller decides that nothing
// references hash any more: the ledger's retention purge and the ADR-0137 erasure
// worker check that no ledger_entries row still names it (since ADR-0157 D5 the
// ledger is the vault's only registry). A hash that is not a plain hex name is refused, so no path can
// escape the vault directory.
//
// A cancelled ctx stops the delete before it touches the disk (domain.VaultEraser
// carries ctx, ENG-1466); the erasure outbox retries the step.
func (v *ArtifactVault) Delete(ctx context.Context, hash string) error {
	if hash == "" || strings.ContainsAny(hash, `/\.:`) {
		return fmt.Errorf("vault delete: invalid hash %q", hash)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("vault delete: %w", err)
	}
	err := os.Remove(filepath.Join(v.VaultPath, hash))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vault delete: %w", err)
	}
	return nil
}

// Verify re-hashes the stored file and compares to the provided hash.
// Returns true if the content matches. On mismatch, the file was mutated.
func (v *ArtifactVault) Verify(hash string) (bool, error) {
	content, err := v.Load(hash)
	if err != nil {
		return false, err
	}
	actual := sha256Hex(content)
	return actual == hash, nil
}

// ArtifactGuard verifies artifacts referenced by a session's event log.
type ArtifactGuard interface {
	VerifySessionArtifacts(sessionID string) ([]domain.Artifact, error)
}

// Close is a lifecycle hook for future resource cleanup.
func (v *ArtifactVault) Close() error {
	return nil
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
