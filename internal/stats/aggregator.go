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
// resourceName selects the cvMeanFloor entry that can exempt the CV check.
func EvaluateReadiness(s store.Stats, firstMs, lastMs int64, cfg ballastv1.ReadinessConfig, resourceName string) bool {
	minPoints := cfg.MinDataPoints
	if minPoints <= 0 {
		minPoints = ballastv1.DefaultMinDataPoints
	}
	if int64(s.Count) < minPoints {
		return false
	}

	minSpan, err := time.ParseDuration(cfg.MinTimeSpan)
	if err != nil || minSpan <= 0 {
		minSpan = ballastv1.DefaultMinTimeSpanDuration
	}
	if time.Duration(lastMs-firstMs)*time.Millisecond < minSpan {
		return false
	}

	// CV (stddev/mean) is numerically unstable near a zero mean: quantization
	// noise and rare one-off spikes produce huge CVs on workloads whose usage
	// is negligible. Below the configured floor the check is skipped — usage
	// that small is safe to size regardless of dispersion. Unparseable floor
	// values are ignored (the CV check applies), matching the maxCV fallback.
	if floor, ok := cfg.CVMeanFloor[resourceName]; ok {
		if q, err := resource.ParseQuantity(strings.TrimSpace(floor)); err == nil {
			floorVal := q.AsApproximateFloat64()
			if resourceName == "cpu" {
				floorVal *= 1000 // stats store cpu in millicores
			}
			if floorVal > 0 && s.Mean < floorVal {
				return true
			}
		}
	}

	maxCV, err := strconv.ParseFloat(strings.TrimSpace(cfg.MaxCV), 64)
	if err != nil || maxCV < 0 {
		maxCV = ballastv1.DefaultMaxCVValue
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

	headroom := ballastv1.DefaultHeadroomValue
	if h, err := strconv.ParseFloat(strings.TrimSpace(metric.Headroom), 64); err == nil && h > 0 {
		headroom = h
	}

	value := int64(base * headroom)
	if metric.Resource == "cpu" {
		return *resource.NewMilliQuantity(value, resource.DecimalSI), nil
	}
	return *resource.NewQuantity(value, resource.BinarySI), nil
}
