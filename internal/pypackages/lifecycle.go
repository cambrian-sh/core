package pypackages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// Lifecycle operations on the per-agent packages (ADR-0157 D7.10, D7.12,
// build stage P5): what the operator plane lists and changes.
//
//   - Packages lists every record with its audit trail and whether the agent's
//     profile declares it;
//   - Remove takes a runtime-requested package out of the agent's layer: the
//     layer is rebuilt without it (the switch is atomic; a running instance
//     keeps its generation and is recycled when idle), then the record moves to
//     removed with the new generation audited. No approval is needed to
//     remove; asking for it again raises a fresh card;
//   - Retry re-runs the agent's package check now: it retries an install that
//     failed after approval (the failure table's "operator can retry") and
//     applies a profile edit to the layer without waiting for a spawn;
//   - GCKeep is what the env's garbage collector must not delete.

// ErrDeclaredPackage refuses removing a package the agent's profile declares:
// the next spawn would install it again, so the removal belongs in the profile.
var ErrDeclaredPackage = errors.New("the package is declared in the agent's profile")

// ErrNoSuchPackage: the agent has no installed runtime package by that name.
var ErrNoSuchPackage = errors.New("no installed runtime package by that name for this agent")

// PackageView is one record as the operator plane shows it.
type PackageView struct {
	Record domain.PythonPackageApproval
	Events []domain.PythonPackageEvent
	// Declared: the agent's profile declares this package (any version).
	Declared bool
	// Runtime: the agent asked for it with request_python_package.
	Runtime bool
}

// Packages lists the records of one target ("" ⇒ every target), newest first,
// each with its audit trail.
func (s *Service) Packages(ctx context.Context, target string) ([]PackageView, error) {
	if s.store == nil {
		return nil, fmt.Errorf("this kernel keeps no durable package approvals (migration 0057_python_package_approvals not applied)")
	}
	var recs []domain.PythonPackageApproval
	var err error
	if target != "" {
		recs, err = s.store.ForTarget(ctx, target)
	} else {
		recs, err = s.store.List(ctx)
	}
	if err != nil {
		return nil, err
	}
	declared := map[string]map[string]bool{} // agent → declared names
	out := make([]PackageView, 0, len(recs))
	for _, r := range recs {
		if _, ok := declared[r.Target]; !ok {
			names := map[string]bool{}
			if s.cfg.Declared != nil {
				for _, p := range s.cfg.Declared(ctx, r.Target) {
					names[domain.NormalizePythonName(p.Name)] = true
				}
			}
			declared[r.Target] = names
		}
		evs, err := s.store.Events(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, PackageView{Record: r, Events: evs, Declared: declared[r.Target][r.Name], Runtime: IsRuntimeRequest(r)})
	}
	return out, nil
}

// Layers is each agent's current layer directory as handed out since boot
// ("" ⇒ checked, no layer).
func (s *Service) Layers() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.delivered))
	for a, d := range s.delivered {
		out[a] = d
	}
	return out
}

// GCKeep returns the current layers and blocks every package check until
// release is called: the collector reads this at the start of a run and calls
// release after its last deletion, so no layer is chosen and deleted at once.
func (s *Service) GCKeep() (layers map[string]string, release func()) {
	s.gcMu.Lock()
	return s.Layers(), s.gcMu.Unlock
}

// Remove takes a runtime-requested package out of the agent's layer (D7.10.3)
// and returns the layer generation built without it ("" ⇒ the agent now has
// no layer). by is the operator principal, reason their stated reason.
func (s *Service) Remove(ctx context.Context, target, name, by, reason string) (string, error) {
	if s.store == nil {
		return "", fmt.Errorf("this kernel keeps no durable package approvals (migration 0057_python_package_approvals not applied)")
	}
	n := domain.NormalizePythonName(strings.TrimSpace(name))
	if target == "" || n == "" {
		return "", fmt.Errorf("target and name are required")
	}
	var decl []domain.PythonPackage
	if s.cfg.Declared != nil {
		decl = s.cfg.Declared(ctx, target)
	}
	for _, p := range decl {
		if domain.NormalizePythonName(p.Name) == n {
			// Cautious choice (needs owner confirmation): an operator command
			// alone would be undone at the next spawn, which re-installs a
			// declared package; the declaration is the thing to change.
			return "", fmt.Errorf("%w: agent %s's profile declares %s; remove it from that profile's python_packages instead (the layer is rebuilt without it, audited)",
				ErrDeclaredPackage, target, p.Spec())
		}
	}
	if s.cfg.Base == nil {
		return "", fmt.Errorf("the managed Python env is not available on this kernel")
	}
	base, err := s.cfg.Base()
	if err != nil {
		return "", fmt.Errorf("the managed Python env is not ready (%w); try again when it is", err)
	}
	unlock := s.lockAgent(target)
	defer unlock()
	recs, err := s.store.ForTarget(ctx, target)
	if err != nil {
		return "", fmt.Errorf("python package approvals unreadable: %w", err)
	}
	var hit []domain.PythonPackageApproval
	for _, r := range recs {
		if r.Name == n && r.State == domain.PythonPackageApproved && IsRuntimeRequest(r) {
			hit = append(hit, r)
		}
	}
	if len(hit) == 0 {
		return "", fmt.Errorf("%w: agent %s, package %s", ErrNoSuchPackage, target, n)
	}
	// Build the layer without it first: the removal is recorded only once the
	// agent's new generation exists (the old one stays current on failure).
	l, err := s.ensureExcluding(ctx, target, decl, base, map[string]bool{n: true})
	if err != nil {
		return "", fmt.Errorf("rebuilding agent %s's layer without %s failed (nothing changed): %w", target, n, err)
	}
	gen := ""
	if l.Dir != "" {
		gen = filepath.Base(l.Dir)
	}
	detail := "removed by operator command: " + reason
	if gen == "" {
		detail += " (the agent now runs on the base env alone)"
	} else {
		detail += " (layer " + gen + " built without it)"
	}
	now := s.cfg.Now().UTC()
	for _, r := range hit {
		if err := s.store.Remove(context.WithoutCancel(ctx), r.ID, by, detail, gen, now); err != nil {
			return gen, fmt.Errorf("agent %s's layer no longer carries %s, but the removal of %s was not recorded: %w", target, n, r.ID, err)
		}
	}
	slog.InfoContext(ctx, "ADR-0157 D7.10: python package removed by an operator", "event", "python_package_removed",
		"agent", target, "package", n, "by", by, "layer", gen)
	return gen, nil
}

