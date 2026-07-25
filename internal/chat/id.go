package chat

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a random message id, mirroring the session manager's generator.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a recoverable application condition; a colliding id
		// would corrupt a transcript, so fail loudly rather than degrade silently.
		panic("chat: crypto/rand unavailable: " + err.Error())
	}
	return "msg_" + hex.EncodeToString(b)
}
