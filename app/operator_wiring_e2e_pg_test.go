package app

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ENG-1305 (T-20261113-operator-service-is-wired-through-64-post): the operator
// plane moved from ~56 post-construction Set* calls in startKernelServices to
// composeOperator → operator.New(Deps, Options). This test composes it on a really
// bootstrapped kernel (scratch database) and checks EVERY seam field of the built
// service against the pre-ENG-1305 wiring: the table below replays what each
// Set* call in app.go (core 396c322) stored, under the same conditions. Do not
// "fix" an entry to match new code — it is the oracle.

type opWant struct {
	field string
	// want is the value the old Set* call stored (nil = the field stayed nil).
	want any
	// typeOnly compares the dynamic type only: adapters and func-struct values
	// built inline in app.go have no identity to compare.
	typeOnly bool
}

func isNilAny(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func legacyOperatorWiring(k *Kernel, feed *operator.Spool, proj *operator.Projection) []opWant {
	var w []opWant
	add := func(field string, want any) { w = append(w, opWant{field: field, want: want}) }
	addType := func(field string, want any) { w = append(w, opWant{field: field, want: want, typeOnly: true}) }
	when := func(cond bool, v any) any {
		if cond {
			return v
		}
		return nil
	}
	cfg := k.Config.Execution

	add("feed", feed)
	add("projection", proj)
	add("sessions", k.SessionMgr)
	add("idp", k.Authenticator)
	add("principalNames", when(k.Principals != nil, k.Principals))
	add("loginObs", k.LoginObserver)
	add("audit", k.OperatorAudit)
	add("grants", k.ToolGrants)
	add("controls", k.Server.ControlHub)
	add("hitl", k.Server.ApprovalHub)
	addType("effects", k.OperatorEffects)
	add("sharer", k.MemorySharer)
	add("tools", k.Server.ToolExecutor)
	add("skills", k.Server.SkillRegistry)
	add("memory", k.Memory.QueryService)
	add("documents", k.Documents)
	add("docGetter", k.DocReader)
	add("ingestStatus", k.Server.IngestStatus) // ENG-2143
	add("answerer", when(cfg.Retrieval.AgenticRetrievalEnabled, k.Memory.QueryService))
	add("toolRunner", k.Server.ToolExecutor)
	addType("ingestor", operator.MemoryIngestorFunc(func(context.Context, operator.IngestRequest) (string, error) { return "", nil }))
	um, ok := k.Server.IngestionProcessor.(interface{ UploadReplaceEnabled() bool })
	addType("uploads", when(ok && um.UploadReplaceEnabled(), operator.UploadMatcherFunc(
		func(context.Context, operator.IngestRequest) (domain.DocumentRecord, error) {
			return domain.DocumentRecord{}, nil
		})))
	tol := time.Duration(cfg.Ingestion.EventTimeFutureToleranceMs) * time.Millisecond
	add("eventTimeTolerance", &tol)
	mc := cfg.Ingestion.SourceMetaCaps()
	// ENG-2170 added the label bounds to the same struct (a new field, not a changed seam).
	add("sourceMetaCaps", operator.SourceMetaCaps{Kind: mc.Kind, Title: mc.Title, Author: mc.Author, Speaker: mc.Speaker,
		LabelCount: mc.LabelCount, LabelChars: mc.LabelChars})
	add("watches", k.Server.WatchHandler)
	addType("pipelines", deferredPipelineLister{})
	addType("pipelineDryRun", deferredPipelineDryRunner{})
	addType("pipelineAuthor", deferredPipelineAuthor{})
	addType("pipelineWriter", deferredPipelineWriter{})
	addType("pipelineLifecycle", deferredPipelineLifecycle{})
	add("deadletters", when(k.Server.WatchHandler != nil && k.WatchDeadLetters != nil, k.WatchDeadLetters))
	add("watchMetrics", when(k.WatchMetrics != nil, k.WatchMetrics))
	add("watchBacktest", when(k.WatchMetrics != nil, k.WatchBacktester))
	gk := k.Metabolism != nil && k.Metabolism.Gatekeeper != nil
	add("policy", when(gk, k.PolicyAdmin))
	addType("routePreview", when(gk, routePreviewAdapter{}))
	_, cpOK := k.CheckpointSource.(checkpointSource)
	addType("checkpoints", when(cpOK, checkpointLister{}))
	addType("mcpServers", when(k.MCPRuntime != nil, mcpLister{}))
	addType("embedding", embeddingReporter{})
	addType("classifier", when(k.Server != nil && k.Server.Router != nil, inputClassifier{}))
	add("tokenSeries", when(k.TokenSeries != nil, k.TokenSeries))
	addType("blastRadius", when(k.Registry != nil, blastRadiusEstimator{}))
	addType("planProposer", when(k.Awareness != nil && k.Awareness.Planner != nil, planProposer{}))
	addType("generators", when(k.LLMProvider != nil, generatorRegistry{}))
	addType("configSchema", when(k.ConfigProvenance != nil, configSchemaSource{}))
	cw := k.ConfigProvenance != nil && k.ConfigStore != nil
	addType("configWriter", when(cw, configWriteSource{}))
	addType("secretWriter", when(cw, configWriteSource{}))
	addType("generatorWriter", when(cw, configWriteSource{}))
	addType("mcpWriter", when(cw, configWriteSource{}))
	addType("integrations", when(cw && k.MCPConnector != nil && k.MCPRuntime != nil, &integrationManager{}))
	add("linkWriter", when(k.linkReviewLane != nil, k.linkReviewLane))
	add("notes", when(k.Server != nil && k.Server.MemoryNotes != nil, k.Server.MemoryNotes))
	add("skillLifecycle", when(k.SkillLifecycle != nil, k.SkillLifecycle))
	add("skillGrants", when(k.SkillAccess != nil && k.SkillAccess.HasStore(), k.SkillAccess))
	add("principals", when(k.PrincipalDirectory != nil, k.PrincipalDirectory))
	add("schedules", when(k.Server != nil && k.Server.ScheduledTasks != nil && k.PrincipalSchedules != nil, k.PrincipalSchedules))
	add("streams", when(k.StreamRegistry != nil, k.StreamRegistry))
	add("watchFires", nil)
	add("planeBudget", nil)
	add("facts", when(k.Memory != nil && k.Memory.FactInspector != nil, k.Memory.FactInspector))
	add("factStats", when(k.Memory != nil && k.Memory.FactExtractor != nil, k.Memory.FactExtractor))
	add("derivations", when(k.Memory != nil && k.Memory.Derivations != nil, k.Memory.Derivations)) // ENG-2139, new seam
	add("forget", k.Server.Forgetter)
	add("logs", when(k.Logs != nil, k.Logs))
	add("kernelVersion", KernelVersion)
	add("contractVersion", "0115")
	add("plugins", pluginHandshake(k.PluginStatuses))
	addType("sessionOps", operator.SessionOpsFuncs{})
	addType("planSubmitter", when(k.SessionMgr != nil && k.Server != nil, planSubmitter{}))
	addType("convOps", when(k.ChatTurns != nil && k.Conversations != nil, &conversationOps{}))
	add("deadletterRetry", nil) // never wired in core
	return w
}

func TestPg_ComposeOperatorWiresEverySeamAsBefore(t *testing.T) {
	cfg := retrievalScratchConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	k, err := bootstrapKernel(ctx, cfg, lis, Options{}, nil)
	if err != nil {
		t.Fatalf("bootstrapKernel: %v", err)
	}
	defer shutdownWithin(t, k, 30*time.Second)

	feed := operator.NewSpool(operator.SpoolConfig{})
	proj := operator.NewProjection()
	svc, err := composeOperator(ctx, k, feed, proj)
	if err != nil {
		t.Fatalf("composeOperator on a booted kernel: %v", err)
	}

	covered := map[string]bool{"UnimplementedOperatorConsoleServer": true, "grantsMu": true,
		"logins": true, "unwired": true, "capabilities": true}
	for _, w := range legacyOperatorWiring(k, feed, proj) {
		covered[w.field] = true
		got := fieldAny(svc, w.field)
		switch {
		case isNilAny(w.want):
			if !isNilAny(got) {
				t.Errorf("%s = %T, the pre-ENG-1305 wiring left it nil", w.field, got)
			}
		case isNilAny(got):
			t.Errorf("%s is nil, the pre-ENG-1305 wiring set %T", w.field, w.want)
		case w.typeOnly:
			if reflect.TypeOf(got) != reflect.TypeOf(w.want) {
				t.Errorf("%s = %T, want %T", w.field, got, w.want)
			}
		default:
			if !reflect.DeepEqual(got, w.want) {
				t.Errorf("%s = %#v, want %#v", w.field, got, w.want)
			}
		}
	}
	st := reflect.TypeOf(operator.Service{})
	for i := 0; i < st.NumField(); i++ {
		if !covered[st.Field(i).Name] {
			t.Errorf("Service field %s has no entry in the reference wiring", st.Field(i).Name)
		}
	}

	// The handshake: computed from the same seams. The unconditional strings are
	// all there, and "authored-plans" is advertised exactly when SubmitPlan is
	// wired (T-20261118-operator-handshake-never-advertises-authored: the check
	// used to run before the submitter was decided, so it was never advertised).
	caps := map[string]bool{}
	for _, c := range fieldAny(svc, "capabilities").([]string) {
		caps[c] = true
	}
	for _, c := range []string{"feed", "snapshot", "commands", "steering", "audit", "kernel-logs",
		"tools-read", "tools-manage", "skills-read", "memory-read", "memory-ingest", "tool-exec",
		"tool-approvals", "document-read", "session-lifecycle", "embedding-config", "run-cancel"} {
		if !caps[c] {
			t.Errorf("capability %q missing from the handshake", c)
		}
	}
	if isNilAny(fieldAny(svc, "planSubmitter")) {
		t.Fatal("SubmitPlan is not wired on a booted kernel (SessionMgr and Server exist)")
	}
	if !caps["authored-plans"] {
		t.Error(`"authored-plans" is not advertised although SubmitPlan is wired`)
	}
	if cfg.Execution.Retrieval.AgenticRetrievalEnabled != caps["memory-answer"] {
		t.Errorf("memory-answer advertised = %v with agentic retrieval %v", caps["memory-answer"], cfg.Execution.Retrieval.AgenticRetrievalEnabled)
	}
	if !caps["document-listing"] {
		t.Error("document-listing missing although the document lister is wired")
	}
	if !caps["ingest-status"] {
		t.Error("ingest-status missing although the ingest status reader is wired (ENG-2143)")
	}

	// A kernel whose composition loses a required seam does not boot.
	k.Authenticator = nil
	if _, err := composeOperator(ctx, k, feed, proj); err == nil {
		t.Fatal("composeOperator without an authenticator built an operator plane")
	}
}
