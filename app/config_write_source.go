package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// configWriteSource implements operator.ConfigWriter and operator.SecretWriter
// against the ADR-0101 store.
type configWriteSource struct {
	store *storage.BoltConfigStore
	// prov is the boot provenance. It answers "is a layer ABOVE the store
	// supplying this key?", which is how a shadowed write is detected at write
	// time rather than left for a later read (ADR-0101 D3).
	prov config.Provenance
	// hotApply applies a value to the running kernel. Returns false when the key
	// has no live path, which makes the write restart-required rather than
	// failed — the value IS stored either way.
	hotApply func(param string, v float64) bool
	// generatorList reports the BOOTED generator list — the base under the
	// store's overrides. Credential and role validation always go through
	// effectiveGenerators (store-first), so a generator saved from the console
	// is immediately a valid target for its key.
	generatorList func() []config.GeneratorConfig
	// liveRoles reports the provider's CURRENT role map — boot config plus every
	// hot-applied assignment since. nil ⇒ no live provider.
	liveRoles func() map[string]string
	// applyRole rebinds a role on the running provider. Returns false when there
	// is no live provider, which makes the write restart-required rather than
	// failed — the value is stored either way.
	applyRole func(role, generatorID string) bool
	// defaultGeneratorID reports the effective global default generator id.
	defaultGeneratorID func() string
	// applyGenerators swaps the running provider's generator table (registry,
	// failover ladder, capability index, default) to the given list — the live
	// half of SaveGenerator/RemoveGenerator (owner directive 2026-08-12: models
	// register dynamically, no restart). Returns false when there is no live
	// provider or the rebuild fails, making the write restart-required rather
	// than failed — the value is stored either way.
	applyGenerators func(gens []config.GeneratorConfig, defaultID string) bool

	// ── MCP write half (contract 0097) ──
	// mcpServerList reports the BOOTED MCP server list, the base for a save when
	// the store holds none yet (the effectiveGenerators pattern).
	mcpServerList func() []config.MCPServerConfig
	// applyMCPServer arms a saved server on the running kernel (connector
	// AddServer: drop the old session/tools, start the health loop). Returns
	// false when there is no live connector, making the write restart-required.
	applyMCPServer func(s config.MCPServerConfig) bool
	// detachMCPServer stops a removed server live (session, watch, tools).
	detachMCPServer func(id string) bool
	// bounceMCPServer reconnects one server so a credential change is picked up
	// (tokens are injected at connect time; a healthy session would keep the old
	// one forever).
	bounceMCPServer func(id string) bool
	// probeMCPServer dials a spec once, ephemeral (TestMCPServer).
	probeMCPServer func(ctx context.Context, s config.MCPServerConfig) operator.MCPTestResult
}

// tunableByKey indexes the catalogue for validation.
func tunableByKey(key string) (tunable, bool) {
	for _, t := range tunables {
		if t.Key == key {
			return t, true
		}
	}
	return tunable{}, false
}

// SetConfig validates, stores, and where possible hot-applies each key.
//
// Order matters and is deliberate: VALIDATE, then STORE, then apply. Storing
// before applying means a value that is valid but whose live application fails
// still persists and takes effect on the next boot — the operator's intent is
// recorded either way. The reverse order could apply a change that then fails to
// persist, which is the state that surprises someone after a restart.
func (c configWriteSource) SetConfig(values map[string]float64) ([]operator.ConfigWriteOutcome, error) {
	if c.store == nil {
		return nil, fmt.Errorf("no configuration store is available")
	}

	// Sorted so the response order is stable and two identical requests produce
	// identical responses — a console diffing them should see no churn.
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]operator.ConfigWriteOutcome, 0, len(keys))
	for _, key := range keys {
		out = append(out, c.setOne(key, values[key]))
	}
	return out, nil
}

