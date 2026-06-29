package resourceadjuster_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"github.com/tight-line/ballast/internal/controller/resourceadjuster"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/killswitch"
)

// -- helpers --

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

// resizeFakeClient wraps a fake client so that SubResource("resize").Patch()
// actually persists the modified object via a regular Update, making the new
// resource values visible to subsequent Get calls in tests.
type resizeFakeClient struct{ client.Client }

func newResizeFakeClient(objs ...client.Object) *resizeFakeClient {
	return &resizeFakeClient{Client: newFakeClient(objs...)}
}

func (c *resizeFakeClient) SubResource(sub string) client.SubResourceClient {
	base := c.Client.SubResource(sub)
	if sub != "resize" {
		return base
	}
	return &resizeSubClient{base: base, fc: c.Client}
}

type resizeSubClient struct {
	base client.SubResourceClient
	fc   client.Client
}

func (r *resizeSubClient) Get(ctx context.Context, obj, sub client.Object, opts ...client.SubResourceGetOption) error {
	return r.base.Get(ctx, obj, sub, opts...)
}

func (r *resizeSubClient) Create(ctx context.Context, obj, sub client.Object, opts ...client.SubResourceCreateOption) error {
	return r.base.Create(ctx, obj, sub, opts...)
}

func (r *resizeSubClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return r.base.Update(ctx, obj, opts...)
}

// Patch ignores the patch and persists the modified object directly so tests
// can inspect the resulting resource values via a subsequent Get.
func (r *resizeSubClient) Patch(ctx context.Context, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	return r.fc.Update(ctx, obj)
}

func (r *resizeSubClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return r.base.Apply(ctx, obj, opts...)
}

func inactiveKS(t *testing.T) *killswitch.KillSwitch {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	ks := killswitch.New(fc, "ballast-system", nil)
	if _, err := ks.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("ks.Reconcile: %v", err)
	}
	return ks
}

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

// noResizePolicy returns a ClusterResourcePolicy with no selector (matches everything),
// with a 20% threshold and 15m resize interval.
func noResizePolicy() *ballastv1.ClusterResourcePolicy {
	return &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Behaviors: ballastv1.BehaviorConfig{
				Thresholds: ballastv1.ThresholdConfig{
					Default: "20%",
					Resize: ballastv1.ResizeThresholds{
						Default: "20%",
					},
				},
				Resize: ballastv1.ResizeConfig{
					MaxChangePerCycle: "50%",
					Interval:          "15m",
				},
			},
		},
	}
}

// readyProfile returns a WorkloadProfile with MeetsThreshold=true and one container recommendation.
func readyProfile(cpuRequest, cpuLimit string) *ballastv1.WorkloadProfile {
	return &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Status: ballastv1.WorkloadProfileStatus{
			TupleLabels:    map[string]string{"app": "app", "env": "prod"},
			MeetsThreshold: true,
			Containers: []ballastv1.ContainerProfile{
				{
					Name: "app",
					Recommendations: map[string]ballastv1.ResourceRecommendation{
						"cpu": {Request: cpuRequest, Limit: cpuLimit},
					},
				},
			},
		},
	}
}

// resizePod returns a pod with the resize annotation, profile-ref, and given CPU resources.
func resizePod(cpuRequest, cpuLimit string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationResize:     "true",
				workloadwatcher.AnnotationProfileRef: "prod",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse(cpuRequest),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse(cpuLimit),
						},
					},
				},
			},
		},
	}
}

func doReconcile(t *testing.T, r *resourceadjuster.Reconciler, profileName string) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: profileName},
	})
}

// -- controller reconcile tests --

func TestReconcile_ProfileNotFound(t *testing.T) {
	fc := newFakeClient()
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	result, err := doReconcile(t, r, "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for missing profile, got %v", result.RequeueAfter)
	}
}

func TestReconcile_KillSwitchActive(t *testing.T) {
	profile := readyProfile("200m", "400m")
	// Pod has enough drift to trigger a resize if the kill switch were inactive.
	pod := resizePod("100m", "200m")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, activeKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	result, err := doReconcile(t, r, profile.Name)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called when kill switch is active")
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when kill switch active")
	}
}

