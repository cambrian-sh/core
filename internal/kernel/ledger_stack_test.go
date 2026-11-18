package kernel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"

	"github.com/cambrian-sh/core/internal/storage"
)

// ADR-0157 Q6 (owner 2026-11-18, ENG-1990): the retired bbolt artifact records
// expire — nothing is imported. These tests drive retireLegacyArtifacts over a
// REAL bbolt store (through the AgentRepoDecorator the composition root passes)
// with a recording body releaser in place of Postgres; the Postgres half of the
// release is TestPg_LedgerReleaseBodies, the whole boot path the app e2e.

const legacySecret = "SECRET-LEGACY-CONTENT"

type recordingReleaser struct {
	got    []string
	called bool
	err    error
}

func (r *recordingReleaser) ReleaseBodies(_ context.Context, hashes []string) (int, int, error) {
	r.called = true
	r.got = append(r.got, hashes...)
	if r.err != nil {
		return 0, len(hashes), r.err
	}
	return len(hashes), 0, nil
}

// seedRetiredArtifactBucket writes the pre-ADR-0157 bucket shape (records keyed by
// vault hash, JSON values) into a fresh agents.db and returns its path.
func seedRetiredArtifactBucket(t *testing.T, hashes ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.db")
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *bbolt.Tx) error {
		bk, err := tx.CreateBucket([]byte("artifacts"))
		if err != nil {
			return err
		}
		for _, h := range hashes {
			v := `{"hash":"` + h + `","semantic_summary":"` + legacySecret + `","tags":["team_a"]}`
			if err := bk.Put([]byte(h), []byte(v)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func openLegacyStore(t *testing.T, path string) (*storage.BBoltAdapter, *AgentRepoDecorator) {
	t.Helper()
	store, err := storage.NewBBoltAdapterNoScan(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, NewAgentRepoDecorator(store)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestRetireLegacyArtifacts_DropsBucketWithoutImport(t *testing.T) {
	h1, h2 := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	store, legacy := openLegacyStore(t, seedRetiredArtifactBucket(t, h1, h2))
	logs := captureLog(t)
	rel := &recordingReleaser{}

	retireLegacyArtifacts(context.Background(), rel, legacy)

	if got := strings.Join(rel.got, ","); got != h1+","+h2 {
		t.Fatalf("bodies offered for release = %q, want both record hashes", got)
	}
	if _, present, err := store.LegacyArtifactHashes(); err != nil || present {
		t.Fatalf("the retired bucket survived: present=%v err=%v", present, err)
	}
	out := logs.String()
	if !strings.Contains(out, "records_dropped=2") || !strings.Contains(out, "vault_bodies_released=2") {
		t.Fatalf("the drop was not logged with its counts: %s", out)
	}
	if strings.Contains(out, h1) || strings.Contains(out, h2) || strings.Contains(out, legacySecret) || strings.Contains(out, "team_a") {
		t.Fatalf("the log carries record ids or content: %s", out)
	}
}

// A failed release keeps the bucket, so the next boot retries (no body is
// orphaned with its only pointer gone).
func TestRetireLegacyArtifacts_KeepsBucketWhenReleaseFails(t *testing.T) {
	h := strings.Repeat("c3", 32)
	store, legacy := openLegacyStore(t, seedRetiredArtifactBucket(t, h))
	logs := captureLog(t)

	retireLegacyArtifacts(context.Background(), &recordingReleaser{err: errors.New("vault delete: " + h)}, legacy)

	if hashes, present, err := store.LegacyArtifactHashes(); err != nil || !present || len(hashes) != 1 {
		t.Fatalf("bucket after a failed release: %v present=%v err=%v (want kept)", hashes, present, err)
	}
	if strings.Contains(logs.String(), h) {
		t.Fatalf("the failure log carries a record id: %s", logs.String())
	}
	// The next boot, with the vault reachable, retires it.
	retireLegacyArtifacts(context.Background(), &recordingReleaser{}, legacy)
	if _, present, _ := store.LegacyArtifactHashes(); present {
		t.Fatal("the retry did not drop the bucket")
	}
}

// A store without the bucket (new, or already retired) is left alone.
func TestRetireLegacyArtifacts_NoBucketIsANoOp(t *testing.T) {
	_, legacy := openLegacyStore(t, filepath.Join(t.TempDir(), "agents.db"))
	rel := &recordingReleaser{}
	retireLegacyArtifacts(context.Background(), rel, legacy)
	if rel.called {
		t.Fatal("release ran for a store with no retired bucket")
	}
	retireLegacyArtifacts(context.Background(), rel, nil) // no legacy source wired
}
