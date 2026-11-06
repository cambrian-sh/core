package gatekeeper

import (
	"math"
	"math/rand"
)

// Beta-distribution helpers for the ADR-0139 D6 posterior merit modes. Kept here,
// not in a shared math package, because the Gatekeeper is their only consumer and
// the workspace carries no statistics dependency (gonum is not in go.mod; three
// small, well-known routines do not justify adding it).

// betaQuantile returns x such that I_x(a, b) = p: the p-quantile of Beta(a, b).
// Bisection on the regularised incomplete beta, which is monotone in x; 100
// halvings of [0, 1] are far below float64 resolution, so the result is exact to
// the precision of regIncBeta. a, b > 0; p in (0, 1).
func betaQuantile(p, a, b float64) float64 {
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 100; i++ {
		mid := (lo + hi) / 2
		if regIncBeta(mid, a, b) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// regIncBeta is the regularised incomplete beta function I_x(a, b), by Lentz's
// continued fraction (Numerical Recipes §6.4), using the symmetry
// I_x(a,b) = 1 − I_{1−x}(b,a) where the fraction converges faster.
func regIncBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a + b)
	lb, _ := math.Lgamma(a)
	lc, _ := math.Lgamma(b)
	front := math.Exp(la - lb - lc + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(x, a, b) / a
	}
	return 1 - front*betaContinuedFraction(1-x, b, a)/b
}

func betaContinuedFraction(x, a, b float64) float64 {
	const (
		maxIter = 300
		eps     = 1e-14
		tiny    = 1e-300
	)
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// sampleBeta draws one value from Beta(a, b) as X/(X+Y), X ~ Gamma(a), Y ~ Gamma(b).
func sampleBeta(r *rand.Rand, a, b float64) float64 {
	x := sampleGamma(r, a)
	y := sampleGamma(r, b)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// sampleGamma draws from Gamma(shape, 1) by Marsaglia–Tsang (2000); a shape < 1 is
// boosted through Gamma(shape+1)·U^(1/shape).
func sampleGamma(r *rand.Rand, shape float64) float64 {
	if shape < 1 {
		u := r.Float64()
		return sampleGamma(r, shape+1) * math.Pow(u, 1/shape)
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		var x, v float64
		for {
			x = r.NormFloat64()
			v = 1 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := r.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
