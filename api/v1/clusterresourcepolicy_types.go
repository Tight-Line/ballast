/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterResourcePolicySpec defines the desired state of ClusterResourcePolicy.
type ClusterResourcePolicySpec struct {
	// Priority determines which policy wins when multiple match a workload.
	// Higher value wins. Same-priority ties break alphabetically by name.
	// +optional
	// +kubebuilder:default=0
	Priority int32 `json:"priority,omitempty"`

	// Selector controls which workloads this policy applies to.
	// +optional
	Selector PolicySelector `json:"selector,omitempty"`

	// Metrics defines what resource fields to measure and recommend.
	// +optional
	Metrics []MetricConfig `json:"metrics,omitempty"`

	// Readiness defines the conditions that must pass before Ballast acts.
	// +optional
	Readiness ReadinessConfig `json:"readiness,omitempty"`

	// Behaviors defines thresholds and parameters for resize.
	// +optional
	Behaviors BehaviorConfig `json:"behaviors,omitempty"`
}

// PolicySelector filters which workloads a policy applies to.
type PolicySelector struct {
	// Kinds lists the owner kinds this policy applies to (e.g. Deployment, StatefulSet).
	// Empty means all kinds.
	// +optional
	Kinds []string `json:"kinds,omitempty"`

	// Namespaces filters by namespace name patterns.
	// +optional
	Namespaces NamespaceSelector `json:"namespaces,omitempty"`

	// Annotations maps annotation keys to regex patterns that must match on the pod.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// LabelSelector is a standard Kubernetes label selector applied to pod labels.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// NamespaceSelector filters workloads by namespace name.
type NamespaceSelector struct {
	// Include is a list of patterns; the pod's namespace must match at least one.
	// Patterns wrapped in forward slashes are full-string regexes (e.g. /.*-prod/).
	// All other patterns are exact string matches. Absent means all namespaces pass.
	// +optional
	Include []string `json:"include,omitempty"`

	// Exclude is a list of patterns; the pod's namespace must NOT match any.
	// Uses the same /regex/ or exact syntax as Include.
	// A namespace matching both Include and Exclude is excluded (logged at WARN).
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// MetricConfig maps a resource usage measurement to a Kubernetes resource field.
type MetricConfig struct {
	// Resource is the Kubernetes resource being measured (e.g. "cpu", "memory").
	// +kubebuilder:validation:MinLength=1
	Resource string `json:"resource"`

	// Field is the Kubernetes resource field to set: "request" or "limit".
	// +kubebuilder:validation:Enum=request;limit
	Field string `json:"field"`

	// Source is the name of the MetricsSource to use.
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`

	// Aggregation is the usage percentile to use: p50, p75, p90, p95, p99, max, avg.
	// +kubebuilder:validation:Enum=p50;p75;p90;p95;p99;max;avg
	Aggregation string `json:"aggregation"`

	// Headroom is the multiplier applied to the aggregated usage value.
	// Expressed as a decimal string (e.g. "1.2"); must be greater than 0.
	// +kubebuilder:default="1.0"
	Headroom string `json:"headroom,omitempty"`
}

// ReadinessConfig defines conditions that must pass before Ballast will act.
type ReadinessConfig struct {
	// MinDataPoints is the minimum number of samples required.
	// +optional
	// +kubebuilder:default=500
	// +kubebuilder:validation:Minimum=1
	MinDataPoints int64 `json:"minDataPoints,omitempty"`

	// MinTimeSpan is the minimum duration of observed history required.
	// +optional
	// +kubebuilder:default="24h"
	MinTimeSpan string `json:"minTimeSpan,omitempty"`

	// MaxCV is the maximum coefficient of variation (stddev/mean) allowed.
	// +optional
	// +kubebuilder:default="1.5"
	MaxCV string `json:"maxCV,omitempty"`

	// CVMeanFloor exempts a resource from the MaxCV check when its observed
	// mean usage is below the given quantity (cpu in cores, others in bytes).
	// CV is numerically unstable near a zero mean: quantization noise and rare
	// one-off spikes (e.g. process startup) produce huge CVs on workloads whose
	// usage is negligible, which would otherwise leave the profile Accruing
	// forever and block recommendations for every other resource. Usage below
	// the floor is too small for a mis-sized recommendation to matter. The
	// floors are deliberately tiny; resources without an entry (or set to "0")
	// always get the CV check.
	// +optional
	// +kubebuilder:default={"cpu": "25m", "memory": "25Mi", "ephemeral-storage": "2Mi"}
	CVMeanFloor map[string]string `json:"cvMeanFloor,omitempty"`
}

// BehaviorConfig defines thresholds and parameters for resize actions.
type BehaviorConfig struct {
	// Thresholds defines drift thresholds that trigger actions.
	// +optional
	Thresholds ThresholdConfig `json:"thresholds,omitempty"`

	// Resize configures in-place pod resize behavior.
	// +optional
	Resize ResizeConfig `json:"resize,omitempty"`
}

// ThresholdConfig defines drift thresholds for resize.
type ThresholdConfig struct {
	// Default is the global fallback drift threshold for all behaviors and resources.
	// +optional
	// +kubebuilder:default="20%"
	Default string `json:"default,omitempty"`

	// Resize contains thresholds specific to the resize behavior.
	// +optional
	Resize ResizeThresholds `json:"resize,omitempty"`
}

// ResizeThresholds defines drift thresholds for resize triggering.
type ResizeThresholds struct {
	// Default overrides the global default threshold for resize.
	// +optional
	// +kubebuilder:default="20%"
	Default string `json:"default,omitempty"`

	// ResourceThresholds provides per-resource per-field overrides.
	// Coalesce order: resourceThresholds -> resize.default -> thresholds.default
	// +optional
	ResourceThresholds map[string]ResourceFieldThresholds `json:"resourceThresholds,omitempty"`
}

// ResourceFieldThresholds maps resource fields (request, limit) to drift thresholds.
type ResourceFieldThresholds struct {
	// Request is the drift threshold for the request field.
	// +optional
	Request string `json:"request,omitempty"`

	// Limit is the drift threshold for the limit field.
	// +optional
	Limit string `json:"limit,omitempty"`
}

// ResizeConfig configures in-place pod resize behavior.
type ResizeConfig struct {
	// MaxChangePerCycle caps how much a single adjustment cycle can change a value,
	// expressed as a percentage of the gap between the current and recommended
	// values. When a capped step would land within the drift threshold of the
	// recommendation, the recommendation is applied exactly instead.
	// +optional
	// +kubebuilder:default="50%"
	MaxChangePerCycle string `json:"maxChangePerCycle,omitempty"`

	// Interval is how often the ResourceAdjuster re-evaluates drift.
	// +optional
	// +kubebuilder:default="15m"
	Interval string `json:"interval,omitempty"`
}

// ClusterResourcePolicyStatus defines the observed state of ClusterResourcePolicy.
type ClusterResourcePolicyStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ClusterResourcePolicy is the Schema for the clusterresourcepolicies API
type ClusterResourcePolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterResourcePolicy
	// +required
	Spec ClusterResourcePolicySpec `json:"spec"`

	// status defines the observed state of ClusterResourcePolicy
	// +optional
	Status ClusterResourcePolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterResourcePolicyList contains a list of ClusterResourcePolicy
type ClusterResourcePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterResourcePolicy `json:"items"`
}

func init() { // coverage:ignore - kubebuilder boilerplate scheme registration
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterResourcePolicy{}, &ClusterResourcePolicyList{})
		return nil
	})
}
