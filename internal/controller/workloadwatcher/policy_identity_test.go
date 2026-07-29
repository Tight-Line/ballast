package workloadwatcher_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/naming"
	"github.com/tight-line/ballast/internal/store"
	"github.com/tight-line/ballast/internal/validation"
)

// -- fixtures --

func enrolledPod(namespace, name, app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{"app": app, validation.LabelMode: validation.ModeMeasure},
		},
	}
}

func fleetPolicy(name string, priority int32) *ballastv1.ClusterResourcePolicy {
	return &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ballastv1.ClusterResourcePolicySpec{Priority: priority},
	}
}

func teamPolicy(namespace, name string) *ballastv1.ResourcePolicy {
	return &ballastv1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       ballastv1.ResourcePolicySpec{},
	}
}

func profileNameFor(tuple string, ref ballastv1.PolicyReference) string {
	return tuple + "--" + naming.PolicyDiscriminator(ref.Kind, ref.Namespace, ref.Name)
}

func clusterRef(name string) ballastv1.PolicyReference {
	return ballastv1.PolicyReference{Kind: ballastv1.KindClusterResourcePolicy, Name: name}
}

func namespacedRef(namespace, name string) ballastv1.PolicyReference {
	return ballastv1.PolicyReference{
		Kind:      ballastv1.KindResourcePolicy,
		Namespace: namespace,
		Name:      name,
	}
}

func getProfile(t *testing.T, c client.Client, name string) *ballastv1.WorkloadProfile {
	t.Helper()
	var p ballastv1.WorkloadProfile
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &p); err != nil {
		t.Fatalf("Get WorkloadProfile %q: %v", name, err)
	}
	return &p
}

// -- identity --

// The workloadwatcher is the only component with a pod in hand, so it is where
// policy is resolved. The resolved policy becomes part of the profile's identity.
func TestPodReconciler_ProfileNameIncludesPolicy(t *testing.T) {
	ref := clusterRef("fleet")
	fc := newFakeClient(defaultBallastConfig(), fleetPolicy("fleet", 0), enrolledPod("default", "web-abc", "web"))
	_, rc := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)

	reconcilePod(t, c, "default", "web-abc")

	want := profileNameFor("web", ref)
	profile := getProfile(t, fc, want)

	if profile.Status.PolicyRef == nil {
		t.Fatal("status.policyRef must record the governing policy")
	}
	if *profile.Status.PolicyRef != ref {
		t.Errorf("policyRef = %+v, want %+v", *profile.Status.PolicyRef, ref)
	}
	if got, want := profile.Status.MeasurementHash, store.MeasurementHash(map[string]string{"app": "web"}, ref.Key()); got != want {
		t.Errorf("measurementHash = %q, want %q", got, want)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &pod); err != nil {
		t.Fatalf("Get pod: %v", err)
	}
	if got := pod.Annotations[workloadwatcher.AnnotationProfileRef]; got != want {
		t.Errorf("profile-ref = %q, want %q", got, want)
	}
	if got := pod.Annotations[validation.AnnotationPolicyRef]; got != "fleet" {
		t.Errorf("policy-ref = %q, want %q", got, "fleet")
	}
}

