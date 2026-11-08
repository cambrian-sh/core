package app

import (
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
)

// wireEffectCheck builds the effect source from config and the tool executor and
// hands it to the gatekeeper (the dispatch check) and the planner (the effect block
// and schema enum). An invalid vocabulary is refused at config load; should one
// still arrive, the vocabulary is EMPTY — every declared effect is then unmatched
// and refused, which is the fail-closed reading, never "no check".
func wireEffectCheck(capCfg config.CapabilityConfig, tools gatekeeper.AgentToolLister, gk *gatekeeper.Gatekeeper, planner *awareness.Planner) *gatekeeper.ToolEffectSource {
	vocab, err := capCfg.StepEffectVocabulary()
	if err != nil {
		slog.Error("effect check: effect_vocabulary is invalid; using an empty vocabulary, so every step that declares an effect is refused",
			"err", err)
		vocab, _ = domain.NewStepEffectVocabulary(nil)
	}
	src := gatekeeper.NewToolEffectSource(vocab, tools)
	if gk != nil {
		gk.Effects = src
	}
	if planner != nil {
		planner.SetEffectSource(src)
		// The planner's tool lines come from the same lister the effect source and
		// the executor read, so the menu it plans from is the set dispatch checks
		// and Execute allows — never the declared allowlist globs, which named tools
		// of unconnected servers and hid what a live server could do
		// (T-20261107-world-system-asks-are-planned-as-local-file).
		if tools != nil {
			planner.SetToolMenuSource(tools)
		}
	}
	slog.Info("effect check: wired (effect-checked dispatch, fail closed)", "vocabulary", vocab.Names())
	return src
}
