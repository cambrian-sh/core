package agentmgr

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The other half of ADR-0114 D24.
//
// This package's full-jitter curve is duplicated in premium's pipeline.RetryPolicy,
// because that package cannot import anything from core's `internal/` tree. A
// "must mirror" comment would drift silently and the symptom would be a retry
// storm in production, so both implementations assert against one canonical file
// instead. If this fails, this curve moved and the pipeline's did not.
const goldenBackoffPath = "../../../testdata/backoff_golden.json"

type backoffVector struct {
	N         int `json:"n"`
	BaseMS    int `json:"base_ms"`
	MaxMS     int `json:"max_ms"`
	CeilingMS int `json:"ceiling_ms"`
}

func TestRestartBackoff_MatchesGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(goldenBackoffPath)
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var doc struct {
		Vectors []backoffVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("golden vector file has no vectors")
	}

	for _, v := range doc.Vectors {
		// MaxAttempts is set beyond the vector's n so the flap guard never fires:
		// this test is about the curve, not about quarantine.
		p := NewDaemonRestartPolicy(v.N+2, time.Hour,
			time.Duration(v.BaseMS)*time.Millisecond,
			time.Duration(v.MaxMS)*time.Millisecond)

		// Returning the argument exposes the ceiling exactly — the deterministic
		// half of a full-jitter curve.
		p.jitter = func(max time.Duration) time.Duration { return max }

		// Register counts attempts per stream, so drive it to the vector's n.
		var got time.Duration
		for i := 0; i <= v.N; i++ {
			var quarantined bool
			got, quarantined = p.Register("golden")
			if quarantined {
				t.Fatalf("n=%d: quarantined before reaching the vector's attempt", v.N)
			}
		}
		want := time.Duration(v.CeilingMS) * time.Millisecond
		if got != want {
			t.Errorf("n=%d base=%dms max=%dms: ceiling should be %v, got %v",
				v.N, v.BaseMS, v.MaxMS, want, got)
		}
	}
}
