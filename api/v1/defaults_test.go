/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1_test

import (
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	ballastv1 "github.com/tight-line/ballast/api/v1"
)

func TestApplyDefaultsFillsSparseSpec(t *testing.T) {
	var spec ballastv1.ClusterResourcePolicySpec
	spec.ApplyDefaults()

	r := spec.Readiness
	if r.MinDataPoints != ballastv1.DefaultMinDataPoints {
		t.Errorf("MinDataPoints = %d, want %d", r.MinDataPoints, ballastv1.DefaultMinDataPoints)
	}
	if r.MinTimeSpan != ballastv1.DefaultMinTimeSpan {
		t.Errorf("MinTimeSpan = %q, want %q", r.MinTimeSpan, ballastv1.DefaultMinTimeSpan)
	}
	if r.MaxCV != ballastv1.DefaultMaxCV {
		t.Errorf("MaxCV = %q, want %q", r.MaxCV, ballastv1.DefaultMaxCV)
	}
	if !maps.Equal(r.CVMeanFloor, ballastv1.DefaultCVMeanFloor()) {
		t.Errorf("CVMeanFloor = %v, want %v", r.CVMeanFloor, ballastv1.DefaultCVMeanFloor())
	}

	b := spec.Behaviors
	if b.Thresholds.Default != ballastv1.DefaultThreshold {
		t.Errorf("Thresholds.Default = %q, want %q", b.Thresholds.Default, ballastv1.DefaultThreshold)
	}
	// resize.default stays empty so the coalesce chain falls through to
	// thresholds.default; filling it would mask a custom thresholds.default.
	if b.Thresholds.Resize.Default != "" {
		t.Errorf("Thresholds.Resize.Default = %q, want empty", b.Thresholds.Resize.Default)
	}
	if b.Resize.MaxChangePerCycle != ballastv1.DefaultMaxChangePerCycle {
		t.Errorf("Resize.MaxChangePerCycle = %q, want %q", b.Resize.MaxChangePerCycle, ballastv1.DefaultMaxChangePerCycle)
	}
	if b.Resize.Interval != ballastv1.DefaultResizeInterval {
		t.Errorf("Resize.Interval = %q, want %q", b.Resize.Interval, ballastv1.DefaultResizeInterval)
	}
}

func TestApplyDefaultsPreservesExplicitValues(t *testing.T) {
	spec := ballastv1.ClusterResourcePolicySpec{
		Readiness: ballastv1.ReadinessConfig{
			MinDataPoints: 250,
			MinTimeSpan:   "48h",
			MaxCV:         "2.0",
			CVMeanFloor:   map[string]string{"cpu": "1m"},
		},
		Behaviors: ballastv1.BehaviorConfig{
			Thresholds: ballastv1.ThresholdConfig{
				Default: "10%",
				Resize:  ballastv1.ResizeThresholds{Default: "15%"},
			},
			Resize: ballastv1.ResizeConfig{
				MaxChangePerCycle: "25%",
				Interval:          "1h",
			},
		},
	}
	spec.ApplyDefaults()

	if spec.Readiness.MinDataPoints != 250 || spec.Readiness.MinTimeSpan != "48h" || spec.Readiness.MaxCV != "2.0" {
		t.Errorf("Readiness scalars changed: %+v", spec.Readiness)
	}
	if !maps.Equal(spec.Readiness.CVMeanFloor, map[string]string{"cpu": "1m"}) {
		t.Errorf("CVMeanFloor changed: %v", spec.Readiness.CVMeanFloor)
	}
	if spec.Behaviors.Thresholds.Default != "10%" ||
		spec.Behaviors.Thresholds.Resize.Default != "15%" ||
		spec.Behaviors.Resize.MaxChangePerCycle != "25%" ||
		spec.Behaviors.Resize.Interval != "1h" {
		t.Errorf("Behaviors changed: %+v", spec.Behaviors)
	}
}

// An explicit empty map is the opt-out that applies the CV check to every
// resource; ApplyDefaults must not confuse it with an omitted (nil) map.
func TestApplyDefaultsPreservesEmptyCVMeanFloor(t *testing.T) {
	spec := ballastv1.ClusterResourcePolicySpec{
		Readiness: ballastv1.ReadinessConfig{CVMeanFloor: map[string]string{}},
	}
	spec.ApplyDefaults()
	if len(spec.Readiness.CVMeanFloor) != 0 {
		t.Errorf("explicit empty CVMeanFloor was overwritten: %v", spec.Readiness.CVMeanFloor)
	}
}

// DefaultCVMeanFloor hands out a fresh map so callers may take ownership.
func TestDefaultCVMeanFloorReturnsFreshMap(t *testing.T) {
	m := ballastv1.DefaultCVMeanFloor()
	m["cpu"] = "mutated"
	if got := ballastv1.DefaultCVMeanFloor()["cpu"]; got == "mutated" {
		t.Error("DefaultCVMeanFloor returns a shared map; mutation leaked across calls")
	}
}

// The parsed-form constants must agree with the string constants they mirror.
func TestDefaultParity(t *testing.T) {
	if d, err := time.ParseDuration(ballastv1.DefaultMinTimeSpan); err != nil || d != ballastv1.DefaultMinTimeSpanDuration {
		t.Errorf("DefaultMinTimeSpan %q != DefaultMinTimeSpanDuration %v (err %v)", ballastv1.DefaultMinTimeSpan, ballastv1.DefaultMinTimeSpanDuration, err)
	}
	if d, err := time.ParseDuration(ballastv1.DefaultResizeInterval); err != nil || d != ballastv1.DefaultResizeIntervalDuration {
		t.Errorf("DefaultResizeInterval %q != DefaultResizeIntervalDuration %v (err %v)", ballastv1.DefaultResizeInterval, ballastv1.DefaultResizeIntervalDuration, err)
	}
	if v, err := strconv.ParseFloat(ballastv1.DefaultMaxCV, 64); err != nil || v != ballastv1.DefaultMaxCVValue {
		t.Errorf("DefaultMaxCV %q != DefaultMaxCVValue %v (err %v)", ballastv1.DefaultMaxCV, ballastv1.DefaultMaxCVValue, err)
	}
	if v, err := strconv.ParseFloat(ballastv1.DefaultHeadroom, 64); err != nil || v != ballastv1.DefaultHeadroomValue {
		t.Errorf("DefaultHeadroom %q != DefaultHeadroomValue %v (err %v)", ballastv1.DefaultHeadroom, ballastv1.DefaultHeadroomValue, err)
	}
	if v, err := strconv.ParseFloat(strings.TrimSuffix(ballastv1.DefaultThreshold, "%"), 64); err != nil || v != ballastv1.DefaultThresholdPercent {
		t.Errorf("DefaultThreshold %q != DefaultThresholdPercent %v (err %v)", ballastv1.DefaultThreshold, ballastv1.DefaultThresholdPercent, err)
	}
	if v, err := strconv.ParseFloat(strings.TrimSuffix(ballastv1.DefaultMaxChangePerCycle, "%"), 64); err != nil || v != ballastv1.DefaultMaxChangePercent {
		t.Errorf("DefaultMaxChangePerCycle %q != DefaultMaxChangePercent %v (err %v)", ballastv1.DefaultMaxChangePerCycle, ballastv1.DefaultMaxChangePercent, err)
	}
}
