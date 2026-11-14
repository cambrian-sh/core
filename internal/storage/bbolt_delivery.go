package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

// ADR-0090 D8.1 (ENG-1172): the outbound delivery journal — one two-phase intent
// per message, keyed conversation + txnID:
//
//	ingress_delivery   key → DeliveryIntentRecord (JSON)
//
// Storage stays domain-agnostic: states are plain strings and transition legality
// is decided by the kernel decorator; this layer only performs the compare-and-set
// inside one bbolt transaction, which is what makes a single attempt the sender
// when a retry and the redriver race.
var deliveryIntentBucket = []byte("ingress_delivery")

// Delivery intent states as stored. They mirror domain.DeliveryIntentState values;
// storage needs to know which ones are settled for pruning.
const (
	deliveryStateConfirmed = "confirmed"
	deliveryStateFailed    = "failed"
	deliveryStateAmbiguous = "ambiguous"
)

// ErrDeliveryIntentNotFound is returned for an unknown intent key.
var ErrDeliveryIntentNotFound = errors.New("storage: delivery intent not found")

// ErrDeliveryIntentConflict is returned when the compare-and-set's expected state
// does not match the stored one.
var ErrDeliveryIntentConflict = errors.New("storage: delivery intent is not in the expected state")

// DeliveryIntentRecord is the stored shape of one delivery intent.
type DeliveryIntentRecord struct {
	Key            string    `json:"key"`
	ConversationID string    `json:"conversation_id"`
	TxnID          string    `json:"txn_id"`
	IngressAgentID string    `json:"ingress_agent_id"`
	ExternalID     string    `json:"external_id"`
	Text           string    `json:"text,omitempty"`
	State          string    `json:"state"`
	Attempts       int       `json:"attempts"`
	Err            string    `json:"err,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// PrepareDeliveryIntent stores rec unless its key exists, and returns the stored
// record; created reports whether rec was written.
func (b *BBoltAdapter) PrepareDeliveryIntent(rec DeliveryIntentRecord) (stored DeliveryIntentRecord, created bool, err error) {
	if rec.Key == "" {
		return DeliveryIntentRecord{}, false, errors.New("storage: delivery intent key is required")
	}
	err = b.db.Update(func(tx *bbolt.Tx) error {
		bucket, berr := tx.CreateBucketIfNotExists(deliveryIntentBucket)
		if berr != nil {
			return berr
		}
		if v := bucket.Get([]byte(rec.Key)); v != nil {
			return json.Unmarshal(v, &stored)
		}
		raw, merr := json.Marshal(rec)
		if merr != nil {
			return merr
		}
		created = true
		stored = rec
		return bucket.Put([]byte(rec.Key), raw)
	})
	if err != nil {
		return DeliveryIntentRecord{}, false, fmt.Errorf("storage: prepare delivery intent: %w", err)
	}
	return stored, created, nil
}

// AdvanceDeliveryIntent moves key from → to when it is currently in from.
// countAttempt increments Attempts; dropText clears the stored text.
func (b *BBoltAdapter) AdvanceDeliveryIntent(key, from, to, reason string, at time.Time, countAttempt, dropText bool) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(deliveryIntentBucket)
		if bucket == nil {
			return ErrDeliveryIntentNotFound
		}
		v := bucket.Get([]byte(key))
		if v == nil {
			return ErrDeliveryIntentNotFound
		}
		var rec DeliveryIntentRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("storage: decode delivery intent %q: %w", key, err)
		}
		if rec.State != from {
			return fmt.Errorf("%w: %q is %s, not %s", ErrDeliveryIntentConflict, key, rec.State, from)
		}
		rec.State = to
		rec.Err = reason
		rec.UpdatedAt = at
		if countAttempt {
			rec.Attempts++
		}
		if dropText {
			rec.Text = ""
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), raw)
	})
}

// ListDeliveryIntents returns intents whose state is in states, least recently
// updated first, at most limit (limit <= 0 ⇒ all).
func (b *BBoltAdapter) ListDeliveryIntents(states []string, limit int) ([]DeliveryIntentRecord, error) {
	want := make(map[string]bool, len(states))
	for _, s := range states {
		want[s] = true
	}
	var out []DeliveryIntentRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(deliveryIntentBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var rec DeliveryIntentRecord
			if json.Unmarshal(v, &rec) != nil || !want[rec.State] {
				return nil
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("storage: list delivery intents: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneDeliveryIntents removes up to limit confirmed, failed or ambiguous intents
// last updated before `before` (limit <= 0 ⇒ no cap). Prepared and dispatched
// intents are never pruned: they still have work to do.
func (b *BBoltAdapter) PruneDeliveryIntents(before time.Time, limit int) (removed int, more bool, err error) {
	err = b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(deliveryIntentBucket)
		if bucket == nil {
			return nil
		}
		var victims [][]byte
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec DeliveryIntentRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			switch rec.State {
			case deliveryStateConfirmed, deliveryStateFailed, deliveryStateAmbiguous:
			default:
				continue
			}
			if !rec.UpdatedAt.Before(before) {
				continue
			}
			if limit > 0 && len(victims) >= limit {
				more = true
				break
			}
			victims = append(victims, append([]byte(nil), k...))
		}
		for _, k := range victims {
			if derr := bucket.Delete(k); derr != nil {
				return derr
			}
			removed++
		}
		return nil
	})
	return removed, more, err
}
