package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type fakeDocLister struct{ ids []string }

func (f fakeDocLister) ListIDsByType(_ context.Context, _ string) ([]string, error) {
	return f.ids, nil
}

type fakeDocRemover struct{ removed []string }

func (f *fakeDocRemover) Remove(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

// reconcileIndex prunes exactly the persisted ids the keep predicate rejects.
func TestReconcileIndex_PrunesRejectedOnly(t *testing.T) {
	lister := fakeDocLister{ids: []string{"keep_a", "drop_b", "keep_c", "drop_d"}}
	rem := &fakeDocRemover{}
	keep := map[string]bool{"keep_a": true, "keep_c": true}

	reconcileIndex(context.Background(), lister, rem, domain.DocTypeSkill, func(id string) bool { return keep[id] })

	if len(rem.removed) != 2 || !contains(rem.removed, "drop_b") || !contains(rem.removed, "drop_d") {
		t.Fatalf("expected drop_b and drop_d pruned, got %v", rem.removed)
	}
	if contains(rem.removed, "keep_a") || contains(rem.removed, "keep_c") {
		t.Errorf("kept doc wrongly pruned: %v", rem.removed)
	}
}

// The MCP retention rule: a tool keeps its doc when it is currently registered OR
// when its server is still configured (even if unreachable this boot). Only a tool
// whose MCP server was removed from config — or a stale native tool — is pruned.
func TestToolKeepFunc_RetainsConfiguredServersAndNativeTools(t *testing.T) {
	currentTools := map[string]bool{"native_read": true, "mcp:live/search": true}
	configuredMCP := map[string]bool{"live": true, "down": true} // "gone" was removed from config

	keep := toolKeepFunc(currentTools, configuredMCP)

	cases := map[string]bool{
		"native_read":     true,  // native tool present in registry
		"mcp:live/search": true,  // connected MCP tool
		"mcp:down/fetch":  true,  // server configured but unreachable this boot — keep
		"mcp:gone/scrape": false, // server removed from config — prune
		"stale_native":    false, // native tool no longer in registry — prune
	}
	for id, want := range cases {
		if got := keep(id); got != want {
			t.Errorf("toolKeepFunc(%q) = %v, want %v", id, got, want)
		}
	}
}

// End-to-end on the tool docType: drives reconcileIndex with the real toolKeepFunc
// to confirm the removed-server tool is the only thing pruned.
func TestReconcileIndex_ToolDocType_PrunesRemovedServerOnly(t *testing.T) {
	lister := fakeDocLister{ids: []string{"native_read", "mcp:live/search", "mcp:down/fetch", "mcp:gone/scrape"}}
	rem := &fakeDocRemover{}
	keep := toolKeepFunc(
		map[string]bool{"native_read": true, "mcp:live/search": true},
		map[string]bool{"live": true, "down": true},
	)

	reconcileIndex(context.Background(), lister, rem, domain.DocTypeTool, keep)

	if len(rem.removed) != 1 || rem.removed[0] != "mcp:gone/scrape" {
		t.Fatalf("expected only mcp:gone/scrape pruned, got %v", rem.removed)
	}
}
