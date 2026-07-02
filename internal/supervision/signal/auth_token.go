package signal

import (
	"crypto/rand"
	"encoding/hex"
)

// GenerateAuthToken produces a cryptographically random 32-char hex token
// suitable for Instance authentication. Used by AgentManager at boot time.
func GenerateAuthToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
