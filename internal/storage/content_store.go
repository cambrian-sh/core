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

	"github.com/cambrian-sh/core/domain"
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
	// Seq is the bucket write sequence at which the node was last written
	// (created, or re-written through dedup while a plan was live). GC compares it
	// with live plans' start sequences (domain.PlanScratchTracker). 0 = written
	// before sequences existed.
	Seq uint64 `json:"seq,omitempty"`
}

// BBoltContentStore implements domain.ContentStore using a BBolt bucket for small
// payloads and the filesystem for data at or above fsSizeThreshold.
type BBoltContentStore struct {
	db    *bolt.DB
	FsDir string // exported so tests can inspect filesystem cleanup

	// mu serialises writers (Put, GC's evict phase, plan registration) so a
	// node's write sequence and the live-plan set are read consistently.
	mu sync.Mutex
	// livePlans maps each live plan to its start sequence (BeginPlan). In memory
	// only: no plan survives a restart. Guarded by mu.
	livePlans map[domain.PlanLease]uint64
	nextLease domain.PlanLease
}

var _ domain.PlanScratchTracker = (*BBoltContentStore)(nil)

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

	var existingRaw []byte
	_ = cs.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b != nil {
			if v := b.Get([]byte(cid)); v != nil {
				existingRaw = append([]byte(nil), v...) // copy: v is only valid inside the tx
			}
		}
		return nil
	})
	if existingRaw != nil {
		// Single-owner v1 (ADR-0048 D4): the first writer owns the cid; a later Put
		// of identical bytes dedups and does NOT change ownership. It may promote
		// the node to a durable type or re-stamp its write sequence (onDedup).
		if err := cs.onDedup(cid, existingRaw, nodeType); err != nil {
			return "", err
		}
		slog.Info("content_store_put", "cid", cid, "dedup", true, "data_bytes", len(data))
		return cid, nil
	}

	// ADR-0048 D4: stamp the owning session from ctx. System/kernel writes carry no
	// session ⇒ empty owner ⇒ readable by anyone; an agent's offload under a managed
	// session is owned and read-gated.
	owner, _ := domain.SessionIDFromContext(ctx)
	rec := nodeRecord{Type: nodeType, Labels: labels, Snippet: snippet, OwnerSession: string(owner)}

	if len(data) >= fsSizeThreshold {
		blobPath := filepath.Join(cs.FsDir, string(cid))
		if err := os.WriteFile(blobPath, data, 0644); err != nil {
			return "", fmt.Errorf("content_store fs write: %w", err)
		}
		rec.Data = nil
	} else {
		rec.Data = data
	}

	if err := cs.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return errors.New("content_store bucket missing")
		}
		// Persistent, monotonic across restarts: the plan-liveness watermark.
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		rec.Seq = seq
		encoded, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("content_store encode: %w", err)
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
		OwnerSession: domain.SessionID(rec.OwnerSession),
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

// onDedup updates an existing record on a dedup hit. Called with cs.mu held.
// Ownership and data never change. Two updates are possible, at most one write:
//
//   - Promotion: the bytes were first stored as plan scratch and a durable writer
//     (evidence, source_document, …) now stores the same bytes, so the node takes
//     the durable type. Otherwise the next plan GC would evict the durable
//     writer's content through the dedup
//     (T-20261103-plan-scoped-contentstore-gc-evicts-evidence).
//   - Re-stamp: a scratch write hits a scratch node written before some live plan
//     began. The writer may be that plan, so the node takes a fresh write
//     sequence and is protected until the plan ends; without this, an earlier
//     plan's EndPlan would evict a CID the later plan still references
//     (T-20261103-a-plan-s-content-store-gc-evicts-a-concurrently).
func (cs *BBoltContentStore) onDedup(cid domain.CID, existingRaw []byte, nodeType string) error {
	var rec nodeRecord
	if err := json.Unmarshal(existingRaw, &rec); err != nil {
		return fmt.Errorf("content_store dedup decode %s: %w", cid, err)
	}
	if !domain.IsPlanScopedNodeType(rec.Type) {
		return nil // already durable: neither promotion nor GC protection applies
	}
	promote := !domain.IsPlanScopedNodeType(nodeType)
	restamp := !promote && cs.someLivePlanBeganAfterLocked(rec.Seq)
	if !promote && !restamp {
		return nil
	}
	from := rec.Type
	if err := cs.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return errors.New("content_store bucket missing")
		}
		if promote {
			rec.Type = nodeType
		} else {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			rec.Seq = seq
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put([]byte(cid), encoded)
	}); err != nil {
		return fmt.Errorf("content_store dedup write %s: %w", cid, err)
	}
	if promote {
		slog.Info("content_store_promote", "cid", cid, "from_type", from, "to_type", nodeType)
	}
	return nil
}

// collectableLocked reports whether a scratch node with write sequence
// seq is collectable: no live plan began before it was written, so none of them
// can have written it. With no live plan, every scratch node is collectable.
// Records from before write sequences existed carry seq 0 and are always
// collectable — no plan survives a restart. Called with cs.mu held.
func (cs *BBoltContentStore) collectableLocked(seq uint64) bool {
	for _, start := range cs.livePlans {
		if seq > start {
			return false
		}
	}
	return true
}

// someLivePlanBeganAfterLocked reports whether some live plan began at or after
// write sequence seq — i.e. that plan cannot be the node's recorded writer, so a
// write by it must re-stamp the node to be protected. (Being shielded by an
// OLDER live plan is not enough: that plan may end first.) Called with cs.mu held.
func (cs *BBoltContentStore) someLivePlanBeganAfterLocked(seq uint64) bool {
	for _, start := range cs.livePlans {
		if seq <= start {
			return true
		}
	}
	return false
}

