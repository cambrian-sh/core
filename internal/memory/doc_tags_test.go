package memory

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// TestDocTags_AgreesWithEnforcementDecoder pins the memory lanes' tag decoding to
// authz.DocTags, the decoder the enforcing read/write stores check
// (T-20261113-internal-memory-query-go-re-implements-authz). A second private
// decoder could learn (or miss) an encoding the chokepoints do not, and then a
// retrieval-side scope filter and the enforcement point would disagree about the
// same row.
func TestDocTags_AgreesWithEnforcementDecoder(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"nil metadata":          nil,
		"no tags key":           {"kind": "chunk"},
		"string slice":          {"tags": []string{"team:a", "pii"}},
		"json round-trip slice": {"tags": []interface{}{"team:a", 7, "pii"}},
		"wrong type":            {"tags": "team:a"},
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			got := docTags(meta)
			want := authz.DocTags(&domain.Document{Metadata: meta})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("docTags(%v) = %#v, authz.DocTags = %#v", meta, got, want)
			}
		})
	}
	if got := docTags(map[string]interface{}{"tags": []interface{}{"a", 1, "b"}}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("non-string elements must be skipped, got %#v", got)
	}
}
