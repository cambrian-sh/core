package harness

// Attempt holds the result of one agent execution cycle.
type Attempt struct {
	ErrorMsg string
	Output   []byte
}

// Detect returns true when the agent is confirmed stuck in a loop.
//
// Tier 1: if error messages differ the agent is progressing — not a loop.
// Tier 2: inspect output similarity only when errors are identical.
func Detect(prev, curr Attempt) bool {
	// Tier 1 — must have the same error to be a candidate loop.
	if prev.ErrorMsg != curr.ErrorMsg {
		return false
	}

	// Tier 2 — assess output similarity.
	if len(curr.Output) == 0 || len(curr.Output) > 8192 {
		return true
	}

	// Levenshtein-based similarity check.
	dist := editDistance(prev.Output, curr.Output)
	maxLen := max(len(prev.Output), len(curr.Output))
	delta := float64(dist) / float64(maxLen)
	return delta < 0.05
}

// editDistance computes the Levenshtein edit distance between two byte slices
// using the standard single-row DP algorithm.
func editDistance(a, b []byte) int {
	la, lb := len(a), len(b)
	dp := make([]int, lb+1)
	for j := range dp {
		dp[j] = j
	}
	for i := 1; i <= la; i++ {
		prev := dp[0]
		dp[0] = i
		for j := 1; j <= lb; j++ {
			tmp := dp[j]
			if a[i-1] == b[j-1] {
				dp[j] = prev
			} else {
				dp[j] = 1 + min(prev, min(dp[j], dp[j-1]))
			}
			prev = tmp
		}
	}
	return dp[lb]
}
