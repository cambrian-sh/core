package app

import (
	"context"
	"fmt"
	"strings"
)

// Setup step 9 (ADR-0122, extended by ADR-0130 D1): mint the default agents'
// plane credentials into the ADR-0101 store, so a fresh install's agents can
// authenticate on their first boot without server.insecure_localhost.
//
// The store is the config bundle's `configs/config.db` (OpenConfigStore on the
// install prefix — setup has chdir'd there, so the kernel it starts opens the
// same file). The agents are whatever stepPython materialised into
// `<prefix>/agents`, read through the same filesystem source the kernel
// registers from, so setup cannot mint for a roster the kernel will not load.
// Idempotent: an agent that already has a credential keeps it.
func (s *setupState) stepAgentCredentials(ctx context.Context) {
	s.ui.section("\n9. Agent credentials")
	if s.agentsDir == "" {
		s.ui.line("ok", "agent credentials", "no agents directory (Python runtime step skipped) — nothing to mint")
		return
	}
	store, err := OpenConfigStore(s.prefix)
	if err != nil {
		s.warnf("could not open the config store for agent credentials", err.Error())
		return
	}
	if store == nil {
		s.ui.line("warn", "agent credentials", ConfigStoreEnv+"=off — no store to mint into; "+
			"agents will need server.insecure_localhost=true to reach the kernel")
		return
	}
	defer func() { _ = store.Close() }()

	discovered, err := newBuiltinFilesystemAgentSource(s.agentsDir).DiscoverAgents(ctx)
	if err != nil {
		s.warnf("could not discover agents for credential minting", err.Error())
		return
	}
	ids := make([]string, 0, len(discovered))
	for _, d := range discovered {
		ids = append(ids, d.Definition.ID)
	}
	minted, err := mintMissingAgentCredentials(store, ids)
	if err != nil {
		s.warnf("agent credential minting stopped early", err.Error())
		return
	}
	switch {
	case len(ids) == 0:
		s.ui.line("ok", "agent credentials", "no agents discovered in "+s.agentsDir)
	case len(minted) == 0:
		s.ui.line("ok", "agent credentials present", fmt.Sprintf("%d agents (agent:<id>:token)", len(ids)))
	default:
		s.ui.line("ok", "agent credentials minted",
			fmt.Sprintf("%d of %d agents: %s", len(minted), len(ids), strings.Join(minted, ", ")))
	}
}
