package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cambrian-sh/core/domain"

	"go.etcd.io/bbolt"
)

var stepCacheBucket = []byte("step_cache")

// BBoltStepCache implements domain.StepCache using a dedicated BBolt bucket.
type BBoltStepCache struct {
	db    *bbolt.DB
	nowFn func() time.Time
}

// cachedHandoffEntry is the on-disk representation of a cached step result.
// StoredAt and ExpiresAt are written alongside the Handoff so that Get can
// enforce TTL without a separate index.
type cachedHandoffEntry struct {
	Handoff   *domain.Handoff `json:"handoff"`
	StoredAt  time.Time       `json:"stored_at"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// NewBBoltStepCache creates the step_cache bucket (if absent) and returns a
// ready cache. nowFn defaults to time.Now; tests may override it.
func NewBBoltStepCache(db *bbolt.DB) (*BBoltStepCache, error) {
	err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(stepCacheBucket)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &BBoltStepCache{db: db, nowFn: time.Now}, nil
}

// Get retrieves a cached Handoff. Returns (nil, false, nil) for misses and
// expired entries; expired entries are deleted on read (lazy expiry).
func (c *BBoltStepCache) Get(_ context.Context, key string) (*domain.Handoff, bool, error) {
	if key == "" {
		return nil, false, nil
	}

	var entry cachedHandoffEntry
	var found bool

	if err := c.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(stepCacheBucket)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &entry)
	}); err != nil {
		return nil, false, err
	}

	if !found {
		return nil, false, nil
	}

	if c.nowFn().After(entry.ExpiresAt) {
		_ = c.db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(stepCacheBucket)
			if b == nil {
				return nil
			}
			return b.Delete([]byte(key))
		})
		return nil, false, nil
	}

	return entry.Handoff, true, nil
}

// Put stores a Handoff with expiration metadata derived from ttl.
func (c *BBoltStepCache) Put(_ context.Context, key string, handoff *domain.Handoff, ttl time.Duration) error {
	if key == "" {
		return nil
	}
	now := c.nowFn()
	entry := cachedHandoffEntry{
		Handoff:   handoff,
		StoredAt:  now,
		ExpiresAt: now.Add(ttl),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return c.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(stepCacheBucket)
		if b == nil {
			return nil
		}
		return b.Put([]byte(key), data)
	})
}
