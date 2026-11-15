package discovery

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1046: a tool manifest's classification tags may not carry an owner tag;
// that tool is refused, the rest of the directory still loads.
func TestLoadRegistry_RefusesOwnerTaggedTool(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "mine_tool.py", "\nTOOL_MANIFEST = '''\n"+
		`{"tools": [
  {"name": "mine", "description": "x", "classification_tags": ["owner:alice"], "effects": ["read"]},
  {"name": "ours", "description": "y", "classification_tags": ["ops"], "effects": ["read"]}
]}`+"\n'''\n")
	reg := domain.NewInMemoryToolRegistry()
	if _, err := LoadRegistry(dir, reg, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("mine"); ok {
		t.Fatal("an owner-tagged tool must not be registered")
	}
	if _, ok := reg.Get("ours"); !ok {
		t.Fatal("the clean tool in the same manifest must still load")
	}
}
