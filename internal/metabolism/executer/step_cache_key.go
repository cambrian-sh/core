package executer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// codeGenKeywords are query sub-strings that indicate a step produces freshly
// generated code. Such steps are never cached (TTL=0) by default.
var codeGenKeywords = []string{"write", "implement", "generate code"}

// resolveCacheTTL returns the effective TTL for a step result.
//
// Priority (highest first):
//  1. Explicit Step.CacheTTLSeconds > 0 — Planner intent beats all heuristics.
//  2. IsThought=true → policies["thought"] or 1h.
//  3. Code-gen query → policies["code_gen"] or 0 (never cache).
//  4. Default → policies["tool"] or 7 days.
//
// A former rung keyed off Step.RecommendedModel — a step carrying a planner-assigned
// model got policies["cognitive"] or 24h. That was a PROXY: the routing field happened
// to correlate with "expensive reasoning step", and nothing validated the correlation.
// The planner no longer assigns models, so the proxy has no carrier and was removed
// rather than replaced with an invented one. A step that needs a shorter TTL says so
// explicitly via CacheTTLSeconds (rung 1), which already outranks every heuristic.
// CONSEQUENCE: steps that previously matched rung 4 now take the 7-day tool default
// instead of 24h. Tune step_cache_policies.tool if that is too long.
//
// policies may be nil — nil map lookups return the zero value without panicking.
func resolveCacheTTL(step domain.Step, policies map[string]int) time.Duration {
	if step.CacheTTLSeconds > 0 {
		return time.Duration(step.CacheTTLSeconds) * time.Second
	}
	if step.IsThought {
		if h := policies["thought"]; h > 0 {
			return time.Duration(h) * time.Hour
		}
		return time.Hour
	}
	q := strings.ToLower(step.Query)
	for _, kw := range codeGenKeywords {
		if strings.Contains(q, kw) {
			if h := policies["code_gen"]; h > 0 {
				return time.Duration(h) * time.Hour
			}
			return 0
		}
	}
	if h := policies["tool"]; h > 0 {
		return time.Duration(h) * time.Hour
	}
	return 7 * 24 * time.Hour
}

// stepCacheKey computes a deterministic cache key for a single step.
//
// Dependent steps (DependsOn non-empty) are keyed by plan subject, step query,
// and a hash of their direct dependency outputs — enabling cross-plan reuse when
// the same query receives identical inputs.
//
// Root steps (DependsOn empty) are additionally salted with planID to prevent
// results from one plan invocation leaking into a different invocation that
// happens to share the same subject and query.
func stepCacheKey(planSubject, planID string, step domain.Step, snapshot map[string]string) string {
	h := sha256.New()

	isRoot := len(step.DependsOn) == 0
	if isRoot {
		io.WriteString(h, planID) //nolint:errcheck
		io.WriteString(h, "|")    //nolint:errcheck
	}

	io.WriteString(h, planSubject) //nolint:errcheck
	io.WriteString(h, "|")         //nolint:errcheck
	io.WriteString(h, step.Query)  //nolint:errcheck
	io.WriteString(h, "|")         //nolint:errcheck

	if !isRoot {
		depsH := sha256.New()
		for _, dep := range step.DependsOn {
			io.WriteString(depsH, snapshot[fmt.Sprintf("step_%d_result", dep)]) //nolint:errcheck
		}
		io.WriteString(h, hex.EncodeToString(depsH.Sum(nil))) //nolint:errcheck
	}

	return hex.EncodeToString(h.Sum(nil))
}
