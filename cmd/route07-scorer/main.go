// Command route07-scorer is the OFFLINE half of ROUTE-07 / ADR-0076 (the learned
// gatekeeper scorer). It has two steps, both reproducible from artifacts alone:
//
//	# 1. extract training samples from benchmark run artifacts (auction funnels joined
//	#    with verifier outcomes) — the data the online decision would have seen:
//	go run ./cmd/route07-scorer extract --in runs/<id>/results.jsonl --out dataset.jsonl
//
//	# 2. train + OFFLINE-EVAL against the calibrated hand-weights baseline, save the model:
//	go run ./cmd/route07-scorer train --in dataset.jsonl --out model.json
//
// `train` prints the offline comparison (learned AUC vs hand-weight AUC on a held-out
// split) and whether the ROUTE-07 gate would ADOPT the learned scorer. Turning the online
// arm on (execution.learned_scorer + learned_scorer_model_path) is a SEPARATE decision made
// only after a published offline win — this tool never touches live routing.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/routescorer"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: route07-scorer {extract|train} [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "extract":
		err = extract(os.Args[2:])
	case "train":
		err = train(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q (want extract|train)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "route07-scorer:", err)
		os.Exit(1)
	}
}

// ── extract ─────────────────────────────────────────────────────────────────────

// orchRow is the subset of an orchestration results.jsonl row we read: each auction's
// winner + its funnel L3 merit breakdown (the decision-time feature snapshot), and the
// verifier outcome that labels it.
type orchRow struct {
	Auctions []struct {
		TaskID   string `json:"task_id"`
		WinnerID string `json:"winner_id"`
		Funnel   struct {
			L3 []struct {
				AgentID     string  `json:"agent_id"`
				SuccessRate float64 `json:"success_rate"`
				TrustScore  float64 `json:"trust_score"`
				LatencyTerm float64 `json:"latency_term"`
				CostTerm    float64 `json:"cost_term"`
				Provisional bool    `json:"provisional"`
			} `json:"l3"`
		} `json:"funnel"`
	} `json:"auctions"`
	VerifierRounds []struct {
		TaskID       string  `json:"task_id"`
		QualityScore float64 `json:"quality_score"`
	} `json:"verifier_rounds"`
	Correct bool `json:"correct"`
}

func extract(args []string) error {
	inPath, outPath := parseInOut(args)
	if inPath == "" {
		return fmt.Errorf("extract: --in <results.jsonl> is required")
	}
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out := os.Stdout
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	enc := json.NewEncoder(out)

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var rows, samples int
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row orchRow
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		rows++
		// Label lookup: verifier quality per task, falling back to the row's correctness.
		quality := map[string]float64{}
		for _, v := range row.VerifierRounds {
			quality[v.TaskID] = v.QualityScore
		}
		for _, a := range row.Auctions {
			if a.WinnerID == "" {
				continue
			}
			for _, m := range a.Funnel.L3 {
				if m.AgentID != a.WinnerID {
					continue
				}
				label, ok := quality[a.TaskID]
				if !ok {
					if row.Correct {
						label = 1
					} else {
						label = 0
					}
				}
				prov := 0.0
				if m.Provisional {
					prov = 1
				}
				s := routescorer.Sample{
					Features: [routescorer.NumFeatures]float64{m.SuccessRate, m.TrustScore, m.LatencyTerm, m.CostTerm, prov},
					Label:    label,
				}
				if err := enc.Encode(s); err != nil {
					return err
				}
				samples++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "extracted %d samples from %d rows\n", samples, rows)
	if samples == 0 {
		return fmt.Errorf("no (funnel-winner, outcome) samples found — need runs with auction funnels (resource_selector=auction, routing_trace on)")
	}
	return nil
}

// ── train ───────────────────────────────────────────────────────────────────────

func train(args []string) error {
	inPath, outPath := parseInOut(args)
	if inPath == "" {
		return fmt.Errorf("train: --in <dataset.jsonl> is required")
	}
	samples, err := readSamples(inPath)
	if err != nil {
		return err
	}
	if len(samples) < 10 {
		return fmt.Errorf("only %d samples — too few to train/evaluate a scorer (accrue more routing artifacts first)", len(samples))
	}

	// Hand-weight baseline from the current config (the calibrated ROUTE-05/06 arm's weights).
	cfg, cfgErr := config.LoadConfig("configs/config.json")
	hw := routescorer.HandWeights{W1: 0.4, W2: 0.4} // defaults if config is unavailable
	if cfgErr == nil {
		hw = routescorer.HandWeights{W1: cfg.Execution.GatekeeperW1, W2: cfg.Execution.GatekeeperW2}
	}

	trainSet, testSet := routescorer.Split(samples, 0.2)
	model := routescorer.Fit(trainSet, routescorer.FitOptions{})
	if model == nil {
		return fmt.Errorf("training produced no model")
	}
	cmp := routescorer.CompareOnHeldout(model, hw, testSet, 0.01)

	fmt.Fprintf(os.Stderr, "trained on %d, held out %d | learned AUC=%.4f vs hand AUC=%.4f (Δ=%.4f) | adopt=%v\n",
		len(trainSet), cmp.N, cmp.LearnedAUC, cmp.HandAUC, cmp.Delta, cmp.AdoptLearned)

	// Emit the comparison as JSON to stdout (the "offline comparison published" artifact).
	rep := map[string]any{"comparison": cmp, "weights": model.Weights, "features": model.Features, "bias": model.Bias}
	repEnc := json.NewEncoder(os.Stdout)
	repEnc.SetIndent("", "  ")
	if err := repEnc.Encode(rep); err != nil {
		return err
	}

	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := model.Save(f); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote model to %s\n", outPath)
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func parseInOut(args []string) (in, out string) {
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--in":
			in = args[i+1]
		case "--out":
			out = args[i+1]
		}
	}
	return in, out
}

func readSamples(path string) ([]routescorer.Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []routescorer.Sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var s routescorer.Sample
		if json.Unmarshal(sc.Bytes(), &s) == nil {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}
