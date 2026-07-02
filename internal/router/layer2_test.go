package router_test

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/router"
)

// ── Layer 2 — Word-boundary keyword heuristics ────────────────────────────────

// Cycle 11 — "watch" → DecisionWatch.
func TestLayer2_Watch_ReturnsWatch(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "watch the server logs"})
	if dec.Type != domain.DecisionWatch {
		t.Fatalf("expected DecisionWatch, got %q", dec.Type)
	}
}

// Cycle 12 — "monitor" → DecisionWatch.
func TestLayer2_Monitor_ReturnsWatch(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "monitor my server"})
	if dec.Type != domain.DecisionWatch {
		t.Fatalf("expected DecisionWatch, got %q", dec.Type)
	}
}

// Cycle 13 — "track" → DecisionWatch.
func TestLayer2_Track_ReturnsWatch(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "track gold prices"})
	if dec.Type != domain.DecisionWatch {
		t.Fatalf("expected DecisionWatch, got %q", dec.Type)
	}
}

// Cycle 14 — "alert" → DecisionWatch.
func TestLayer2_Alert_ReturnsWatch(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "alert me when price drops"})
	if dec.Type != domain.DecisionWatch {
		t.Fatalf("expected DecisionWatch, got %q", dec.Type)
	}
}

// Cycle 15 — "plan" → DecisionPlan.
func TestLayer2_Plan_ReturnsPlan(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "plan the deployment"})
	if dec.Type != domain.DecisionPlan {
		t.Fatalf("expected DecisionPlan, got %q", dec.Type)
	}
}

// Cycle 16 — "execute" → DecisionPlan.
func TestLayer2_Execute_ReturnsPlan(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "execute the migration script"})
	if dec.Type != domain.DecisionPlan {
		t.Fatalf("expected DecisionPlan, got %q", dec.Type)
	}
}

// Cycle 17 — "run" → DecisionPlan.
func TestLayer2_Run_ReturnsPlan(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "run the test suite"})
	if dec.Type != domain.DecisionPlan {
		t.Fatalf("expected DecisionPlan, got %q", dec.Type)
	}
}

// Ingestion is no longer a router decision: ingest-ish keywords ("ingest",
// "import", "upload") are NOT short-circuited at Layer 2 — they fall through to
// Layer 3 to be judged on actual intent (storing content into LTM is an automatic
// memory-subsystem function, not a user-routed request). With a nil generator the
// fall-through surfaces as a Layer-3 error, proving Layer 2 did not classify them.
func TestLayer2_IngestKeywords_NoLongerShortCircuit(t *testing.T) {
	r := router.New(nil)
	for _, body := range []string{"ingest this document", "import the CSV file", "upload the knowledge base"} {
		if _, err := r.Resolve(context.Background(), domain.RouterInput{Body: body}); err == nil {
			t.Errorf("body=%q: expected fall-through to Layer 3 (nil-gen error); Layer 2 wrongly classified it", body)
		}
	}
}

// Cycle 21 — Word-boundary enforced: "watching" must NOT trigger WATCH.
func TestLayer2_Substring_DoesNotMatch(t *testing.T) {
	r := router.New(nil)
	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: "I am watching the game"})
	if err == nil {
		t.Fatal("'watching' should not match WATCH keyword — must fall through to Layer 3 (nil generator error)")
	}
}

// Cycle 22 — Word-boundary enforced: "executor" must NOT trigger PLAN.
func TestLayer2_ExecutorSubstring_DoesNotMatch(t *testing.T) {
	r := router.New(nil)
	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: "the executor failed"})
	if err == nil {
		t.Fatal("'executor' should not match PLAN keyword — must fall through to Layer 3")
	}
}

// Cycle 23 — Word-boundary enforced: "planner" alone must NOT trigger PLAN.
// Body deliberately contains no standalone "plan" word — only "planner".
func TestLayer2_PlannerSubstring_DoesNotMatch(t *testing.T) {
	r := router.New(nil)
	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: "the planner generated something useful"})
	if err == nil {
		t.Fatal("'planner' should not match PLAN keyword — must fall through to Layer 3")
	}
}

// Cycle 24 — Multi-match falls through to Layer 3 (not first-match wins).
func TestLayer2_MultiMatch_FallsThrough(t *testing.T) {
	r := router.New(nil)
	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: "watch and plan this refactor"})
	if err == nil {
		t.Fatal("multi-match (watch + plan) must fall through to Layer 3, not return first match")
	}
}

// Cycle 25 — Case-insensitive: "MONITOR" → DecisionWatch.
func TestLayer2_CaseInsensitive(t *testing.T) {
	r := router.New(nil)
	dec := resolve(t, r, domain.RouterInput{Body: "MONITOR the server"})
	if dec.Type != domain.DecisionWatch {
		t.Fatalf("expected DecisionWatch, got %q", dec.Type)
	}
}

// Cycle 26 — Empty body falls through to Layer 3.
func TestLayer2_EmptyBody_FallsThrough(t *testing.T) {
	r := router.New(nil)
	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: ""})
	if err == nil {
		t.Fatal("empty body must fall through to Layer 3")
	}
}
