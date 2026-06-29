package workloadwatcher_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/plugin"
	"github.com/tight-line/ballast/internal/store"
)

// -- scheme & client helpers --

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = ballastv1.AddToScheme(s)
	return s
}

func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&ballastv1.WorkloadProfile{}).
		WithObjects(objs...).
		Build()
}

// inactiveKS returns a KillSwitch whose IsActive() returns false.
func inactiveKS(t *testing.T) *killswitch.KillSwitch {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	ks := killswitch.New(fc, "ballast-system", nil)
	if _, err := ks.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("ks.Reconcile: %v", err)
	}
	return ks
}

// activeKS returns a KillSwitch whose IsActive() returns true (ConfigMap present).
func activeKS(t *testing.T) *killswitch.KillSwitch {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      killswitch.ConfigMapName,
		Namespace: "ballast-system",
	}}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()
	ks := killswitch.New(fc, "ballast-system", nil)
	if _, err := ks.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("ks.Reconcile: %v", err)
	}
	return ks
}

// newMiniredisClient starts an in-memory Redis and returns a store.Client backed by it.
func newMiniredisClient(t *testing.T) (*miniredis.Miniredis, store.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return mr, rc
}

// defaultBallastConfig returns a BallastConfig with identityLabels=["app"].
func defaultBallastConfig() *ballastv1.BallastConfig {
	return &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec: ballastv1.BallastConfigSpec{
			IdentityLabels: []string{"app"},
			OrphanTTL:      "168h",
		},
	}
}

func reconcilePod(t *testing.T, c *workloadwatcher.Controller, ns, name string) {
	t.Helper()
	_, err := c.Pod.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	if err != nil {
		t.Fatalf("Pod.Reconcile: %v", err)
	}
}

func reconcileProfile(t *testing.T, c *workloadwatcher.Controller, name string) (ctrl.Result, error) {
	t.Helper()
	return c.Profile.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
}

// -- pod reconciler unit tests --

func TestPodReconciler_NewPod(t *testing.T) {
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-abc",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"app": "web"},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), pod)
	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	reconcilePod(t, c, "default", "web-abc")

	// WorkloadProfile should be created.
	var profile ballastv1.WorkloadProfile
	profName := "web"
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &profile); err != nil {
		t.Fatalf("Get WorkloadProfile %q: %v", profName, err)
	}
	if profile.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads: got %d, want 1", profile.Status.ActiveWorkloads)
	}
	if profile.Status.TupleLabels["app"] != "web" {
		t.Errorf("tupleLabels[app]: got %q, want %q", profile.Status.TupleLabels["app"], "web")
	}

	// Pod should have the finalizer.
	var gotPod corev1.Pod
	if err := fc.Get(ctx, types.NamespacedName{Namespace: "default", Name: "web-abc"}, &gotPod); err != nil {
		t.Fatalf("Get pod: %v", err)
	}
	hasFinalizer := false
	for _, f := range gotPod.Finalizers {
		if f == workloadwatcher.FinalizerName {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("expected pod to have workloadwatcher finalizer")
	}

	// Pod should have the profile-ref annotation.
	if gotPod.Annotations[workloadwatcher.AnnotationProfileRef] != profName {
		t.Errorf("profile-ref annotation: got %q, want %q",
			gotPod.Annotations[workloadwatcher.AnnotationProfileRef], profName)
	}
}

func TestPodReconciler_AlreadyProcessed(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationMeasure:    "true",
				workloadwatcher.AnnotationProfileRef: profName,
			},
			Labels:     map[string]string{"app": "web"},
			Finalizers: []string{workloadwatcher.FinalizerName},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, pod)
	// Set initial activeWorkloads=1 via status subresource.
	profile.Status.ActiveWorkloads = 1
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)
	reconcilePod(t, c, "default", "web-abc")
	reconcilePod(t, c, "default", "web-abc") // second reconcile must be a no-op

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads: got %d, want 1 (second reconcile must not double-increment)", got.Status.ActiveWorkloads)
	}
}

