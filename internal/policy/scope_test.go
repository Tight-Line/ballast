package policy_test

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/policy"
)

// TestResolve_NoNamespace_ExcludesResourcePolicies reproduces the defect from
// issue #87. client.InNamespace("") means *all namespaces*, so an Input with no
// namespace used to make every ResourcePolicy in the cluster a candidate, and the
// scope-before-priority rule then handed one of them precedence over every
// ClusterResourcePolicy. A namespace-scoped policy can only be "the more specific
// match" for a workload whose namespace is known, so with none it must not match.
func TestResolve_NoNamespace_ExcludesResourcePolicies(t *testing.T) {
	tuple := map[string]string{"app.kubernetes.io/name": "checkout"}

	tests := []struct {
		name string
		rp   *ballastv1.ResourcePolicy
	}{
		{
			name: "RP whose labelSelector matches the tuple",
			rp: namespacedPolicy("team-a", "rp-teama", 0, ballastv1.PolicySelector{
				LabelSelector: &metav1.LabelSelector{MatchLabels: tuple},
			}),
		},
		{
			name: "RP with an empty selector, which matches everything",
			rp:   namespacedPolicy("team-a", "rp-teama", 0, ballastv1.PolicySelector{}),
		},
		{
			name: "RP with a higher priority than the cluster policy",
			rp:   namespacedPolicy("team-a", "rp-teama", 500, ballastv1.PolicySelector{}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			crp := clusterPolicy("crp-fleet", 100, ballastv1.PolicySelector{})
			r := policy.NewResolver(newClient(t, tc.rp, crp), logr.Discard())

			got, err := r.Resolve(context.Background(), policy.Input{Labels: tuple})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("expected crp-fleet, got nil")
			}
			if got.Name != "crp-fleet" {
				t.Errorf("got policy %q, want %q", got.Name, "crp-fleet")
			}
		})
	}
}

