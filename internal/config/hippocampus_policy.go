package config

import "github.com/cambrian-sh/core/domain"

// StaticPolicyProvider implements domain.PolicyProvider by reading from a
// pre-built map of HippocampusPolicy values (ADR-0027).
// Constructed once at startup from ExecutionConfig; safe for concurrent reads.
type StaticPolicyProvider struct {
	policies    map[string]domain.HippocampusPolicy
	defaultName string
}

// NewStaticPolicyProvider constructs a StaticPolicyProvider.
// defaultName must be a key in policies; callers are responsible for validating
// this at startup (config.Validate enforces it).
func NewStaticPolicyProvider(policies map[string]domain.HippocampusPolicy, defaultName string) *StaticPolicyProvider {
	return &StaticPolicyProvider{policies: policies, defaultName: defaultName}
}

// GetPolicy returns the named policy and true, or (zero, false) if unknown.
func (p *StaticPolicyProvider) GetPolicy(name string) (domain.HippocampusPolicy, bool) {
	pol, ok := p.policies[name]
	return pol, ok
}

// DefaultPolicy returns the policy identified by the configured default name.
func (p *StaticPolicyProvider) DefaultPolicy() domain.HippocampusPolicy {
	return p.policies[p.defaultName]
}
