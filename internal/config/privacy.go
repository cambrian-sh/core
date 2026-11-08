package config

// PrivacyConfig is the `privacy` block (ADR-0142 D12).
type PrivacyConfig struct {
	// PrivateByDefault stamps owner:<conversation owner> on everything an agent
	// writes while working a person's conversation (memory, facts, documents,
	// artifacts, tool-written files), so the owner fence makes it readable only
	// by that person; sharing to team/org knowledge is the explicit ShareToTeam
	// publish. Absent ⇒ true (the owner's ADR-0142 D12 decision). false restores
	// the pre-D12 behaviour, where such writes carried only the writing agent's
	// own classification — and could be read by every principal its ceiling
	// admits. Read by app → network.Server.PrivateByDefault.
	PrivateByDefault *bool `json:"private_by_default,omitempty"`
}

// PrivateByDefaultOn resolves the flag (absent ⇒ on).
func (p PrivacyConfig) PrivateByDefaultOn() bool {
	return p.PrivateByDefault == nil || *p.PrivateByDefault
}
