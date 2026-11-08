package app

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/integrations"
	"github.com/cambrian-sh/core/internal/procenv"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// integrationManager implements operator.IntegrationManager
// (T-20261106-integrations-manage-mcp-servers-from-the).
//
// It adds no storage and no lifecycle of its own: every write is a full MCP
// server entry handed to configWriteSource.saveMCPEntry / removeMCPEntry /
// setMCPSecret — the same per-id store entries and the same live connector
// closures SaveMCPServer (contract 0097) uses. What it adds is the product
// layer: catalog-driven installs, field validation, secret bookkeeping, and the
// read model joining config, durable state, catalog and live health.
type integrationManager struct {
	writer    configWriteSource
	catalog   *integrations.Catalog
	runtime   *mcpRuntime
	connector *mcp.Connector
	// testTimeout bounds one TestIntegration (a cold npx/uvx start can take tens
	// of seconds).
	testTimeout time.Duration
}

var _ operator.IntegrationManager = (*integrationManager)(nil)

// ── reads ────────────────────────────────────────────────────────────────────

func (m *integrationManager) Catalog() []*pb.IntegrationCatalogEntryOp {
	entries := m.catalog.Entries()
	out := make([]*pb.IntegrationCatalogEntryOp, 0, len(entries))
	for _, e := range entries {
		out = append(out, catalogEntryToOp(e))
	}
	return out
}

func (m *integrationManager) Categories() []string { return m.catalog.Categories() }

func (m *integrationManager) Integrations() []*pb.IntegrationOp {
	stored := m.writer.storedMCPServers()
	fileIDs := m.writer.fileMCPServerIDs()
	var out []*pb.IntegrationOp
	for _, sc := range m.runtime.list() {
		out = append(out, m.integrationOp(sc.ID, stored, fileIDs))
	}
	return out
}

// one returns the read model for one id, or nil.
func (m *integrationManager) one(id string) *pb.IntegrationOp {
	if _, ok := m.runtime.get(id); !ok {
		return nil
	}
	return m.integrationOp(id, m.writer.storedMCPServers(), m.writer.fileMCPServerIDs())
}

func (m *integrationManager) integrationOp(id string, stored map[string]config.StoredMCPServer, fileIDs map[string]bool) *pb.IntegrationOp {
	sc, _ := m.runtime.get(id)
	cfg, _, hasCfg := m.runtime.config(id)
	op := &pb.IntegrationOp{Id: id, Name: id, Transport: sc.Transport}
	switch {
	case m.runtime.source(id) == mcpSourcePlugin:
		op.Source = mcpSourcePlugin
	case stored[id].ID != "":
		op.Source = mcpSourceStore
		op.OverridesFile = fileIDs[id]
	default:
		op.Source = mcpSourceFile
	}
	op.Editable = op.Source != mcpSourcePlugin
	op.Enabled = !hasCfg || cfg.IsEnabled()
	if sc.Transport == "stdio" {
		op.Command, op.Args = sc.Endpoint, sc.Args
	} else {
		op.Url = sc.Endpoint
	}
	op.ClassificationTags = sc.DefaultClassificationTags
	op.PassthroughEnv = sc.Env

	var entry integrations.Entry
	var fromCatalog bool
	if hasCfg {
		op.CatalogId = cfg.CatalogID
		op.InstalledVersion = cfg.CatalogVersion
		op.Settings = cfg.EnvValues
		op.Category = cfg.Category
		if cfg.DisplayName != "" {
			op.Name = cfg.DisplayName
		}
		if cfg.CatalogID != "" {
			entry, fromCatalog = m.catalog.Get(cfg.CatalogID)
		}
	}
	if fromCatalog {
		op.Name, op.Category, op.TrustTier = entry.Name, entry.Category, entry.TrustTier
		if cfg.DisplayName != "" {
			op.Name = cfg.DisplayName
		}
		op.UpdateAvailable = cfg.CatalogVersion != "" && cfg.CatalogVersion != entry.Version
	}

	// Policy (what the kernel enforces for this server's tools).
	for _, name := range sortedPolicyNames(sc.Tools) {
		p := sc.Tools[name]
		op.ToolPolicies = append(op.ToolPolicies, &pb.IntegrationToolPolicyOp{Name: name, Dangerous: p.Dangerous, Effects: p.Effects})
	}
	if sc.DefaultPolicy != nil {
		op.HasDefaultPolicy = true
		op.DefaultPolicy = &pb.IntegrationToolPolicyOp{Dangerous: sc.DefaultPolicy.Dangerous, Effects: sc.DefaultPolicy.Effects}
	}
	op.PolicySummary = policySummary(len(sc.Tools), sc.DefaultPolicy)
	// ADR-0139 D8: the effective trust-tier containment (the last launch, or
	// what the server would get) rides on the summary string until
	// IntegrationOp has a structured field
	// (T-20261106-integrationop-has-no-structured-containment).
	if rep, ok := m.runtime.containment(m.connector, id); ok {
		op.PolicySummary += "; " + rep.Summary()
	}

	// Secrets and missing required fields.
	for _, f := range m.secretFields(cfg, entry, fromCatalog) {
		st := m.secretState(id, cfg, f)
		op.Secrets = append(op.Secrets, st)
		if f.Required && !st.GetSet() {
			op.MissingRequired = append(op.MissingRequired, f.Name)
		}
	}
	if fromCatalog {
		for _, f := range entry.Fields {
			if !f.Secret && f.Required && cfg.EnvValues[f.Name] == "" {
				op.MissingRequired = append(op.MissingRequired, f.Name)
			}
		}
	}

	if h, ok := m.connector.Health(id); ok {
		op.Health = operator.IntegrationHealthToOp(healthEvent(h))
	} else {
		op.Health = &pb.IntegrationHealthOp{ServerId: id, State: mcp.HealthStopped}
	}
	return op
}