func TestReconcile_ThresholdNotMet(t *testing.T) {
	profile := readyProfile("200m", "400m")
	profile.Status.MeetsThreshold = false
	fc := newFakeClient(profile, noResizePolicy())
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called when MeetsThreshold is false")
	}
}

func TestReconcile_NoMatchingPolicy(t *testing.T) {
	profile := readyProfile("200m", "400m")
	// No policy objects in fake client — resolver returns nil.
	fc := newFakeClient(profile)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called when no policy matches")
	}
}

func TestReconcile_NoDrift_NoResize(t *testing.T) {
	// Pod resources exactly match recommendations — no drift.
	profile := readyProfile("200m", "400m")
	pod := resizePod("200m", "400m")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called when there is no drift")
	}
}

func TestReconcile_DriftExceedsThreshold_ResizeCalled(t *testing.T) {
	// Pod has 100m CPU request; recommendation is 200m — 100% drift, exceeds 20% threshold.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, adjs []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		if len(adjs) != 1 || adjs[0].Name != "app" {
			t.Errorf("unexpected adjustments: %+v", adjs)
		}
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resizeCalled {
		t.Error("resize should have been called when drift exceeds threshold")
	}
}

func TestReconcile_AutoresizeAnnotation_ResizeCalled(t *testing.T) {
	// autoresize annotation should behave the same as resize once MeetsThreshold is true.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Annotations[workloadwatcher.AnnotationResize] = ""
	pod.Annotations[workloadwatcher.AnnotationAutoresize] = "true"
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resizeCalled {
		t.Error("resize should have been called for autoresize pod with sufficient drift")
	}
}

func TestReconcile_NoResizeAnnotation_NoResize(t *testing.T) {
	// Pod only has measure annotation — no resize should be issued.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	delete(pod.Annotations, workloadwatcher.AnnotationResize)
	pod.Annotations[workloadwatcher.AnnotationMeasure] = "true"
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called for measure-only pod")
	}
}

func TestReconcile_DryRun_NoResize(t *testing.T) {
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), true /* dryRunResize */, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called in dry-run mode")
	}
}

func TestReconcile_ResizeFails_BlockedAnnotationStamped(t *testing.T) {
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		return errors.New("node pressure: infeasible")
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if updated.Annotations[resourceadjuster.AnnotationResizeBlocked] != "true" {
		t.Errorf("expected resize-blocked annotation, got %q", updated.Annotations[resourceadjuster.AnnotationResizeBlocked])
	}
}

func TestReconcile_ResizeSucceeds_LastResizeAnnotationStamped(t *testing.T) {
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if updated.Annotations[resourceadjuster.AnnotationLastResize] == "" {
		t.Error("expected last-resize annotation to be set after successful resize")
	}
}

func TestReconcile_CooldownActive_ResizeSkipped(t *testing.T) {
	// Pod was resized 5 minutes ago — within the 15m interval — so resize should be skipped.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Annotations[resourceadjuster.AnnotationLastResize] = time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called when pod is within cooldown window")
	}
}

func TestReconcile_RequeueInterval_FromPolicy(t *testing.T) {
	policy := noResizePolicy()
	policy.Spec.Behaviors.Resize.Interval = "5m"
	profile := readyProfile("200m", "400m")
	// Pod resources match recommendations so no resize is triggered, but we still check requeue.
	pod := resizePod("200m", "400m")
	fc := newFakeClient(profile, policy, pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		return nil
	}
	result, err := doReconcile(t, r, profile.Name)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m requeue, got %v", result.RequeueAfter)
	}
}

// -- unit tests for pure functions --

func TestExceedsDrift(t *testing.T) {
	cases := []struct {
		name      string
		current   string
		recommend string
		threshold float64
		want      bool
	}{
		{"no drift", "200m", "200m", 20, false},
		{"drift below threshold", "200m", "220m", 20, false},
		{"drift exactly at threshold", "200m", "240m", 20, false},
		{"drift above threshold", "200m", "250m", 20, true},
		{"drift downward above threshold", "300m", "200m", 20, true},
		{"drift downward below threshold", "220m", "200m", 20, false},
		{"current zero, recommend positive", "0", "100m", 20, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := resource.MustParse(tc.current)
			rec := resource.MustParse(tc.recommend)
			got := resourceadjuster.ExceedsDrift(cur, rec, tc.threshold)
			if got != tc.want {
				t.Errorf("ExceedsDrift(%s, %s, %.0f%%) = %v, want %v",
					tc.current, tc.recommend, tc.threshold, got, tc.want)
			}
		})
	}
}