// Two pods sharing a label tuple but governed by different policies cannot share
// one profile: a profile holds one set of recommendations, and the policy decides
// how they are produced. Here the distinguishing dimension is the namespace, which
// is expressible in a pod query, so the split is real.
func TestPodReconciler_SameTupleDifferentPolicies_SplitsProfiles(t *testing.T) {
	fc := newFakeClient(
		defaultBallastConfig(),
		fleetPolicy("fleet", 0),
		teamPolicy("team-a", "local"),
		enrolledPod("team-a", "web-a", "web"),
		enrolledPod("team-b", "web-b", "web"),
	)
	_, rc := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)

	reconcilePod(t, c, "team-a", "web-a")
	reconcilePod(t, c, "team-b", "web-b")

	// team-a has its own ResourcePolicy, which outranks the fleet default for pods
	// in that namespace only.
	teamProfile := getProfile(t, fc, profileNameFor("web", namespacedRef("team-a", "local")))
	fleetProfile := getProfile(t, fc, profileNameFor("web", clusterRef("fleet")))

	if teamProfile.Name == fleetProfile.Name {
		t.Fatal("expected two distinct profiles")
	}
	if teamProfile.Status.MeasurementHash == fleetProfile.Status.MeasurementHash {
		t.Error("sibling profiles must own separate Redis key namespaces")
	}
	if teamProfile.Status.ActiveWorkloads != 1 || fleetProfile.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads = %d and %d, want 1 each",
			teamProfile.Status.ActiveWorkloads, fleetProfile.Status.ActiveWorkloads)
	}
	// The tuple itself is unchanged: it stays a map of real pod labels, which is
	// what keeps pod selection working.
	if teamProfile.Status.TupleLabels["app"] != "web" {
		t.Errorf("tupleLabels = %v, want app=web", teamProfile.Status.TupleLabels)
	}
}

// Applying a policy to a running cluster must migrate pods without waiting for pod
// churn. This is the reconcile the policy watch triggers.
func TestPodReconciler_PolicyAppliedAtRuntime_MigratesProfile(t *testing.T) {
	pod := enrolledPod("default", "web-abc", "web")
	fc := newFakeClient(defaultBallastConfig(), pod)
	_, rc := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)

	// No policy yet: the pod is tracked under a NoPolicy profile.
	reconcilePod(t, c, "default", "web-abc")
	before := getProfile(t, fc, noPolicyProfile("web"))
	if before.Status.PolicyRef != nil {
		t.Error("expected no policyRef before any policy exists")
	}

	// A policy is applied at runtime.
	if err := fc.Create(context.Background(), fleetPolicy("fleet", 0)); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	reconcilePod(t, c, "default", "web-abc")

	after := getProfile(t, fc, profileNameFor("web", clusterRef("fleet")))
	if after.Status.PolicyRef == nil || after.Status.PolicyRef.Name != "fleet" {
		t.Fatalf("policyRef = %+v, want fleet", after.Status.PolicyRef)
	}
	if after.Status.ActiveWorkloads != 1 {
		t.Errorf("new profile activeWorkloads = %d, want 1", after.Status.ActiveWorkloads)
	}

	// The profile the pod left drops to zero and orphans, so it ages out.
	old := getProfile(t, fc, noPolicyProfile("web"))
	if old.Status.ActiveWorkloads != 0 {
		t.Errorf("old profile activeWorkloads = %d, want 0", old.Status.ActiveWorkloads)
	}

	// The pod's policy-ref must not keep advertising the stale answer.
	var got corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &got); err != nil {
		t.Fatalf("Get pod: %v", err)
	}
	if ref := got.Annotations[validation.AnnotationPolicyRef]; ref != "fleet" {
		t.Errorf("policy-ref = %q, want fleet", ref)
	}
}

// Removing the last matching policy moves the pod back to a NoPolicy profile and
// clears the policy-ref annotation rather than leaving a dangling reference.
func TestPodReconciler_PolicyRemovedAtRuntime_ClearsPolicyRef(t *testing.T) {
	policy := fleetPolicy("fleet", 0)
	fc := newFakeClient(defaultBallastConfig(), policy, enrolledPod("default", "web-abc", "web"))
	_, rc := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)

	reconcilePod(t, c, "default", "web-abc")
	if err := fc.Delete(context.Background(), policy); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	reconcilePod(t, c, "default", "web-abc")

	profile := getProfile(t, fc, noPolicyProfile("web"))
	if profile.Status.PolicyRef != nil {
		t.Errorf("policyRef = %+v, want nil", profile.Status.PolicyRef)
	}

	var pod corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &pod); err != nil {
		t.Fatalf("Get pod: %v", err)
	}
	if ref, ok := pod.Annotations[validation.AnnotationPolicyRef]; ok {
		t.Errorf("policy-ref = %q, want the annotation removed", ref)
	}
}

