package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// pipelineListers collects the reactive-pipeline read surfaces plugins register.
//
// A SLICE, not a slot. Ingresses come from more than one place — the Ingress
// Studio authors graphs for the ones it creates, and the reactive plane
// describes the data flow of the ones registered under ADR-0090 — and every one
// of them is an ingress. A single slot made the last registrant silently erase
// the others, which is the kind of bug that looks like "those pipelines don't
// exist" rather than like a bug.
var pipelineListers listerSet

type listerSet struct {
	mu   sync.Mutex
	all  []domain.PipelineLister
	seen bool
}

func (s *listerSet) add(l domain.PipelineLister) {
	if l == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = append(s.all, l)
	s.seen = true
}

func (s *listerSet) list() []domain.PipelineLister {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.PipelineLister(nil), s.all...)
}

// any reports whether any plugin contributes pipelines, for the capability.
func (s *listerSet) any() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// deferredPipelineLister resolves the registered listers at CALL time and
// concatenates what they return.
type deferredPipelineLister struct {
	holder *listerSet
}

func (d deferredPipelineLister) ListPipelines(ctx context.Context, armedOnly bool, ingressID string) ([]domain.PipelineSummary, error) {
	var out []domain.PipelineSummary
	for _, l := range d.holder.list() {
		got, err := l.ListPipelines(ctx, armedOnly, ingressID)
		if err != nil {
			// One contributor failing must not blank the panel: the others
			// describe real ingresses, and an operator losing sight of them
			// because an unrelated source is broken is the worse outcome.
			slog.Warn("operator: a pipeline source failed to list", "err", err)
			continue
		}
		out = append(out, got...)
	}
	return preferAuthored(out), nil
}

// ListNodeItems asks every contributor and returns the first non-empty answer.
//
// Concatenating would be wrong here, unlike ListPipelines: a node belongs to
// exactly one pipeline, so two sources answering means one of them is describing
// a pipeline it does not own. Taking the first that HAS the node keeps a derived
// source from padding an authored source's item list with nothing.
func (d deferredPipelineLister) ListNodeItems(ctx context.Context, pipelineID, nodeID string, limit int) ([]domain.NodeItem, error) {
	for _, l := range d.holder.list() {
		got, err := l.ListNodeItems(ctx, pipelineID, nodeID, limit)
		if err != nil {
			slog.Warn("operator: a pipeline source failed to list node items", "err", err)
			continue
		}
		if len(got) > 0 {
			return got, nil
		}
	}
	return nil, nil
}

// preferAuthored collapses rows that describe the same pipeline.
//
// Two kinds of source contribute. One AUTHORS pipelines and reports the revision
// it stored (1 or higher). The other DERIVES a description of an entry point that
// has no authored graph, and reports revision 0 — "nothing was authored".
//
// Both legitimately describe the same ingress during a rollout, and showing both
// is how one ingress appears twice in the console with different node counts. The
// authored row wins, because it is the one that actually runs; the derived row is
// a fallback for an ingress nobody has authored anything for yet.
func preferAuthored(rows []domain.PipelineSummary) []domain.PipelineSummary {
	best := make(map[string]domain.PipelineSummary, len(rows))
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		// One ENTRY POINT, one row.
		//
		// The same ingress can be described twice under two ids: a watch that
		// was migrated keeps its original id, and the chat pipeline generated
		// for the same ingress is `ingress:<id>`. They are the same entry organ
		// — one Telegram bot — and listing both makes a deployment with two bots
		// look like it has five pipelines, which is exactly how it read.
		key := entryKeyOf(r)
		prev, seen := best[key]
		if !seen {
			best[key] = r
			order = append(order, key)
			continue
		}
		if betterRow(r, prev) {
			best[key] = r
		}
	}
	out := make([]domain.PipelineSummary, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	return out
}

// entryKeyOf identifies the ENTRY POINT a row describes, not the row's own id.
//
// A chat pipeline is generated as `ingress:<agent>` while the watch that was
// migrated for the same agent keeps the bare `<agent>`. They are one Telegram
// bot, and keying on the pipeline id treated them as two — which is how a
// deployment with two bots listed five pipelines.
//
// Deliberately narrow: it strips only that one prefix. Keying on the trigger
// reference instead would look tidier and would be wrong, because two different
// watches on one stream are two pipelines and must both be shown.
func entryKeyOf(r domain.PipelineSummary) string {
	return strings.TrimPrefix(r.PipelineID, "ingress:")
}

// betterRow picks which of two descriptions of one entry point to show.
//
// An AUTHORED revision beats a derived one (revision 0 means nothing was
// authored). Between two authored revisions the later wins. And an `ingress`
// trigger beats a `stream` one for the same entry organ, because the chat
// pipeline is the thing that actually handles the turn — the migrated watch is
// the shape it used to have.
func betterRow(a, b domain.PipelineSummary) bool {
	if (a.Revision > 0) != (b.Revision > 0) {
		return a.Revision > 0
	}
	if a.TriggerType != b.TriggerType {
		return a.TriggerType == "ingress"
	}
	return a.Revision > b.Revision
}

