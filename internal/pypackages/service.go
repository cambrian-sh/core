// Package pypackages holds an agent until the Python packages it declares are
// approved and installed into its own layer (ADR-0157 D7.6-D7.8, Amendment A1
// stage P3; owner decisions 2026-11-18 on ENG-1993):
//
//   - packages are DECLARED (a profile's or AGENT_MANIFEST's python_packages);
//     agents never run pip;
//   - each declared package + exact version is resolved by the kernel against
//     the shared base env into a hash-pinned, wheels-only lock (pyenv);
//   - each lock is put to the Operator role as one python_package card on THE
//     approval hub (async, durable across restarts, no standing approvals),
//     with its dependencies listed; the approval is per target agent (A1-Q9)
//     and recorded with its audit trail (domain.PythonPackageStore);
//   - only when every declared package is approved does the kernel install the
//     agent's layer (hash-checking mode, --no-deps, wheels only) and let the
//     agent start; until then the spawn is refused with the approvals it waits
//     on named, and other agents keep running (A1-Q6);
//   - a rebuild of an approved lock (same agent, package, version and lock)
//     raises no card and is audited as a reinstall (D7.8.5).
//
// The P4 runtime request tool and the P5 lifecycle views build on this.
package pypackages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// ErrNotApproved is wrapped by a refused spawn whose declared packages are not
// all approved (waiting on a card, denied, or no durable approval store).
var ErrNotApproved = errors.New("declared python packages not approved")

// ErrDeclaration is wrapped by a refused spawn whose declaration itself is
// invalid or unusable (malformed, unresolvable, conflicting with the base, a
// non-Python agent).
var ErrDeclaration = errors.New("python_packages declaration refused")

// Env is the kernel-managed env as this service uses it (pyenv.Builder).
type Env interface {
	ResolvePackage(ctx context.Context, base pyenv.Interpreter, pkg domain.PythonPackage) (domain.PythonPackageLock, error)
	BuildLayer(ctx context.Context, req pyenv.LayerRequest) (pyenv.Layer, error)
	FindLayer(agent string, base pyenv.Interpreter, entries []domain.PythonLockEntry) (pyenv.Layer, bool)
}

// Hub is the approval hub as this service uses it (domain.AddressedHub).
type Hub interface {
	Raise(ctx context.Context, req domain.ApprovalRequest) (domain.ApprovalItem, error)
	Get(ctx context.Context, id string) (domain.ApprovalItem, error)
	HandleKind(kind string, fn domain.ApprovalHandler)
}

// Config tunes the service. Every field is optional.
type Config struct {
	// ApprovalTTL bounds an unanswered card (execution.agents.python.
	// approval_ttl_hours, D7.8.7); it then expires fail-closed and the kernel
	// asks again at the agent's next spawn. 0 ⇒ 72 h.
	ApprovalTTL time.Duration
	// Prepare runs on a new layer's empty build directory: the agent's own
	// AppContainer read grant on Windows (sandbox.GrantAgentPrivateRead).
	Prepare func(agentID, dir string) error
	// Base and Declared let an approval install the layer at once, in the
	// background, instead of at the agent's next spawn: the current base
	// generation, and the agent's current declaration. nil ⇒ install at spawn.
	Base     func() (pyenv.Interpreter, error)
	Declared func(ctx context.Context, agentID string) []domain.PythonPackage
	Now      func() time.Time
	// OnLayerChange is told an agent's layer changed (a new generation became its
	// current one): the agent manager marks the agent's live instances stale and
	// recycles each when idle, so the change is usable within one task
	// (ADR-0157 D7.10.2, A1-Q11, P4). nil ⇒ the next natural spawn picks it up.
	OnLayerChange func(agentID string)
	// RequestPoll is how often Request re-reads a pending card while it waits.
	// 0 ⇒ 500 ms.
	RequestPoll time.Duration
}

// Service implements agentmgr.PythonLayers. Construct with New.
type Service struct {
	store domain.PythonPackageStore
	hub   Hub
	env   Env
	cfg   Config

	mu      sync.Mutex
	current map[string]pyenv.Layer // agent → layer of its last successful Ensure ("" Dir: none)
	// delivered is the layer dir each agent was last handed after a
	// successful Ensure ("" = none); absent until the first. A different one
	// later is a layer change (OnLayerChange).
	delivered map[string]string
	agentMu   map[string]*sync.Mutex
	resolved  map[string]resolution // agent|pkg|base generation → resolution (one per process)

	wg sync.WaitGroup // background installs started by approvals; Stop waits
}