func TestPodReconciler_AbsentIdentityLabelUsesPlaceholder(t *testing.T) {
	ctx := context.Background()

	// Pod is opted in but missing the "app" identity label entirely.
	// The reconciler should create a profile using the "noapp" placeholder.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-abc",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"tier": "backend"},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), pod)
	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	reconcilePod(t, c, "default", "web-abc")

	var list ballastv1.WorkloadProfileList
	if err := fc.List(ctx, &list); err != nil {
		t.Fatalf("List WorkloadProfiles: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 WorkloadProfile, got %d", len(list.Items))
	}
	if got := list.Items[0].Name; got != "noapp" {
		t.Errorf("profile name = %q, want %q", got, "noapp")
	}
}

func TestPodReconciler_KillSwitchSuppresses(t *testing.T) {
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-abc",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"app": "web"},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), pod)
	c := workloadwatcher.New(fc, activeKS(t), nil, nil)

	reconcilePod(t, c, "default", "web-abc")

	var list ballastv1.WorkloadProfileList
	if err := fc.List(ctx, &list); err != nil {
		t.Fatalf("List WorkloadProfiles: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no WorkloadProfiles when kill switch active, got %d", len(list.Items))
	}
}

func TestPodReconciler_DeleteDecrement(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationMeasure:    "true",
				workloadwatcher.AnnotationProfileRef: profName,
			},
			Labels:     map[string]string{"app": "web"},
			Finalizers: []string{workloadwatcher.FinalizerName},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, pod)
	profile.Status.ActiveWorkloads = 2
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	// Trigger deletion — fake client sets DeletionTimestamp because finalizer is present.
	if err := fc.Delete(ctx, pod); err != nil {
		t.Fatalf("Delete pod: %v", err)
	}

	reconcilePod(t, c, "default", "web-abc")

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads: got %d, want 1", got.Status.ActiveWorkloads)
	}
	// No Orphaned condition when count > 0.
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Orphaned"); c != nil {
		t.Errorf("expected no Orphaned condition when activeWorkloads=1, got %+v", c)
	}

	// Finalizer should be removed; the fake client fully deletes the pod once
	// no finalizers remain, so NotFound is also acceptable here.
	var gotPod corev1.Pod
	err := fc.Get(ctx, types.NamespacedName{Namespace: "default", Name: "web-abc"}, &gotPod)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("Get pod: %v", err)
	}
	if err == nil {
		for _, f := range gotPod.Finalizers {
			if f == workloadwatcher.FinalizerName {
				t.Error("expected workloadwatcher finalizer to be removed after delete")
			}
		}
	}
}

func TestPodReconciler_DeleteOrphanTransition(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationMeasure:    "true",
				workloadwatcher.AnnotationProfileRef: profName,
			},
			Labels:     map[string]string{"app": "web"},
			Finalizers: []string{workloadwatcher.FinalizerName},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, pod)
	profile.Status.ActiveWorkloads = 1
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	if err := fc.Delete(ctx, pod); err != nil {
		t.Fatalf("Delete pod: %v", err)
	}

	reconcilePod(t, c, "default", "web-abc")

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.ActiveWorkloads != 0 {
		t.Errorf("activeWorkloads: got %d, want 0", got.Status.ActiveWorkloads)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "Orphaned")
	if cond == nil {
		t.Fatal("expected Orphaned condition to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Orphaned condition status: got %v, want True", cond.Status)
	}
}

func TestPodReconciler_DeleteKillSwitchAllowsDecrement(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationMeasure:    "true",
				workloadwatcher.AnnotationProfileRef: profName,
			},
			Labels:     map[string]string{"app": "web"},
			Finalizers: []string{workloadwatcher.FinalizerName},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, pod)
	profile.Status.ActiveWorkloads = 1
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	// Kill switch is active, but decrement must still happen.
	c := workloadwatcher.New(fc, activeKS(t), nil, nil)

	if err := fc.Delete(ctx, pod); err != nil {
		t.Fatalf("Delete pod: %v", err)
	}

	reconcilePod(t, c, "default", "web-abc")

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.ActiveWorkloads != 0 {
		t.Errorf("activeWorkloads: got %d, want 0 (kill switch must not suppress decrement)", got.Status.ActiveWorkloads)
	}
}

