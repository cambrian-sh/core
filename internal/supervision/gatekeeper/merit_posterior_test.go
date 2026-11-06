package gatekeeper

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0139 D6 — outcome-based merit as a Beta posterior per (agent, capability).

func posteriorCfg(mode string) config.ExecutionConfig {
	cfg := config.DefaultConfig().Execution
	cfg.Gatekeeper.MeritMode = mode
	cfg.Gatekeeper.GatekeeperMaxCandidates = 0
	return cfg
}

func ev(s, f float64) domain.OutcomeEvidence {
	return domain.OutcomeEvidence{VerifiedSuccess: s, VerifiedFailure: f}
}

func rankIDs(cs []domain.ScoredCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Agent.ID
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func slateGatekeeper(cfg config.ExecutionConfig, profiles map[string]*domain.AgentProfile, opts ...GatekeeperOption) *Gatekeeper {
	var agents []domain.AgentDefinition
	byKey := map[string]*domain.AgentProfile{}
	for id, p := range profiles {
		agents = append(agents, domain.AgentDefinition{ID: id})
		byKey[id+":"] = p
	}
	// Deterministic registry order, so a stable sort's tie order is reproducible.
	for i := 1; i < len(agents); i++ {
		for j := i; j > 0 && agents[j].ID < agents[j-1].ID; j-- {
			agents[j], agents[j-1] = agents[j-1], agents[j]
		}
	}
	opts = append([]GatekeeperOption{WithProfiles(&mockGatekeeperProfileReader{profiles: byKey})}, opts...)
	return NewGatekeeper(newMockAgentDeclarationSource(agents, nil), cfg, opts...)
}

// The quantile routine must be exact on the closed forms Beta(a,1) and Beta(1,b).
func TestBetaQuantile_ClosedForms(t *testing.T) {
	for _, p := range []float64{0.05, 0.1, 0.5, 0.9} {
		if got := betaQuantile(p, 1, 1); math.Abs(got-p) > 1e-9 {
			t.Errorf("Beta(1,1) q%.2f = %v, want %v", p, got, p)
		}
		for _, k := range []float64{2, 4, 9} {
			if got, want := betaQuantile(p, k, 1), math.Pow(p, 1/k); math.Abs(got-want) > 1e-9 {
				t.Errorf("Beta(%v,1) q%.2f = %v, want %v", k, p, got, want)
			}
			if got, want := betaQuantile(p, 1, k), 1-math.Pow(1-p, 1/k); math.Abs(got-want) > 1e-9 {
				t.Errorf("Beta(1,%v) q%.2f = %v, want %v", k, p, got, want)
			}
		}
	}
}

// Legacy is unchanged by D6: golden scores of the pre-D6 formula, including the
// slate-mean latency fill, and outcome evidence on a profile must not move them.
func TestScoreMerit_LegacyGolden(t *testing.T) {
	cfg := posteriorCfg(config.MeritModeLegacy)
	cfg.Plan.ContextGrowthK = 0.001
	profiles := map[string]*domain.AgentProfile{
		// 0.4·0.8 + 0.4·0.6 + 0.2/200 − 0.15·(0.005/0.01) = 0.486
		"a": {SuccessRate: 0.8, TrustScore: 0.6, NetworkLatencyMedianMs: 100, ComputationLatencyMedianMs: 100,
			ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.005}},
		// 0.4·0.5 + 0.4·0.5 + slate-mean latency term (0.001 + 0.004)/2 = 0.4025;
		// the outcome evidence is ignored under legacy.
		"b": {SuccessRate: 0.5, TrustScore: 0.5, Outcomes: ev(100, 0),
			CapabilityStats: map[string]domain.CapabilityStat{"x": {Outcomes: ev(50, 0)}}},
		// 0.4 + 0.4 + 0.2/50 − 0.15·1 (cost clamped) = 0.654
		"c": {SuccessRate: 1, TrustScore: 1, NetworkLatencyMedianMs: 50,
			ModelMetrics: &domain.ModelMetrics{AvgCostPerTask: 0.02}},
	}
	want := map[string]float64{"a": 0.486, "b": 0.4025, "c": 0.654}

	// An unset mode reads as posterior_lcb since 2026-11-05 (config test), so only the
	// explicit legacy mode must reproduce the golden scores.
	for _, mode := range []string{config.MeritModeLegacy} {
		cfg.Gatekeeper.MeritMode = mode
		got, err := slateGatekeeper(cfg, profiles).FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
		if err != nil {
			t.Fatalf("FindCandidates: %v", err)
		}
		if ids := rankIDs(got); !equalIDs(ids, []string{"c", "a", "b"}) {
			t.Fatalf("mode %q order = %v, want [c a b]", mode, ids)
		}
		for _, c := range got {
			if math.Abs(c.Score-want[c.Agent.ID]) > 1e-12 {
				t.Errorf("mode %q %s = %.12f, want %.12f", mode, c.Agent.ID, c.Score, want[c.Agent.ID])
			}
		}
	}
	if mb := ScoreMerit(profiles["a"], domain.TraitCognitive, nil, cfg, nil); mb.Mode != config.MeritModeLegacy || mb.Alpha != 0 {
		t.Errorf("legacy breakdown = %+v, want Mode legacy and no posterior", mb)
	}
}

