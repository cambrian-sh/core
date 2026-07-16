package calibration

// Sample is one (agent, self_confidence, verifier_quality) observation, extracted from
// a verified TaskEvent.
type Sample struct {
	AgentID    string
	Confidence float64
	Quality    float64
}

// Model calibrates a bid confidence to expected verifier quality per agent, with
// shrinkage toward a fleet-global curve for agents below the sample threshold — so a
// rarely-seen agent is corrected by the fleet prior, not by n=1 noise.
type Model struct {
	global     *Isotonic
	perAgent   map[string]*Isotonic
	counts     map[string]int
	minSamples int
}

// Fit builds a Model from samples. minSamples is the shrinkage threshold (default 10):
// an agent with fewer observations is blended toward the global curve.
func Fit(samples []Sample, minSamples int) *Model {
	if minSamples <= 0 {
		minSamples = 10
	}
	byAgent := make(map[string][]Sample)
	gx := make([]float64, 0, len(samples))
	gy := make([]float64, 0, len(samples))
	for _, s := range samples {
		byAgent[s.AgentID] = append(byAgent[s.AgentID], s)
		gx = append(gx, s.Confidence)
		gy = append(gy, s.Quality)
	}
	m := &Model{
		global:     FitIsotonic(gx, gy),
		perAgent:   make(map[string]*Isotonic, len(byAgent)),
		counts:     make(map[string]int, len(byAgent)),
		minSamples: minSamples,
	}
	for id, ss := range byAgent {
		xs := make([]float64, len(ss))
		ys := make([]float64, len(ss))
		for i, s := range ss {
			xs[i] = s.Confidence
			ys[i] = s.Quality
		}
		m.perAgent[id] = FitIsotonic(xs, ys)
		m.counts[id] = len(ss)
	}
	return m
}

// Calibrate maps a raw bid confidence to a calibrated one for the agent. An agent with
// >= minSamples uses its own curve; below that it is blended toward the global curve by
// weight n/minSamples; an unknown agent uses the global curve; a nil Model (or no data)
// is the identity.
func (m *Model) Calibrate(agentID string, conf float64) float64 {
	if m == nil {
		return conf
	}
	g := m.global.Predict(conf)
	a, ok := m.perAgent[agentID]
	if !ok {
		return g
	}
	ap := a.Predict(conf)
	n := m.counts[agentID]
	if n >= m.minSamples {
		return ap
	}
	w := float64(n) / float64(m.minSamples)
	return w*ap + (1-w)*g
}

// AgentCount returns the number of agents with a fitted curve (for reporting).
func (m *Model) AgentCount() int {
	if m == nil {
		return 0
	}
	return len(m.perAgent)
}

// Curve returns the (x, y) knots of an agent's fitted curve for offline artifact export;
// nil when the agent is unknown.
func (m *Model) Curve(agentID string) (xs, ys []float64) {
	if m == nil {
		return nil, nil
	}
	iso, ok := m.perAgent[agentID]
	if !ok || iso == nil {
		return nil, nil
	}
	return append([]float64(nil), iso.xs...), append([]float64(nil), iso.ys...)
}

// Agents returns the ids with fitted curves.
func (m *Model) Agents() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.perAgent))
	for id := range m.perAgent {
		out = append(out, id)
	}
	return out
}