func TestPodReconciler_NewPodClearsOrphan(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-xyz",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"app": "web"},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, pod)

	// Set profile as orphaned with zero workloads.
	profile.Status.ActiveWorkloads = 0
	profile.Status.TupleLabels = map[string]string{"app": "web"}
	profile.Status.Conditions = []metav1.Condition{{
		Type:               "Orphaned",
		Status:             metav1.ConditionTrue,
		Reason:             "NoActiveWorkloads",
		LastTransitionTime: metav1.Now(),
	}}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)
	reconcilePod(t, c, "default", "web-xyz")

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads: got %d, want 1", got.Status.ActiveWorkloads)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, "Orphaned"); cond != nil && cond.Status == metav1.ConditionTrue {
		t.Error("expected Orphaned condition to be cleared when new pod arrives")
	}
}

func TestPodReconciler_BallastConfigNotFound(t *testing.T) {
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-abc",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"app": "web"},
		},
	}
	// No BallastConfig in the fake store.
	fc := newFakeClient(pod)
	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	reconcilePod(t, c, "default", "web-abc")

	var list ballastv1.WorkloadProfileList
	if err := fc.List(ctx, &list); err != nil {
		t.Fatalf("List WorkloadProfiles: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no WorkloadProfiles when BallastConfig is absent, got %d", len(list.Items))
	}
}

func TestPodReconciler_RecoveryAddFinalizer(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: profName}}
	// Pod already has profile-ref (was previously processed) but our finalizer is missing.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationMeasure:    "true",
				workloadwatcher.AnnotationProfileRef: profName,
			},
			Labels: map[string]string{"app": "web"},
			// No finalizer — simulates a partial failure on a prior reconcile.
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, pod)
	profile.Status.ActiveWorkloads = 1
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)
	reconcilePod(t, c, "default", "web-abc")

	var gotPod corev1.Pod
	if err := fc.Get(ctx, types.NamespacedName{Namespace: "default", Name: "web-abc"}, &gotPod); err != nil {
		t.Fatalf("Get pod: %v", err)
	}
	hasFinalizer := false
	for _, f := range gotPod.Finalizers {
		if f == workloadwatcher.FinalizerName {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("expected finalizer to be re-added during recovery")
	}

	// activeWorkloads must NOT be double-incremented.
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads: got %d, want 1 (must not increment on recovery)", got.Status.ActiveWorkloads)
	}
}

func TestPodReconciler_DeleteNoFinalizer(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: profName}}
	// Pod is being deleted (held by a foreign finalizer) but lacks our finalizer.
	// Our finalizer is the "we've counted this pod" marker — without it, we skip
	// the decrement to avoid double-counting the reconcile that fires after we
	// remove the finalizer on a normally-tracked pod. If the finalizer was externally
	// stripped, activeWorkloads leaks by 1; recovery requires manual correction of
	// the WorkloadProfile status.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationMeasure:    "true",
				workloadwatcher.AnnotationProfileRef: profName,
			},
			Labels:     map[string]string{"app": "web"},
			Finalizers: []string{"other-controller.io/cleanup"}, // foreign finalizer keeps pod alive
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, pod)
	profile.Status.ActiveWorkloads = 1
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	if err := fc.Delete(ctx, pod); err != nil {
		t.Fatalf("Delete pod: %v", err)
	}

	reconcilePod(t, c, "default", "web-abc")

	// No decrement: our finalizer is absent, so this is a no-op to prevent
	// the double-decrement that would occur when we remove our own finalizer.
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads: got %d, want 1 (no decrement without our finalizer)", got.Status.ActiveWorkloads)
	}
}

func TestPodReconciler_SpecialLabelChars(t *testing.T) {
	ctx := context.Background()

	// Label values with underscores and dots must be sanitized in the profile name.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-abc",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"app": "my_app.v2"},
		},
	}
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ballast"},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app"}, OrphanTTL: "168h"},
	}
	fc := newFakeClient(cfg, pod)
	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	reconcilePod(t, c, "default", "web-abc")

	// "my_app.v2" sanitizes to "my-app-v2" → profile name "my-app-v2".
	expectedName := "my-app-v2"
	var profile ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: expectedName}, &profile); err != nil {
		t.Fatalf("Get WorkloadProfile %q: %v (sanitization of special chars may be wrong)", expectedName, err)
	}
}