// Re-reconciling an unchanged pod must not rewrite anything, or every policy event
// would churn the whole fleet's annotations.
func TestPodReconciler_StableIdentityDoesNotRewriteAnnotations(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), fleetPolicy("fleet", 0), enrolledPod("default", "web-abc", "web"))
	_, rc := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)

	reconcilePod(t, c, "default", "web-abc")
	var first corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &first); err != nil {
		t.Fatalf("Get pod: %v", err)
	}

	reconcilePod(t, c, "default", "web-abc")
	var second corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &second); err != nil {
		t.Fatalf("Get pod: %v", err)
	}

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("pod was rewritten on a no-op reconcile: %s -> %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}

// -- finalizer purge --

// Each profile owns its own Redis key namespace, which is what lets the finalizer
// purge without reference counting. Deleting one profile must leave its sibling's
// samples intact even though both cover the same label tuple.
func TestProfileReconciler_PurgeLeavesSiblingHistoryIntact(t *testing.T) {
	ctx := context.Background()
	tuple := map[string]string{"app": "web"}
	doomedHash := store.MeasurementHash(tuple, namespacedRef("team-a", "local").Key())
	siblingHash := store.MeasurementHash(tuple, clusterRef("fleet").Key())

	doomed := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:       profileNameFor("web", namespacedRef("team-a", "local")),
			Finalizers: []string{workloadwatcher.ProfileFinalizerName},
		},
		Status: ballastv1.WorkloadProfileStatus{TupleLabels: tuple, MeasurementHash: doomedHash},
	}
	fc := newFakeClient(defaultBallastConfig(), doomed)
	_, rc := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)

	doomedKey := store.MetricKey(doomedHash, "app", "cpu")
	siblingKey := store.MetricKey(siblingHash, "app", "cpu")
	for _, key := range []string{doomedKey, siblingKey} {
		if err := store.AddSample(ctx, rc, key, 1, "100", 100); err != nil {
			t.Fatalf("AddSample %s: %v", key, err)
		}
	}

	if err := fc.Delete(ctx, doomed); err != nil {
		t.Fatalf("delete profile: %v", err)
	}
	if _, err := reconcileProfile(t, c, doomed.Name); err != nil {
		t.Fatalf("Profile.Reconcile: %v", err)
	}

	if n, err := store.SampleCount(ctx, rc, doomedKey); err != nil || n != 0 {
		t.Errorf("deleted profile's samples: got %d (err %v), want 0", n, err)
	}
	if n, err := store.SampleCount(ctx, rc, siblingKey); err != nil || n != 1 {
		t.Errorf("sibling profile's samples: got %d (err %v), want 1 (must survive)", n, err)
	}
}

// A profile inherited from a release that predates measurement hashes holds its
// samples under the bare tuple hash. Its keys must still be purged when it ages
// out, which is how every profile carried across the upgrade is cleaned up.
func TestProfileReconciler_PurgeFallsBackToTupleHash(t *testing.T) {
	ctx := context.Background()
	tuple := map[string]string{"app": "web"}

	legacy := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "web",
			Finalizers: []string{workloadwatcher.ProfileFinalizerName},
		},
		Status: ballastv1.WorkloadProfileStatus{TupleLabels: tuple}, // no MeasurementHash
	}
	fc := newFakeClient(defaultBallastConfig(), legacy)
	_, rc := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)

	key := store.MetricKey(store.TupleHash(tuple), "app", "cpu")
	if err := store.AddSample(ctx, rc, key, 1, "100", 100); err != nil {
		t.Fatalf("AddSample: %v", err)
	}

	if err := fc.Delete(ctx, legacy); err != nil {
		t.Fatalf("delete profile: %v", err)
	}
	if _, err := reconcileProfile(t, c, "web"); err != nil {
		t.Fatalf("Profile.Reconcile: %v", err)
	}

	if n, err := store.SampleCount(ctx, rc, key); err != nil || n != 0 {
		t.Errorf("legacy samples: got %d (err %v), want 0", n, err)
	}
}
