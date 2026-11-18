package operator

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/pkg/util"
)

// ENG-1305 (T-20261113-operator-service-is-wired-through-64-post): the operator
// plane used to be built by NewService(feed) and then ~56 post-construction Set*
// calls, so a forgotten call was silent until an operator clicked the control and
// got Unimplemented. Every seam is now a constructor argument (AGENTS.md §2.3):
// the REQUIRED ones in Deps, which New refuses to build without, and the optional
// ones in Options, explicit fields whose absence New reports (Service.Unwired) so
// the composition root can log them once at boot.
//
// The rule for "required": a seam is required iff the capability it backs is
// advertised unconditionally in the handshake — the console assumes it on every
// kernel (feed, snapshot, commands, steering, audit, tools-read, skills-read,
// memory-read, memory-ingest, tool-exec, session-lifecycle, login/logout), plus
// the contract 0087-0093 pipeline ports, which the composition root always wires
// through an indirection. Everything else is capability-gated and optional.

// ErrMissingSeam is returned (wrapped) by New when a required seam is nil.
var ErrMissingSeam = errors.New("operator: required seam not wired")

// Deps are the seams every operator plane must have. New refuses to build a
// Service with any of them nil (a typed nil counts as nil).
type Deps struct {
	// Feed is the sequenced operator feed (WatchFeed, Snapshot as_of_seq).
	Feed *Spool
	// Projection folds plan state for Snapshot; Sessions lists sessions for it.
	Projection *Projection
	Sessions   SessionLister
	// Identity backs Login, Logout and DescribeAuth (ADR-0142 D1; ADR-0047 D13).
	// Login is served only when it also implements domain.PasswordLogin.
	Identity domain.Authenticator
	// Audit and Grants back the mutating commands (ADR-0047 D15/0047-24):
	// audit-stamped, idempotent on command_id, over the durable audit store.
	Audit  AuditStore
	Grants GrantsStore
	// Controls is the execution-control rendezvous a live DAGExecutor registers
	// into; Approvals resolves HITL through the kernel ApprovalHub (ADR-0047 D11).
	Controls  *ExecutionControlHub
	Approvals domain.ApprovalHub
	// Effects binds the remaining mutations to kernel surfaces (0047-07/0047-16).
	Effects CommandEffects
	// Tools, Skills and Memory are the ADR-0047 Amendment A2 paged reads: the
	// whole tool catalog, the skill registry and ScopeSystem memory recall.
	Tools  ToolCatalog
	Skills SkillLister
	Memory MemoryQuerier
	// ToolRunner executes operator-triggered tools at ScopeSystem (A2.2).
	ToolRunner ToolRunner
	// Ingestor requests a KERNEL document ingest for IngestMemory (A2.4).
	Ingestor MemoryIngestor
	// SessionOps backs CreateSession / SendMessage and the session lifecycle
	// (ADR-0047 0047-10/0047-21).
	SessionOps SessionOps
	// The reactive-pipeline ports (contracts 0087-0093). The composition root wires
	// them through an always-on indirection over what plugins register.
	PipelineLister    domain.PipelineLister
	PipelineDryRunner domain.PipelineDryRunner
	PipelineAuthor    domain.PipelineAuthor
	PipelineWriter    domain.PipelineWriter
	PipelineLifecycle domain.PipelineLifecycle
	// Handshake (ADR-0047 D14): the kernel and operator contract versions and the
	// capability strings this build advertises. Both versions are required — a
	// console warns on contract skew, which an empty version would defeat.
	KernelVersion   string
	ContractVersion string
	Capabilities    []string
}

