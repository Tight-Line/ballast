/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MetricsSourceSpec defines the desired state of MetricsSource.
type MetricsSourceSpec struct {
	// Type identifies the metrics plugin (e.g. "kubernetesMetrics").
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`

	// Config holds plugin-specific configuration.
	// +optional
	Config MetricsSourceConfig `json:"config,omitempty"`
}

// MetricsSourceConfig holds plugin configuration parameters.
type MetricsSourceConfig struct {
	// PollInterval is the minimum time between metric collection cycles.
	// +optional
	// +kubebuilder:default="60s"
	PollInterval string `json:"pollInterval,omitempty"`

	// ReservoirSize is the hard cap on Redis sorted-set entries per container per metric.
	// +optional
	// +kubebuilder:default=10000
	// +kubebuilder:validation:Minimum=1
	ReservoirSize int64 `json:"reservoirSize,omitempty"`
}

// MetricsSourceStatus defines the observed state of MetricsSource.
type MetricsSourceStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="PollInterval",type="string",JSONPath=".spec.config.pollInterval"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MetricsSource is the Schema for the metricssources API
type MetricsSource struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of MetricsSource
	// +required
	Spec MetricsSourceSpec `json:"spec"`

	// status defines the observed state of MetricsSource
	// +optional
	Status MetricsSourceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MetricsSourceList contains a list of MetricsSource
type MetricsSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MetricsSource `json:"items"`
}

func init() { // coverage:ignore - kubebuilder boilerplate scheme registration
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &MetricsSource{}, &MetricsSourceList{})
		return nil
	})
}
