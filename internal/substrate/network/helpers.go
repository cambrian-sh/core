package network

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

const finalResultKey = "_dag_final_result"

func newPlanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func meanConfidence(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func stepTimeout(bidLatencyMs int, multiplier float64, baseBufferMs int) time.Duration {
	ms := float64(bidLatencyMs)*multiplier + float64(baseBufferMs)
	return time.Duration(ms) * time.Millisecond
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

