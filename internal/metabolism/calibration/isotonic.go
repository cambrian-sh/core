// Package calibration fits and applies bid-confidence calibration maps from verifier
// outcomes (ROUTE-05 / ADR-0068). Pure and dependency-free: it turns
// (self_confidence → verifier_quality) samples into a monotonic map that corrects a
// uniformly-overconfident LLM self-assessment. Offline-first — nothing here touches
// live routing; the auctioneer applies a fitted Model only behind an arm.
package calibration

import "sort"

// Isotonic is a monotonic non-decreasing calibration curve fit by PAVA
// (pool-adjacent-violators). Predict interpolates between fitted knots.
type Isotonic struct {
	// xs are sorted, distinct input confidences; ys are the fitted (monotonic) outputs.
	xs []float64
	ys []float64
}

// point is one (x,y) sample with a weight (count of merged duplicates).
type point struct {
	x, y float64
	w    float64
}

// FitIsotonic fits a non-decreasing step curve to the samples via PAVA. Returns nil
// when there are no samples (callers treat a nil Isotonic as identity).
func FitIsotonic(xs, ys []float64) *Isotonic {
	if len(xs) == 0 || len(xs) != len(ys) {
		return nil
	}
	// Sort by x and merge duplicate x's into one weighted point (mean y, summed weight).
	pts := make([]point, len(xs))
	for i := range xs {
		pts[i] = point{x: xs[i], y: ys[i], w: 1}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].x < pts[j].x })
	merged := make([]point, 0, len(pts))
	for _, p := range pts {
		if n := len(merged); n > 0 && merged[n-1].x == p.x {
			last := &merged[n-1]
			last.y = (last.y*last.w + p.y*p.w) / (last.w + p.w)
			last.w += p.w
			continue
		}
		merged = append(merged, p)
	}

	// PAVA: a stack of blocks, each covering `count` consecutive merged points with a
	// weighted mean ySum/w. Merge the top two while the earlier block's mean exceeds the
	// later block's (a monotonicity violation).
	type block struct {
		ySum, w float64
		count   int
	}
	blocks := make([]block, 0, len(merged))
	for _, p := range merged {
		blocks = append(blocks, block{ySum: p.y * p.w, w: p.w, count: 1})
		for len(blocks) >= 2 {
			a := blocks[len(blocks)-2]
			b := blocks[len(blocks)-1]
			if a.ySum/a.w <= b.ySum/b.w {
				break
			}
			blocks = blocks[:len(blocks)-2]
			blocks = append(blocks, block{ySum: a.ySum + b.ySum, w: a.w + b.w, count: a.count + b.count})
		}
	}

	// Expand each block's mean back onto the x's of the merged points it covers.
	iso := &Isotonic{xs: make([]float64, 0, len(merged)), ys: make([]float64, 0, len(merged))}
	idx := 0
	for _, bl := range blocks {
		mean := bl.ySum / bl.w
		for k := 0; k < bl.count; k++ {
			iso.xs = append(iso.xs, merged[idx].x)
			iso.ys = append(iso.ys, mean)
			idx++
		}
	}
	return iso
}

// Predict returns the calibrated value for x by clamping to the fitted range and
// linearly interpolating between knots. A nil Isotonic is the identity map.
func (iso *Isotonic) Predict(x float64) float64 {
	if iso == nil || len(iso.xs) == 0 {
		return x
	}
	if x <= iso.xs[0] {
		return iso.ys[0]
	}
	last := len(iso.xs) - 1
	if x >= iso.xs[last] {
		return iso.ys[last]
	}
	// Binary search for the bracketing knots.
	lo, hi := 0, last
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if iso.xs[mid] <= x {
			lo = mid
		} else {
			hi = mid
		}
	}
	x0, x1 := iso.xs[lo], iso.xs[hi]
	y0, y1 := iso.ys[lo], iso.ys[hi]
	if x1 == x0 {
		return y0
	}
	t := (x - x0) / (x1 - x0)
	return y0 + t*(y1-y0)
}

// Samples returns the number of knots (distinct input confidences) the curve was fit on.
func (iso *Isotonic) Samples() int {
	if iso == nil {
		return 0
	}
	return len(iso.xs)
}
