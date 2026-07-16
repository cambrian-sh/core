// Command calibration-report is the OFFLINE half of ROUTE-05 / ADR-0068. It reads the
// verified events in the kernel's event log, fits per-agent bid-calibration curves, and
// prints them as JSON — the "calibration curves published to artifacts" deliverable —
// WITHOUT touching live routing. Turning the online arm (execution.calibrated_bids) on
// is a separate decision made only after an offline replay shows lift.
//
//	go run ./cmd/calibration-report > calibration.json
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/calibration"
	"github.com/cambrian-sh/core/internal/storage"
)

type agentReport struct {
	Samples int       `json:"samples"`
	KnotsX  []float64 `json:"knots_x"`
	KnotsY  []float64 `json:"knots_y"`
	// Calibrated is the fitted curve sampled at a few reference confidences, so a reader
	// sees the correction at a glance (e.g. how far 0.9 self-confidence is pulled down).
	Calibrated map[string]float64 `json:"calibrated_at"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "calibration-report:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadConfig("configs/config.json")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	dbPath := filepath.Join(cfg.Storage.DataDir, cfg.Storage.DBName)
	store, err := storage.NewBBoltAdapter(dbPath, cfg.Metabolism.AgentsDir, domain.IsSystemAgent)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	recs, err := store.ReadAllTaskEventRecords()
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	var samples []calibration.Sample
	for _, r := range recs {
		if !r.Verified || r.AgentID == "" || r.BidConfidence <= 0 {
			continue
		}
		samples = append(samples, calibration.Sample{AgentID: r.AgentID, Confidence: r.BidConfidence, Quality: r.VerifierScore})
	}
	if len(samples) == 0 {
		return fmt.Errorf("no verified bid/quality samples in the event log yet")
	}

	model := calibration.Fit(samples, cfg.Execution.BidCalibrationMinSamples)
	refs := []float64{0.5, 0.7, 0.8, 0.9, 0.95, 1.0}
	out := map[string]agentReport{}
	agents := model.Agents()
	sort.Strings(agents)
	for _, id := range agents {
		xs, ys := model.Curve(id)
		cal := map[string]float64{}
		for _, r := range refs {
			cal[fmt.Sprintf("%.2f", r)] = model.Calibrate(id, r)
		}
		out[id] = agentReport{Samples: len(xs), KnotsX: xs, KnotsY: ys, Calibrated: cal}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	fmt.Fprintf(os.Stderr, "fit %d samples across %d agents (min_samples=%d)\n",
		len(samples), model.AgentCount(), cfg.Execution.BidCalibrationMinSamples)
	return enc.Encode(out)
}