// ingressPipelineRetirers removes the pipeline armed for a removed entry organ.
//
// A function rather than an interface: it is one verb, and the plugin that owns
// the pipelines is not otherwise a port.
var ingressPipelineRetirers atomic.Pointer[func(ctx context.Context, agentID string) error]

// ingressDeregistrars holds the write half of the ADR-0090 registry.
//
// Separate from ingressListers because reading who may enter and WITHDRAWING an
// entry are different powers, and a plugin that only needs to list should not
// acquire the second by asking for the first.
var ingressDeregistrars atomic.Pointer[domain.IngressDeregistrar]

// ingressSchemaDeclarers holds the schema half of the ADR-0090 registry
// (ADR-0117): a plugin that owns an entry point declares what its items carry.
var ingressSchemaDeclarers atomic.Pointer[domain.IngressSchemaDeclarer]

// ingressRegistrars holds the CREATE half of the ADR-0090 registry. Its three
// siblings were wired long before it, which is how a deployment ended up able to
// withdraw an entry point it had no way to create.
var ingressRegistrars atomic.Pointer[domain.IngressRegistrar]

// ingressListers holds the ADR-0090 ingress registry a plugin registers.
//
// One slot, unlike the pipeline listers: there is a single registry of record
// for who may enter, and two of them would be a security question, not a
// display one.
var ingressListers atomic.Pointer[domain.IngressLister]

// KernelIngressLister exposes the registered ingresses to a plugin that needs to
// describe them, resolving at CALL time so registration order does not matter.
type KernelIngressLister struct{}

func (KernelIngressLister) ListIngresses(ctx context.Context) ([]domain.IngressRegistration, error) {
	p := ingressListers.Load()
	if p == nil || *p == nil {
		return nil, nil
	}
	return (*p).ListIngresses(ctx)
}

// The per-pipeline surfaces plugins register (contracts 0088/0089/0090).
//
// SETS, not single values — the correction to a design mistake worth recording.
// These were singular on the reasoning that "a read names ONE pipeline, so a
// second contributor could only answer about a pipeline it does not own." That
// is backwards. Naming one pipeline is exactly why every source has to be asked:
// only one of them holds it, and which one is not knowable here.
//
// Two sources contribute today. The reactive engine holds migrated watches and
// generated chat graphs; the Ingress Studio holds what it generates from
// mappings. A console that could read only one of them would show a list of
// pipelines where half refuse to open.
var (
	pipelineDryRunners pipelineSourceSet[domain.PipelineDryRunner]
	pipelineAuthors    pipelineSourceSet[domain.PipelineAuthor]
	pipelineWriters    pipelineSourceSet[domain.PipelineWriter]
	pipelineLifecycles pipelineSourceSet[domain.PipelineLifecycle]
)

// pipelineSourceSet collects the sources registered for one per-pipeline surface.
type pipelineSourceSet[T any] struct {
	mu   sync.Mutex
	list []T
}

func (s *pipelineSourceSet[T]) add(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, v)
}

func (s *pipelineSourceSet[T]) all() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]T(nil), s.list...)
}

// deferredPipelineAuthor offers a read to each source until one recognises it.
type deferredPipelineAuthor struct {
	holder *pipelineSourceSet[domain.PipelineAuthor]
}

func (d deferredPipelineAuthor) GetPipeline(ctx context.Context, pipelineID string, revision int) (domain.PipelineGraph, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineGraph{
			Refused: "this build has no pipeline runtime, so there is no graph to read",
		}, nil
	}
	for _, a := range sources {
		got, err := a.GetPipeline(ctx, pipelineID, revision)
		if errors.Is(err, domain.ErrPipelineNotFound) {
			continue
		}
		if err != nil {
			// A source that is broken must not hide a source that would have
			// answered, so this is logged and the rest are still asked.
			slog.Warn("operator: a pipeline source failed to read", "err", err)
			continue
		}
		return got, nil
	}
	return domain.PipelineGraph{
		Refused: "no pipeline " + pipelineID + " is authored on this kernel",
	}, nil
}

