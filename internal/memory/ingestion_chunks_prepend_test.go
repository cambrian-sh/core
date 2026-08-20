package memory

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// prependChunkContext: title + breadcrumb reach the stored text exactly once.
func TestPrependChunkContext(t *testing.T) {
	cases := []struct {
		name, title, body, section, want string
	}{
		{"title+section", "ATP Synthase", "It couples proton flux to phosphorylation.", "Ch 2 › Energetics",
			"ATP Synthase › Ch 2 › Energetics\n\nIt couples proton flux to phosphorylation."},
		{"title only (flat doc)", "Guidelines for ulimits", "Set the nofile limit to 8192.", "",
			"Guidelines for ulimits\n\nSet the nofile limit to 8192."},
		{"no context", "", "Bare text.", "", "Bare text."},
		{"body already opens with title", "Intro", "Intro\n\nBare text.", "", "Intro\n\nBare text."},
		{"already prepended (upsert refeed)", "ATP Synthase", "ATP Synthase › Ch 2\n\nBody.", "Ch 2",
			"ATP Synthase › Ch 2\n\nBody."},
		{"section equals title", "Same", "Body.", "Same", "Same\n\nBody."},
	}
	for _, c := range cases {
		ch := domain.Chunk{Body: c.body}
		if c.section != "" {
			ch.Metadata = map[string]any{"section_path": c.section}
		}
		if got := prependChunkContext(c.title, ch); got != c.want {
			t.Errorf("%s:\n got:  %q\n want: %q", c.name, got, c.want)
		}
	}
}
