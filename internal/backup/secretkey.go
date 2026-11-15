package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// secret.key (ADR-0101 D6) decrypts every secret in config.db. A backup that holds both
// in plaintext hands anyone with the backup folder every stored credential, so the owner
// decision (ENG-681) is: secret.key is backed up ENCRYPTED and SEPARATELY. The cipher is
// AES-256-GCM; the key is derived from an operator passphrase with PBKDF2-HMAC-SHA256
// (stdlib crypto/pbkdf2) over a random salt. The plaintext only ever exists in memory.

// SecretKeyFileName is the encrypted copy's file name inside the key root.
const SecretKeyFileName = "secret.key.enc"

const (
	encFormat     = 1
	pbkdf2Iter    = 600_000 // OWASP 2023 guidance for PBKDF2-HMAC-SHA256
	keyLen        = 32
	saltLen       = 16
	encryptedKind = "cambrian-backup-secret"
)

type encryptedFile struct {
	Kind   string `json:"kind"`
	Format int    `json:"format"`
	KDF    string `json:"kdf"`
	Iter   int    `json:"iterations"`
	Salt   []byte `json:"salt"`
	Nonce  []byte `json:"nonce"`
	Cipher []byte `json:"ciphertext"`
}

// Encrypt seals plaintext with a key derived from passphrase.
func Encrypt(plaintext, passphrase []byte) ([]byte, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("empty passphrase")
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	aead, err := deriveAEAD(passphrase, salt, pbkdf2Iter)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	e := encryptedFile{Kind: encryptedKind, Format: encFormat, KDF: "pbkdf2-hmac-sha256", Iter: pbkdf2Iter,
		Salt: salt, Nonce: nonce, Cipher: aead.Seal(nil, nonce, plaintext, []byte(encryptedKind))}
	return json.MarshalIndent(e, "", "  ")
}

// Decrypt opens what Encrypt sealed. A wrong passphrase fails authentication.
func Decrypt(sealed, passphrase []byte) ([]byte, error) {
	var e encryptedFile
	if err := json.Unmarshal(sealed, &e); err != nil {
		return nil, fmt.Errorf("not an encrypted backup secret: %w", err)
	}
	if e.Kind != encryptedKind || e.Format != encFormat || e.KDF != "pbkdf2-hmac-sha256" || e.Iter < 100_000 {
		return nil, errors.New("unsupported encrypted backup secret format")
	}
	aead, err := deriveAEAD(passphrase, e.Salt, e.Iter)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, e.Nonce, e.Cipher, []byte(encryptedKind))
	if err != nil {
		return nil, errors.New("decryption failed: wrong passphrase or a corrupted file")
	}
	return pt, nil
}

func deriveAEAD(passphrase, salt []byte, iter int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, string(passphrase), salt, iter, keyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptFile reads src, encrypts it and writes dst (which must not exist).
func EncryptFile(src, dst string, passphrase []byte) error {
	pt, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	sealed, err := Encrypt(pt, passphrase)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(sealed); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// DecryptFile decrypts src into dst, which must not exist (a restore never overwrites a
// live secret.key).
func DecryptFile(src, dst string, passphrase []byte) error {
	sealed, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	pt, err := Decrypt(sealed, passphrase)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%s: %w (a restore never overwrites an existing key)", dst, err)
	}
	if _, err := f.Write(pt); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
