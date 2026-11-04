package config

// ADR-0136 Amendment A3.1 defaults for the agent-plane ingest's descriptive source
// metadata caps (execution.ingestion.source_*_max_chars, part_speaker_max_chars).
const (
	DefaultSourceKindMaxChars   = 64
	DefaultSourceTitleMaxChars  = 256
	DefaultSourceAuthorMaxChars = 128
	DefaultPartSpeakerMaxChars  = 64
)

// SourceMetaCaps is the resolved cap set (every value >= 1).
type SourceMetaCaps struct {
	Kind, Title, Author, Speaker int
}

// SourceMetaCaps resolves the configured caps; 0 (unset) is the default.
func (c IngestionConfig) SourceMetaCaps() SourceMetaCaps {
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return SourceMetaCaps{
		Kind:    pick(c.SourceKindMaxChars, DefaultSourceKindMaxChars),
		Title:   pick(c.SourceTitleMaxChars, DefaultSourceTitleMaxChars),
		Author:  pick(c.SourceAuthorMaxChars, DefaultSourceAuthorMaxChars),
		Speaker: pick(c.PartSpeakerMaxChars, DefaultPartSpeakerMaxChars),
	}
}
