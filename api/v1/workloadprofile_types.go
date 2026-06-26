/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WorkloadProfile represents accumulated operational history for a workload identity tuple.
// Users never create or modify these; the operator owns them entirely.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ActiveWorkloads",type="integer",JSONPath=".status.activeWorkloads"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Orphaned",type="string",JSONPath=".status.conditions[?(@.type=='Orphaned')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type WorkloadProfile struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// Status is the observed state. No spec — operator-managed.
	// +optional
	Status WorkloadProfileStatus `json:"status,omitempty"`
}

// WorkloadProfileStatus holds the observed state of a WorkloadProfile.
type WorkloadProfileStatus struct {
	// TupleLabels are the identity labels that define this profile.
	// +optional
	TupleLabels map[string]string `json:"tupleLabels,omitempty"`

	// Containers holds per-container usage statistics and recommendations.
	// +optional
	Containers []ContainerProfile `json:"containers,omitempty"`

	// MeetsThreshold is true when the profile has sufficient history to act on.
	// +optional
	MeetsThreshold bool `json:"meetsThreshold,omitempty"`

	// ActiveWorkloads is the count of workloads currently contributing to this profile.
	// +optional
	ActiveWorkloads int32 `json:"activeWorkloads,omitempty"`

	// Conditions reflect the current state of the profile.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ContainerProfile holds usage statistics and recommendations for a single container.
type ContainerProfile struct {
	// Name is the container name.
	Name string `json:"name"`

	// UsageStats holds raw aggregated usage data per resource per source.
	// +optional
	UsageStats []ContainerUsageStats `json:"usageStats,omitempty"`

	// Recommendations holds the cached recommended resource values.
	// +optional
	Recommendations map[string]ResourceRecommendation `json:"recommendations,omitempty"`
}

// ContainerUsageStats holds aggregated usage statistics for one resource from one source.
type ContainerUsageStats struct {
	// Resource is the Kubernetes resource name (e.g. "cpu", "memory").
	Resource string `json:"resource"`

	// Source is the MetricsSource name that produced these stats.
	Source string `json:"source"`

	// Samples is the number of data points in the window.
	// +optional
	Samples int64 `json:"samples,omitempty"`

	// TimeSpan is the duration covered by the samples.
	// +optional
	TimeSpan string `json:"timeSpan,omitempty"`

	// P50 is the 50th percentile of observed usage.
	// +optional
	P50 string `json:"p50,omitempty"`

	// P95 is the 95th percentile of observed usage.
	// +optional
	P95 string `json:"p95,omitempty"`

	// P99 is the 99th percentile of observed usage.
	// +optional
	P99 string `json:"p99,omitempty"`

	// Mean is the mean observed usage.
	// +optional
	Mean string `json:"mean,omitempty"`

	// StdDev is the standard deviation of observed usage.
	// +optional
	StdDev string `json:"stdDev,omitempty"`

	// CV is the coefficient of variation (stddev/mean).
	// +optional
	CV string `json:"cv,omitempty"`

	// LastUpdated is when this stats block was last refreshed.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

// ResourceRecommendation holds the recommended request and limit for a resource.
type ResourceRecommendation struct {
	// Request is the recommended resource request value.
	// +optional
	Request string `json:"request,omitempty"`

	// Limit is the recommended resource limit value.
	// +optional
	Limit string `json:"limit,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadProfileList contains a list of WorkloadProfile
type WorkloadProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkloadProfile `json:"items"`
}

func init() { // coverage:ignore - kubebuilder boilerplate scheme registration
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WorkloadProfile{}, &WorkloadProfileList{})
		return nil
	})
}
