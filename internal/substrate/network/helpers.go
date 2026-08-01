package network

import (
	"crypto/rand"
	"encoding/hex"
)

const finalResultKey = "_dag_final_result"

func newPlanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// metadataToPayload converts gRPC metadata (map[string]string) to a
// domain.Signal payload (map[string]any). Used when routing CHAT decisions
// through ProcessSync. ADR-0032.
func metadataToPayload(md map[string]string) map[string]any {
	if len(md) == 0 {
		return nil
	}
	out := make(map[string]any, len(md))
	for k, v := range md {
		out[k] = v
	}
	return out
}