// secretFields lists the secret fields of a server: the catalog's, plus any
// secret_env name or auth token the config declares beyond them (custom and
// hand-written servers).
func (m *integrationManager) secretFields(cfg config.MCPServerConfig, entry integrations.Entry, fromCatalog bool) []integrations.Field {
	var out []integrations.Field
	seen := map[string]bool{}
	hasToken := false
	if fromCatalog {
		for _, f := range entry.Fields {
			if f.Secret {
				out = append(out, f)
				seen[f.Name] = true
				hasToken = hasToken || f.Kind == integrations.KindToken
			}
		}
	}
	for _, n := range cfg.SecretEnv {
		if !seen[n] {
			out = append(out, integrations.Field{Name: n, Label: n, Secret: true, Required: true, Kind: integrations.KindEnv})
			seen[n] = true
		}
	}
	if !hasToken && cfg.Auth.Type != "" && cfg.Auth.Type != "none" {
		out = append(out, integrations.Field{Name: customTokenField, Label: "Access token", Secret: true, Required: true, Kind: integrations.KindToken})
	}
	return out
}

// customTokenField names the auth credential of a server with no catalog entry.
const customTokenField = "token"

// secretState reports set / source / updated-at for one secret — never a value.
func (m *integrationManager) secretState(id string, cfg config.MCPServerConfig, f integrations.Field) *pb.IntegrationSecretStateOp {
	st := &pb.IntegrationSecretStateOp{Name: f.Name, Label: f.Label, Required: f.Required, Kind: f.Kind}
	storeName, envVar := m.secretLocation(id, cfg, f)
	if envVar != "" && os.Getenv(envVar) != "" {
		st.Set, st.Source = true, "env:"+envVar
		return st
	}
	if ok, at := sharedMCPCredentialStatus(m.writer.store, id, storeName); ok {
		st.Set, st.Source = true, "store"
		if orgCredentialWriter() != nil {
			st.Source = "account" // ADR-0141: an org account bound to this slot
		}
		if !at.IsZero() {
			st.UpdatedAtUnixMs = at.UnixMilli()
		}
	}
	return st
}

// secretLocation is where one secret lives and which kernel variable outranks
// it (ADR-0101 D5): the token under mcp:<id>:token beaten by auth.token_env; an
// env secret under mcp:<id>:env:<NAME> beaten by NAME only when the server
// passes NAME through.
func (m *integrationManager) secretLocation(id string, cfg config.MCPServerConfig, f integrations.Field) (storeName, envVar string) {
	if f.Kind == integrations.KindToken {
		return mcp.TokenSecretName(id), cfg.Auth.TokenEnv
	}
	for _, n := range cfg.Env {
		if procenv.Key(n) == procenv.Key(f.Name) {
			envVar = f.Name
		}
	}
	return mcp.EnvSecretName(id, f.Name), envVar
}

