package operator

import (
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
	"github.com/cambrian-sh/core/internal/memory/forget"
	"github.com/cambrian-sh/core/pkg/util"
)

// ENG-1305: production builds the operator plane with New(Deps, Options) — every
// seam a constructor argument. The package's own tests exercise one handler at a
// time against a partly wired service, so they keep the pre-ENG-1305 shape here,
// compiled only into this package's tests (internal and external): NewService
// builds the bare service and the Set*/Has* methods attach and probe one seam.
// Never move these back into a production file.

// NewService builds a bare service over feed (test-only; production uses New).
func NewService(feed *Spool) *Service { return newService(feed) }

// Wave1Capabilities is the contract-0072 capability set of a test-built service.
func (s *Service) Wave1Capabilities() []string {
	return wave1Capabilities(s.checkpoints != nil, s.mcpServers != nil, s.embedding != nil,
		s.classifier != nil, s.generators != nil, s.controls != nil)
}

func (s *Service) SetPolicyAdmin(a domain.PolicyAdmin) { s.policy = a }

func (s *Service) SetPrincipalDirectory(d domain.PrincipalDirectory) { s.principals = d }

func (s *Service) HasPrincipalDirectory() bool { return s.principals != nil }

func (s *Service) SetBlastRadiusEstimator(e BlastRadiusEstimator) { s.blastRadius = e }

func (s *Service) HasBlastRadiusEstimator() bool { return s.blastRadius != nil }

func (s *Service) SetSessionOps(ops SessionOps) { s.sessionOps = ops }

func (s *Service) SetCommandSources(audit AuditStore, grants GrantsStore) {
	s.audit = audit
	s.grants = grants
}

func (s *Service) SetConfigSchemaReporter(r ConfigSchemaReporter) { s.configSchema = r }

func (s *Service) HasConfigSchema() bool { return s.configSchema != nil }

func (s *Service) SetConfigWriter(w ConfigWriter) { s.configWriter = w }

func (s *Service) SetSecretWriter(w SecretWriter) { s.secretWriter = w }

func (s *Service) HasConfigWriter() bool { return s.configWriter != nil }

func (s *Service) SetConversationOps(ops ConversationOps) { s.convOps = ops }

func (s *Service) HasConversations() bool { return s.convOps != nil }

func (s *Service) SetForgetService(f *forget.Service) { s.forget = f }

func (s *Service) SetToolExec(runner ToolRunner) { s.toolRunner = runner }

func (s *Service) SetFactInspector(fi domain.FactInspector) { s.facts = fi }

func (s *Service) SetFactStats(src FactStatsSource) { s.factStats = src }

func (s *Service) SetGeneratorRegistry(g GeneratorRegistry) { s.generators = g }

func (s *Service) SetGeneratorWriter(w GeneratorWriter) { s.generatorWriter = w }

func (s *Service) SetSourceMetaCaps(c SourceMetaCaps) { s.sourceMetaCaps = c }

func (s *Service) SetMemoryIngestor(ing MemoryIngestor) { s.ingestor = ing }

func (s *Service) SetUploadMatcher(m UploadMatcher) { s.uploads = m }

func (s *Service) HasUploadMatcher() bool { return s.uploads != nil }

func (s *Service) SetEventTimeFutureTolerance(d time.Duration) { s.eventTimeTolerance = &d }

func (s *Service) SetIntegrationManager(m IntegrationManager) { s.integrations = m }

func (s *Service) HasIntegrationManager() bool { return s.integrations != nil }

func (s *Service) SetLinkWriter(w LinkWriter) { s.linkWriter = w }

func (s *Service) HasLinkWriter() bool { return s.linkWriter != nil }

func (s *Service) SetLogRing(r *util.LogRing) { s.logs = r }

func (s *Service) SetMCPWriter(w MCPWriter) { s.mcpWriter = w }

func (s *Service) SetMemoryNotesAdmin(n MemoryNotesAdmin) { s.notes = n }

func (s *Service) HasMemoryNotes() bool { return s.notes != nil }

func (s *Service) SetCommandEffects(e CommandEffects) { s.effects = e }

func (s *Service) SetPipelineLister(l domain.PipelineLister) { s.pipelines = l }

func (s *Service) SetPipelineDryRunner(r domain.PipelineDryRunner) { s.pipelineDryRun = r }