// The scope-before-priority rule is still correct when the namespace is known: a
// namespace owner's policy beats a cluster-wide default.
func TestResolve_WithNamespace_ResourcePolicyStillWins(t *testing.T) {
	rp := namespacedPolicy("team-a", "rp-teama", 0, ballastv1.PolicySelector{})
	crp := clusterPolicy("crp-fleet", 100, ballastv1.PolicySelector{})
	r := policy.NewResolver(newClient(t, rp, crp), logr.Discard())

	got, err := r.Resolve(context.Background(), policy.Input{Namespace: "team-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Name != "rp-teama" {
		t.Fatalf("got %v, want rp-teama", got)
	}
	if !got.Namespaced {
		t.Error("expected Namespaced=true for a ResourcePolicy")
	}
}

// A ResourcePolicy in another namespace must not reach this pod.
func TestResolve_ResourcePolicyDoesNotReachOtherNamespaces(t *testing.T) {
	rp := namespacedPolicy("team-a", "rp-teama", 500, ballastv1.PolicySelector{})
	crp := clusterPolicy("crp-fleet", 0, ballastv1.PolicySelector{})
	r := policy.NewResolver(newClient(t, rp, crp), logr.Discard())

	got, err := r.Resolve(context.Background(), policy.Input{Namespace: "team-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Name != "crp-fleet" {
		t.Fatalf("got %v, want crp-fleet", got)
	}
}

func TestResolve_PopulatesRef(t *testing.T) {
	rp := namespacedPolicy("team-a", "rp-teama", 0, ballastv1.PolicySelector{})
	r := policy.NewResolver(newClient(t, rp), logr.Discard())

	got, err := r.Resolve(context.Background(), policy.Input{Namespace: "team-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ballastv1.PolicyReference{
		Kind:      ballastv1.KindResourcePolicy,
		Namespace: "team-a",
		Name:      "rp-teama",
	}
	if got.Ref != want {
		t.Errorf("Ref = %+v, want %+v", got.Ref, want)
	}
}

func TestResolve_PopulatesClusterRef(t *testing.T) {
	crp := clusterPolicy("crp-fleet", 0, ballastv1.PolicySelector{})
	r := policy.NewResolver(newClient(t, crp), logr.Discard())

	got, err := r.Resolve(context.Background(), policy.Input{Namespace: "team-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ballastv1.PolicyReference{Kind: ballastv1.KindClusterResourcePolicy, Name: "crp-fleet"}
	if got.Ref != want {
		t.Errorf("Ref = %+v, want %+v", got.Ref, want)
	}
}

// -- Load --

func TestLoad_ClusterPolicy(t *testing.T) {
	crp := clusterPolicy("crp-fleet", 7, ballastv1.PolicySelector{})
	r := policy.NewResolver(newClient(t, crp), logr.Discard())

	got, err := r.Load(context.Background(), ballastv1.PolicyReference{
		Kind: ballastv1.KindClusterResourcePolicy,
		Name: "crp-fleet",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Name != "crp-fleet" {
		t.Fatalf("got %v, want crp-fleet", got)
	}
	if got.Namespaced {
		t.Error("expected Namespaced=false for a ClusterResourcePolicy")
	}
	// Defaults are applied at load time, exactly as at resolve time, so a sparse
	// policy tracks the running release rather than whatever was current when it
	// was written.
	if got.Spec.Readiness.MinTimeSpan != ballastv1.DefaultMinTimeSpan {
		t.Errorf("MinTimeSpan = %q, want the release default %q",
			got.Spec.Readiness.MinTimeSpan, ballastv1.DefaultMinTimeSpan)
	}
}

func TestLoad_ResourcePolicy(t *testing.T) {
	rp := namespacedPolicy("team-a", "rp-teama", 0, ballastv1.PolicySelector{})
	r := policy.NewResolver(newClient(t, rp), logr.Discard())

	got, err := r.Load(context.Background(), ballastv1.PolicyReference{
		Kind:      ballastv1.KindResourcePolicy,
		Namespace: "team-a",
		Name:      "rp-teama",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Name != "rp-teama" || !got.Namespaced {
		t.Fatalf("got %v, want namespaced rp-teama", got)
	}
}

// A deleted policy resolves to nil rather than an error: the workloadwatcher's
// policy watch is already migrating those pods to a different profile.
func TestLoad_MissingReturnsNil(t *testing.T) {
	r := policy.NewResolver(newClient(t), logr.Discard())

	for _, ref := range []ballastv1.PolicyReference{
		{Kind: ballastv1.KindClusterResourcePolicy, Name: "gone"},
		{Kind: ballastv1.KindResourcePolicy, Namespace: "team-a", Name: "gone"},
	} {
		got, err := r.Load(context.Background(), ref)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", ref.Key(), err)
		}
		if got != nil {
			t.Errorf("%s: got %v, want nil", ref.Key(), got)
		}
	}
}

func TestLoad_UnknownKind(t *testing.T) {
	r := policy.NewResolver(newClient(t), logr.Discard())

	_, err := r.Load(context.Background(), ballastv1.PolicyReference{Kind: "Nonsense", Name: "x"})
	if err == nil {
		t.Fatal("expected an error for an unknown policy kind")
	}
}

// -- helpers shared with the controllers --

func TestPolicyReferenceKey(t *testing.T) {
	tests := []struct {
		ref  ballastv1.PolicyReference
		want string
	}{
		{ballastv1.PolicyReference{Kind: "ClusterResourcePolicy", Name: "fleet"}, "ClusterResourcePolicy//fleet"},
		{ballastv1.PolicyReference{Kind: "ResourcePolicy", Namespace: "team-a", Name: "local"}, "ResourcePolicy/team-a/local"},
	}
	for _, tc := range tests {
		if got := tc.ref.Key(); got != tc.want {
			t.Errorf("Key() = %q, want %q", got, tc.want)
		}
	}
}

func TestPodAnnotationValue(t *testing.T) {
	cluster := ballastv1.PolicyReference{Kind: ballastv1.KindClusterResourcePolicy, Name: "fleet"}
	if got := policy.PodAnnotationValue(cluster); got != "fleet" {
		t.Errorf("cluster policy annotation = %q, want %q", got, "fleet")
	}

	namespaced := ballastv1.PolicyReference{
		Kind:      ballastv1.KindResourcePolicy,
		Namespace: "team-a",
		Name:      "local",
	}
	if got := policy.PodAnnotationValue(namespaced); got != "team-a/local" {
		t.Errorf("namespaced policy annotation = %q, want %q", got, "team-a/local")
	}
}

func TestInputForPod(t *testing.T) {
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "team-a",
			Name:        "checkout-abc",
			Labels:      map[string]string{"app": "checkout"},
			Annotations: map[string]string{"note": "hi"},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "checkout-1", Controller: &controller},
			},
		},
	}

	got := policy.InputForPod(pod)
	if got.Namespace != "team-a" {
		t.Errorf("Namespace = %q, want team-a", got.Namespace)
	}
	if got.OwnerKind != "ReplicaSet" {
		t.Errorf("OwnerKind = %q, want ReplicaSet", got.OwnerKind)
	}
	if got.Labels["app"] != "checkout" {
		t.Errorf("Labels = %v", got.Labels)
	}
	if got.Annotations["note"] != "hi" {
		t.Errorf("Annotations = %v", got.Annotations)
	}
}

// A pod with only non-controller owner references has no owner kind, so it matches
// only policies that set no Kinds selector.
func TestInputForPod_NoController(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "team-a",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs"}},
		},
	}
	if got := policy.InputForPod(pod).OwnerKind; got != "" {
		t.Errorf("OwnerKind = %q, want empty", got)
	}
}
