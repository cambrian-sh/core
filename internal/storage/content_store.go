package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	bolt "go.etcd.io/bbolt"
)

const (
	contentStoreBucket = "content_store"
	// fsSizeThreshold is the minimum data size routed to the filesystem instead
	// of BBolt. Above this threshold BBolt's copy-on-write overhead dominates.
	fsSizeThreshold = 64 * 1024 // 64 KB
)

// nodeRecord is the JSON-serialised form stored in BBolt.
type nodeRecord struct {
	Type    string   `json:"type"`
	Data    []byte   `json:"data,omitempty"` // nil for fs-routed entries
	Labels  []string `json:"labels,omitempty"`
	Snippet string   `json:"snippet,omitempty"`
	// OwnerSession is the session that wrote this node (ADR-0048 D4). Empty for
	// system/kernel writes (tool results, step results) — those stay readable by
	// anyone; an agent's offload (PutContextNode under a managed session) is owned
	// and read-gated. Single-owner v1: the first writer of identical bytes owns it.
	OwnerSession string `json:"owner_session,omitempty"`
}

// BBoltContentStore implements domain.ContentStore using a BBolt bucket for small
// payloads and the filesystem for data at or above fsSizeThreshold.
type BBoltContentStore struct {
	db    *bolt.DB
	FsDir string // exported so tests can inspect filesystem cleanup
	mu    sync.Mutex
}

// NewBBoltContentStore opens (or creates) the BBolt database at dbPath and
// ensures the content_store bucket exists. fsDir is used for large blobs.
func NewBBoltContentStore(dbPath, fsDir string) (*BBoltContentStore, error) {
	if err := os.MkdirAll(fsDir, 0755); err != nil {
		return nil, fmt.Errorf("content_store fsDir: %w", err)
	}

	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 1e9}) // 1s
	if err != nil {
		return nil, fmt.Errorf("content_store bbolt open: %w", err)
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(contentStoreBucket))
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("content_store bucket init: %w", err)
	}

	return &BBoltContentStore{db: db, FsDir: fsDir}, nil
}

// Close releases the BBolt database handle.
func (cs *BBoltContentStore) Close() error {
	return cs.db.Close()
}

// computeCID returns the hex SHA-256 of data.
func computeCID(data []byte) domain.CID {
	sum := sha256.Sum256(data)
	return domain.CID(hex.EncodeToString(sum[:]))
}

// Put stores data and returns its CID. Idempotent on duplicate content.
func (cs *BBoltContentStore) Put(ctx context.Context, data []byte, nodeType string, labels []string, snippet string) (domain.CID, error) {
	cid := computeCID(data)

	cs.mu.Lock()
	defer cs.mu.Unlock()

	var existing bool
	_ = cs.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		existing = b != nil && b.Get([]byte(cid)) != nil
		return nil
	})
	if existing {
		// Single-owner v1 (ADR-0048 D4): the first writer owns the cid; a later Put
		// of identical bytes dedups and does NOT change ownership.
		slog.Info("content_store_put", "cid", cid, "dedup", true, "data_bytes", len(data))
		return cid, nil
	}

	// ADR-0048 D4: stamp the owning session from ctx. System/kernel writes carry no
	// session ⇒ empty owner ⇒ readable by anyone; an agent's offload under a managed
	// session is owned and read-gated.
	owner, _ := domain.SessionIDFromContext(ctx)
	rec := nodeRecord{Type: nodeType, Labels: labels, Snippet: snippet, OwnerSession: owner}

	if len(data) >= fsSizeThreshold {
		blobPath := filepath.Join(cs.FsDir, string(cid))
		if err := os.WriteFile(blobPath, data, 0644); err != nil {
			return "", fmt.Errorf("content_store fs write: %w", err)
		}
		rec.Data = nil
	} else {
		rec.Data = data
	}

	encoded, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("content_store encode: %w", err)
	}

	if err := cs.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return errors.New("content_store bucket missing")
		}
		return b.Put([]byte(cid), encoded)
	}); err != nil {
		return "", fmt.Errorf("content_store bbolt write: %w", err)
	}

	slog.Info("content_store_put", "cid", cid, "dedup", false, "data_bytes", len(data), "has_snippet", snippet != "")
	return cid, nil
}

// Get retrieves a ContextNode by CID. Returns an error if the CID is unknown.
func (cs *BBoltContentStore) Get(_ context.Context, cid domain.CID) (*domain.ContextNode, error) {
	var encoded []byte
	if err := cs.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return errors.New("content_store bucket missing")
		}
		v := b.Get([]byte(cid))
		if v == nil {
			return fmt.Errorf("content_store: CID %q not found", cid)
		}
		encoded = make([]byte, len(v))
		copy(encoded, v)
		return nil
	}); err != nil {
		return nil, err
	}

	var rec nodeRecord
	if err := json.Unmarshal(encoded, &rec); err != nil {
		return nil, fmt.Errorf("content_store decode: %w", err)
	}

	var data []byte
	if rec.Data != nil {
		data = rec.Data
	} else {
		blobPath := filepath.Join(cs.FsDir, string(cid))
		var err error
		data, err = os.ReadFile(blobPath)
		if err != nil {
			return nil, fmt.Errorf("content_store fs read %q: %w", blobPath, err)
		}
	}

	return &domain.ContextNode{
		CID:          cid,
		Type:         rec.Type,
		Data:         data,
		Labels:       rec.Labels,
		Snippet:      rec.Snippet,
		OwnerSession: rec.OwnerSession,
	}, nil
}

// Has reports whether the given CID exists.
func (cs *BBoltContentStore) Has(_ context.Context, cid domain.CID) (bool, error) {
	var found bool
	err := cs.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		found = b != nil && b.Get([]byte(cid)) != nil
		return nil
	})
	return found, err
}

// GC evicts all CIDs not present in the keep list.
func (cs *BBoltContentStore) GC(_ context.Context, keep []domain.CID) error {
	keepSet := make(map[domain.CID]bool, len(keep))
	for _, c := range keep {
		keepSet[c] = true
	}

	type eviction struct {
		cid  domain.CID
		inFS bool
	}
	var evictions []eviction

	if err := cs.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			c := domain.CID(k)
			if !keepSet[c] {
				var rec nodeRecord
				_ = json.Unmarshal(v, &rec)
				evictions = append(evictions, eviction{cid: c, inFS: rec.Data == nil})
			}
			return nil
		})
	}); err != nil {
		return fmt.Errorf("content_store GC scan: %w", err)
	}

	if len(evictions) == 0 {
		return nil
	}

	if err := cs.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return nil
		}
		for _, e := range evictions {
			if err := b.Delete([]byte(e.cid)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("content_store GC evict: %w", err)
	}

	var fsDeleted int
	for _, e := range evictions {
		if e.inFS {
			if err := os.Remove(filepath.Join(cs.FsDir, string(e.cid))); err == nil {
				fsDeleted++
			}
		}
	}

	slog.Info("content_store_gc", "kept_cids", len(keepSet), "evicted_cids", len(evictions), "fs_deleted", fsDeleted)
	return nil
}
