package stats_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/stats"
	"github.com/tight-line/ballast/internal/store"
)

func TestEvaluateReadiness(t *testing.T) {
	cfg := ballastv1.ReadinessConfig{
		MinDataPoints: 10,
		MinTimeSpan:   "1h",
		MaxCV:         "1.5",
	}

	// 1h in milliseconds
	spanMs := int64(60 * 60 * 1000)

	cfgFloor := ballastv1.ReadinessConfig{
		MinDataPoints: 10,
		MinTimeSpan:   "1h",
		MaxCV:         "1.5",
		CVMeanFloor: map[string]string{
			"cpu":               "10m",
			"memory":            "25Mi",
			"ephemeral-storage": "100Ki",
		},
	}

	tests := []struct {
		name     string
		s        store.Stats
		firstMs  int64
		lastMs   int64
		cfg      ballastv1.ReadinessConfig
		resource string
		want     bool
	}{
		{
			name:    "all conditions pass",
			s:       store.Stats{Count: 20, CV: 0.5},
			firstMs: 0, lastMs: spanMs,
			cfg:  cfg,
			want: true,
		},
		{
			name:    "too few data points",
			s:       store.Stats{Count: 5, CV: 0.5},
			firstMs: 0, lastMs: spanMs,
			cfg:  cfg,
			want: false,
		},
		{
			name:    "time span too short",
			s:       store.Stats{Count: 20, CV: 0.5},
			firstMs: 0, lastMs: spanMs / 2,
			cfg:  cfg,
			want: false,
		},
		{
			name:    "CV too high",
			s:       store.Stats{Count: 20, CV: 2.0},
			firstMs: 0, lastMs: spanMs,
			cfg:  cfg,
			want: false,
		},
		{
			name:    "CV exactly at max",
			s:       store.Stats{Count: 10, CV: 1.5},
			firstMs: 0, lastMs: spanMs,
			cfg:  cfg,
			want: true,
		},
		{
			name:    "invalid minTimeSpan falls back to 24h default",
			s:       store.Stats{Count: 20, CV: 0.5},
			firstMs: 0, lastMs: spanMs, // only 1h, less than 24h default
			cfg: ballastv1.ReadinessConfig{
				MinDataPoints: 10,
				MinTimeSpan:   "not-a-duration",
				MaxCV:         "1.5",
			},
			want: false,
		},
		{
			name:    "invalid maxCV falls back to 1.5 default",
			s:       store.Stats{Count: 20, CV: 1.6},
			firstMs: 0, lastMs: spanMs,
			cfg: ballastv1.ReadinessConfig{
				MinDataPoints: 10,
				MinTimeSpan:   "1h",
				MaxCV:         "bad",
			},
			want: false,
		},
		{
			name:    "exactly at minDataPoints",
			s:       store.Stats{Count: 10, CV: 0.5},
			firstMs: 0, lastMs: spanMs,
			cfg:  cfg,
			want: true,
		},
		{
			// The observed near-idle case: mean 1m, CV inflated by a startup
			// spike. The 10m cpu floor exempts the CV check.
			name:    "cpu mean below floor exempts CV check",
			s:       store.Stats{Count: 20, CV: 10.343, Mean: 1},
			firstMs: 0, lastMs: spanMs,
			cfg: cfgFloor, resource: "cpu",
			want: true,
		},
		{
			name:    "cpu mean above floor keeps CV check",
			s:       store.Stats{Count: 20, CV: 2.0, Mean: 50},
			firstMs: 0, lastMs: spanMs,
			cfg: cfgFloor, resource: "cpu",
			want: false,
		},
		{
			// Memory stats are bytes; the floor must not get the millicore
			// conversion. 10Mi mean is below the 25Mi floor.
			name:    "memory mean below floor exempts CV check",
			s:       store.Stats{Count: 20, CV: 3.0, Mean: 10 * 1024 * 1024},
			firstMs: 0, lastMs: spanMs,
			cfg: cfgFloor, resource: "memory",
			want: true,
		},
		{
			name:    "resource without floor entry keeps CV check",
			s:       store.Stats{Count: 20, CV: 2.0, Mean: 1},
			firstMs: 0, lastMs: spanMs,
			cfg: cfgFloor, resource: "example.com/foo",
			want: false,
		},
		{
			name:    "invalid floor value keeps CV check",
			s:       store.Stats{Count: 20, CV: 2.0, Mean: 1},
			firstMs: 0, lastMs: spanMs,
			cfg: ballastv1.ReadinessConfig{
				MinDataPoints: 10,
				MinTimeSpan:   "1h",
				MaxCV:         "1.5",
				CVMeanFloor:   map[string]string{"cpu": "bogus"},
			},
			resource: "cpu",
			want:     false,
		},
		{
			name:    "floor of zero disables the exemption",
			s:       store.Stats{Count: 20, CV: 5.0, Mean: 0.5},
			firstMs: 0, lastMs: spanMs,
			cfg: ballastv1.ReadinessConfig{
				MinDataPoints: 10,
				MinTimeSpan:   "1h",
				MaxCV:         "1.5",
				CVMeanFloor:   map[string]string{"cpu": "0"},
			},
			resource: "cpu",
			want:     false,
		},
		{
			// The floor exempts only the CV check, not the history gates.
			name:    "floor does not bypass minDataPoints",
			s:       store.Stats{Count: 5, CV: 10.0, Mean: 1},
			firstMs: 0, lastMs: spanMs,
			cfg: cfgFloor, resource: "cpu",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stats.EvaluateReadiness(tc.s, tc.firstMs, tc.lastMs, tc.cfg, tc.resource)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComputeRecommendation(t *testing.T) {
	s := store.Stats{
		P50:  100,
		P75:  175,
		P90:  190,
		P95:  200,
		P99:  300,
		Max:  400,
		Mean: 150,
	}

	tests := []struct {
		name    string
		metric  ballastv1.MetricConfig
		want    resource.Quantity
		wantErr bool
	}{
		{
			name:   "cpu p95 with headroom",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "p95", Headroom: "1.2"},
			want:   resource.MustParse("240m"), // 200 * 1.2 = 240m
		},
		{
			name:   "cpu p99 with headroom",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "p99", Headroom: "1.25"},
			want:   resource.MustParse("375m"), // 300 * 1.25 = 375m
		},
		{
			name:   "memory p99 bytes",
			metric: ballastv1.MetricConfig{Resource: "memory", Aggregation: "p99", Headroom: "1.1"},
			want:   *resource.NewQuantity(330, resource.BinarySI),
		},
		{
			name:   "p50 aggregation",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "p50", Headroom: "1.0"},
			want:   resource.MustParse("100m"),
		},
		{
			name:   "p75 aggregation",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "p75", Headroom: "1.0"},
			want:   resource.MustParse("175m"),
		},
		{
			name:   "p90 aggregation",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "p90", Headroom: "1.0"},
			want:   resource.MustParse("190m"),
		},
		{
			name:   "max aggregation",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "max", Headroom: "1.0"},
			want:   resource.MustParse("400m"),
		},
		{
			name:   "avg aggregation",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "avg", Headroom: "1.0"},
			want:   resource.MustParse("150m"),
		},
		{
			name:    "unknown aggregation returns error",
			metric:  ballastv1.MetricConfig{Resource: "cpu", Aggregation: "unknown", Headroom: "1.0"},
			wantErr: true,
		},
		{
			name:   "invalid headroom falls back to 1.0",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "p95", Headroom: "bad"},
			want:   resource.MustParse("200m"),
		},
		{
			name:   "zero headroom falls back to 1.0",
			metric: ballastv1.MetricConfig{Resource: "cpu", Aggregation: "p95", Headroom: "0"},
			want:   resource.MustParse("200m"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stats.ComputeRecommendation(s, tc.metric)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Cmp(tc.want) != 0 {
				t.Errorf("got %v, want %v", got.String(), tc.want.String())
			}
		})
	}
}

// A zero MinDataPoints means the field was omitted; EvaluateReadiness applies
// the canonical default rather than treating it as "no minimum". The resolver
// normally fills the field first, so this is the defense for direct callers.
func TestEvaluateReadinessZeroMinDataPointsUsesDefault(t *testing.T) {
	cfg := ballastv1.ReadinessConfig{MinTimeSpan: "1h", MaxCV: "1.5"}
	spanMs := int64(60 * 60 * 1000)
	s := store.Stats{Count: int(ballastv1.DefaultMinDataPoints) - 1, Mean: 100, StdDev: 10, CV: 0.1}

	if stats.EvaluateReadiness(s, 0, spanMs, cfg, "cpu") {
		t.Errorf("count %d below default %d should not be ready", s.Count, ballastv1.DefaultMinDataPoints)
	}
	s.Count = int(ballastv1.DefaultMinDataPoints)
	if !stats.EvaluateReadiness(s, 0, spanMs, cfg, "cpu") {
		t.Errorf("count %d at default should be ready", s.Count)
	}
}