// -- ExtractTupleLabels unit tests --

func TestExtractTupleLabels(t *testing.T) {
	cases := []struct {
		name           string
		podLabels      map[string]string
		identityLabels []string
		want           map[string]string
	}{
		{
			name:           "all labels present",
			podLabels:      map[string]string{"app": "web", "tier": "frontend"},
			identityLabels: []string{"app", "tier"},
			want:           map[string]string{"app": "web", "tier": "frontend"},
		},
		{
			name:           "absent label uses placeholder - bare key",
			podLabels:      map[string]string{"tier": "backend"},
			identityLabels: []string{"app"},
			want:           map[string]string{"app": "noapp"},
		},
		{
			name:           "absent label uses placeholder - namespaced key",
			podLabels:      map[string]string{"app.kubernetes.io/name": "nginx"},
			identityLabels: []string{"app.kubernetes.io/name", "app.kubernetes.io/component"},
			want: map[string]string{
				"app.kubernetes.io/name":      "nginx",
				"app.kubernetes.io/component": "nocomponent",
			},
		},
		{
			name:           "absent label uses placeholder - dotted key no slash",
			podLabels:      map[string]string{},
			identityLabels: []string{"foo.bar.baz"},
			want:           map[string]string{"foo.bar.baz": "nofoobarbaz"},
		},
		{
			name:           "all labels absent",
			podLabels:      map[string]string{},
			identityLabels: []string{"app", "tier"},
			want:           map[string]string{"app": "noapp", "tier": "notier"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workloadwatcher.ExtractTupleLabels(tc.podLabels, tc.identityLabels)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, wantV := range tc.want {
				if got[k] != wantV {
					t.Errorf("key %q: got %q, want %q", k, got[k], wantV)
				}
			}
		})
	}
}

// -- ExtractSelectorLabels unit tests --

func TestExtractSelectorLabels(t *testing.T) {
	cases := []struct {
		name           string
		podLabels      map[string]string
		identityLabels []string
		want           map[string]string
	}{
		{
			name:           "all labels present",
			podLabels:      map[string]string{"app": "web", "tier": "frontend"},
			identityLabels: []string{"app", "tier"},
			want:           map[string]string{"app": "web", "tier": "frontend"},
		},
		{
			name:           "absent label uses LabelAbsent sentinel",
			podLabels:      map[string]string{"app.kubernetes.io/name": "nginx"},
			identityLabels: []string{"app.kubernetes.io/name", "app.kubernetes.io/component"},
			want: map[string]string{
				"app.kubernetes.io/name":      "nginx",
				"app.kubernetes.io/component": plugin.LabelAbsent,
			},
		},
		{
			name:           "all labels absent",
			podLabels:      map[string]string{},
			identityLabels: []string{"app", "tier"},
			want:           map[string]string{"app": plugin.LabelAbsent, "tier": plugin.LabelAbsent},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workloadwatcher.ExtractSelectorLabels(tc.podLabels, tc.identityLabels)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, wantV := range tc.want {
				if got[k] != wantV {
					t.Errorf("key %q: got %q, want %q", k, got[k], wantV)
				}
			}
		})
	}
}

// -- profile reconciler unit tests --

func TestProfileReconciler_NotOrphaned(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	fc := newFakeClient(defaultBallastConfig(), profile)

	_, mr := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), mr, nil)

	result, err := reconcileProfile(t, c, profName)
	if err != nil {
		t.Fatalf("Profile.Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for non-orphaned profile, got RequeueAfter=%v", result.RequeueAfter)
	}

	// Profile must still exist.
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
}

func TestProfileReconciler_OrphanTTLNotExpired(t *testing.T) {
	orphanedAt := metav1.Now()
	profName := "web"
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec: ballastv1.BallastConfigSpec{
			IdentityLabels: []string{"app"},
			OrphanTTL:      "1h",
		},
	}
	fc := newFakeClient(cfg, profile)

	profile.Status.ActiveWorkloads = 0
	profile.Status.Conditions = []metav1.Condition{{
		Type:               "Orphaned",
		Status:             metav1.ConditionTrue,
		Reason:             "NoActiveWorkloads",
		LastTransitionTime: orphanedAt,
	}}
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, mr := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), mr, nil)

	result, err := reconcileProfile(t, c, profName)
	if err != nil {
		t.Fatalf("Profile.Reconcile: %v", err)
	}
	// TTL is 1h and the profile was just orphaned; expect requeue close to 1h.
	if result.RequeueAfter < 59*time.Minute {
		t.Errorf("RequeueAfter: got %v, want ≥ 59m (TTL not expired)", result.RequeueAfter)
	}

	// Profile must still exist.
	var got ballastv1.WorkloadProfile
	if err := fc.Get(context.Background(), types.NamespacedName{Name: profName}, &got); err != nil {
		t.Fatalf("Get profile after not-expired TTL: %v", err)
	}
}