// Retry re-runs the agent's package check now and returns the resulting
// layer generation ("" ⇒ no layer): an approved package whose install failed
// is installed, a profile edit is applied, a missing card is raised.
func (s *Service) Retry(ctx context.Context, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("target is required")
	}
	if s.cfg.Base == nil {
		return "", fmt.Errorf("the managed Python env is not available on this kernel")
	}
	base, err := s.cfg.Base()
	if err != nil {
		return "", fmt.Errorf("the managed Python env is not ready (%w); try again when it is", err)
	}
	var decl []domain.PythonPackage
	if s.cfg.Declared != nil {
		decl = s.cfg.Declared(ctx, target)
	}
	l, err := s.Ensure(ctx, target, decl, base)
	if err != nil {
		return "", err
	}
	if l.Dir == "" {
		return "", nil
	}
	return filepath.Base(l.Dir), nil
}

// Collector is the env's GC as the operator plane drives it.
type Collector interface {
	Run(ctx context.Context) pyenv.GCReport
	Last() pyenv.GCReport
}

// Listing is the operator plane's view of the sandbox Python: every package
// record with its audit trail, the generations on disk with their sizes, the
// agents' current layers and the last GC run.
type Listing struct {
	Packages  []PackageView
	Inventory pyenv.Inventory
	Layers    map[string]string
	LastGC    pyenv.GCReport
}

// Admin is the operator plane's seam (internal/substrate/operator
// PythonPackageAdmin): the package service, the env's builder and its GC.
type Admin struct {
	svc *Service
	b   *pyenv.Builder
	gc  Collector
}

// NewAdmin composes the operator seam; every part is required.
func NewAdmin(svc *Service, b *pyenv.Builder, gc Collector) (*Admin, error) {
	if svc == nil || b == nil || gc == nil {
		return nil, fmt.Errorf("pypackages: the operator seam needs the package service, the env builder and its collector")
	}
	return &Admin{svc: svc, b: b, gc: gc}, nil
}

// List is ListPythonPackages: one target's records ("" ⇒ all) plus the env.
func (a *Admin) List(ctx context.Context, target string) (Listing, error) {
	pk, err := a.svc.Packages(ctx, target)
	if err != nil {
		return Listing{}, err
	}
	inv := a.b.Inventory()
	if target != "" {
		kept := inv.Layers[:0:0]
		for _, l := range inv.Layers {
			if l.Agent == target {
				kept = append(kept, l)
			}
		}
		inv.Layers = kept
	}
	layers := a.svc.Layers()
	if target != "" {
		one := map[string]string{}
		if d, ok := layers[target]; ok {
			one[target] = d
		}
		layers = one
	}
	sort.SliceStable(pk, func(i, j int) bool { return pk[i].Record.RequestedAt.After(pk[j].Record.RequestedAt) })
	return Listing{Packages: pk, Inventory: inv, Layers: layers, LastGC: a.gc.Last()}, nil
}

// Remove is RemovePythonPackage.
func (a *Admin) Remove(ctx context.Context, target, name, by, reason string) (string, error) {
	return a.svc.Remove(ctx, target, name, by, reason)
}

// Retry is RetryPythonPackageInstall.
func (a *Admin) Retry(ctx context.Context, target string) (string, error) {
	return a.svc.Retry(ctx, target)
}

// Prune is PrunePythonEnv: one GC run now.
func (a *Admin) Prune(ctx context.Context) pyenv.GCReport { return a.gc.Run(ctx) }
