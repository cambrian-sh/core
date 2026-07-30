package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// ADR-0101: config and secrets in an embedded store.
//
// This store lives in its OWN bbolt file, separate from the agent database, for
// a reason that is not stylistic: config must load before the storage factory
// can choose a database path, so the config store has to be openable without a
// loaded config. Sharing a file would make the boot order circular.
var (
	configOverrideBucket = []byte("config_overrides")
	secretBucket         = []byte("secrets")
	configMetaBucket     = []byte("config_meta")
)

// ConfigStoreSchemaVersion versions the bucket layout. The Postgres migration
// runner (ADR-0064) governs Postgres only; bbolt versions itself here.
const ConfigStoreSchemaVersion = 1

// BoltConfigStore implements config.Store and config.SecretStore.
type BoltConfigStore struct {
	db   *bbolt.DB
	aead cipher.AEAD
}

// OpenConfigStore opens (creating if needed) the config store at dbPath.
//
// The secret-encryption key is resolved from CAMBRIAN_SECRET_KEY, or from a
// 0600 key file beside the store, generated on first use. ADR-0101 D6: the point
// is that the store file ALONE is useless — it defends the realistic path where a
// database file travels somewhere its environment does not (a backup, a copied
// container volume, a bug report), not against an attacker who already holds
// both the file and the running process's environment.
func OpenConfigStore(dbPath string) (*BoltConfigStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("config store: create directory: %w", err)
	}
	db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("config store: open %s: %w", dbPath, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{configOverrideBucket, secretBucket, configMetaBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		meta := tx.Bucket(configMetaBucket)
		if meta.Get([]byte("schema_version")) == nil {
			return meta.Put([]byte("schema_version"), []byte(fmt.Sprint(ConfigStoreSchemaVersion)))
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("config store: init buckets: %w", err)
	}

	key, err := resolveSecretKey(filepath.Join(filepath.Dir(dbPath), "secret.key"))
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("config store: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("config store: gcm: %w", err)
	}
	return &BoltConfigStore{db: db, aead: aead}, nil
}

// Close releases the underlying file.
func (s *BoltConfigStore) Close() error { return s.db.Close() }

// ── config.Store ─────────────────────────────────────────────────────────────

// Overrides returns every stored override keyed by flat dotted path.
//
// A value that fails to decode is SKIPPED rather than failing the whole load: a
// single corrupt entry must not stop the kernel booting, because the operator's
// route to fixing it runs through a kernel that is up.
func (s *BoltConfigStore) Overrides() (map[string]any, error) {
	out := map[string]any{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(configOverrideBucket).ForEach(func(k, v []byte) error {
			var val any
			if err := json.Unmarshal(v, &val); err != nil {
				return nil
			}
			out[string(k)] = val
			return nil
		})
	})
	return out, err
}

// SetOverride durably records one override.
func (s *BoltConfigStore) SetOverride(key string, value any) error {
	if key == "" {
		return errors.New("config store: empty key")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("config store: encode %s: %w", key, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(configOverrideBucket).Put([]byte(key), b)
	})
}

// DeleteOverride removes one override. Deleting an absent key is not an error —
// the post-condition the caller wants already holds.
func (s *BoltConfigStore) DeleteOverride(key string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(configOverrideBucket).Delete([]byte(key))
	})
}

// ── config.SecretStore ───────────────────────────────────────────────────────

// SetSecret stores an encrypted credential under a logical name.
func (s *BoltConfigStore) SetSecret(name, value string) error {
	if name == "" {
		return errors.New("config store: empty secret name")
	}
	sealed, err := s.seal([]byte(value))
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(secretBucket).Put([]byte(name), sealed)
	})
}

// ClearSecret removes a credential.
func (s *BoltConfigStore) ClearSecret(name string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(secretBucket).Delete([]byte(name))
	})
}

// Configured reports whether a credential is stored under name, without
// decrypting it.
func (s *BoltConfigStore) Configured(name string) bool {
	var found bool
	_ = s.db.View(func(tx *bbolt.Tx) error {
		found = tx.Bucket(secretBucket).Get([]byte(name)) != nil
		return nil
	})
	return found
}

// LastFour returns the last four characters of a stored credential, or "".
//
// Four characters identify WHICH key is installed without being enough to use
// one — the same trade every provider dashboard makes. A secret shorter than
// four characters returns "" rather than a prefix of itself.
func (s *BoltConfigStore) LastFour(name string) string {
	v, err := s.reveal(name)
	if err != nil || len(v) < 4 {
		return ""
	}
	return v[len(v)-4:]
}

// Resolve returns the credential the kernel should USE for name, applying
// ADR-0101 D5's precedence: the environment variable wins, the store is the
// fallback. It is deliberately NOT on the config.SecretStore interface — nothing
// reachable from the operator plane can call it.
//
// envVar may be empty, meaning "this credential has no environment form".
func (s *BoltConfigStore) Resolve(name, envVar string) string {
	if envVar != "" {
		if v := os.Getenv(envVar); v != "" {
			return v
		}
	}
	v, err := s.reveal(name)
	if err != nil {
		return ""
	}
	return v
}

// reveal decrypts one stored secret. Unexported on purpose: it is the only path
// to a plaintext credential, and keeping it unexported means a future read RPC
// cannot reach it by accident.
func (s *BoltConfigStore) reveal(name string) (string, error) {
	var sealed []byte
	if err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(secretBucket).Get([]byte(name))
		if v != nil {
			sealed = append([]byte(nil), v...)
		}
		return nil
	}); err != nil {
		return "", err
	}
	if sealed == nil {
		return "", nil
	}
	plain, err := s.open(sealed)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *BoltConfigStore) seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("config store: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plain, nil), nil
}

func (s *BoltConfigStore) open(sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("config store: sealed value truncated")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], nil)
}

// resolveSecretKey returns the 32-byte AES key, from CAMBRIAN_SECRET_KEY (hex)
// or from keyPath, generating and persisting one on first use.
//
// ADR-0101 D6 states the consequence plainly and so does this comment: losing
// the key makes stored secrets unrecoverable, and they must be re-entered. The
// key file belongs in the documented backup set.
func resolveSecretKey(keyPath string) ([]byte, error) {
	if env := os.Getenv("CAMBRIAN_SECRET_KEY"); env != "" {
		key, err := hex.DecodeString(env)
		if err != nil || len(key) != 32 {
			return nil, errors.New("config store: CAMBRIAN_SECRET_KEY must be 64 hex characters (32 bytes)")
		}
		return key, nil
	}

	if b, err := os.ReadFile(keyPath); err == nil {
		key, decErr := hex.DecodeString(string(b))
		if decErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("config store: key file %s is corrupt; stored secrets cannot be decrypted", keyPath)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config store: read key file: %w", err)
	}

	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("config store: generate key: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("config store: write key file: %w", err)
	}
	return key, nil
}
