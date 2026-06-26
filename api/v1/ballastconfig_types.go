/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BallastConfigSpec defines the desired state of BallastConfig.
type BallastConfigSpec struct {
	// IdentityLabels is the ordered list of pod label keys that define a
	// WorkloadProfile identity tuple. Must be set at install time.
	// Changing this after enrollment requires a migration.
	// +kubebuilder:validation:MinItems=1
	IdentityLabels []string `json:"identityLabels"`

	// OrphanTTL is how long to retain an Orphaned WorkloadProfile before deleting it.
	// +optional
	// +kubebuilder:default="168h"
	OrphanTTL string `json:"orphanTTL,omitempty"`

	// RetentionWindow is the default Redis sample retention window.
	// +optional
	// +kubebuilder:default="168h"
	RetentionWindow string `json:"retentionWindow,omitempty"`

	// Suspended halts all Ballast actions when true.
	// Equivalent to the emergency kill-switch ConfigMap.
	// +optional
	// +kubebuilder:default=false
	Suspended bool `json:"suspended,omitempty"`
}

// BallastConfigStatus defines the observed state of BallastConfig.
type BallastConfigStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Suspended",type="boolean",JSONPath=".spec.suspended"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// BallastConfig is the Schema for the ballastconfigs API
type BallastConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BallastConfig
	// +required
	Spec BallastConfigSpec `json:"spec"`

	// status defines the observed state of BallastConfig
	// +optional
	Status BallastConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BallastConfigList contains a list of BallastConfig
type BallastConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BallastConfig `json:"items"`
}

func init() { // coverage:ignore - kubebuilder boilerplate scheme registration
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BallastConfig{}, &BallastConfigList{})
		return nil
	})
}