// posterior_lcb prefers well-evidenced success over a lucky streak with a higher
// mean, and puts an agent with no evidence on the prior's lower bound.
func TestPosteriorLCB_Ordering(t *testing.T) {
	cfg := posteriorCfg(config.MeritModePosteriorLCB)
	profiles := map[string]*domain.AgentProfile{
		"steady": {Outcomes: ev(30, 12)}, // Beta(31,13): mean 0.705
		"lucky":  {Outcomes: ev(2, 0)},   // Beta(3,1):   mean 0.75, q0.1 = 0.1^(1/3) = 0.464
		"fresh":  nil,                    // Beta(1,1):   q0.1 = 0.1
		"bad":    {Outcomes: ev(0, 5)},   // Beta(1,6):   q0.1 = 1 − 0.9^(1/6) = 0.0174
	}
	got, err := slateGatekeeper(cfg, profiles).FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if ids := rankIDs(got); !equalIDs(ids, []string{"steady", "lucky", "fresh", "bad"}) {
		t.Fatalf("order = %v, want [steady lucky fresh bad]", ids)
	}
	wantLucky := math.Pow(0.1, 1.0/3)
	if math.Abs(got[1].Score-wantLucky) > 1e-9 {
		t.Errorf("lucky LCB = %v, want %v", got[1].Score, wantLucky)
	}
	if math.Abs(got[2].Score-0.1) > 1e-9 {
		t.Errorf("fresh LCB = %v, want the prior's 0.1", got[2].Score)
	}
	// The quantile is configurable: at the median the higher-mean lucky streak wins.
	cfg.Gatekeeper.MeritLCBQuantile = 0.5
	got, _ = slateGatekeeper(cfg, profiles).FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
	if got[0].Agent.ID != "lucky" {
		t.Errorf("at q=0.5 the winner = %s, want lucky", got[0].Agent.ID)
	}
}

// Only VERIFIED outcomes move the posterior by default; merit_unverified_weight
// lets unverified completions count, toward success only.
func TestPosterior_UnverifiedNeutralByDefault(t *testing.T) {
	cfg := posteriorCfg(config.MeritModePosteriorLCB)
	unverified := &domain.AgentProfile{Outcomes: domain.OutcomeEvidence{Unverified: 10}}
	prior := ScoreMerit(nil, domain.TraitCognitive, nil, cfg, nil)
	got := ScoreMerit(unverified, domain.TraitCognitive, nil, cfg, nil)
	if got.Score != prior.Score || got.Alpha != 1 || got.Beta != 1 {
		t.Fatalf("unverified-only = %+v, want the prior %+v", got, prior)
	}
	cfg.Gatekeeper.MeritUnverifiedWeight = 0.5
	got = ScoreMerit(unverified, domain.TraitCognitive, nil, cfg, nil)
	if got.Alpha != 6 || got.Beta != 1 {
		t.Fatalf("weighted unverified posterior = Beta(%v,%v), want Beta(6,1)", got.Alpha, got.Beta)
	}
	// Legacy SR is binarised; the posterior is not: a 0.6 verifier score is 0.6 of a success.
	cfg.Gatekeeper.MeritUnverifiedWeight = 0
	got = ScoreMerit(&domain.AgentProfile{Outcomes: ev(0.6, 0.4)}, domain.TraitCognitive, nil, cfg, nil)
	if math.Abs(got.Alpha-1.6) > 1e-12 || math.Abs(got.Beta-1.4) > 1e-12 {
		t.Fatalf("fractional posterior = Beta(%v,%v), want Beta(1.6,1.4)", got.Alpha, got.Beta)
	}
}