func TestCapChange(t *testing.T) {
	cases := []struct {
		name        string
		current     string
		recommended string
		maxPct      float64
		wantAtMost  string // upper bound on result (result must be <= recommended and <= wantAtMost)
		wantAtLeast string // result must be > current when drifting upward
	}{
		{
			name: "small move, no cap needed",
			// current=200m, recommended=210m, maxPct=50% → delta 10m <= 100m cap → use recommended
			current: "200m", recommended: "210m", maxPct: 50,
			wantAtMost: "210m", wantAtLeast: "200m",
		},
		{
			name: "large move, cap applied upward",
			// current=100m, recommended=300m, maxPct=50% → cap at 150m
			current: "100m", recommended: "300m", maxPct: 50,
			wantAtMost: "150m", wantAtLeast: "100m",
		},
		{
			name: "large move, cap applied downward",
			// current=300m, recommended=100m, maxPct=50% → cap at 150m
			current: "300m", recommended: "100m", maxPct: 50,
			wantAtMost: "300m", wantAtLeast: "149m",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := resource.MustParse(tc.current)
			rec := resource.MustParse(tc.recommended)
			result := resourceadjuster.CapChange(cur, rec, tc.maxPct)
			atMost := resource.MustParse(tc.wantAtMost)
			atLeast := resource.MustParse(tc.wantAtLeast)
			if result.Cmp(atMost) > 0 {
				t.Errorf("CapChange result %s exceeds cap %s", result.String(), tc.wantAtMost)
			}
			if result.Cmp(atLeast) < 0 {
				t.Errorf("CapChange result %s is less than minimum %s", result.String(), tc.wantAtLeast)
			}
		})
	}
}

func TestResolveFieldThreshold(t *testing.T) {
	behaviors := ballastv1.BehaviorConfig{
		Thresholds: ballastv1.ThresholdConfig{
			Default: "20%",
			Resize: ballastv1.ResizeThresholds{
				Default: "15%",
				ResourceThresholds: map[string]ballastv1.ResourceFieldThresholds{
					"cpu": {Request: "10%", Limit: "12%"},
				},
			},
		},
	}

	cases := []struct {
		res, field string
		want       float64
	}{
		{"cpu", "request", 10},    // resourceThresholds[cpu].request
		{"cpu", "limit", 12},      // resourceThresholds[cpu].limit
		{"memory", "request", 15}, // resize.default
		{"memory", "limit", 15},   // resize.default
	}
	for _, tc := range cases {
		got := resourceadjuster.ResolveFieldThreshold(behaviors, tc.res, tc.field)
		if got != tc.want {
			t.Errorf("ResolveFieldThreshold(%s, %s) = %v, want %v", tc.res, tc.field, got, tc.want)
		}
	}
}

func TestResolveFieldThreshold_FallsBackToGlobalDefault(t *testing.T) {
	behaviors := ballastv1.BehaviorConfig{
		Thresholds: ballastv1.ThresholdConfig{
			Default: "25%",
			// No resize-level overrides.
		},
	}
	got := resourceadjuster.ResolveFieldThreshold(behaviors, "memory", "request")
	if got != 25 {
		t.Errorf("expected 25, got %v", got)
	}
}

func TestReconcile_PodWrongProfileRef_Excluded(t *testing.T) {
	// Pod referencing a different profile should not be resized.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Annotations[workloadwatcher.AnnotationProfileRef] = "other--profile"
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called for pod with wrong profile-ref")
	}
}

func TestReconcile_DeletingPod_Excluded(t *testing.T) {
	// A pod with a non-nil DeletionTimestamp should be skipped.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	// fake client requires a finalizer to allow objects with DeletionTimestamp.
	pod.Finalizers = []string{"test/fake"}
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resizeCalled {
		t.Error("resize should not be called for a deleting pod")
	}
}