func TestProfileReconciler_InvalidOrphanTTL(t *testing.T) {
	profName := "web"
	profile := &ballastv1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: profName}}
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ballast"},
		Spec: ballastv1.BallastConfigSpec{
			IdentityLabels: []string{"app"},
			OrphanTTL:      "not-a-duration", // invalid
		},
	}
	fc := newFakeClient(cfg, profile)

	profile.Status.ActiveWorkloads = 0
	profile.Status.Conditions = []metav1.Condition{{
		Type:               "Orphaned",
		Status:             metav1.ConditionTrue,
		Reason:             "NoActiveWorkloads",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-24 * time.Hour)),
	}}
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, mr := newMiniredisClient(t)
	c := workloadwatcher.New(fc, inactiveKS(t), mr, nil)

	_, err := reconcileProfile(t, c, profName)
	if err == nil {
		t.Fatal("expected error for invalid orphanTTL, got nil")
	}
}

func TestProfileReconciler_OrphanTTLExpired(t *testing.T) {
	ctx := context.Background()

	profName := "web"
	tupleLabels := map[string]string{"app": "web"}
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec: ballastv1.BallastConfigSpec{
			IdentityLabels: []string{"app"},
			OrphanTTL:      "1ms",
		},
	}
	fc := newFakeClient(cfg, profile)

	profile.Status.ActiveWorkloads = 0
	profile.Status.TupleLabels = tupleLabels
	profile.Status.Conditions = []metav1.Condition{{
		Type:   "Orphaned",
		Status: metav1.ConditionTrue,
		Reason: "NoActiveWorkloads",
		// Use a timestamp well in the past to ensure TTL is expired.
		LastTransitionTime: metav1.NewTime(time.Now().Add(-24 * time.Hour)),
	}}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	mr, rc := newMiniredisClient(t)

	// Seed some Redis keys for this profile's hash.
	tupleHash := store.TupleHash(tupleLabels)
	key1 := store.MetricKey(tupleHash, "app", "cpu")
	key2 := store.MetricKey(tupleHash, "app", "memory")
	if err := store.AddSample(ctx, rc, key1, 1000, "100m", 0); err != nil {
		t.Fatalf("AddSample key1: %v", err)
	}
	if err := store.AddSample(ctx, rc, key2, 1000, "256Mi", 0); err != nil {
		t.Fatalf("AddSample key2: %v", err)
	}

	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)
	_, err := reconcileProfile(t, c, profName)
	if err != nil {
		t.Fatalf("Profile.Reconcile: %v", err)
	}

	// Profile must be deleted.
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: profName}, &got); !apierrors.IsNotFound(err) {
		t.Errorf("expected profile to be deleted, got err=%v", err)
	}

	// Redis keys must be gone.
	remaining := mr.Keys()
	if len(remaining) != 0 {
		t.Errorf("expected all Redis keys deleted, remaining: %v", remaining)
	}
}

// -- predicate / helper unit tests --

func TestHasBallastAnnotationOrFinalizer(t *testing.T) {
	// No annotations, no finalizer → false.
	obj := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}
	if workloadwatcher.HasBallastAnnotationOrFinalizer(obj) {
		t.Error("expected false for pod with no annotations or finalizer")
	}

	// Has a behavior annotation → true (loop returns before reaching slices.Contains).
	obj.Annotations = map[string]string{workloadwatcher.AnnotationMeasure: "true"}
	if !workloadwatcher.HasBallastAnnotationOrFinalizer(obj) {
		t.Error("expected true for pod with behavior annotation")
	}

	// No behavior annotations but has our finalizer → true.
	// This is the path that keeps deletion reconciliation firing after annotations
	// have been stripped from a previously-processed pod.
	obj.Annotations = nil
	obj.Finalizers = []string{workloadwatcher.FinalizerName}
	if !workloadwatcher.HasBallastAnnotationOrFinalizer(obj) {
		t.Error("expected true for pod with finalizer but no behavior annotations")
	}
}