// The posterior is per (agent, capability): a step requiring "pdf" is not scored
// on the agent's "browser" record or its global record.
func TestPosterior_CapabilityKeyed(t *testing.T) {
	cfg := posteriorCfg(config.MeritModePosteriorLCB)
	p := &domain.AgentProfile{
		Outcomes: ev(20, 0),
		CapabilityStats: map[string]domain.CapabilityStat{
			"browser": {Outcomes: ev(20, 0)},
			"pdf":     {Outcomes: ev(1, 3)},
		},
	}
	if mb := ScoreMerit(p, domain.TraitCognitive, []string{"pdf"}, cfg, nil); mb.Alpha != 2 || mb.Beta != 4 {
		t.Errorf("pdf step posterior = Beta(%v,%v), want Beta(2,4)", mb.Alpha, mb.Beta)
	}
	if mb := ScoreMerit(p, domain.TraitCognitive, []string{"shell"}, cfg, nil); mb.Alpha != 1 || mb.Beta != 1 {
		t.Errorf("unevidenced capability = Beta(%v,%v), want the prior Beta(1,1)", mb.Alpha, mb.Beta)
	}
	if mb := ScoreMerit(p, domain.TraitCognitive, nil, cfg, nil); mb.Alpha != 21 {
		t.Errorf("no-capability step alpha = %v, want the global 21", mb.Alpha)
	}
	// Model providers keep the legacy formula in every mode.
	if mb := ScoreMerit(p, domain.TraitModel, nil, cfg, nil); mb.Mode != config.MeritModeLegacy {
		t.Errorf("TraitModel mode = %q, want legacy", mb.Mode)
	}
}

// At cold start every agent sits on the same prior: cost, then latency decide, and
// a missing value is neutral (the slate mean of the known ones).
func TestPosterior_TieBreaksCostThenLatency_MissingNeutral(t *testing.T) {
	cfg := posteriorCfg(config.MeritModePosteriorLCB)
	mm := func(c float64) *domain.ModelMetrics { return &domain.ModelMetrics{AvgCostPerTask: c} }
	byCost := map[string]*domain.AgentProfile{
		"cheap":   {ModelMetrics: mm(0.001), NetworkLatencyMedianMs: 900},
		"pricey":  {ModelMetrics: mm(0.010), NetworkLatencyMedianMs: 10},
		"unknown": {NetworkLatencyMedianMs: 100}, // cost ⇒ mean 0.0055
	}
	got, _ := slateGatekeeper(cfg, byCost).FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
	if ids := rankIDs(got); !equalIDs(ids, []string{"cheap", "unknown", "pricey"}) {
		t.Errorf("cost tie-break order = %v, want [cheap unknown pricey]", ids)
	}
	byLatency := map[string]*domain.AgentProfile{
		"fast":    {ModelMetrics: mm(0.5), ComputationLatencyMedianMs: 100},
		"slow":    {ModelMetrics: mm(0.5), ComputationLatencyMedianMs: 300},
		"nolat":   {ModelMetrics: mm(0.5)}, // latency ⇒ mean 200
		"aaa-nil": nil,                     // no profile: cost and latency both neutral
	}
	got, _ = slateGatekeeper(cfg, byLatency).FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
	if ids := rankIDs(got); !equalIDs(ids, []string{"fast", "aaa-nil", "nolat", "slow"}) {
		t.Errorf("latency tie-break order = %v, want [fast aaa-nil nolat slow]", ids)
	}
	// Evidence outranks every tie-break.
	byCost["pricey"].Outcomes = ev(5, 0)
	got, _ = slateGatekeeper(cfg, byCost).FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
	if got[0].Agent.ID != "pricey" {
		t.Errorf("winner with evidence = %s, want pricey", got[0].Agent.ID)
	}
}