type resolution struct {
	lock   domain.PythonPackageLock
	inBase bool
}

// New builds the service and registers the python_package kind handler on the
// hub. hub and env are required (AGENTS.md §2.3). store nil means no durable
// approvals (migration 0057 not applied): an agent that declares packages is
// refused with that reason; agents that declare none are unaffected.
func New(store domain.PythonPackageStore, hub Hub, env Env, cfg Config) (*Service, error) {
	if hub == nil {
		return nil, fmt.Errorf("pypackages: an approval hub is required")
	}
	if env == nil {
		return nil, fmt.Errorf("pypackages: the managed env is required")
	}
	if cfg.ApprovalTTL <= 0 {
		cfg.ApprovalTTL = 72 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{store: store, hub: hub, env: env, cfg: cfg,
		current: map[string]pyenv.Layer{}, delivered: map[string]string{}, agentMu: map[string]*sync.Mutex{}, resolved: map[string]resolution{}}
	hub.HandleKind(domain.ApprovalKindPythonPackage, s.onApproved)
	return s, nil
}

// Stop waits for background installs started by approvals.
func (s *Service) Stop() { s.wg.Wait() }

func (s *Service) lockAgent(agent string) func() {
	s.mu.Lock()
	m := s.agentMu[agent]
	if m == nil {
		m = &sync.Mutex{}
		s.agentMu[agent] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Current is the launch path's view (no I/O): the layer the agent's last
// Ensure produced on this base generation, a zero Layer when it declares none,
// or an error when the base generation changed since (the spawn is refused and
// the next one re-checks).
func (s *Service) Current(agentID string, base pyenv.Interpreter) (pyenv.Layer, error) {
	s.mu.Lock()
	l, ok := s.current[agentID]
	s.mu.Unlock()
	if !ok || l.Dir == "" {
		return pyenv.Layer{}, nil
	}
	if filepath.Clean(l.Base) != filepath.Clean(base.Home) {
		return pyenv.Layer{}, fmt.Errorf("agent %s: the base env generation changed since its package layer was checked; the next spawn re-checks it", agentID)
	}
	return l, nil
}

func (s *Service) setCurrent(agent string, l pyenv.Layer) {
	s.mu.Lock()
	s.current[agent] = l
	s.mu.Unlock()
}

// outcome of one declared package.
type outcome struct {
	rec     domain.PythonPackageApproval
	state   string // approved | pending | denied | base (satisfied by the base env)
	card    string
	waitWhy string
}

// Ensure is the pre-spawn check (agentmgr.PythonLayers): nil layer and nil
// error when the agent declares nothing; the built layer when every declared
// package is approved; else an error wrapping ErrNotApproved or ErrDeclaration
// that names what the agent waits on. A missing card is raised here; an
// approved set is installed here when no background install did it yet.
func (s *Service) Ensure(ctx context.Context, agentID string, decl []domain.PythonPackage, base pyenv.Interpreter) (pyenv.Layer, error) {
	unlock := s.lockAgent(agentID)
	defer unlock()
	return s.ensureLocked(ctx, agentID, decl, base)
}

// ensureLocked is Ensure under the agent's lock.
func (s *Service) ensureLocked(ctx context.Context, agentID string, decl []domain.PythonPackage, base pyenv.Interpreter) (pyenv.Layer, error) {
	// P4: packages an Operator approved for this agent at runtime
	// (request_python_package) belong to its layer like declared ones, so an
	// approval after the tool's window applies to the agent's later runs.
	runtime := s.runtimeApproved(ctx, agentID, base)
	if len(decl) == 0 && len(runtime) == 0 {
		s.setCurrent(agentID, pyenv.Layer{})
		s.noteChange(agentID, pyenv.Layer{})
		return pyenv.Layer{}, nil
	}
	s.setCurrent(agentID, pyenv.Layer{}) // nothing launches on an old answer while this one is pending
	if err := domain.ValidatePythonPackages(decl); err != nil {
		return pyenv.Layer{}, fmt.Errorf("agent %s not started: %w: %v", agentID, ErrDeclaration, err)
	}
	if s.store == nil {
		return pyenv.Layer{}, fmt.Errorf("agent %s not started: %w: this kernel keeps no durable package approvals (migration 0057_python_package_approvals not applied; run `orchestrator migrate up`)", agentID, ErrNotApproved)
	}
	if base.Home == "" {
		return pyenv.Layer{}, fmt.Errorf("agent %s not started: %w: the base env generation is unknown", agentID, ErrDeclaration)
	}
	recs, err := s.store.ForTarget(ctx, agentID)
	if err != nil {
		return pyenv.Layer{}, fmt.Errorf("agent %s not started: python package approvals unreadable (fail closed): %w", agentID, err)
	}
	var outs []outcome
	for _, pkg := range decl {
		o, err := s.ensurePackage(ctx, agentID, pkg, base, recs, "profile:"+agentID, "")
		if err != nil {
			return pyenv.Layer{}, err
		}
		outs = append(outs, o)
	}
	var waiting, denied []string
	var approved []domain.PythonPackageApproval
	var declared []string
	for i, o := range outs {
		switch o.state {
		case domain.PythonPackageApproved:
			approved = append(approved, o.rec)
			declared = append(declared, domain.NormalizePythonName(decl[i].Name))
		case domain.PythonPackageDenied:
			by := o.rec.DecidedBy
			if by == "" {
				by = "an operator"
			}
			denied = append(denied, fmt.Sprintf("%s (denied by %s, approval %s)", decl[i].Spec(), by, o.rec.ApprovalID))
		case domain.PythonPackagePending:
			waiting = append(waiting, fmt.Sprintf("%s (approval %s%s)", decl[i].Spec(), o.card, o.waitWhy))
		}
	}
	if len(denied) > 0 || len(waiting) > 0 {
		var parts []string
		if len(waiting) > 0 {
			parts = append(parts, "waiting for an Operator to approve "+strings.Join(waiting, ", "))
		}
		if len(denied) > 0 {
			parts = append(parts, "denied: "+strings.Join(denied, ", ")+" (remove it from python_packages or ask an operator)")
		}
		err := fmt.Errorf("agent %s not started: %w: %s (ADR-0157 D7.6)", agentID, ErrNotApproved, strings.Join(parts, "; "))
		slog.WarnContext(ctx, "ADR-0157 D7: agent held until its declared python packages are approved", "agent", agentID,
			"waiting", waiting, "denied", denied)
		return pyenv.Layer{}, err
	}
	for _, r := range runtime {
		if !containsName(declared, r.Name) {
			approved = append(approved, r)
			declared = append(declared, r.Name)
		}
	}
	if len(approved) == 0 {
		// Every declared package is in the base env already: no layer.
		s.setCurrent(agentID, pyenv.Layer{})
		s.noteChange(agentID, pyenv.Layer{})
		return pyenv.Layer{}, nil
	}
	entries, err := union(approved)
	if err != nil {
		return pyenv.Layer{}, fmt.Errorf("agent %s not started: %w: %v", agentID, ErrDeclaration, err)
	}
	sort.Strings(declared)
	if l, ok := s.env.FindLayer(agentID, base, entries); ok {
		s.setCurrent(agentID, l)
		s.noteChange(agentID, l)
		return l, nil
	}
	l, err := s.build(ctx, agentID, base, entries, declared, approved)
	if err != nil {
		return pyenv.Layer{}, fmt.Errorf("agent %s not started: installing its approved python packages failed: %w", agentID, err)
	}
	s.setCurrent(agentID, l)
	s.noteChange(agentID, l)
	return l, nil
}

// deliveredLayer is the layer dir the agent was last handed ("" none) and
// whether it was handed one at all.
func (s *Service) deliveredLayer(agent string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.delivered[agent]
	return d, ok
}

// noteChange records the layer the agent is handed now and tells
// OnLayerChange when it differs from the one it was handed before. The first
// answer is not a change: no instance runs on a layer the kernel never handed
// out.
func (s *Service) noteChange(agent string, now pyenv.Layer) {
	dir := ""
	if now.Dir != "" {
		dir = filepath.Clean(now.Dir)
	}
	s.mu.Lock()
	old, known := s.delivered[agent]
	s.delivered[agent] = dir
	s.mu.Unlock()
	if s.cfg.OnLayerChange != nil && known && old != dir {
		s.cfg.OnLayerChange(agent)
	}
}

// runtimeApproved lists the agent's approved records that a runtime request
// opened (requested_by agent:…) on this base generation.
func (s *Service) runtimeApproved(ctx context.Context, agentID string, base pyenv.Interpreter) []domain.PythonPackageApproval {
	if s.store == nil || base.Home == "" {
		return nil
	}
	recs, err := s.store.ForTarget(ctx, agentID)
	if err != nil {
		return nil // Ensure reads the store again and fails closed with the reason
	}
	baseGen := filepath.Base(base.Home)
	seen := map[string]bool{}
	var out []domain.PythonPackageApproval
	for _, r := range recs { // newest first: the newest approved version of a name wins
		if r.State != domain.PythonPackageApproved || r.BaseGeneration != baseGen || !IsRuntimeRequest(r) || seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		out = append(out, r)
	}
	return out
}

// IsRuntimeRequest reports whether a record was opened by an agent's runtime
// request (request_python_package) rather than a declaration.
func IsRuntimeRequest(r domain.PythonPackageApproval) bool {
	return strings.HasPrefix(r.RequestedBy, RuntimeRequester)
}

// RuntimeRequester prefixes requested_by for a runtime request.
const RuntimeRequester = "agent:"

func containsName(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

// build installs the layer and audits it on every record it contains.
func (s *Service) build(ctx context.Context, agentID string, base pyenv.Interpreter, entries []domain.PythonLockEntry,
	declared []string, recs []domain.PythonPackageApproval) (pyenv.Layer, error) {
	var prepare func(string) error
	if s.cfg.Prepare != nil {
		prepare = func(dir string) error { return s.cfg.Prepare(agentID, dir) }
	}
	l, err := s.env.BuildLayer(ctx, pyenv.LayerRequest{Agent: agentID, Base: base, Entries: entries, Declared: declared, Prepare: prepare})
	now := s.cfg.Now().UTC()
	for _, r := range recs {
		e := domain.PythonPackageEvent{ApprovalID: r.ID, Target: r.Target, Name: r.Name, Version: r.Version, Actor: "system:kernel", At: now}
		if err != nil {
			e.Action, e.Detail = domain.PythonPackageActionInstallFailed, err.Error()
		} else {
			e.Action, e.Generation = domain.PythonPackageActionInstalled, filepath.Base(l.Dir)
			if s.installedBefore(ctx, r.ID) {
				e.Action = domain.PythonPackageActionReinstalled
			}
			e.Detail = "layer " + l.ID[:16] + " on base " + filepath.Base(base.Home)
		}
		// The audit row is the record of the install (D7.8.4); a write that fails
		// is logged with the record id rather than dropped silently (§3.2).
		if aerr := s.store.AppendEvent(context.WithoutCancel(ctx), e); aerr != nil {
			slog.WarnContext(ctx, "ADR-0157 D7: python package install audit not recorded", "record", r.ID, "action", e.Action, "err", aerr)
		}
	}
	if err != nil {
		return pyenv.Layer{}, err
	}
	slog.InfoContext(ctx, "ADR-0157 D7: agent package layer installed", "agent", agentID, "layer", l.Dir,
		"distributions", len(entries), "pre_granted", l.Granted)
	return l, nil
}

func (s *Service) installedBefore(ctx context.Context, recordID string) bool {
	evs, err := s.store.Events(ctx, recordID)
	if err != nil {
		return false
	}
	for _, e := range evs {
		if e.Action == domain.PythonPackageActionInstalled || e.Action == domain.PythonPackageActionReinstalled {
			return true
		}
	}
	return false
}

func sameExtras(a, b []string) bool {
	norm := func(x []string) string {
		out := make([]string, len(x))
		for i, v := range x {
			out[i] = domain.NormalizePythonName(v)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	return norm(a) == norm(b)
}

// ensurePackage decides one declared package: an existing record on this base
// generation is acted on by its state; otherwise the package is resolved and
// its lock's record looked up (an approved lock is reused even from an older
// base generation, D7.10.4) or opened with a new card.
func (s *Service) ensurePackage(ctx context.Context, agentID string, pkg domain.PythonPackage, base pyenv.Interpreter,
	recs []domain.PythonPackageApproval, requestedBy, rationale string) (outcome, error) {
	baseGen := filepath.Base(base.Home)
	name := domain.NormalizePythonName(pkg.Name)
	for _, r := range recs { // newest first
		if r.Name == name && r.Version == pkg.Version && sameExtras(r.Extras, pkg.Extras) && r.BaseGeneration == baseGen {
			return s.act(ctx, r)
		}
	}
	res, err := s.resolve(ctx, agentID, pkg, base)
	if err != nil {
		return outcome{}, err
	}
	if res.inBase {
		return outcome{state: "base"}, nil
	}
	lockSHA := res.lock.SHA256()
	id := domain.PythonPackageApprovalID(agentID, name, pkg.Version, lockSHA)
	if r, err := s.store.Get(ctx, id); err == nil {
		return s.act(ctx, r)
	} else if !errors.Is(err, domain.ErrPythonPackageNotFound) {
		return outcome{}, fmt.Errorf("agent %s not started: python package approval %s unreadable (fail closed): %w", agentID, id, err)
	}
	rec, created, err := s.store.Open(ctx, domain.PythonPackageApproval{
		ID: id, Target: agentID, Name: name, Version: pkg.Version, Extras: append([]string(nil), pkg.Extras...),
		LockSHA256: lockSHA, Lock: res.lock, BaseGeneration: baseGen, State: domain.PythonPackagePending,
		RequestedBy: requestedBy, RequestedAt: s.cfg.Now().UTC(), Detail: rationale,
	}, requestedBy)
	if err != nil {
		return outcome{}, fmt.Errorf("agent %s not started: python package request for %s not recorded: %w", agentID, pkg.Spec(), err)
	}
	if !created {
		return s.act(ctx, rec)
	}
	return s.raiseFor(ctx, rec, false)
}

// resolve resolves one package once per process per base generation.
func (s *Service) resolve(ctx context.Context, agentID string, pkg domain.PythonPackage, base pyenv.Interpreter) (resolution, error) {
	key := agentID + "|" + pkg.Spec() + "|" + filepath.Clean(base.Home)
	s.mu.Lock()
	r, ok := s.resolved[key]
	s.mu.Unlock()
	if ok {
		return r, nil
	}
	lock, err := s.env.ResolvePackage(ctx, base, pkg)
	switch {
	case err == nil:
		r = resolution{lock: lock}
	case errors.Is(err, pyenv.ErrInBase) && !errors.Is(err, pyenv.ErrUnresolvable):
		r = resolution{inBase: true}
		slog.InfoContext(ctx, "ADR-0157 D7: declared python package is already in the base env; no layer or card", "agent", agentID, "package", pkg.Spec())
	default:
		// Unresolvable: no card is raised (failure table); the agent stays stopped
		// with the reason named.
		return resolution{}, fmt.Errorf("agent %s not started: %w: python package %s cannot be resolved against the base env: %v", agentID, ErrDeclaration, pkg.Spec(), err)
	}
	s.mu.Lock()
	s.resolved[key] = r
	s.mu.Unlock()
	return r, nil
}

// act turns a record's state into an outcome, reconciling a pending record
// with its card (an answer recorded while this process was down, a denial,
// an expiry) and asking again when the card expired.
func (s *Service) act(ctx context.Context, r domain.PythonPackageApproval) (outcome, error) {
	switch r.State {
	case domain.PythonPackageApproved, domain.PythonPackageDenied:
		return outcome{rec: r, state: r.State, card: r.ApprovalID}, nil
	case domain.PythonPackageExpired:
		return s.raiseFor(ctx, r, true)
	}
	if r.ApprovalID == "" {
		return s.raiseFor(ctx, r, false) // opened, but the card was never raised (crash in between)
	}
	it, err := s.hub.Get(ctx, r.ApprovalID)
	if err != nil {
		return outcome{rec: r, state: domain.PythonPackagePending, card: r.ApprovalID, waitWhy: "; card unreadable: " + err.Error()}, nil
	}
	now := s.cfg.Now().UTC()
	switch it.State {
	case domain.ApprovalPending:
		return outcome{rec: r, state: domain.PythonPackagePending, card: r.ApprovalID}, nil
	case domain.ApprovalApproved:
		if err := s.store.Decide(ctx, r.ID, domain.PythonPackageApproved, it.ResolvedBy, it.Detail, now); err != nil {
			return outcome{}, fmt.Errorf("python package approval %s not recorded: %w", r.ID, err)
		}
		r.State, r.DecidedBy = domain.PythonPackageApproved, it.ResolvedBy
		return outcome{rec: r, state: r.State, card: r.ApprovalID}, nil
	case domain.ApprovalDenied:
		if err := s.store.Decide(ctx, r.ID, domain.PythonPackageDenied, it.ResolvedBy, it.Detail, now); err != nil {
			return outcome{}, fmt.Errorf("python package denial %s not recorded: %w", r.ID, err)
		}
		r.State, r.DecidedBy = domain.PythonPackageDenied, it.ResolvedBy
		return outcome{rec: r, state: r.State, card: r.ApprovalID}, nil
	}
	// expired, withdrawn, unroutable…: fail closed for this card, ask again.
	if err := s.store.Decide(ctx, r.ID, domain.PythonPackageExpired, "", "card "+r.ApprovalID+" "+string(it.State)+": "+it.Detail, now); err != nil {
		return outcome{}, fmt.Errorf("python package expiry %s not recorded: %w", r.ID, err)
	}
	return s.raiseFor(ctx, r, true)
}

// raiseFor raises the record's card and records it (reopen=true puts an
// expired record back to pending on the new card).
func (s *Service) raiseFor(ctx context.Context, r domain.PythonPackageApproval, reopen bool) (outcome, error) {
	others, _ := s.store.ForPackage(ctx, r.Name, r.Version) // best-effort note on the card
	it, err := s.hub.Raise(ctx, domain.ApprovalRequest{
		Kind:        domain.ApprovalKindPythonPackage,
		AgentID:     r.Target,
		ToolName:    "python_package:" + r.Name + "==" + r.Version,
		Resource:    r.ID,
		ArgsPreview: CardPreview(r),
		Object:      CardText(r, others),
		Timeout:     s.cfg.ApprovalTTL,
	})
	if err != nil {
		return outcome{}, fmt.Errorf("agent %s not started: the approval card for %s==%s could not be raised: %w", r.Target, r.Name, r.Version, err)
	}
	if it.State != domain.ApprovalPending && it.State != domain.ApprovalApproved {
		return outcome{}, fmt.Errorf("agent %s not started: %w: the approval card for %s==%s is %s: %s", r.Target, ErrNotApproved, r.Name, r.Version, it.State, it.Detail)
	}
	if reopen {
		err = s.store.Reopen(ctx, r.ID, it.ID, "profile:"+r.Target, s.cfg.Now().UTC())
	} else {
		err = s.store.AttachCard(ctx, r.ID, it.ID)
	}
	if err != nil {
		return outcome{}, fmt.Errorf("agent %s not started: card %s for %s==%s not recorded: %w", r.Target, it.ID, r.Name, r.Version, err)
	}
	slog.InfoContext(ctx, "ADR-0157 D7: python package approval raised", "event", "python_package_requested",
		"agent", r.Target, "package", r.Name+"=="+r.Version, "approval", it.ID, "record", r.ID, "distributions", len(r.Lock.Entries))
	r.ApprovalID = it.ID
	return outcome{rec: r, state: domain.PythonPackagePending, card: it.ID}, nil
}

// onApproved is the python_package kind handler: the hub runs it when an
// Operator approves a card, also after a restart. It records the approval and,
// when the service can see the agent's declaration, installs the layer in the
// background so the agent's next spawn finds it ready.
func (s *Service) onApproved(ctx context.Context, it domain.ApprovalItem) error {
	if s.store == nil {
		return fmt.Errorf("no durable package approval store")
	}
	if it.Resource == "" {
		return fmt.Errorf("python_package card %s names no approval record", it.ID)
	}
	if err := s.store.Decide(ctx, it.Resource, domain.PythonPackageApproved, it.ResolvedBy, it.Detail, s.cfg.Now().UTC()); err != nil {
		return err
	}
	slog.InfoContext(ctx, "ADR-0157 D7: python package approved", "event", "python_package_approved",
		"agent", it.AgentID, "package", strings.TrimPrefix(it.ToolName, "python_package:"), "approval", it.ID, "by", it.ResolvedBy)
	if s.cfg.Base == nil || s.cfg.Declared == nil || it.AgentID == "" {
		return nil
	}
	agent := it.AgentID
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		bctx := context.WithoutCancel(ctx)
		base, err := s.cfg.Base()
		if err != nil {
			return // the next spawn installs it once the env is ready
		}
		if _, err := s.Ensure(bctx, agent, s.cfg.Declared(bctx, agent), base); err != nil {
			slog.InfoContext(bctx, "ADR-0157 D7: package layer not installed yet after an approval", "agent", agent, "reason", err)
		}
	}()
	return nil
}

// union merges approved locks into one install set; two locks that pin one
// distribution at different versions cannot share a layer.
func union(recs []domain.PythonPackageApproval) ([]domain.PythonLockEntry, error) {
	byName := map[string]domain.PythonLockEntry{}
	from := map[string]string{}
	for _, r := range recs {
		for _, e := range r.Lock.Entries {
			n := domain.NormalizePythonName(e.Name)
			if prev, ok := byName[n]; ok {
				if prev.Version != e.Version {
					return nil, fmt.Errorf("%s==%s needs %s==%s but %s needs %s==%s; declare versions that agree",
						r.Name, r.Version, n, e.Version, from[n], n, prev.Version)
				}
				prev.Hashes = mergeHashes(prev.Hashes, e.Hashes)
				byName[n] = prev
				continue
			}
			byName[n] = domain.PythonLockEntry{Name: n, Version: e.Version, Hashes: append([]string(nil), e.Hashes...)}
			from[n] = r.Name + "==" + r.Version
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]domain.PythonLockEntry, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out, nil
}

// mergeHashes keeps only hashes both approved locks allow: a file is
// installable only if every approval that names its distribution covers it.
func mergeHashes(a, b []string) []string {
	in := map[string]bool{}
	for _, h := range b {
		in[h] = true
	}
	var out []string
	for _, h := range a {
		if in[h] {
			out = append(out, h)
		}
	}
	return out
}

// CardPreview is the card's one-line summary (ApprovalItem.ArgsPreview).
func CardPreview(r domain.PythonPackageApproval) string {
	deps := len(r.Lock.Entries) - 1
	if deps < 0 {
		deps = 0
	}
	spec := r.Name + "==" + r.Version
	if len(r.Extras) > 0 {
		spec = r.Name + "[" + strings.Join(r.Extras, ",") + "]==" + r.Version
	}
	how := "declared in its python_packages"
	if IsRuntimeRequest(r) {
		how = "requested at runtime by the agent (request_python_package)"
	}
	return fmt.Sprintf("agent %s needs Python package %s (+%d dependencies; wheels only, hash-pinned) in its own layer; %s",
		r.Target, spec, deps, how)
}

// CardText is the card's full content (ApprovalItem.Object, D7.8.6): what is
// approved, from where, every distribution with its version and the SHA-256 of
// each file it may install, and whether another agent has this version
// approved already (A1-Q9).
func CardText(r domain.PythonPackageApproval, others []domain.PythonPackageApproval) string {
	var b strings.Builder
	fmt.Fprintf(&b, "target: agent %s (approval is for this agent only)\n", r.Target)
	fmt.Fprintf(&b, "package: %s==%s", r.Name, r.Version)
	if len(r.Extras) > 0 {
		fmt.Fprintf(&b, " extras [%s]", strings.Join(r.Extras, ", "))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "requested by: %s\n", r.RequestedBy)
	if IsRuntimeRequest(r) && r.Detail != "" && r.State == domain.PythonPackagePending {
		fmt.Fprintf(&b, "the agent's rationale (its own words, not verified): %s\n", r.Detail)
	}
	if r.Lock.Index != "" {
		fmt.Fprintf(&b, "source: %s (wheels only, no source builds)\n", r.Lock.Index)
	}
	fmt.Fprintf(&b, "base env generation: %s (a layer cannot change a base package)\n", r.BaseGeneration)
	fmt.Fprintf(&b, "lock sha256: %s\n", r.LockSHA256)
	fmt.Fprintf(&b, "distributions to install (%d), each pinned to these file hashes:\n", len(r.Lock.Entries))
	for _, e := range r.Lock.Entries {
		fmt.Fprintf(&b, "  %s==%s\n", e.Name, e.Version)
		for _, h := range e.Hashes {
			fmt.Fprintf(&b, "    sha256:%s\n", h)
		}
	}
	var already []string
	for _, o := range others {
		if o.Target != r.Target && o.State == domain.PythonPackageApproved {
			note := o.Target
			if o.LockSHA256 == r.LockSHA256 {
				note += " (same lock)"
			}
			already = append(already, note)
		}
	}
	if len(already) > 0 {
		sort.Strings(already)
		fmt.Fprintf(&b, "already approved at this version for: %s\n", strings.Join(already, ", "))
	}
	return b.String()
}
