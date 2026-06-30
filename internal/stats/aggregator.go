/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package stats

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/store"
)

// EvaluateReadiness returns true when all three readiness conditions are satisfied:
// sampleCount >= minDataPoints, timeSpan >= minTimeSpan, and CV <= maxCV.
// firstMs and lastMs are Unix timestamps (milliseconds) of the oldest and newest samples.
func EvaluateReadiness(s store.Stats, firstMs, lastMs int64, cfg ballastv1.ReadinessConfig) bool {
	if int64(s.Count) < cfg.MinDataPoints {
		return false
	}

	minSpan, err := time.ParseDuration(cfg.MinTimeSpan)
	if err != nil || minSpan <= 0 {
		minSpan = 24 * time.Hour
	}
	if time.Duration(lastMs-firstMs)*time.Millisecond < minSpan {
		return false
	}

	maxCV, err := strconv.ParseFloat(strings.TrimSpace(cfg.MaxCV), 64)
	if err != nil || maxCV < 0 {
		maxCV = 1.5
	}
	return s.CV <= maxCV
}

// ComputeRecommendation returns the recommended resource.Quantity for metric.
// Stats values must be in millicores for CPU and bytes for memory/ephemeral-storage.
func ComputeRecommendation(s store.Stats, metric ballastv1.MetricConfig) (resource.Quantity, error) {
	var base float64
	switch metric.Aggregation {
	case "p50":
		base = s.P50
	case "p75":
		base = s.P75
	case "p90":
		base = s.P90
	case "p95":
		base = s.P95
	case "p99":
		base = s.P99
	case "max":
		base = s.Max
	case "avg":
		base = s.Mean
	default:
		return resource.Quantity{}, fmt.Errorf("unknown aggregation %q", metric.Aggregation)
	}

	headroom := 1.0
	if h, err := strconv.ParseFloat(strings.TrimSpace(metric.Headroom), 64); err == nil && h > 0 {
		headroom = h
	}

	value := int64(base * headroom)
	if metric.Resource == "cpu" {
		return *resource.NewMilliQuantity(value, resource.DecimalSI), nil
	}
	return *resource.NewQuantity(value, resource.BinarySI), nil
}