// ── writes ───────────────────────────────────────────────────────────────────

func rejected(key, msg string) operator.ConfigWriteOutcome {
	return operator.ConfigWriteOutcome{Key: key, Effect: operator.EffectRejected, Error: msg}
}

// effective returns the durable config entry for id (store-over-file).
func (m *integrationManager) effective(id string) (config.MCPServerConfig, bool) {
	for _, s := range m.writer.effectiveMCPServers() {
		if s.ID == id {
			return s, true
		}
	}
	return config.MCPServerConfig{}, false
}

func (m *integrationManager) Install(_ context.Context, req *pb.InstallIntegrationOpRequest) (operator.ConfigWriteOutcome, *pb.IntegrationOp, error) {
	id := req.GetId()
	if id == "" {
		id = req.GetCatalogId()
	}
	key := config.MCPServerOverrideKey(id)
	if !config.ValidMCPServerID(id) {
		return rejected(key, "id must be lowercase letters, digits, '.', '_' or '-' (max 63)"), nil, nil
	}
	if _, exists := m.effective(id); exists {
		return rejected(key, fmt.Sprintf("an integration with id %q is already installed; use UpdateIntegration, or install under another id", id)), nil, nil
	}
	if _, plugin := m.runtime.get(id); plugin && m.runtime.source(id) == mcpSourcePlugin {
		return rejected(key, fmt.Sprintf("id %q is used by a plugin MCP server", id)), nil, nil
	}

	var (
		entry  config.MCPServerConfig
		fields []integrations.Field
	)
	if req.GetCatalogId() != "" {
		e, ok := m.catalog.Get(req.GetCatalogId())
		if !ok {
			return rejected(key, fmt.Sprintf("no catalog entry %q", req.GetCatalogId())), nil, nil
		}
		if !e.MultiInstance {
			for _, s := range m.writer.effectiveMCPServers() {
				if s.CatalogID == e.ID {
					return rejected(key, fmt.Sprintf("%s is already installed as %q and cannot be installed twice", e.Name, s.ID)), nil, nil
				}
			}
		}
		if req.GetEnabled() && !e.PlatformSupported() {
			return rejected(key, fmt.Sprintf("%s does not run on %s (supported: %s)", e.Name, hostOS(), strings.Join(e.Platforms, ", "))), nil, nil
		}
		fields = e.Fields
		entry = e.Server
		entry.ID = id
		entry.CatalogID, entry.CatalogVersion = e.ID, e.Version
		// Deep-copy what the catalog shares between installs.
		entry.Args = append([]string(nil), e.Server.Args...)
		entry.Env = append([]string(nil), e.Server.Env...)
		values, msg := catalogSettings(e, req.GetSettings())
		if msg != "" {
			return rejected(key, msg), nil, nil
		}
		entry.EnvValues = values
		for _, f := range e.Fields {
			if f.Secret && f.Kind == integrations.KindEnv {
				entry.SecretEnv = append(entry.SecretEnv, f.Name)
			}
		}
	} else {
		var msg string
		entry, fields, msg = customEntry(id, req.GetCustom(), req.GetSettings())
		if msg != "" {
			return rejected(key, msg), nil, nil
		}
	}
	if len(req.GetClassificationTags()) > 0 {
		entry.ClassificationTags = req.GetClassificationTags()
	}
	enabled := req.GetEnabled()
	entry.Enabled = &enabled

	// Secrets: only declared secret fields, and — when starting now — every
	// required one must be available, so "enabled" never means "crash-looping".
	secretByName := map[string]integrations.Field{}
	for _, f := range fields {
		if f.Secret {
			secretByName[f.Name] = f
		}
	}
	for name := range req.GetSecrets() {
		if _, ok := secretByName[name]; !ok {
			return rejected(key, fmt.Sprintf("%q is not a secret field of this integration", name)), nil, nil
		}
	}
	if enabled {
		var missing []string
		for _, f := range fields {
			if !f.Secret || !f.Required || req.GetSecrets()[f.Name] != "" {
				continue
			}
			if _, envVar := m.secretLocation(id, entry, f); envVar == "" || os.Getenv(envVar) == "" {
				missing = append(missing, f.Name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return rejected(key, "cannot start without the required secrets: "+strings.Join(missing, ", ")+
				"; supply them in secrets, or install disabled and set them first"), nil, nil
		}
	}
	if errs := config.ValidateMCPServer(entry); len(errs) > 0 {
		return rejected(key, strings.Join(errs, "; ")), nil, nil
	}

	// Secrets go in BEFORE the entry, so the first launch already has them.
	for _, name := range sortedSecretNames(req.GetSecrets()) {
		v := req.GetSecrets()[name]
		if v == "" {
			continue
		}
		storeName, _ := m.secretLocation(id, entry, secretByName[name])
		if err := writeSharedMCPCredential(m.writer.store, id, storeName, v); err != nil {
			return rejected(key, "could not write to the credential store: "+err.Error()), nil, nil
		}
	}
	out := m.writer.saveMCPEntry(entry)
	return out, m.after(out, id), nil
}

func (m *integrationManager) Update(_ context.Context, req *pb.UpdateIntegrationOpRequest) (operator.ConfigWriteOutcome, *pb.IntegrationOp, error) {
	id := req.GetId()
	key := config.MCPServerOverrideKey(id)
	cur, ok := m.effective(id)
	if !ok {
		return m.notEditable(id, key), nil, nil
	}
	next := cur
	if cur.CatalogID != "" {
		e, found := m.catalog.Get(cur.CatalogID)
		if !found {
			return rejected(key, fmt.Sprintf("catalog entry %q is no longer in the catalog; remove and re-install", cur.CatalogID)), nil, nil
		}
		if req.GetCustom() != nil {
			return rejected(key, "custom applies only to integrations not installed from the catalog"), nil, nil
		}
		values, msg := catalogSettings(e, req.GetSettings())
		if msg != "" {
			return rejected(key, msg), nil, nil
		}
		next.EnvValues = values
	} else {
		if req.GetCustom() != nil {
			spec, _, msg := customEntry(id, req.GetCustom(), req.GetSettings())
			if msg != "" {
				return rejected(key, msg), nil, nil
			}
			spec.Enabled = cur.Enabled
			if len(spec.ClassificationTags) == 0 {
				spec.ClassificationTags = cur.ClassificationTags
			}
			next = spec
		} else {
			next.EnvValues = cloneSettings(req.GetSettings())
		}
	}
	if req.GetSetTags() {
		next.ClassificationTags = req.GetClassificationTags()
	}
	out := m.writer.saveMCPEntry(next)
	return out, m.after(out, id), nil
}

func (m *integrationManager) SetSecret(_ context.Context, id, name, value string) (operator.ConfigWriteOutcome, *pb.IntegrationOp, error) {
	cur, ok := m.effective(id)
	if !ok {
		return m.notEditable(id, config.MCPServerOverrideKey(id)), nil, nil
	}
	f, ok := m.secretField(cur, name)
	if !ok {
		return rejected(mcp.EnvSecretName(id, name), fmt.Sprintf("%q is not a secret field of integration %q", name, id)), nil, nil
	}
	storeName, envVar := m.secretLocation(id, cur, f)
	out := m.writer.setMCPSecret(id, storeName, envVar, value)
	return out, m.after(out, id), nil
}

func (m *integrationManager) ClearSecret(_ context.Context, id, name string) (operator.ConfigWriteOutcome, *pb.IntegrationOp, error) {
	cur, ok := m.effective(id)
	if !ok {
		return m.notEditable(id, config.MCPServerOverrideKey(id)), nil, nil
	}
	f, ok := m.secretField(cur, name)
	if !ok {
		return rejected(mcp.EnvSecretName(id, name), fmt.Sprintf("%q is not a secret field of integration %q", name, id)), nil, nil
	}
	storeName, _ := m.secretLocation(id, cur, f)
	res := operator.ConfigWriteOutcome{Key: storeName}
	if err := clearSharedMCPCredential(m.writer.store, id, storeName); err != nil {
		return rejected(storeName, "could not write to the credential store: "+err.Error()), nil, nil
	}
	res.Set = true
	if m.writer.bounceMCPServer != nil && m.writer.bounceMCPServer(id) {
		res.Effect = operator.EffectLive
	} else {
		res.Effect = operator.EffectRestartRequired
	}
	return res, m.after(res, id), nil
}

func (m *integrationManager) SetEnabled(_ context.Context, id string, enabled bool) (operator.ConfigWriteOutcome, *pb.IntegrationOp, error) {
	key := config.MCPServerOverrideKey(id)
	cur, ok := m.effective(id)
	if !ok {
		return m.notEditable(id, key), nil, nil
	}
	if enabled {
		if cur.CatalogID != "" {
			if e, found := m.catalog.Get(cur.CatalogID); found && !e.PlatformSupported() {
				return rejected(key, fmt.Sprintf("%s does not run on %s", e.Name, hostOS())), nil, nil
			}
		}
		// Judged on the read model so "enable" and the console's
		// missing_required can never disagree.
		if op := m.one(id); op != nil && len(op.GetMissingRequired()) > 0 {
			return rejected(key, "cannot start until these are set: "+strings.Join(op.GetMissingRequired(), ", ")), op, nil
		}
	}
	next := cur
	next.Enabled = &enabled
	out := m.writer.saveMCPEntry(next)
	return out, m.after(out, id), nil
}

func (m *integrationManager) Remove(_ context.Context, id string, keepSecrets bool) (operator.ConfigWriteOutcome, error) {
	cur, ok := m.effective(id)
	if !ok {
		return m.notEditable(id, config.MCPServerOverrideKey(id)), nil
	}
	var secretNames []string
	if !keepSecrets {
		entry, fromCatalog := m.catalog.Get(cur.CatalogID)
		for _, f := range m.secretFields(cur, entry, fromCatalog && cur.CatalogID != "") {
			name, _ := m.secretLocation(id, cur, f)
			secretNames = append(secretNames, name)
		}
	}
	out := m.writer.removeMCPEntry(id)
	if out.Effect == operator.EffectRejected {
		return out, nil
	}
	for _, n := range secretNames {
		if err := clearSharedMCPCredential(m.writer.store, id, n); err != nil {
			out.Error = "removed, but a stored secret could not be cleared: " + err.Error()
		}
	}
	return out, nil
}

// RevertToFile drops the console's override or removal of a server a config
// file declares and restarts it on the file entry
// (T-20261106-integrations-no-revert-to-file-for-an-mcp). The store write and
// the live re-apply are configWriteSource.revertMCPEntryToFile — the same
// writer and connector closures every other mutation ends in.
func (m *integrationManager) RevertToFile(_ context.Context, id string) (operator.ConfigWriteOutcome, *pb.IntegrationOp, error) {
	if m.runtime.source(id) == mcpSourcePlugin {
		return m.notEditable(id, config.MCPServerOverrideKey(id)), nil, nil
	}
	out := m.writer.revertMCPEntryToFile(id)
	return out, m.after(out, id), nil
}

func (m *integrationManager) Test(ctx context.Context, id string, callProbe bool) (*pb.IntegrationTestResultOp, error) {
	sc, ok := m.runtime.get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no integration %q", id)
	}
	var probeName string
	var probeArgs map[string]any
	var skipped string
	if callProbe {
		cfg, _, _ := m.runtime.config(id)
		if e, found := m.catalog.Get(cfg.CatalogID); found && cfg.CatalogID != "" && e.Probe != nil {
			probeName, probeArgs = e.Probe.Name, e.Probe.Arguments
		} else {
			skipped = "the catalog declares no probe tool for this integration"
		}
	}
	timeout := m.testTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rep := m.connector.TestServer(tctx, sc, probeName, probeArgs)
	out := &pb.IntegrationTestResultOp{
		Ok: rep.OK, LatencyMs: rep.LatencyMs, Error: rep.Err, UsedLiveSession: rep.UsedLive,
		ProbeTool: rep.ProbeTool, ProbeOk: rep.ProbeOK, ProbeOutput: rep.ProbeOutput,
		ProbeSkippedReason: rep.ProbeSkipped,
	}
	if skipped != "" {
		out.ProbeSkippedReason = skipped
	}
	for _, t := range rep.Tools {
		out.Tools = append(out.Tools, &pb.IntegrationToolOp{
			Name: t.Name, Description: t.Description, Dangerous: t.Dangerous, Effects: t.Effects, PolicySource: t.PolicySource,
		})
	}
	return out, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// after is the read model following a write — the unchanged state after a
// rejection, when there is one.
func (m *integrationManager) after(_ operator.ConfigWriteOutcome, id string) *pb.IntegrationOp {
	return m.one(id)
}

func (m *integrationManager) notEditable(id, key string) operator.ConfigWriteOutcome {
	if m.runtime.source(id) == mcpSourcePlugin {
		return rejected(key, fmt.Sprintf("%q is contributed by a plugin and cannot be changed here", id))
	}
	return rejected(key, fmt.Sprintf("no integration with id %q", id))
}

func (m *integrationManager) secretField(cfg config.MCPServerConfig, name string) (integrations.Field, bool) {
	entry, fromCatalog := m.catalog.Get(cfg.CatalogID)
	for _, f := range m.secretFields(cfg, entry, fromCatalog && cfg.CatalogID != "") {
		if f.Name == name {
			return f, true
		}
	}
	return integrations.Field{}, false
}

// catalogSettings validates the non-secret values for a catalog entry and fills
// defaults. Only declared, non-secret fields are accepted: a settings map is
// not a way to hand a catalog server an arbitrary environment.
func catalogSettings(e integrations.Entry, settings map[string]string) (map[string]string, string) {
	out := map[string]string{}
	for name, v := range settings {
		f, ok := e.FieldByName(name)
		switch {
		case !ok:
			return nil, fmt.Sprintf("%q is not a setting of %s", name, e.Name)
		case f.Secret:
			return nil, fmt.Sprintf("%q is a secret: use the secrets map or SetIntegrationSecret, never settings", name)
		}
		if v != "" {
			out[name] = v
		}
	}
	var missing []string
	for _, f := range e.Fields {
		if f.Secret {
			continue
		}
		if out[f.Name] == "" && f.DefaultValue != "" {
			out[f.Name] = f.DefaultValue
		}
		if f.Required && out[f.Name] == "" {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		return nil, "required settings missing: " + strings.Join(missing, ", ")
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// customEntry builds a non-catalog server from its spec. The policy rules are
// the catalog's: no policy means fail-closed, and a non-dangerous default may
// not declare write/spend/admin.
func customEntry(id string, spec *pb.IntegrationCustomSpecOp, settings map[string]string) (config.MCPServerConfig, []integrations.Field, string) {
	if spec == nil {
		return config.MCPServerConfig{}, nil, "custom spec is required"
	}
	e := config.MCPServerConfig{
		ID: id, Transport: spec.GetTransport(), Endpoint: spec.GetEndpoint(), Args: spec.GetArgs(),
		Env: spec.GetPassthroughEnv(), SecretEnv: spec.GetSecretNames(),
		ClassificationTags: spec.GetClassificationTags(), DisplayName: spec.GetName(), Category: spec.GetCategory(),
		EnvValues: cloneSettings(settings),
	}
	e.Auth.Type, e.Auth.Header = spec.GetAuthType(), spec.GetAuthHeader()
	if e.Auth.Type == "" {
		e.Auth.Type = "none"
	}
	if e.Transport != "stdio" && (len(e.SecretEnv) > 0 || len(e.EnvValues) > 0) {
		return e, nil, "only stdio servers take environment settings or secrets; use auth_type for http/sse"
	}
	for _, p := range spec.GetToolPolicies() {
		if msg := checkEffects(p.GetEffects()); msg != "" {
			return e, nil, "tool " + p.GetName() + ": " + msg
		}
		e.Tools = append(e.Tools, config.MCPToolConfig{Name: p.GetName(), Dangerous: p.GetDangerous(), Effects: p.GetEffects()})
	}
	if spec.GetHasDefaultPolicy() {
		d := spec.GetDefaultPolicy()
		if msg := checkEffects(d.GetEffects()); msg != "" {
			return e, nil, "default policy: " + msg
		}
		if !d.GetDangerous() && mutating(d.GetEffects()) {
			return e, nil, "a non-dangerous default policy may not declare write, spend or admin: every future tool would be safe by default"
		}
		e.DefaultToolPolicy = &config.MCPToolConfig{Dangerous: d.GetDangerous(), Effects: d.GetEffects()}
	}
	var fields []integrations.Field
	for _, n := range e.SecretEnv {
		fields = append(fields, integrations.Field{Name: n, Label: n, Secret: true, Required: true, Kind: integrations.KindEnv})
	}
	if e.Auth.Type != "none" {
		fields = append(fields, integrations.Field{Name: customTokenField, Label: "Access token", Secret: true, Required: true, Kind: integrations.KindToken})
	}
	return e, fields, ""
}

func checkEffects(effects []string) string {
	for _, name := range effects {
		known := false
		for _, e := range domain.AllToolEffects {
			if string(e) == name {
				known = true
			}
		}
		if !known {
			return fmt.Sprintf("effect %q is outside the closed set", name)
		}
	}
	return ""
}

func mutating(effects []string) bool {
	for _, e := range effects {
		if e != string(domain.EffectRead) && e != string(domain.EffectEgress) {
			return true
		}
	}
	return false
}

func cloneSettings(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func sortedSecretNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPolicyNames(m map[string]mcp.ToolPolicy) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// policySummary is the one-line policy description a console shows.
func policySummary(listed int, def *mcp.ToolPolicy) string {
	var b strings.Builder
	if listed > 0 {
		fmt.Fprintf(&b, "%d tools with explicit policy; ", listed)
	}
	switch {
	case def == nil:
		b.WriteString("every other tool fails closed (approval, read+write+egress)")
	case def.Dangerous:
		b.WriteString("every other tool needs approval (" + strings.Join(def.Effects, "+") + ")")
	default:
		b.WriteString("every other tool: " + strings.Join(def.Effects, " + "))
	}
	return b.String()
}

func healthEvent(h mcp.ServerHealth) domain.MCPServerHealthEvent {
	return domain.MCPServerHealthEvent{
		ServerID: h.ServerID, State: h.State, LastHandshake: h.LastHandshake, ToolCount: h.ToolCount,
		LastError: h.LastError, Restarts: h.Restarts, ChangedAt: h.ChangedAt,
	}
}

func catalogEntryToOp(e integrations.Entry) *pb.IntegrationCatalogEntryOp {
	op := &pb.IntegrationCatalogEntryOp{
		Id: e.ID, Name: e.Name, Description: e.Description, Category: e.Category, Publisher: e.Publisher,
		License: e.License, TrustTier: e.TrustTier, DocsUrl: e.DocsURL, Version: e.Version,
		Transport: e.Server.Transport, Runtimes: e.Runtimes, Platforms: e.Platforms,
		PlatformSupported: e.PlatformSupported(), Notes: e.Notes, Source: e.Source, MultiInstance: e.MultiInstance,
	}
	if e.Server.Transport == "stdio" {
		op.Command, op.Args = e.Server.Endpoint, e.Server.Args
	} else {
		op.Url = e.Server.Endpoint
	}
	for _, f := range e.Fields {
		op.Fields = append(op.Fields, &pb.IntegrationFieldOp{
			Name: f.Name, Label: f.Label, Secret: f.Secret, Required: f.Required, Help: f.Help,
			Kind: f.Kind, DefaultValue: f.DefaultValue, Placeholder: f.Placeholder,
		})
	}
	for _, t := range e.Server.Tools {
		op.ToolPolicies = append(op.ToolPolicies, &pb.IntegrationToolPolicyOp{Name: t.Name, Dangerous: t.Dangerous, Effects: t.Effects})
	}
	var def *mcp.ToolPolicy
	if d := e.Server.DefaultToolPolicy; d != nil {
		op.HasDefaultPolicy = true
		op.DefaultPolicy = &pb.IntegrationToolPolicyOp{Dangerous: d.Dangerous, Effects: d.Effects}
		def = &mcp.ToolPolicy{Dangerous: d.Dangerous, Effects: d.Effects}
	}
	op.PolicySummary = policySummary(len(e.Server.Tools), def)
	if e.Probe != nil {
		op.ProbeTool = e.Probe.Name
	}
	return op
}

func hostOS() string { return runtimeGOOS }

var runtimeGOOS = goruntime.GOOS
