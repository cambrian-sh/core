package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0137 D7: DeleteContent removes an erased evidence or source-document blob (record
// and blob file), keeps a node of any other type that holds the same bytes, and treats
// an absent CID as done.
func TestDeleteContent_ErasesOnlyErasableTypes(t *testing.T) {
	cs := newTestContentStore(t)
	ctx := context.Background()

	small, err := cs.Put(ctx, []byte("erase me"), "evidence", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	big, err := cs.Put(ctx, bytes.Repeat([]byte("x"), fsSizeThreshold+1), "source_document", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := cs.Put(ctx, []byte("an artifact"), "tool_artifact", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{string(small), string(big), string(kept), "no-such-cid"} {
		if err := cs.DeleteContent(ctx, domain.CID(c)); err != nil {
			t.Fatalf("DeleteContent(%s): %v", c, err)
		}
	}
	for _, c := range []string{string(small), string(big)} {
		if ok, _ := cs.Has(ctx, domain.CID(c)); ok {
			t.Fatalf("%s survived", c)
		}
	}
	if _, err := os.Stat(filepath.Join(cs.FsDir, string(big))); !os.IsNotExist(err) {
		t.Fatalf("blob file survived: %v", err)
	}
	if ok, _ := cs.Has(ctx, kept); !ok {
		t.Fatal("a non-erasable node must be kept")
	}
}
