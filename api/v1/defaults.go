/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1

import (
	"strings"
	"time"
)

// Canonical runtime defaults for policy spec fields.
//
// Defaulting happens at policy-resolve time, not in the CRD schema. Admission-time
// (+kubebuilder:default) defaults persist into stored objects the moment they are
// written and are never revisited: a policy created under an older release keeps
// that release's defaults forever, even after a CRD upgrade changes them. Read-time
// defaulting means a sparse policy always tracks the running release's defaults,
// and changing a default is a code change plus a release — no migration of stored
// objects.
//
// The string constants are the values ApplyDefaults writes into a sparse spec; the
// parsed forms below them serve code paths that need numeric values (for example
// when a user-supplied string fails to parse). TestDefaultParity asserts the two
// forms agree.
const (
	// DefaultMinDataPoints is the minimum sample count required before Ballast acts.
	DefaultMinDataPoints int64 = 500
	// DefaultMinTimeSpan is the minimum observed-history duration required.
	DefaultMinTimeSpan = "24h"
	// DefaultMaxCV is the maximum coefficient of variation (stddev/mean) allowed.
	DefaultMaxCV = "1.5"
	// DefaultHeadroom is the multiplier applied to an aggregated usage value.
	DefaultHeadroom = "1.0"
	// DefaultThreshold is the drift threshold that triggers a resize.
	DefaultThreshold = "20%"
	// DefaultMaxChangePerCycle caps a single adjustment step as a percentage of
	// the current-to-recommended gap.
	DefaultMaxChangePerCycle = "50%"
	// DefaultResizeInterval is how often the ResourceAdjuster re-evaluates drift.
	DefaultResizeInterval = "15m"
)

// Parsed forms of the string defaults above.
const (
	// DefaultMinTimeSpanDuration is DefaultMinTimeSpan as a time.Duration.
	DefaultMinTimeSpanDuration = 24 * time.Hour
	// DefaultMaxCVValue is DefaultMaxCV as a float64.
	DefaultMaxCVValue = 1.5
	// DefaultHeadroomValue is DefaultHeadroom as a float64.
	DefaultHeadroomValue = 1.0
	// DefaultThresholdPercent is DefaultThreshold as a float64 percentage.
	DefaultThresholdPercent = 20.0
	// DefaultMaxChangePercent is DefaultMaxChangePerCycle as a float64 percentage.
	DefaultMaxChangePercent = 50.0
	// DefaultResizeIntervalDuration is DefaultResizeInterval as a time.Duration.
	DefaultResizeIntervalDuration = 15 * time.Minute
)

// DefaultCVMeanFloor returns the per-resource mean-usage floors below which the
// maxCV check is skipped (cpu in cores, others in bytes). Returned fresh on every
// call so callers may take ownership of the map.
func DefaultCVMeanFloor() map[string]string {
	return map[string]string{
		"cpu":               "25m",
		"memory":            "25Mi",
		"ephemeral-storage": "2Mi",
	}
}

// ApplyDefaults fills unset spec fields with the canonical runtime defaults.
//
// The policy resolver calls this on the spec copy it returns, never on objects in
// the informer cache — the copy shares maps and slices with the cached object, so
// only value fields are written and maps are only assigned when nil (a fresh
// allocation, never a mutation of a shared map). An explicit empty cvMeanFloor map
// is preserved: it is the way to disable all floors and apply the CV check to every
// resource. Metrics headroom is deliberately not filled here because the Metrics
// slice is shared with the cache; ComputeRecommendation coalesces an empty headroom
// to DefaultHeadroom at use time instead.
func (s *ClusterResourcePolicySpec) ApplyDefaults() {
	r := &s.Readiness
	if r.MinDataPoints <= 0 {
		r.MinDataPoints = DefaultMinDataPoints
	}
	if strings.TrimSpace(r.MinTimeSpan) == "" {
		r.MinTimeSpan = DefaultMinTimeSpan
	}
	if strings.TrimSpace(r.MaxCV) == "" {
		r.MaxCV = DefaultMaxCV
	}
	if r.CVMeanFloor == nil {
		r.CVMeanFloor = DefaultCVMeanFloor()
	}

	b := &s.Behaviors
	if strings.TrimSpace(b.Thresholds.Default) == "" {
		b.Thresholds.Default = DefaultThreshold
	}
	// Thresholds.Resize.Default is deliberately left empty: it is a link in the
	// coalesce chain (resourceThresholds -> resize.default -> thresholds.default),
	// and filling it here would mask a custom thresholds.default. An omitted
	// resize.default falls through to thresholds.default at lookup time.
	if strings.TrimSpace(b.Resize.MaxChangePerCycle) == "" {
		b.Resize.MaxChangePerCycle = DefaultMaxChangePerCycle
	}
	if strings.TrimSpace(b.Resize.Interval) == "" {
		b.Resize.Interval = DefaultResizeInterval
	}
}
