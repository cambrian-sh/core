package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Per-agent Python packages (ADR-0157 D7.5-D7.8, Amendment A1, owner decisions
// 2026-11-18 on ENG-1993): an agent declares the packages it needs beyond the
// kernel-managed base env (AgentManifest.PythonPackages, a profile's
// python_packages); the kernel resolves each to an exact, hash-pinned lock
// against the base, asks an Operator on the approval hub (kind python_package,
// one card per package + exact version, its dependencies listed), and only
// after approval installs the agent's own layer. Agents never run pip.
//
// Ports and entities only; the resolve/install mechanics live in internal/pyenv,
// the rules in internal/pypackages and the durable adapter in
// internal/infrastructure/postgres (PgPythonPackageStore).

// ApprovalKindPythonPackage is the ApprovalRequest.Kind (and default action
// class) of a package install card (D7.8.2). The default policy routes it to
// the operator role with no standing approvals (D7.8.3).
const ApprovalKindPythonPackage = "python_package"

// PythonPackage is one declared package: a PEP 503 project name, one exact
// version (no range, no "==") and optional extras (owner A1-Q5).
type PythonPackage struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Extras  []string `json:"extras,omitempty"`
}

// Spec renders the requirement the resolver gets: name[extra,…]==version.
func (p PythonPackage) Spec() string {
	s := p.Name
	if len(p.Extras) > 0 {
		s += "[" + strings.Join(p.Extras, ",") + "]"
	}
	return s + "==" + p.Version
}

var (
	rePyProject = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	// rePyVersion is a PEP 440 public or local version without operators,
	// wildcards or whitespace: the one exact version a declaration names.
	rePyVersion = regexp.MustCompile(`^[0-9][A-Za-z0-9.+!_-]*$`)
	rePyExtra   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	rePySep     = regexp.MustCompile(`[-_.]+`)
)

// NormalizePythonName is the PEP 503 normalized project name.
func NormalizePythonName(n string) string { return rePySep.ReplaceAllString(strings.ToLower(n), "-") }

// ValidatePythonPackages checks a declaration list with the rules the profile
// validators apply (agentprofile, the SDK's profiles.py): a valid project
// name, an exact version, well-formed extras, no project twice. An
// AGENT_MANIFEST's list is checked with this at spawn, so a manifest cannot
// declare what a profile could not.
func ValidatePythonPackages(pkgs []PythonPackage) error {
	seen := map[string]bool{}
	for _, p := range pkgs {
		if !rePyProject.MatchString(p.Name) {
			return fmt.Errorf("python_packages name %q is not a PEP 508 project name", p.Name)
		}
		if !rePyVersion.MatchString(p.Version) || strings.Contains(p.Version, "*") {
			return fmt.Errorf("python_packages %s: version %q must be one exact version (no range, no '==', no wildcard)", p.Name, p.Version)
		}
		ex := map[string]bool{}
		for _, e := range p.Extras {
			if !rePyExtra.MatchString(e) {
				return fmt.Errorf("python_packages %s: extra %q is malformed", p.Name, e)
			}
			if ex[NormalizePythonName(e)] {
				return fmt.Errorf("python_packages %s: extra %q listed twice", p.Name, e)
			}
			ex[NormalizePythonName(e)] = true
		}
		n := NormalizePythonName(p.Name)
		if seen[n] {
			return fmt.Errorf("python_packages declares %s twice", p.Name)
		}
		seen[n] = true
	}
	return nil
}

// PythonLockEntry is one distribution of a resolved lock: exact version and
// the SHA-256 of every file the pin may install (D7.7.2).
type PythonLockEntry struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Hashes  []string `json:"hashes"`
}

// PythonPackageLock is one declared package's resolution against the base env:
// the distributions its layer adds (the package itself first, then its
// dependencies that the base does not already carry), wheels only.
type PythonPackageLock struct {
	Entries []PythonLockEntry `json:"entries"`
	// Index is where the files come from (the index URL and/or wheelhouse), for
	// the card (D7.8.6).
	Index string `json:"index,omitempty"`
}