func (c configWriteSource) setOne(key string, v float64) operator.ConfigWriteOutcome {
	res := operator.ConfigWriteOutcome{Key: key}

	t, known := tunableByKey(key)
	if !known {
		// Skip-and-warn, not a whole-request failure. A console built against a
		// different kernel revision must not lose the writes that WERE valid, and
		// a retired axis must not be writable back in by accident.
		res.Effect = operator.EffectRejected
		res.Error = "unknown configuration key on this kernel"
		return res
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		res.Effect = operator.EffectRejected
		res.Error = "value must be a finite number"
		return res
	}
	if v < t.Min || v > t.Max {
		res.Effect = operator.EffectRejected
		res.Error = fmt.Sprintf("value %g is outside the permitted range [%g, %g]", v, t.Min, t.Max)
		return res
	}

	if err := c.store.SetOverride(key, v); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the configuration store: " + err.Error()
		return res
	}
	res.Set = true

	// ADR-0101 D3: a higher layer wins over the store. The value is STORED — it
	// is the operator's stated intent and takes effect the moment the shadowing
	// variable is removed — but nothing changes now, and saying so here is the
	// entire point. Left to a later read, the operator sees "saved", observes no
	// change, and has nothing to explain it.
	if pin := c.prov.PinnedAbove(key); pin != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = pin
		return res
	}

	if t.Param != "" && c.hotApply != nil && c.hotApply(t.Param, v) {
		res.Effect = operator.EffectLive
		return res
	}
	res.Effect = operator.EffectRestartRequired
	return res
}

// DeleteConfig removes stored overrides.
//
// Deleting a key the store never held is NOT an error: the post-condition the
// caller wants — "the store does not pin this" — already holds, and reporting a
// failure would train an operator to ignore the response.
func (c configWriteSource) DeleteConfig(keys []string) ([]operator.ConfigWriteOutcome, error) {
	if c.store == nil {
		return nil, fmt.Errorf("no configuration store is available")
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	out := make([]operator.ConfigWriteOutcome, 0, len(sorted))
	for _, key := range sorted {
		res := operator.ConfigWriteOutcome{Key: key}
		if err := c.store.DeleteOverride(key); err != nil {
			res.Effect = operator.EffectRejected
			res.Error = "could not write to the configuration store: " + err.Error()
			out = append(out, res)
			continue
		}
		res.Set = true
		// Always restart-required, even for a key with a live path. Reverting means
		// "use whatever the layers beneath supply", and the kernel only re-reads
		// those at boot — claiming "live" here would assert the running value had
		// changed back when it has not.
		res.Effect = operator.EffectRestartRequired
		out = append(out, res)
	}
	return out, nil
}

// SetGeneratorKey stores a provider credential.
func (c configWriteSource) SetGeneratorKey(generatorID, key string) (operator.ConfigWriteOutcome, error) {
	res := operator.ConfigWriteOutcome{Key: llm.GeneratorKeySecretName(generatorID)}
	if c.store == nil {
		return res, fmt.Errorf("no credential store is available")
	}

	// Validate against the EFFECTIVE list (store-first), not the booted one:
	// "save a generator, then paste its key" is the console's primary add-model
	// flow, and validating against boot config refused the key for every
	// generator added since the last restart — the paired writes could never
	// succeed in one sitting. (The c.generators closure kept the same bug alive
	// as a fallback and was removed with it.)
	var envVar string
	if c.generatorList != nil || c.store != nil {
		known := false
		for _, g := range c.effectiveGenerators() {
			if g.ID == generatorID {
				known = true
				envVar = g.APIKeyEnv
				break
			}
		}
		if !known {
			// Refused rather than stored. A credential filed against a generator
			// that does not exist is invisible: it never gets used, nothing errors,
			// and the operator believes the provider is configured.
			res.Effect = operator.EffectRejected
			res.Error = fmt.Sprintf("no generator with id %q is configured", generatorID)
			return res, nil
		}
	}

	if err := c.store.SetSecret(res.Key, key); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the credential store: " + err.Error()
		return res, nil
	}
	res.Set = true

	// Same shadow rule as config, and it bites harder here: an operator who pastes
	// a new key while CAMBRIAN's env var still holds the old one would otherwise
	// see "saved" and keep getting authentication failures from the old credential.
	if envVar != "" && os.Getenv(envVar) != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = "env:" + envVar
		return res, nil
	}

	// Credentials are resolved per call, so a stored key is live immediately —
	// no restart, and no process-wide cache to invalidate.
	res.Effect = operator.EffectLive
	return res, nil
}

// ClearGeneratorKey removes a stored credential.
func (c configWriteSource) ClearGeneratorKey(generatorID string) error {
	if c.store == nil {
		return fmt.Errorf("no credential store is available")
	}
	return c.store.ClearSecret("generator:" + generatorID + ":api_key")
}

// hotApplyFor builds the live-apply hook from the existing ADR-0054 seam, so
// there is ONE code path that mutates live tunables rather than two that can
// drift.
func hotApplyFor(effects interface {
	SetRuntimeConfig(ctx context.Context, params map[string]float64) error
}) func(string, float64) bool {
	if effects == nil {
		return nil
	}
	return func(param string, v float64) bool {
		return effects.SetRuntimeConfig(context.Background(), map[string]float64{param: v}) == nil
	}
}