func TestProfileName_LongLabel(t *testing.T) {
	ctx := context.Background()

	// A value long enough to exceed the 253-char profile name limit on its own.
	longValue := strings.Repeat("a", 260)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-abc",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"app": longValue},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), pod)
	c := workloadwatcher.New(fc, inactiveKS(t), nil, nil)

	reconcilePod(t, c, "default", "web-abc")

	var list ballastv1.WorkloadProfileList
	if err := fc.List(ctx, &list); err != nil {
		t.Fatalf("List WorkloadProfiles: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 WorkloadProfile, got %d", len(list.Items))
	}
	if n := len(list.Items[0].Name); n > 253 {
		t.Errorf("profile name is %d chars, want ≤ 253", n)
	}
}

func TestProfileReconciler_RedisFailure(t *testing.T) {
	profName := "web"
	tupleLabels := map[string]string{"app": "web"}
	profile := &ballastv1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: profName}}
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ballast"},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app"}, OrphanTTL: "1ms"},
	}
	fc := newFakeClient(cfg, profile)

	profile.Status.ActiveWorkloads = 0
	profile.Status.TupleLabels = tupleLabels
	profile.Status.Conditions = []metav1.Condition{{
		Type:               "Orphaned",
		Status:             metav1.ConditionTrue,
		Reason:             "NoActiveWorkloads",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-24 * time.Hour)),
	}}
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	mr, rc := newMiniredisClient(t)
	mr.Close() // shut down the server so Redis commands fail

	c := workloadwatcher.New(fc, inactiveKS(t), rc, nil)
	_, err := reconcileProfile(t, c, profName)
	if err == nil {
		t.Fatal("expected error when Redis is unavailable, got nil")
	}
}

// -- envtest integration test --

func TestController_SetupWithManager(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 newScheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	mr := miniredis.RunT(t)
	if err := workloadwatcher.Setup(mgr, "default", "redis://"+mr.Addr()); err != nil {
		t.Fatalf("workloadwatcher Setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgrErr := make(chan error, 1)
	go func() { mgrErr <- mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	c := mgr.GetClient()

	// Create required namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	// Create BallastConfig with identityLabels=["app"].
	ballastCfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec: ballastv1.BallastConfigSpec{
			IdentityLabels: []string{"app"},
			OrphanTTL:      "168h",
		},
	}
	if err := c.Create(ctx, ballastCfg); err != nil {
		t.Fatalf("create BallastConfig: %v", err)
	}

	// Create a pod with the measure annotation — the controller should create a WorkloadProfile.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-pod",
			Namespace:   "default",
			Annotations: map[string]string{workloadwatcher.AnnotationMeasure: "true"},
			Labels:      map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Wait for WorkloadProfile to appear.
	waitForProfile(t, ctx, c, "web")

	// Verify activeWorkloads=1.
	var profile ballastv1.WorkloadProfile
	if err := c.Get(ctx, types.NamespacedName{Name: "web"}, &profile); err != nil {
		t.Fatalf("Get WorkloadProfile: %v", err)
	}
	if profile.Status.ActiveWorkloads != 1 {
		t.Errorf("activeWorkloads: got %d, want 1", profile.Status.ActiveWorkloads)
	}

	// Delete the pod and wait for the Orphaned condition to be set.
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	waitForOrphaned(t, ctx, c, "web")
}

func waitForProfile(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var p ballastv1.WorkloadProfile
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &p); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("timed out waiting for WorkloadProfile %q to appear", name)
}

func waitForOrphaned(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var p ballastv1.WorkloadProfile
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &p); err == nil {
			if cond := apimeta.FindStatusCondition(p.Status.Conditions, "Orphaned"); cond != nil && cond.Status == metav1.ConditionTrue {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("timed out waiting for WorkloadProfile %q to become orphaned", name)
}
