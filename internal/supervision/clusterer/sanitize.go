package clusterer

import "strings"

// maxClusterNameLen bounds a capability label — the naming prompt asks for a
// 1-3 word label; anything materially longer is a malformed response.
const maxClusterNameLen = 40

// sanitizeClusterName validates an LLM-generated capability label before it is
// persisted and injected into the Planner prompt. It returns a cleaned label, or
// "" when the response is unusable (empty, multi-clause JSON / error blob, or
// over-long) so the caller can substitute a deterministic fallback. This stops a
// model/serving-layer error (e.g. an invalid-OutputSchema-format rejection) from
// becoming a cluster name.
func sanitizeClusterName(raw string) string {
	name := strings.TrimSpace(raw)
	if i := strings.IndexAny(name, "\r\n"); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	if name == "" || len(name) > maxClusterNameLen {
		return ""
	}
	if strings.ContainsAny(name, "{}") {
		return ""
	}
	if strings.Contains(strings.ToLower(name), "error") {
		return ""
	}
	return name
}

// preview returns at most n runes of s for safe log output.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fallbackClusterName derives a deterministic, safe label from the cluster's
// representative agent when LLM naming yields nothing usable (e.g. "analyst_agent"
// → "analyst"). Never empty.
func fallbackClusterName(repAgentID string) string {
	label := strings.TrimSpace(strings.TrimSuffix(repAgentID, "_agent"))
	if label == "" {
		return "capability_cluster"
	}
	return label
}