// Options are the optional, capability-gated seams. A nil field leaves its RPCs
// answering Unimplemented (and, where a capability gates it, unadvertised).
type Options struct {
	// PrincipalNames gives WhoAmI the caller's display name (contract 0102).
	PrincipalNames PrincipalNames
	// LoginObserver receives failed (and successful) logins (ADR-0130 1f).
	// Observation only: the limiter runs with or without it.
	LoginObserver LoginObserver
	// Sharer applies the ADR-0142 D12 publish (ShareToTeam).
	Sharer MemorySharer
	// Documents enumerates documents by row (ListDocuments, "document-listing").
	Documents memory.DocumentLister
	// DocumentGetter is the keyed document read (GetDocument, contract 0086).
	DocumentGetter memory.DocumentGetter
	// MemoryAnswerer is the ADR-0081 answer lane ("memory-answer").
	MemoryAnswerer MemoryAnswerer
	// UploadMatcher answers the ADR-0138 B1.3 replace-by-name lookup ("upload-replace").
	UploadMatcher UploadMatcher
	// EventTimeFutureTolerance is the ADR-0134 future-time clamp tolerance; nil =
	// the default.
	EventTimeFutureTolerance *time.Duration
	// SourceMetaCaps bounds IngestMemory's descriptive fields (ADR-0136 A3.5).
	SourceMetaCaps SourceMetaCaps
	// WatchHandler is watch CRUD (ADR-0047 A2.6; premium).
	WatchHandler domain.WatchConfigHandler
	// DeadLetters is the reactive dead-letter read surface (REACT-01 / ADR-0061).
	DeadLetters domain.WatchDeadLetterReader
	// DeadLetterRetrier replays from the dead-letter queue (premium-only).
	DeadLetterRetrier DeadLetterRetrier
	// WatchMetrics / WatchBacktester are watch observability (REACT-05 / ADR-0071).
	WatchMetrics    domain.WatchMetricsReader
	WatchBacktester domain.WatchBacktester
	// Streams, WatchFires and PlaneBudget are contract 0074 reactive liveness.
	Streams     domain.StreamRegistry
	WatchFires  domain.WatchFireReader
	PlaneBudget domain.ReactiveBudgetReader
	// PolicyAdmin is the access-policy administration surface (ADR-0085).
	PolicyAdmin domain.PolicyAdmin
	// RoutePreviewer is gatekeeper route preview (ROUTE-07 / ADR-0077).
	RoutePreviewer RoutePreviewer
	// The contract 0072 (Wave 1) read sources.
	Checkpoints CheckpointLister
	MCPServers  MCPServerLister
	Embedding   EmbeddingReporter
	Classifier  InputClassifier
	// Generators is the generator registry read ("generator-registry").
	Generators GeneratorRegistry
	// TokenSeries backs the spend sparkline (contract 0075, "token-series").
	TokenSeries domain.TokenSeriesReader
	// BlastRadius previews a mutation's effect (contract 0076, "blast-radius").
	BlastRadius BlastRadiusEstimator
	// PlanProposer plans WITHOUT committing (contract 0075, "propose-plan").
	PlanProposer PlanProposer
	// PlanSubmitter runs operator-authored plans (contract 0074, "authored-plans").
	PlanSubmitter PlanSubmitter
	// ConfigSchema is the ADR-0101 read half of the runtime-config surface.
	ConfigSchema ConfigSchemaReporter
	// ConfigWriter / SecretWriter are the ADR-0101 durable write halves.
	ConfigWriter ConfigWriter
	SecretWriter SecretWriter
	// GeneratorWriter is the write half of the generator surface (contract 0083).
	GeneratorWriter GeneratorWriter
	// MCPWriter is the write half of the MCP server surface (contract 0097).
	MCPWriter MCPWriter
	// IntegrationManager is the integrations surface ("integrations").
	IntegrationManager IntegrationManager
	// LinkWriter is the identity plane's review lane (contract 0098, "links").
	LinkWriter LinkWriter
	// MemoryNotes is the ADR-0139 D14 memory-notes surface ("memory-notes").
	MemoryNotes MemoryNotesAdmin
	// SkillLifecycle is the ADR-0139 D13 skill lifecycle ("skills-lifecycle").
	SkillLifecycle SkillLifecycleAdmin
	// SkillGrants is per-agent skill access ("skill-grants").
	SkillGrants SkillGrantAdmin
	// PrincipalDirectory refuses unknown principals for act-for conversations;
	// nil ⇒ act-for is refused (fail closed).
	PrincipalDirectory domain.PrincipalDirectory
	// PrincipalSchedules is the operator view of principal-owned scheduled tasks.
	PrincipalSchedules domain.PrincipalSchedules
	// FactInspector is read-only fact inspection (contract 0103, "facts-inspect").
	FactInspector domain.FactInspector
	// FactStats is the fact extractor whose counters Snapshot reports (ENG-431).
	FactStats FactStatsSource
	// Forget is the ADR-0137 forget service (contract 0105); nil or disabled ⇒
	// the erasure RPCs answer Unimplemented.
	Forget *forget.Service
	// LogRing is the kernel's in-process log retention window (contract 0082).
	LogRing *util.LogRing
	// ConversationOps is the ADR-0084 D9 OSS chat lane ("chat").
	ConversationOps ConversationOps
	// Plugins is plugin identity on the handshake (ADR-0089).
	Plugins []PluginInfo
}

