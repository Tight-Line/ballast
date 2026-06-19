/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