// SHA256 is the lock's identity: approving a card approves exactly this set; a
// later resolution with any other version or file asks again (D7.8.1).
func (l PythonPackageLock) SHA256() string {
	parts := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		h := append([]string(nil), e.Hashes...)
		sort.Strings(h)
		parts = append(parts, NormalizePythonName(e.Name)+"=="+e.Version+"|"+strings.Join(h, ","))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte("python-package-lock\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// Python package approval states.
const (
	PythonPackagePending  = "pending"
	PythonPackageApproved = "approved"
	PythonPackageDenied   = "denied"
	PythonPackageExpired  = "expired"
)

// Python package audit actions (python_package_events.action).
const (
	PythonPackageActionRequested     = "requested"
	PythonPackageActionApproved      = "approved"
	PythonPackageActionDenied        = "denied"
	PythonPackageActionExpired       = "expired"
	PythonPackageActionInstalled     = "installed"
	PythonPackageActionReinstalled   = "reinstalled"
	PythonPackageActionInstallFailed = "install_failed"
)

// PythonPackageApproval is one (target, package, version, lock) approval
// record (D7.8.4). Target is an agent id (per-target scope, owner A1-Q9).
type PythonPackageApproval struct {
	ID         string // PythonPackageApprovalID(...) — deterministic (AGENTS.md §2.6)
	Target     string
	Name       string // normalized
	Version    string
	Extras     []string
	LockSHA256 string
	Lock       PythonPackageLock
	// BaseGeneration is the base env generation the lock was resolved against.
	BaseGeneration string
	State          string
	ApprovalID     string // the hub item (the card)
	RequestedBy    string // "profile:<agent>" for a declaration
	RequestedAt    time.Time
	DecidedBy      string
	DecidedAt      time.Time
	Detail         string
}

// PythonPackageApprovalID is the record id of one (target, name, version, lock).
func PythonPackageApprovalID(target, name, version, lockSHA string) string {
	return "ppa-" + approvalHash(target, NormalizePythonName(name), version, lockSHA)
}

// PythonPackageEvent is one append-only audit row.
type PythonPackageEvent struct {
	ID         string
	ApprovalID string // the record id (ppa-…)
	Seq        int64
	Target     string
	Name       string
	Version    string
	Action     string
	Actor      string
	Detail     string
	Generation string // the layer generation an install produced
	At         time.Time
}

// PythonPackageEventID is the audit row id of one record's seq-th event.
func PythonPackageEventID(recordID string, seq int64) string {
	return "ppe-" + approvalHash(recordID, strconv.FormatInt(seq, 10))
}

// ErrPythonPackageNotFound: no record for that id.
var ErrPythonPackageNotFound = errors.New("python package approval: not found")

// PythonPackageStore is the durable record of package approvals and their
// audit trail (D7.8.4). Every changing write appends its event in the same
// transaction.
type PythonPackageStore interface {
	// Open records a new pending request (state pending), or returns the
	// existing record for the same id unchanged (created=false).
	Open(ctx context.Context, a PythonPackageApproval, actor string) (rec PythonPackageApproval, created bool, err error)
	// Decide moves a record to approved, denied or expired.
	Decide(ctx context.Context, id, state, by, detail string, at time.Time) error
	// AttachCard records the card raised for a pending record (approval id).
	AttachCard(ctx context.Context, id, approvalID string) error
	// Reopen puts an expired record back to pending on a new card (the
	// declaration still stands, so the kernel asks again).
	Reopen(ctx context.Context, id, approvalID, actor string, at time.Time) error
	// Get returns one record.
	Get(ctx context.Context, id string) (PythonPackageApproval, error)
	// ForTarget lists a target's records, newest first.
	ForTarget(ctx context.Context, target string) ([]PythonPackageApproval, error)
	// ForPackage lists every target's records of one (name, version), newest
	// first (the card's "already approved for" note, A1-Q9).
	ForPackage(ctx context.Context, name, version string) ([]PythonPackageApproval, error)
	// AppendEvent audits an install, reinstall or failure of a record.
	AppendEvent(ctx context.Context, e PythonPackageEvent) error
	// Events lists a record's audit trail, oldest first.
	Events(ctx context.Context, recordID string) ([]PythonPackageEvent, error)
}