// New builds the OperatorConsole service from its seams. It refuses (wrapping
// ErrMissingSeam, naming every missing seam) when a required one is nil, so a
// composition that forgets one fails at boot rather than when an operator clicks.
func New(d Deps, o Options) (*Service, error) {
	if missing := missingDeps(d); len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingSeam, strings.Join(missing, ", "))
	}
	s := newService(d.Feed)
	s.projection = d.Projection
	s.sessions = d.Sessions
	s.idp = d.Identity
	s.audit, s.grants = d.Audit, d.Grants
	s.controls, s.hitl = d.Controls, d.Approvals
	s.effects = d.Effects
	s.tools, s.skills, s.memory = d.Tools, d.Skills, d.Memory
	s.toolRunner = d.ToolRunner
	s.ingestor = d.Ingestor
	s.sessionOps = d.SessionOps
	s.pipelines = d.PipelineLister
	s.pipelineDryRun = d.PipelineDryRunner
	s.pipelineAuthor = d.PipelineAuthor
	s.pipelineWriter = d.PipelineWriter
	s.pipelineLifecycle = d.PipelineLifecycle
	s.kernelVersion, s.contractVersion, s.capabilities = d.KernelVersion, d.ContractVersion, d.Capabilities

	s.principalNames = o.PrincipalNames
	s.loginObs = o.LoginObserver
	s.sharer = o.Sharer
	s.documents = o.Documents
	s.docGetter = o.DocumentGetter
	s.answerer = o.MemoryAnswerer
	s.uploads = o.UploadMatcher
	if o.EventTimeFutureTolerance != nil {
		tol := *o.EventTimeFutureTolerance
		s.eventTimeTolerance = &tol
	}
	s.sourceMetaCaps = o.SourceMetaCaps
	s.watches = o.WatchHandler
	s.deadletters = o.DeadLetters
	s.deadletterRetry = o.DeadLetterRetrier
	s.watchMetrics, s.watchBacktest = o.WatchMetrics, o.WatchBacktester
	s.streams, s.watchFires, s.planeBudget = o.Streams, o.WatchFires, o.PlaneBudget
	s.policy = o.PolicyAdmin
	s.routePreview = o.RoutePreviewer
	s.checkpoints, s.mcpServers, s.embedding, s.classifier = o.Checkpoints, o.MCPServers, o.Embedding, o.Classifier
	s.generators = o.Generators
	s.tokenSeries = o.TokenSeries
	s.blastRadius = o.BlastRadius
	s.planProposer = o.PlanProposer
	s.planSubmitter = o.PlanSubmitter
	s.configSchema = o.ConfigSchema
	s.configWriter, s.secretWriter = o.ConfigWriter, o.SecretWriter
	s.generatorWriter = o.GeneratorWriter
	s.mcpWriter = o.MCPWriter
	s.integrations = o.IntegrationManager
	s.linkWriter = o.LinkWriter
	s.notes = o.MemoryNotes
	s.skillLifecycle = o.SkillLifecycle
	s.skillGrants = o.SkillGrants
	s.principals = o.PrincipalDirectory
	s.schedules = o.PrincipalSchedules
	s.facts = o.FactInspector
	s.factStats = o.FactStats
	s.forget = o.Forget
	s.logs = o.LogRing
	s.convOps = o.ConversationOps
	s.plugins = o.Plugins
	s.unwired = unwiredOptions(o)
	return s, nil
}

