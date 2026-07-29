/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package v1_test

import (
	"testing"

	ballastv1 "github.com/tight-line/ballast/api/v1"
)

// Key is what distinguishes genuinely different policies wherever a policy
// identity is hashed or compared, so kind and namespace must both participate:
// two ResourcePolicies in different namespaces may share a name.
func TestPolicyReference_Key(t *testing.T) {
	tests := []struct {
		name string
		ref  ballastv1.PolicyReference
		want string
	}{
		{
			name: "cluster-scoped policy has no namespace segment",
			ref:  ballastv1.PolicyReference{Kind: ballastv1.KindClusterResourcePolicy, Name: "fleet"},
			want: "ClusterResourcePolicy//fleet",
		},
		{
			name: "namespaced policy carries its namespace",
			ref: ballastv1.PolicyReference{
				Kind:      ballastv1.KindResourcePolicy,
				Namespace: "team-a",
				Name:      "local",
			},
			want: "ResourcePolicy/team-a/local",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.Key(); got != tc.want {
				t.Errorf("Key() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Same name, different namespaces: different policies, so different keys.
func TestPolicyReference_KeyDistinguishesNamespace(t *testing.T) {
	a := ballastv1.PolicyReference{Kind: ballastv1.KindResourcePolicy, Namespace: "team-a", Name: "defaults"}
	b := ballastv1.PolicyReference{Kind: ballastv1.KindResourcePolicy, Namespace: "team-b", Name: "defaults"}
	if a.Key() == b.Key() {
		t.Errorf("same key %q for policies in different namespaces", a.Key())
	}
}