func (s *Service) SetPipelineAuthor(a domain.PipelineAuthor) { s.pipelineAuthor = a }

func (s *Service) SetPipelineWriter(w domain.PipelineWriter) { s.pipelineWriter = w }

func (s *Service) SetPipelineLifecycle(l domain.PipelineLifecycle) { s.pipelineLifecycle = l }

func (s *Service) SetPlanProposer(p PlanProposer) { s.planProposer = p }

func (s *Service) HasPlanProposer() bool { return s.planProposer != nil }

func (s *Service) SetReadSources(tools ToolCatalog, skills SkillLister, memory MemoryQuerier) {
	s.tools = tools
	s.skills = skills
	s.memory = memory
}

func (s *Service) SetDocumentLister(l memory.DocumentLister) { s.documents = l }

func (s *Service) HasDocumentLister() bool { return s.documents != nil }

func (s *Service) SetMemoryAnswerer(a MemoryAnswerer) { s.answerer = a }

func (s *Service) HasMemoryAnswerer() bool { return s.answerer != nil }

func (s *Service) SetDocumentGetter(g memory.DocumentGetter) { s.docGetter = g }

func (s *Service) SetRoutePreviewer(p RoutePreviewer) { s.routePreview = p }

func (s *Service) SetPrincipalNames(r PrincipalNames) { s.principalNames = r }

func (s *Service) SetPrincipalSchedules(p domain.PrincipalSchedules) { s.schedules = p }

func (s *Service) SetIdentity(idp domain.Authenticator) { s.idp = idp }

func (s *Service) SetLoginObserver(o LoginObserver) { s.loginObs = o }

func (s *Service) SetSnapshotSources(projection *Projection, sessions SessionLister) {
	if projection != nil {
		s.projection = projection
	}
	s.sessions = sessions
}

func (s *Service) SetHandshake(kernelVersion, contractVersion string, capabilities []string) {
	s.kernelVersion = kernelVersion
	s.contractVersion = contractVersion
	s.capabilities = capabilities
}

func (s *Service) SetPlugins(plugins []PluginInfo) { s.plugins = plugins }

func (s *Service) SetMemorySharer(m MemorySharer) { s.sharer = m }

func (s *Service) SetSkillGrants(g SkillGrantAdmin) { s.skillGrants = g }

func (s *Service) HasSkillGrants() bool { return s.skillGrants != nil }

func (s *Service) SetSkillLifecycle(l SkillLifecycleAdmin) { s.skillLifecycle = l }

func (s *Service) HasSkillLifecycle() bool { return s.skillLifecycle != nil }

func (s *Service) SetSteeringSources(hub *ExecutionControlHub, hitl domain.ApprovalHub) {
	s.controls = hub
	s.hitl = hitl
}

func (s *Service) SetPlanSubmitter(p PlanSubmitter) { s.planSubmitter = p }

func (s *Service) HasPlanSubmitter() bool { return s.planSubmitter != nil }

func (s *Service) SetTokenSeriesReader(r domain.TokenSeriesReader) { s.tokenSeries = r }

func (s *Service) HasTokenSeries() bool { return s.tokenSeries != nil }

func (s *Service) SetWatchHandler(h domain.WatchConfigHandler) { s.watches = h }

func (s *Service) SetDeadLetterReader(r domain.WatchDeadLetterReader) { s.deadletters = r }

func (s *Service) SetWatchObservability(m domain.WatchMetricsReader, b domain.WatchBacktester) {
	s.watchMetrics = m
	s.watchBacktest = b
}

func (s *Service) SetDeadLetterRetrier(r DeadLetterRetrier) { s.deadletterRetry = r }

func (s *Service) SetWatchLiveness(streams domain.StreamRegistry, fires domain.WatchFireReader, budget domain.ReactiveBudgetReader) {
	s.streams = streams
	s.watchFires = fires
	s.planeBudget = budget
}

func (s *Service) SetWave1Reads(
	checkpoints CheckpointLister,
	mcp MCPServerLister,
	embedding EmbeddingReporter,
	classifier InputClassifier,
) {
	s.checkpoints = checkpoints
	s.mcpServers = mcp
	s.embedding = embedding
	s.classifier = classifier
}