func TestReconcile_ContainerNotInRecommendations_Skipped(t *testing.T) {
	// Pod has a container "sidecar" with no recommendations — only "app" is recommended.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "sidecar",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		},
	})
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	adjustedContainers := []string{}
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, adjs []resourceadjuster.ContainerAdjustment) error {
		for _, a := range adjs {
			adjustedContainers = append(adjustedContainers, a.Name)
		}
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(adjustedContainers, "app") {
		t.Error("app container with recommendations should have been adjusted")
	}
	if slices.Contains(adjustedContainers, "sidecar") {
		t.Error("sidecar container with no recommendations should not be in adjustments")
	}
}

func TestReconcile_NilContainerResources_HandledGracefully(t *testing.T) {
	// Container with nil Requests and Limits — should still produce an adjustment.
	profile := readyProfile("200m", "400m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-nil",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationResize:     "true",
				workloadwatcher.AnnotationProfileRef: "prod",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{}, // nil Requests and Limits
			}},
		},
	}
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resizeCalled {
		t.Error("resize should be called when current resources are absent (treated as zero drift from zero)")
	}
}

func TestReconcile_ApplyResizeDefault_FakeClientSucceeds(t *testing.T) {
	// Exercises the real applyResize path end-to-end using resizeFakeClient so the
	// SubResource("resize").Patch() actually persists the new resource values.
	// current=100m req / 200m limit; recommended=200m req / 400m limit; maxChangePct=50%
	// → req capped at 150m (100m + 50%), limit capped at 300m (200m + 50%)
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	fc := newResizeFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if updated.Annotations[resourceadjuster.AnnotationLastResize] == "" {
		t.Error("expected last-resize annotation after successful resize")
	}
	gotReq := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if gotReq.Cmp(resource.MustParse("150m")) != 0 {
		t.Errorf("CPU request: want 150m, got %s", gotReq.String())
	}
	gotLim := updated.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	if gotLim.Cmp(resource.MustParse("300m")) != 0 {
		t.Errorf("CPU limit: want 300m, got %s", gotLim.String())
	}
}

func TestReconcile_PodNilAnnotations_BlockedAnnotationStamped(t *testing.T) {
	// Pod arrives with nil annotations (defensive path in stampPodAnnotation).
	profile := readyProfile("200m", "400m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-noanns",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationResize:     "true",
				workloadwatcher.AnnotationProfileRef: "prod",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				},
			}},
		},
	}
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	r.ResizePod = func(_ context.Context, p *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		// Simulate resize failure after clearing annotations to exercise nil-annotation guard.
		p.Annotations = nil
		return errors.New("resize failed")
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-noanns"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if updated.Annotations[resourceadjuster.AnnotationResizeBlocked] != "true" {
		t.Errorf("expected resize-blocked annotation to be stamped, got %q", updated.Annotations[resourceadjuster.AnnotationResizeBlocked])
	}
}

func TestReconcile_EmptyMaxChangePerCycle_UsesDefault(t *testing.T) {
	// Policy with empty MaxChangePerCycle — resolveMaxChangePercent falls back to 50%.
	policy := noResizePolicy()
	policy.Spec.Behaviors.Resize.MaxChangePerCycle = ""
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	fc := newFakeClient(profile, policy, pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	resizeCalled := false
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		resizeCalled = true
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resizeCalled {
		t.Error("resize should still be called with default maxChangePerCycle")
	}
}

func TestResolveFieldThreshold_AllLevelsEmpty_UsesHardcodedDefault(t *testing.T) {
	// All threshold fields empty — falls back to hardcoded 20%.
	behaviors := ballastv1.BehaviorConfig{}
	got := resourceadjuster.ResolveFieldThreshold(behaviors, "cpu", "request")
	if got != 20 {
		t.Errorf("expected 20 (hardcoded default), got %v", got)
	}
}

func TestReconcile_ApplyResize_MultiContainer_SkipsNonMatching(t *testing.T) {
	// Pod has two containers; only "app" has recommendations.
	// After resize: "app" resources are updated, "sidecar" resources are unchanged.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "sidecar",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		},
	})
	fc := newResizeFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if updated.Annotations[resourceadjuster.AnnotationLastResize] == "" {
		t.Error("expected last-resize annotation after successful resize")
	}
	// "app": current=100m, recommended=200m, maxChangePct=50% → capped at 150m
	appReq := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if appReq.Cmp(resource.MustParse("150m")) != 0 {
		t.Errorf("app CPU request: want 150m, got %s", appReq.String())
	}
	// "sidecar": has no recommendations, should be unchanged at 50m
	sidecarReq := updated.Spec.Containers[1].Resources.Requests[corev1.ResourceCPU]
	if sidecarReq.Cmp(resource.MustParse("50m")) != 0 {
		t.Errorf("sidecar CPU request should be unchanged at 50m, got %s", sidecarReq.String())
	}
}