// BeginPlan implements domain.PlanScratchTracker. The plan's start is the
// bucket's current write sequence: every scratch node written from here on has a
// larger sequence and is protected from every GC until EndPlan.
func (cs *BBoltContentStore) BeginPlan(_ context.Context) (domain.PlanLease, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var start uint64
	if err := cs.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(contentStoreBucket)); b != nil {
			start = b.Sequence()
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("content_store begin plan: %w", err)
	}
	if cs.livePlans == nil {
		cs.livePlans = make(map[domain.PlanLease]uint64)
	}
	cs.nextLease++
	lease := cs.nextLease
	cs.livePlans[lease] = start
	return lease, nil
}

// EndPlan implements domain.PlanScratchTracker: deregister, then collect every
// plan-scoped scratch node no still-live plan can need.
func (cs *BBoltContentStore) EndPlan(ctx context.Context, lease domain.PlanLease) error {
	cs.mu.Lock()
	delete(cs.livePlans, lease)
	cs.mu.Unlock()
	return cs.GC(ctx, nil)
}

// GC evicts plan-scoped scratch nodes (domain.IsPlanScopedNodeType) that are not
// in the keep list and that no live plan (BeginPlan) can need. Durable node
// types are never evicted here — see the port's doc comment in
// domain/content_store.go for why.
//
// Lock discipline (ADR-0022 noted bbolt write-lock serialisation): the full
// bucket is scanned in a read-only View with no lock held; only the candidates
// are re-read and re-checked inside the one write transaction, under cs.mu, so a
// concurrent Put cannot re-stamp or re-create a node between the check and the
// delete. Blob files are removed before cs.mu is released for the same reason.
func (cs *BBoltContentStore) GC(_ context.Context, keep []domain.CID) error {
	keepSet := make(map[domain.CID]bool, len(keep))
	for _, c := range keep {
		keepSet[c] = true
	}

	var candidates []domain.CID
	if err := cs.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			c := domain.CID(k)
			if keepSet[c] {
				return nil
			}
			if _, ok := decodeScratch(c, v); ok {
				candidates = append(candidates, c)
			}
			return nil
		})
	}); err != nil {
		return fmt.Errorf("content_store GC scan: %w", err)
	}

	if len(candidates) == 0 {
		return nil
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	var evicted, protected int
	var fsBlobs []domain.CID
	if err := cs.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return nil
		}
		for _, c := range candidates {
			v := b.Get([]byte(c))
			if v == nil {
				continue // already gone (a concurrent GC)
			}
			rec, ok := decodeScratch(c, v) // re-check: may have been promoted since the scan
			if !ok {
				continue
			}
			if !cs.collectableLocked(rec.Seq) {
				protected++
				continue
			}
			if err := b.Delete([]byte(c)); err != nil {
				return err
			}
			evicted++
			if rec.Data == nil {
				fsBlobs = append(fsBlobs, c)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("content_store GC evict: %w", err)
	}

	var fsDeleted int
	for _, c := range fsBlobs {
		if err := os.Remove(filepath.Join(cs.FsDir, string(c))); err == nil {
			fsDeleted++
		}
	}

	slog.Info("content_store_gc", "kept_cids", len(keepSet), "evicted_cids", evicted,
		"protected_by_live_plans", protected, "live_plans", len(cs.livePlans), "fs_deleted", fsDeleted)
	return nil
}

// decodeScratch decodes v and reports whether it is a plan-scoped scratch node.
// An undecodable record has no knowable type: it is kept rather than risk
// destroying durable content.
func decodeScratch(c domain.CID, v []byte) (nodeRecord, bool) {
	var rec nodeRecord
	if err := json.Unmarshal(v, &rec); err != nil {
		slog.Warn("content_store_gc: undecodable record kept", "cid", c, "err", err)
		return rec, false
	}
	return rec, domain.IsPlanScopedNodeType(rec.Type)
}

// erasableNodeTypes are the durable node types an ADR-0137 erasure may delete: evidence
// bytes (internal/evidence, contentNodeType) and source-document bodies
// (memory.IngestionManager.mintSourceDoc). A blob of any other type that happens to
// hold identical bytes belongs to another owner (a tool artifact, a ledger offload) and
// is kept.
var erasableNodeTypes = map[string]bool{"evidence": true, "source_document": true}

var _ domain.ContentEraser = (*BBoltContentStore)(nil)

// DeleteContent is the targeted delete-by-CID of an erasure's async tail (ADR-0137 D7;
// GC is not one). Absent CIDs are not an error; a node whose type is not erasable is
// kept (logged). The record and its blob file go under cs.mu, like GC's evict phase.
func (cs *BBoltContentStore) DeleteContent(_ context.Context, cid domain.CID) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var (
		blob    bool
		kept    string
		deleted bool
	)
	if err := cs.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(contentStoreBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(cid))
		if v == nil {
			return nil
		}
		var rec nodeRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("content_store erase decode %s: %w", cid, err)
		}
		if !erasableNodeTypes[rec.Type] {
			kept = rec.Type
			return nil
		}
		blob, deleted = rec.Data == nil, true
		return b.Delete([]byte(cid))
	}); err != nil {
		return fmt.Errorf("content_store erase: %w", err)
	}
	if kept != "" {
		slog.Info("content_store_erase: node kept (not an erasable type)", "cid", cid, "type", kept)
		return nil
	}
	if blob {
		if err := os.Remove(filepath.Join(cs.FsDir, string(cid))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("content_store erase blob: %w", err)
		}
	}
	if deleted {
		slog.Info("content_store_erase", "cid", cid, "blob_file", blob)
	}
	return nil
}
