package store

import "math"

// Stats holds statistical measures for a set of samples.
type Stats struct {
	P50    float64
	P75    float64
	P90    float64
	P95    float64
	P99    float64
	Max    float64
	Mean   float64
	StdDev float64
	CV     float64
	Count  int
}

// ComputeStats computes statistics from a sorted int64 slice (ascending order).
// Returns zero Stats for an empty slice.
func ComputeStats(values []int64) Stats {
	n := len(values)
	if n == 0 {
		return Stats{}
	}

	var sum float64
	for _, v := range values {
		sum += float64(v)
	}
	mean := sum / float64(n)

	var variance float64
	for _, v := range values {
		d := float64(v) - mean
		variance += d * d
	}
	variance /= float64(n)
	stddev := math.Sqrt(variance)

	var cv float64
	if mean != 0 {
		cv = stddev / mean
	}

	// Nearest-rank percentile (1-indexed, ceil).
	percentile := func(p float64) float64 {
		rank := int(math.Ceil(p / 100.0 * float64(n)))
		rank = max(1, min(rank, n))
		return float64(values[rank-1])
	}

	return Stats{
		P50:    percentile(50),
		P75:    percentile(75),
		P90:    percentile(90),
		P95:    percentile(95),
		P99:    percentile(99),
		Max:    float64(values[n-1]),
		Mean:   mean,
		StdDev: stddev,
		CV:     cv,
		Count:  n,
	}
}
