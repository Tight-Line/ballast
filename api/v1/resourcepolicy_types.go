/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ResourcePolicySpec defines the desired state of ResourcePolicy.
// It uses the same shape as ClusterResourcePolicySpec.
type ResourcePolicySpec = ClusterResourcePolicySpec

// ResourcePolicyStatus defines the observed state of ResourcePolicy.
type ResourcePolicyStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ResourcePolicy is the Schema for the resourcepolicies API
type ResourcePolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ResourcePolicy
	// +required
	Spec ResourcePolicySpec `json:"spec"`

	// status defines the observed state of ResourcePolicy
	// +optional
	Status ResourcePolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ResourcePolicyList contains a list of ResourcePolicy
type ResourcePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ResourcePolicy `json:"items"`
}

func init() { // coverage:ignore - kubebuilder boilerplate scheme registration
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ResourcePolicy{}, &ResourcePolicyList{})
		return nil
	})
}