func TestReconcile_ApplyResize_NilResources_InitializedCorrectly(t *testing.T) {
	// Pod container has nil Requests and Limits. applyResize must initialize them
	// before writing. When current is zero, CapChange returns recommended directly
	// (no cap), so the result should equal the full recommended value.
	profile := readyProfile("200m", "400m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-nil-res",
			Namespace: "default",
			Annotations: map[string]string{
				workloadwatcher.AnnotationResize:     "true",
				workloadwatcher.AnnotationProfileRef: "prod",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{}, // nil Requests and Limits
			}},
		},
	}
	fc := newResizeFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-nil-res"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if updated.Annotations[resourceadjuster.AnnotationLastResize] == "" {
		t.Error("expected last-resize annotation after successful resize with nil resources")
	}
	// current=0, recommended=200m → no cap, result is the full recommended value
	gotReq := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if gotReq.Cmp(resource.MustParse("200m")) != 0 {
		t.Errorf("CPU request: want 200m, got %s", gotReq.String())
	}
	// current=0, recommended=400m → no cap
	gotLim := updated.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	if gotLim.Cmp(resource.MustParse("400m")) != 0 {
		t.Errorf("CPU limit: want 400m, got %s", gotLim.String())
	}
}

func TestCapChange_Memory(t *testing.T) {
	// Memory uses BinarySI (bytes), not milli-units. Large move triggers the cap.
	current := resource.MustParse("100Mi")
	recommended := resource.MustParse("300Mi")
	result := resourceadjuster.CapChange(current, recommended, 50)
	// maxDelta = 100Mi * 50% = 50Mi; capped result = 150Mi
	expectedCap := resource.MustParse("150Mi")
	if result.Cmp(expectedCap) > 0 {
		t.Errorf("CapChange memory result %s exceeds cap %s", result.String(), expectedCap.String())
	}
	if result.Cmp(current) <= 0 {
		t.Errorf("CapChange memory result %s should be greater than current %s", result.String(), current.String())
	}
}

func TestCapChange_CurrentZero(t *testing.T) {
	// When current is zero, CapChange returns the recommended value directly.
	current := resource.MustParse("0")
	recommended := resource.MustParse("200m")
	result := resourceadjuster.CapChange(current, recommended, 50)
	if result.Cmp(recommended) != 0 {
		t.Errorf("CapChange with current=0: expected %s, got %s", recommended.String(), result.String())
	}
}

func TestParseResizeInterval(t *testing.T) {
	cases := []struct {
		interval string
		want     time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"1h", time.Hour},
		{"", 15 * time.Minute},        // default
		{"invalid", 15 * time.Minute}, // default on parse failure
	}
	for _, tc := range cases {
		cfg := ballastv1.ResizeConfig{Interval: tc.interval}
		got := resourceadjuster.ParseResizeInterval(cfg)
		if got != tc.want {
			t.Errorf("ParseResizeInterval(%q) = %v, want %v", tc.interval, got, tc.want)
		}
	}
}

// -- envtest integration test --

func TestReconciler_SetupWithManager(t *testing.T) {
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

	ks := killswitch.New(mgr.GetClient(), "default", nil)
	if err := ks.SetupWithManager(mgr); err != nil {
		t.Fatalf("ks.SetupWithManager: %v", err)
	}
	if err := resourceadjuster.Setup(mgr, ks, false, nil); err != nil {
		t.Fatalf("resourceadjuster.Setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}
}
