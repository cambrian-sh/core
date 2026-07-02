package executer

// meanConfidence returns the arithmetic mean of values.
// It returns 0 for an empty slice.
func meanConfidence(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
