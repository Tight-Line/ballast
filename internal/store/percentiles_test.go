package store_test

import (
	"math"
	"testing"

	"github.com/tight-line/ballast/internal/store"
)

func TestComputeStats_Empty(t *testing.T) {
	s := store.ComputeStats(nil)
	if s.Count != 0 || s.P50 != 0 || s.Mean != 0 || s.StdDev != 0 || s.CV != 0 {
		t.Errorf("expected zero Stats for empty input, got %+v", s)
	}
}

func TestComputeStats_Single(t *testing.T) {
	s := store.ComputeStats([]int64{100})
	if s.Count != 1 {
		t.Errorf("Count = %d, want 1", s.Count)
	}
	if s.P50 != 100 || s.P95 != 100 || s.P99 != 100 || s.Max != 100 || s.Mean != 100 {
		t.Errorf("unexpected stats for single value: %+v", s)
	}
	if s.StdDev != 0 || s.CV != 0 {
		t.Errorf("expected zero StdDev/CV for single value: %+v", s)
	}
}

func TestComputeStats_Percentiles(t *testing.T) {
	// 20 values: 1..20 (sorted ascending)
	values := make([]int64, 20)
	for i := range values {
		values[i] = int64(i + 1)
	}
	s := store.ComputeStats(values)

	// p50: ceil(50/100 * 20) = rank 10 → value 10
	if s.P50 != 10 {
		t.Errorf("P50 = %v, want 10", s.P50)
	}
	// p95: ceil(95/100 * 20) = rank 19 → value 19
	if s.P95 != 19 {
		t.Errorf("P95 = %v, want 19", s.P95)
	}
	// p99: ceil(99/100 * 20) = rank 20 → value 20
	if s.P99 != 20 {
		t.Errorf("P99 = %v, want 20", s.P99)
	}
	if s.Max != 20 {
		t.Errorf("Max = %v, want 20", s.Max)
	}
	if math.Abs(s.Mean-10.5) > 1e-9 {
		t.Errorf("Mean = %v, want 10.5", s.Mean)
	}
	if s.Count != 20 {
		t.Errorf("Count = %d, want 20", s.Count)
	}
}

func TestComputeStats_CV(t *testing.T) {
	// Uniform values → stddev=0, CV=0
	s := store.ComputeStats([]int64{5, 5, 5, 5})
	if s.StdDev != 0 || s.CV != 0 {
		t.Errorf("uniform: StdDev=%v CV=%v, want 0, 0", s.StdDev, s.CV)
	}

	// [2,4,4,4,5,5,7,9]: mean=5, variance=4, stddev=2, CV=0.4
	s = store.ComputeStats([]int64{2, 4, 4, 4, 5, 5, 7, 9})
	if math.Abs(s.Mean-5) > 1e-9 {
		t.Errorf("Mean = %v, want 5", s.Mean)
	}
	if math.Abs(s.StdDev-2) > 1e-9 {
		t.Errorf("StdDev = %v, want 2", s.StdDev)
	}
	if math.Abs(s.CV-0.4) > 1e-9 {
		t.Errorf("CV = %v, want 0.4", s.CV)
	}
}

func TestComputeStats_ZeroMeanCV(t *testing.T) {
	// mean=0 → CV must be 0, not NaN/inf
	s := store.ComputeStats([]int64{0, 0, 0})
	if s.CV != 0 || math.IsNaN(s.CV) || math.IsInf(s.CV, 0) {
		t.Errorf("CV for zero-mean = %v, want 0", s.CV)
	}
}