// newService is the zero state both New and the package tests start from: the
// feed, a fresh projection and the login limiter (always on, never attached later).
func newService(feed *Spool) *Service {
	return &Service{feed: feed, projection: NewProjection(), logins: newLoginLimiter()}
}

// Unwired names the optional seams this service was built without, sorted. The
// composition root logs them once at boot, so "why does this RPC answer
// Unimplemented" has an answer in the boot log.
func (s *Service) Unwired() []string {
	return append([]string(nil), s.unwired...)
}

// missingDeps names every required seam that is nil (typed nils included) and
// every empty handshake version.
func missingDeps(d Deps) []string {
	var missing []string
	v := reflect.ValueOf(d)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			if f.String() == "" {
				missing = append(missing, t.Field(i).Name)
			}
		case reflect.Slice:
			// Capabilities: an empty list is a legitimate (if useless) handshake.
		default:
			if isNilValue(f) {
				missing = append(missing, t.Field(i).Name)
			}
		}
	}
	return missing
}

// unwiredOptions names the nil seam fields of o (typed nils included), sorted.
// Value fields (SourceMetaCaps) are configuration, not seams, and never listed.
func unwiredOptions(o Options) []string {
	var out []string
	v := reflect.ValueOf(o)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Func, reflect.Map, reflect.Chan:
			if isNilValue(f) {
				out = append(out, t.Field(i).Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// isNilValue reports a nil interface, or an interface holding a nil pointer, map,
// func, chan or slice — the typed nil that a plain `== nil` misses.
func isNilValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return true
		}
		return isNilValue(v.Elem())
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// The capability predicates the composition root evaluates while it assembles
// Options, before the service exists: each is true iff the seam that backs the
// capability is set.

func (o Options) HasIntegrationManager() bool   { return o.IntegrationManager != nil }
func (o Options) HasMemoryAnswerer() bool       { return o.MemoryAnswerer != nil }
func (o Options) HasDocumentLister() bool       { return o.Documents != nil }
func (o Options) HasUploadMatcher() bool        { return o.UploadMatcher != nil }
func (o Options) HasConfigSchema() bool         { return o.ConfigSchema != nil }
func (o Options) HasConfigWriter() bool         { return o.ConfigWriter != nil }
func (o Options) HasPlanSubmitter() bool        { return o.PlanSubmitter != nil }
func (o Options) HasTokenSeries() bool          { return o.TokenSeries != nil }
func (o Options) HasBlastRadiusEstimator() bool { return o.BlastRadius != nil }
func (o Options) HasPlanProposer() bool         { return o.PlanProposer != nil }
func (o Options) HasLinkWriter() bool           { return o.LinkWriter != nil }
func (o Options) HasMemoryNotes() bool          { return o.MemoryNotes != nil }
func (o Options) HasSkillLifecycle() bool       { return o.SkillLifecycle != nil }
func (o Options) HasSkillGrants() bool          { return o.SkillGrants != nil }
func (o Options) HasPrincipalDirectory() bool   { return o.PrincipalDirectory != nil }

// Wave1Capabilities returns the contract-0072 capability strings the given seams
// back. controls is Deps.Controls ("run-cancel", contract 0102).
func (o Options) Wave1Capabilities(controls *ExecutionControlHub) []string {
	return wave1Capabilities(o.Checkpoints != nil, o.MCPServers != nil, o.Embedding != nil,
		o.Classifier != nil, o.Generators != nil, controls != nil)
}