// ── Generator write half (contract 0083) ─────────────────────────────────────

// generatorsKey is the store key holding the WHOLE generator list.
//
// One key for the list, not one per generator, because the store layer merges
// per key and koanf replaces a list wholesale: a per-generator key would produce
// a list containing only the last generator written.
const generatorsKey = "llm_provider.generators"

// effectiveGenerators returns the generator list a save should be applied on top
// of: what the store holds now, or the booted config when the store holds none.
//
// Reading the STORE first and the boot config only as a fallback is what makes
// two saves in one process lifetime compose. Taking the boot config every time
// would make the second save silently discard the first, since nothing reloads
// config in between.
func (c configWriteSource) effectiveGenerators() []config.GeneratorConfig {
	if c.store != nil {
		if overrides, err := c.store.Overrides(); err == nil {
			if raw, ok := overrides[generatorsKey]; ok {
				if b, mErr := json.Marshal(raw); mErr == nil {
					var list []config.GeneratorConfig
					if json.Unmarshal(b, &list) == nil {
						return list
					}
				}
			}
		}
	}
	if c.generatorList != nil {
		return c.generatorList()
	}
	return nil
}

// defaultGeneratorKey is the store key for the global default generator id.
const defaultGeneratorKey = "llm_provider.default"

// effectiveDefaultID returns the default a write should judge against: the
// store's value when set, else the live/boot default. Store-first for the same
// composition reason as effectiveGenerators.
func (c configWriteSource) effectiveDefaultID() string {
	if c.store != nil {
		if overrides, err := c.store.Overrides(); err == nil {
			if raw, ok := overrides[defaultGeneratorKey]; ok {
				if s, ok := raw.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	if c.defaultGeneratorID != nil {
		return c.defaultGeneratorID()
	}
	return ""
}

// SaveGenerator creates or replaces one generator and stores the whole list.
func (c configWriteSource) SaveGenerator(spec operator.GeneratorSpec) (operator.ConfigWriteOutcome, error) {
	res := operator.ConfigWriteOutcome{Key: generatorsKey}
	if c.store == nil {
		return res, fmt.Errorf("no configuration store is available")
	}

	entry := config.GeneratorConfig{
		ID:              spec.ID,
		Provider:        spec.Provider,
		Model:           spec.Model,
		Endpoint:        spec.Endpoint,
		APIKeyEnv:       spec.APIKeyEnv,
		TimeoutMs:       int(spec.TimeoutMs),
		Capabilities:    spec.Capabilities,
		NativeTools:     spec.NativeTools,
		DisableThinking: spec.DisableThinking,
	}

	list := c.effectiveGenerators()
	replaced := false
	for i := range list {
		if list[i].ID == spec.ID {
			// Cost fields are preserved rather than zeroed: they are catalogue
			// data this plane deliberately never carries (no money crosses the
			// operator wire), so a save that echoed back what the console knows
			// would erase them.
			entry.CostPer1MInput = list[i].CostPer1MInput
			entry.CostPer1MOutput = list[i].CostPer1MOutput
			// APIKeyEnv likewise. The console never sends a variable NAME (it has
			// no way to know one, and the key itself travels by another route
			// entirely), so taking the request at face value silently unset the
			// variable a deployment supplies its credential through -- and, with
			// the old validation, left a kernel that would not boot.
			if entry.APIKeyEnv == "" {
				entry.APIKeyEnv = list[i].APIKeyEnv
			}
			list[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, entry)
	}

	if err := c.store.SetOverride(generatorsKey, list); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the configuration store: " + err.Error()
		return res, nil
	}
	res.Set = true

	// First-generator auto-default. With no default configured anywhere, a
	// stored generator list REFUSES THE NEXT BOOT (`llm_provider.default is
	// required` is a hard validation error) — the exact chicken-egg ADR-0123
	// constraint 2 warns about: the console that could undo the write needs a
	// kernel that is up. Defaulting the generator being saved keeps every
	// store-reachable state bootable. Measured in the field: a console-added
	// generator with no default bricked a deployment on 2026-08-11.
	def := c.effectiveDefaultID()
	if def == "" {
		def = spec.ID
		if err := c.store.SetOverride(defaultGeneratorKey, def); err != nil {
			res.Effect = operator.EffectRejected
			res.Error = "could not store llm_provider.default: " + err.Error()
			return res, nil
		}
	}

	if pin := c.prov.PinnedAbove(generatorsKey); pin != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = pin
		return res, nil
	}
	// Live when a provider is running: the table swap makes the generator
	// routable, assignable and testable on the next call — no restart (owner
	// directive 2026-08-12; the generator half of contract 0096's role rule).
	// Restart-required only when there is no live provider to apply to.
	if c.applyGenerators != nil && c.applyGenerators(list, def) {
		res.Effect = operator.EffectLive
		return res, nil
	}
	res.Effect = operator.EffectRestartRequired
	return res, nil
}

// RemoveGenerator drops one generator from the stored list.
//
// It REFUSES to remove the global default or a generator a role points at.
// Boot validation is hard on `llm_provider.default`: storing a list without it
// would leave a kernel that refuses to boot — and the console that could undo
// the write needs a running kernel, so the mistake would be unrecoverable from
// the surface that made it. A role-serving generator is refused for the softer
// version of the same reason: the role would silently fall back to the default
// while the console still shows the name the operator once chose.
func (c configWriteSource) RemoveGenerator(id string) (operator.ConfigWriteOutcome, error) {
	res := operator.ConfigWriteOutcome{Key: generatorsKey}
	if c.store == nil {
		return res, fmt.Errorf("no configuration store is available")
	}

	// Judged against the EFFECTIVE default (store-first): the boot value may
	// have been superseded by an auto-default or a stored change this lifetime.
	if c.effectiveDefaultID() == id {
		res.Effect = operator.EffectRejected
		res.Error = fmt.Sprintf("generator %q is the global default (llm_provider.default); point the default elsewhere first", id)
		return res, nil
	}
	if c.liveRoles != nil {
		var serving []string
		for role, rid := range c.liveRoles() {
			if rid == id {
				serving = append(serving, role)
			}
		}
		if len(serving) > 0 {
			sort.Strings(serving)
			res.Effect = operator.EffectRejected
			res.Error = fmt.Sprintf("generator %q serves %s; reassign the role(s) first",
				id, strings.Join(serving, ", "))
			return res, nil
		}
	}

	list := c.effectiveGenerators()
	kept := make([]config.GeneratorConfig, 0, len(list))
	found := false
	for _, g := range list {
		if g.ID == id {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	if !found {
		// Named a specific thing the caller believes exists, unlike a config key
		// whose desired post-condition already holds. Silence here hides a typo
		// and leaves the real generator serving traffic.
		res.Effect = operator.EffectRejected
		res.Error = "no generator with id " + id
		return res, nil
	}

	if err := c.store.SetOverride(generatorsKey, kept); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the configuration store: " + err.Error()
		return res, nil
	}
	res.Set = true
	// Live removal: the table swap drops the generator from the ladder on the
	// next call. An in-flight call finishes on the old client (same "nothing in
	// flight moves" tolerance as SetRole).
	if c.applyGenerators != nil && c.applyGenerators(kept, c.effectiveDefaultID()) {
		res.Effect = operator.EffectLive
		return res, nil
	}
	res.Effect = operator.EffectRestartRequired
	return res, nil
}

// ── Role assignment write half (contract 0096) ───────────────────────────────

// rolesKeyPrefix + role is the store key for one role binding. Per-ROLE keys,
// unlike the whole-list generatorsKey: koanf merges maps per key, so one role
// stored here composes with roles configured in files rather than replacing the
// whole map the way a stored list would.
const rolesKeyPrefix = "llm_provider.roles."

// assignableRoles is the closed system-organ vocabulary (ADR-0042). Deterministic
// and Zero-Hardcode-legal — roles are organs, not agents bidding for tasks. The
// agent_step purpose is deliberately absent: agent-step model choice belongs to
// the EFE/dispatch preference hook, and binding it here would hardcode routing.
var assignableRoles = map[string]bool{
	string(domain.PurposePlanner):   true,
	string(domain.PurposeVerifier):  true,
	string(domain.PurposeRouter):    true,
	string(domain.PurposeInterview): true,
	string(domain.PurposeMemory):    true,
}

// SetRoleAssignment durably binds a system role to a generator and hot-applies
// it to the running provider, so the change serves the organ's next call.
func (c configWriteSource) SetRoleAssignment(role, generatorID string) (operator.ConfigWriteOutcome, error) {
	key := rolesKeyPrefix + role
	res := operator.ConfigWriteOutcome{Key: key}
	if c.store == nil {
		return res, fmt.Errorf("no configuration store is available")
	}

	if !assignableRoles[role] {
		res.Effect = operator.EffectRejected
		res.Error = fmt.Sprintf("unknown role %q; roles are planner, verifier, router, interview, memory", role)
		return res, nil
	}
	// Refused rather than stored, like a credential against a missing generator:
	// a dangling role binding is invisible — the ladder quietly serves the
	// default while the console shows the name the operator typed.
	known := false
	for _, g := range c.effectiveGenerators() {
		if g.ID == generatorID {
			known = true
			break
		}
	}
	if !known {
		res.Effect = operator.EffectRejected
		res.Error = fmt.Sprintf("no generator with id %q is configured", generatorID)
		return res, nil
	}

	if err := c.store.SetOverride(key, generatorID); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the configuration store: " + err.Error()
		return res, nil
	}
	res.Set = true

	if pin := c.prov.PinnedAbove(key); pin != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = pin
		return res, nil
	}

	if c.applyRole != nil && c.applyRole(role, generatorID) {
		res.Effect = operator.EffectLive
		return res, nil
	}
	res.Effect = operator.EffectRestartRequired
	return res, nil
}

// ── MCP server write half (contract 0097) ────────────────────────────────────

// mcpServersKey is the store key holding the WHOLE MCP server list — one key
// for the same koanf reason as generatorsKey: lists replace wholesale, so a
// per-server key would leave only the last server written.
const mcpServersKey = "mcp.servers"

// validMCPTransports is the closed transport vocabulary the connector dials.
var validMCPTransports = map[string]bool{"stdio": true, "http": true, "sse": true}

// effectiveMCPServers is the list a save applies on top of: the store's, or the
// booted config when the store holds none. Store-first is what makes two saves
// in one process compose (the effectiveGenerators reasoning).
func (c configWriteSource) effectiveMCPServers() []config.MCPServerConfig {
	if c.store != nil {
		if overrides, err := c.store.Overrides(); err == nil {
			if raw, ok := overrides[mcpServersKey]; ok {
				if b, mErr := json.Marshal(raw); mErr == nil {
					var list []config.MCPServerConfig
					if json.Unmarshal(b, &list) == nil {
						return list
					}
				}
			}
		}
	}
	if c.mcpServerList != nil {
		return c.mcpServerList()
	}
	return nil
}

// mcpConfigFromSpec builds the config entry an operator's spec describes,
// preserving the fields the operator plane deliberately never carries.
func mcpConfigFromSpec(spec operator.MCPServerSpec, prior *config.MCPServerConfig) config.MCPServerConfig {
	entry := config.MCPServerConfig{
		ID:                 spec.ID,
		Transport:          spec.Transport,
		Endpoint:           spec.Endpoint,
		Args:               spec.Args,
		ClassificationTags: spec.ClassificationTags,
	}
	entry.Auth.Type = spec.AuthType
	entry.Auth.Header = spec.AuthHeader
	if prior != nil {
		// Per-tool policy (dangerous flags, pricing, per-tool tags) never crosses
		// the operator wire; a save that echoed the console's ignorance would
		// erase it. Same for the token env-var NAME — the credential itself
		// travels by another route entirely.
		entry.Tools = prior.Tools
		entry.Auth.TokenEnv = prior.Auth.TokenEnv
	}
	return entry
}

// SaveMCPServer creates or replaces one server, stores the whole list, and arms
// the server live.
func (c configWriteSource) SaveMCPServer(spec operator.MCPServerSpec) (operator.ConfigWriteOutcome, error) {
	res := operator.ConfigWriteOutcome{Key: mcpServersKey}
	if c.store == nil {
		return res, fmt.Errorf("no configuration store is available")
	}
	if !validMCPTransports[spec.Transport] {
		res.Effect = operator.EffectRejected
		res.Error = fmt.Sprintf("unknown transport %q (want stdio, http or sse)", spec.Transport)
		return res, nil
	}
	if spec.AuthType == "header" && spec.AuthHeader == "" {
		res.Effect = operator.EffectRejected
		res.Error = `auth_type "header" requires auth_header to name the header`
		return res, nil
	}

	list := c.effectiveMCPServers()
	var entry config.MCPServerConfig
	replaced := false
	for i := range list {
		if list[i].ID == spec.ID {
			entry = mcpConfigFromSpec(spec, &list[i])
			list[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entry = mcpConfigFromSpec(spec, nil)
		list = append(list, entry)
	}

	if err := c.store.SetOverride(mcpServersKey, list); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the configuration store: " + err.Error()
		return res, nil
	}
	res.Set = true

	if pin := c.prov.PinnedAbove(mcpServersKey); pin != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = pin
		return res, nil
	}
	// Live when a connector exists: the health loop is armed now, and the tools
	// appear as soon as the server answers. "live" here means the KERNEL acted;
	// whether the server is reachable is the list read's `connected` to report.
	if c.applyMCPServer != nil && c.applyMCPServer(entry) {
		res.Effect = operator.EffectLive
		return res, nil
	}
	res.Effect = operator.EffectRestartRequired
	return res, nil
}

// RemoveMCPServer drops one server from the stored list and the running kernel.
func (c configWriteSource) RemoveMCPServer(id string) (operator.ConfigWriteOutcome, error) {
	res := operator.ConfigWriteOutcome{Key: mcpServersKey}
	if c.store == nil {
		return res, fmt.Errorf("no configuration store is available")
	}

	list := c.effectiveMCPServers()
	kept := make([]config.MCPServerConfig, 0, len(list))
	found := false
	for _, s := range list {
		if s.ID == id {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	if !found {
		res.Effect = operator.EffectRejected
		res.Error = "no MCP server with id " + id
		return res, nil
	}

	if err := c.store.SetOverride(mcpServersKey, kept); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the configuration store: " + err.Error()
		return res, nil
	}
	res.Set = true

	if pin := c.prov.PinnedAbove(mcpServersKey); pin != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = pin
		return res, nil
	}
	if c.detachMCPServer != nil && c.detachMCPServer(id) {
		res.Effect = operator.EffectLive
		return res, nil
	}
	res.Effect = operator.EffectRestartRequired
	return res, nil
}

// SetMCPServerToken stores a server credential and bounces the connection so
// the new token is actually presented.
func (c configWriteSource) SetMCPServerToken(id, token string) (operator.ConfigWriteOutcome, error) {
	res := operator.ConfigWriteOutcome{Key: mcp.TokenSecretName(id)}
	if c.store == nil {
		return res, fmt.Errorf("no credential store is available")
	}

	var envVar string
	known := false
	for _, s := range c.effectiveMCPServers() {
		if s.ID == id {
			known = true
			envVar = s.Auth.TokenEnv
			break
		}
	}
	if !known {
		// The SetGeneratorKey reasoning: a credential filed against a server that
		// does not exist is invisible — never used, never erroring, while the
		// operator believes the integration is configured.
		res.Effect = operator.EffectRejected
		res.Error = fmt.Sprintf("no MCP server with id %q is configured", id)
		return res, nil
	}

	if err := c.store.SetSecret(res.Key, token); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the credential store: " + err.Error()
		return res, nil
	}
	res.Set = true

	if envVar != "" && os.Getenv(envVar) != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = "env:" + envVar
		return res, nil
	}

	// Tokens are injected at CONNECT time, so unlike a generator key a live
	// session keeps the old one; the bounce is what makes "saved" observable.
	if c.bounceMCPServer != nil && c.bounceMCPServer(id) {
		res.Effect = operator.EffectLive
		return res, nil
	}
	res.Effect = operator.EffectRestartRequired
	return res, nil
}

// ClearMCPServerToken removes a stored credential and bounces the connection.
func (c configWriteSource) ClearMCPServerToken(id string) error {
	if c.store == nil {
		return fmt.Errorf("no credential store is available")
	}
	if err := c.store.ClearSecret(mcp.TokenSecretName(id)); err != nil {
		return err
	}
	if c.bounceMCPServer != nil {
		c.bounceMCPServer(id)
	}
	return nil
}

// TestMCPServer dials the spec once. The probe resolves any stored token for
// spec.ID itself, so a credential can be verified without travelling back.
func (c configWriteSource) TestMCPServer(ctx context.Context, spec operator.MCPServerSpec) operator.MCPTestResult {
	if c.probeMCPServer == nil {
		return operator.MCPTestResult{Error: "this kernel cannot probe MCP servers"}
	}
	// A probe of a CONFIGURED server keeps its file-declared token env var, so
	// env-supplied credentials work; an unsaved spec has none and relies on the
	// store.
	var prior *config.MCPServerConfig
	for _, s := range c.effectiveMCPServers() {
		if s.ID == spec.ID {
			prior = &s
			break
		}
	}
	return c.probeMCPServer(ctx, mcpConfigFromSpec(spec, prior))
}
