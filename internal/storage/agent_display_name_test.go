package storage

import (
	"strings"
	"testing"
)

// ENG-1475 (staticcheck SA1019): agentDisplayName replaces the deprecated
// strings.Title and must produce the same stored names, so no discovered agent's
// display name changes.
func TestAgentDisplayName_MatchesStringsTitle(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"kg_extractor_agent", "reranker_agent", "chat_session_agent", "a", "", "agent2b",
		"foo-bar_baz", "x.y_z", "already_Title", "__lead", "trail__", "ölçüm_ajanı", "çay-şeker",
		"it's_ok", "v1.2_agent", "ALL_CAPS",
	} {
		//lint:ignore SA1019 the deprecated function is the reference being matched.
		want := strings.Title(strings.ReplaceAll(id, "_", " ")) //nolint:staticcheck
		if got := agentDisplayName(id); got != want {
			t.Errorf("agentDisplayName(%q) = %q, strings.Title gives %q", id, got, want)
		}
	}
}
