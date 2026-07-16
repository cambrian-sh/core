package kernel

import "github.com/cambrian-sh/core/internal/metabolism/calibration"

// BidCalibrationSamples extracts (agent, bid_confidence, verifier_quality) tuples from
// the event log for ROUTE-05 / ADR-0068. Only VERIFIED events are used — an unverified
// event has no ground-truth quality — and only those with a real bid confidence. The
// event log already carries both, so no new data is collected.
func (d *AgentRepoDecorator) BidCalibrationSamples() ([]calibration.Sample, error) {
	recs, err := d.store.ReadAllTaskEventRecords()
	if err != nil {
		return nil, err
	}
	out := make([]calibration.Sample, 0, len(recs))
	for _, r := range recs {
		if !r.Verified || r.AgentID == "" || r.BidConfidence <= 0 {
			continue
		}
		out = append(out, calibration.Sample{
			AgentID:    r.AgentID,
			Confidence: r.BidConfidence,
			Quality:    r.VerifierScore,
		})
	}
	return out, nil
}