func (d deferredPipelineAuthor) ValidatePipeline(ctx context.Context, graphJSON string) (domain.PipelineValidation, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineValidation{
			Refused: "this build has no pipeline compiler, so it cannot check a graph",
		}, nil
	}
	// Validation is the COMPILER, which every source shares — the VERDICT is
	// identical from all of them. The field projection is not: only the author
	// holding the trigger's capture profile can say which fields arrive, and
	// which author that is depends on plugin registration order. So each source
	// is asked until one answers with the trigger schema RESOLVED, and the
	// first answer stands when none does.
	//
	// The preference keys on FieldsResolved, never on FieldsJSON being
	// non-empty — that check shipped a live defect TWICE: a schema-less author
	// still returns a full projection (every availability a named unknown),
	// which is non-empty JSON, so the editor's picker showed "no schema source"
	// for an ingress pipeline whenever the reactive plugin registered first.
	var fallback *domain.PipelineValidation
	for _, a := range sources {
		got, err := a.ValidatePipeline(ctx, graphJSON)
		if err != nil {
			slog.Warn("operator: a pipeline source failed to validate", "err", err)
			continue
		}
		if got.FieldsResolved {
			return got, nil
		}
		if fallback == nil {
			fallback = &got
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return domain.PipelineValidation{
		Refused: "no pipeline compiler could check this graph",
	}, nil
}

// deferredPipelineDryRunner offers a shadow run to each source in turn.
type deferredPipelineDryRunner struct {
	holder *pipelineSourceSet[domain.PipelineDryRunner]
}

func (d deferredPipelineDryRunner) DryRunPipeline(ctx context.Context, pipelineID string, revision, sampleLimit int) (domain.PipelineDryRun, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineDryRun{
			Refused: "this build has no pipeline runtime, so it cannot dry-run one",
		}, nil
	}
	for _, r := range sources {
		got, err := r.DryRunPipeline(ctx, pipelineID, revision, sampleLimit)
		if errors.Is(err, domain.ErrPipelineNotFound) {
			continue
		}
		if err != nil {
			slog.Warn("operator: a pipeline source failed to dry-run", "err", err)
			continue
		}
		return got, nil
	}
	return domain.PipelineDryRun{
		Refused: "no pipeline " + pipelineID + " is authored on this kernel",
	}, nil
}

// deferredPipelineWriter routes a save to the source that already holds the id.
type deferredPipelineWriter struct {
	holder *pipelineSourceSet[domain.PipelineWriter]
}

func (d deferredPipelineWriter) SavePipeline(ctx context.Context, graphJSON string) (domain.PipelineSaved, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineSaved{
			Refused: "this build cannot author pipelines, so there is nowhere to save one",
		}, nil
	}
	// Route by ownership BEFORE writing. A read can be offered to each source
	// until one recognises the id; a save cannot, because writing to the wrong
	// store is not something the next attempt can undo — the two stores would
	// then disagree about what that pipeline is.
	if id := pipelineIDIn(graphJSON); id != "" {
		for _, w := range sources {
			h, ok := w.(domain.PipelineHolder)
			if !ok {
				continue
			}
			held, err := h.HoldsPipeline(ctx, id)
			if err != nil {
				slog.Warn("operator: a pipeline writer failed an ownership check", "err", err)
				continue
			}
			if held {
				return w.SavePipeline(ctx, graphJSON)
			}
		}
	}
	// Nobody holds it, so it is new. The first registered writer takes it.
	return sources[0].SavePipeline(ctx, graphJSON)
}

// deferredPipelineLifecycle routes a transition to the source holding the id.
type deferredPipelineLifecycle struct {
	holder *pipelineSourceSet[domain.PipelineLifecycle]
}

func (d deferredPipelineLifecycle) TransitionPipeline(ctx context.Context, pipelineID string, revision int, toState string) (domain.PipelineTransitioned, error) {
	sources := d.holder.all()
	if len(sources) == 0 {
		return domain.PipelineTransitioned{
			Refused: "this build has no pipeline registry, so nothing can transition",
		}, nil
	}
	// A transition is a WRITE: route by ownership first, exactly as saves are.
	// Arming in the wrong registry would not fail — it would create a second
	// authority over which revision is live, which is the split-brain D11
	// exists to forbid.
	for _, l := range sources {
		h, ok := l.(domain.PipelineHolder)
		if !ok {
			continue
		}
		held, err := h.HoldsPipeline(ctx, pipelineID)
		if err != nil {
			slog.Warn("operator: a pipeline lifecycle failed an ownership check", "err", err)
			continue
		}
		if held {
			return l.TransitionPipeline(ctx, pipelineID, revision, toState)
		}
	}
	return domain.PipelineTransitioned{
		Refused: "no pipeline " + pipelineID + " is authored on this kernel",
	}, nil
}

// pipelineIDIn reads just the id out of a graph document.
//
// Deliberately not a full unmarshal into a pipeline type: that type lives in the
// premium module, and the composition root routes by identity without needing to
// understand the graph.
func pipelineIDIn(graphJSON string) string {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(graphJSON), &head); err != nil {
		return ""
	}
	return head.ID
}

// turnRouters holds the seam a plugin registers for shaping admitted turns.
var turnRouters atomic.Pointer[domain.TurnRouter]

// deferredTurnRouter resolves the registered router at CALL time.
//
// Always wired, rather than only when a plugin has already registered: whether
// plugin Build runs before or after the inbound service is a boot-order detail,
// and a nil read at wiring time would leave chat permanently unrouted with
// nothing to show for it.
type deferredTurnRouter struct {
	holder *atomic.Pointer[domain.TurnRouter]
}

func (d deferredTurnRouter) RouteTurn(
	ctx context.Context,
	ingressAgentID, conversationID string,
	msg domain.TurnMessage,
	run domain.TurnFunc,
) (bool, error) {
	p := d.holder.Load()
	if p == nil || *p == nil {
		// No router in this build: the turn runs directly, exactly as before.
		return false, nil
	}
	return (*p).RouteTurn(ctx, ingressAgentID, conversationID, msg, run)
}