// Thompson sampling: reproducible under a seeded RNG, and it explores in proportion
// to uncertainty — a clearly better agent nearly always wins, an evidence-free one
// sometimes beats a mediocre one.
func TestThompson_SeededAndExplores(t *testing.T) {
	cfg := posteriorCfg(config.MeritModeThompson)
	profiles := map[string]*domain.AgentProfile{
		"mid":   {Outcomes: ev(5, 5)},
		"fresh": nil,
		"best":  {Outcomes: ev(60, 0)},
		"worst": {Outcomes: ev(0, 60)},
	}
	run := func(seed int64, n int) []string {
		gk := slateGatekeeper(cfg, profiles, WithRand(rand.New(rand.NewSource(seed))))
		var winners []string
		for i := 0; i < n; i++ {
			got, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
			if err != nil {
				t.Fatalf("FindCandidates: %v", err)
			}
			winners = append(winners, got[0].Agent.ID)
		}
		return winners
	}
	a, b := run(42, 50), run(42, 50)
	if !equalIDs(a, b) {
		t.Fatal("the same seed must reproduce the same Thompson rankings")
	}
	for _, w := range a {
		if w != "best" {
			t.Fatalf("winner %s beat Beta(61,1); want best every time in 50 draws", w)
		}
	}

	delete(profiles, "best")
	delete(profiles, "worst")
	wins := map[string]int{}
	for _, w := range run(7, 400) {
		wins[w]++
	}
	// P(Beta(1,1) > Beta(6,6)) = 0.5 by symmetry — both must win a real share.
	if wins["fresh"] < 120 || wins["mid"] < 120 {
		t.Errorf("Thompson wins = %v, want both fresh and mid to win a real share of 400", wins)
	}

	// PreviewRoute's deterministic view of thompson: the posterior mean, no sampler.
	mbs := ScoreSlate([]SlateEntry{{Profile: profiles["mid"]}}, nil, cfg, nil, nil)
	if mbs[0].Score != 0.5 {
		t.Errorf("unsampled thompson score = %v, want the posterior mean 0.5", mbs[0].Score)
	}
}

// The slate scorer is what PreviewRoute shares with the live path: the legacy
// slate-mean latency fill must come out of ScoreSlate identically to FindCandidates
// (T-20261104-previewroute-scores-missing-latency-as-0-while).
func TestScoreSlate_MatchesFindCandidates(t *testing.T) {
	for _, mode := range []string{config.MeritModeLegacy, config.MeritModePosteriorLCB} {
		cfg := posteriorCfg(mode)
		profiles := map[string]*domain.AgentProfile{
			"a": {SuccessRate: 0.9, TrustScore: 0.8, NetworkLatencyMedianMs: 40, Outcomes: ev(4, 1)},
			"b": {SuccessRate: 0.7, TrustScore: 0.7, Outcomes: ev(1, 0)},
			"c": {SuccessRate: 0.6, TrustScore: 0.9, ComputationLatencyMedianMs: 10, Outcomes: ev(9, 3)},
		}
		live, _ := slateGatekeeper(cfg, profiles).FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
		ids := []string{"a", "b", "c"}
		entries := make([]SlateEntry, len(ids))
		for i, id := range ids {
			entries[i] = SlateEntry{Profile: profiles[id]}
		}
		mbs := ScoreSlate(entries, nil, cfg, nil, nil)
		for _, c := range live {
			for i, id := range ids {
				if id == c.Agent.ID && mbs[i].Score != c.Score {
					t.Errorf("%s: %s slate score %v != live %v", mode, id, mbs[i].Score, c.Score)
				}
			}
		}
	}
}

// FindCandidates runs concurrently for parallel plan steps; the Thompson sampler's
// shared RNG must be safe under -race.
func TestThompson_ConcurrentDispatchIsRaceFree(t *testing.T) {
	cfg := posteriorCfg(config.MeritModeThompson)
	gk := slateGatekeeper(cfg, map[string]*domain.AgentProfile{"a": {Outcomes: ev(3, 1)}, "b": nil})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				if _, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
