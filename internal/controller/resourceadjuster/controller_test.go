package resourceadjuster_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
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
	"github.com/tight-line/ballast/internal/validation"
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

// resizePod returns a pod enrolled at resize mode, carrying a profile-ref, with
// the given CPU resources.
func resizePod(cpuRequest, cpuLimit string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Labels:    map[string]string{validation.LabelMode: validation.ModeResize},
			Annotations: map[string]string{
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

// TestReconcile_RestartableInitSidecar_Resized asserts a restartable-init
// (native sidecar) container is resized in place, while a run-to-completion init
// container is not — even when both carry a drifting recommendation (#30).
func TestReconcile_RestartableInitSidecar_Resized(t *testing.T) {
	restartAlways := corev1.ContainerRestartPolicyAlways
	profile := readyProfile("200m", "400m")
	profile.Status.Containers = []ballastv1.ContainerProfile{
		{Name: "otc", Recommendations: map[string]ballastv1.ResourceRecommendation{"cpu": {Request: "200m", Limit: "400m"}}},
		{Name: "init-db", Recommendations: map[string]ballastv1.ResourceRecommendation{"cpu": {Request: "200m", Limit: "400m"}}},
	}
	pod := resizePod("100m", "200m") // regular "app" container is absent from the profile, so it never drifts
	drift := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
	}
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "init-db", Resources: drift},                                  // run-once init: excluded from resize
		{Name: "otc", RestartPolicy: &restartAlways, Resources: drift},       // native sidecar with a rec: resized
		{Name: "otc-norec", RestartPolicy: &restartAlways, Resources: drift}, // native sidecar, no profile rec: skipped
	}
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	var gotAdjs []resourceadjuster.ContainerAdjustment
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, adjs []resourceadjuster.ContainerAdjustment) error {
		gotAdjs = adjs
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotAdjs) != 1 || gotAdjs[0].Name != "otc" {
		t.Fatalf("adjustments = %+v, want exactly one for the restartable-init container 'otc'", gotAdjs)
	}
}

func TestReconcile_MeasureMode_NoResize(t *testing.T) {
	// A pod enrolled at measure mode must never be resized.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Labels[validation.LabelMode] = validation.ModeMeasure
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

func TestReconcile_ResizeFails_BlockedAnnotationsStamped(t *testing.T) {
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
	if got := updated.Annotations[resourceadjuster.AnnotationResizeBlocked]; got != "node pressure: infeasible" {
		t.Errorf("resize-blocked should carry the failure reason, got %q", got)
	}
	at := updated.Annotations[resourceadjuster.AnnotationResizeBlockedAt]
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("resize-blocked-at should be RFC3339, got %q: %v", at, err)
	}
}

func TestReconcile_ResizeFails_LongError_ReasonTruncated(t *testing.T) {
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, _ []resourceadjuster.ContainerAdjustment) error {
		return errors.New(strings.Repeat("x", 1000))
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if got := len(updated.Annotations[resourceadjuster.AnnotationResizeBlocked]); got != 256 {
		t.Errorf("resize-blocked reason length = %d, want truncated to 256", got)
	}
}

func TestReconcile_BlockedRecently_NoRetry(t *testing.T) {
	// The pod's last resize failed 5 minutes ago — within the 15m interval — so
	// no new attempt is made even though drift persists.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Annotations[resourceadjuster.AnnotationResizeBlocked] = "some earlier failure"
	pod.Annotations[resourceadjuster.AnnotationResizeBlockedAt] = time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
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
		t.Error("resize should not be retried while the blocked backoff is active")
	}
}

func TestReconcile_BlockedStale_RetriedAndClearedOnSuccess(t *testing.T) {
	// The pod's last resize failed longer than one interval ago: it is retried,
	// and a successful resize removes both blocked annotations.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Annotations[resourceadjuster.AnnotationResizeBlocked] = "some earlier failure"
	pod.Annotations[resourceadjuster.AnnotationResizeBlockedAt] = time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
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
		t.Fatal("resize should be retried once the blocked backoff has elapsed")
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if v, ok := updated.Annotations[resourceadjuster.AnnotationResizeBlocked]; ok {
		t.Errorf("resize-blocked should be cleared after a successful resize, got %q", v)
	}
	if v, ok := updated.Annotations[resourceadjuster.AnnotationResizeBlockedAt]; ok {
		t.Errorf("resize-blocked-at should be cleared after a successful resize, got %q", v)
	}
	if updated.Annotations[resourceadjuster.AnnotationLastResize] == "" {
		t.Error("expected last-resize annotation after successful resize")
	}
}

func TestReconcile_BlockedAtUnparseable_Retried(t *testing.T) {
	// A mangled resize-blocked-at value must not wedge the pod: the backoff is
	// ignored and the resize proceeds.
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Annotations[resourceadjuster.AnnotationResizeBlockedAt] = "not-a-timestamp"
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
		t.Error("resize should proceed when resize-blocked-at cannot be parsed")
	}
}

func TestReconcile_LegacyBlockedTrue_StillRetried(t *testing.T) {
	// Pods stamped resize-blocked: "true" by versions before the backoff have no
	// resize-blocked-at, so they are evaluated normally (and the annotation is
	// rewritten by the next failure or cleared by the next success).
	profile := readyProfile("200m", "400m")
	pod := resizePod("100m", "200m")
	pod.Annotations[resourceadjuster.AnnotationResizeBlocked] = "true"
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
		t.Error("legacy resize-blocked=true without resize-blocked-at should not suppress evaluation")
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

// readyProfileWithRecs returns a ready WorkloadProfile with the given
// recommendations for the "app" container.
func readyProfileWithRecs(recs map[string]ballastv1.ResourceRecommendation) *ballastv1.WorkloadProfile {
	return &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Status: ballastv1.WorkloadProfileStatus{
			TupleLabels:    map[string]string{"app": "app", "env": "prod"},
			MeetsThreshold: true,
			Containers: []ballastv1.ContainerProfile{
				{Name: "app", Recommendations: recs},
			},
		},
	}
}

func TestReconcile_NonResizableDriftOnly_NoResizeNoBlock(t *testing.T) {
	// Only ephemeral-storage drifts (5Mi -> 66Ki). The resize subresource cannot
	// mutate it, so no resize is attempted and the pod is not marked blocked.
	profile := readyProfileWithRecs(map[string]ballastv1.ResourceRecommendation{
		"ephemeral-storage": {Request: "66Ki"},
	})
	pod := resizePod("100m", "200m")
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("5Mi")
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
		t.Error("resize should not be called when only non-resizable resources drift")
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-abc"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if updated.Annotations[resourceadjuster.AnnotationResizeBlocked] != "" {
		t.Errorf("pod should not be marked resize-blocked, got %q", updated.Annotations[resourceadjuster.AnnotationResizeBlocked])
	}
	if updated.Annotations[resourceadjuster.AnnotationLastResize] != "" {
		t.Errorf("last-resize should not be stamped when nothing was resized, got %q", updated.Annotations[resourceadjuster.AnnotationLastResize])
	}
}

func TestReconcile_MixedDrift_NonResizableExcludedFromAdjustments(t *testing.T) {
	// cpu and ephemeral-storage both drift; the resize proceeds with the cpu
	// change only and leaves ephemeral-storage at its current value.
	profile := readyProfileWithRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu":               {Request: "200m", Limit: "400m"},
		"ephemeral-storage": {Request: "66Ki"},
	})
	pod := resizePod("100m", "200m")
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("5Mi")
	fc := newFakeClient(profile, noResizePolicy(), pod)
	r := resourceadjuster.New(fc, inactiveKS(t), false, nil)
	var captured []resourceadjuster.ContainerAdjustment
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, adjs []resourceadjuster.ContainerAdjustment) error {
		captured = adjs
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 adjustment, got %d", len(captured))
	}
	// 100m -> 200m is capped at 50% per cycle: 150m.
	if got := captured[0].Requests[corev1.ResourceCPU]; got.String() != "150m" {
		t.Errorf("cpu request = %s, want 150m", got.String())
	}
	// ephemeral-storage keeps its current value: drift on it must not be applied.
	if got := captured[0].Requests[corev1.ResourceEphemeralStorage]; got.String() != "5Mi" {
		t.Errorf("ephemeral-storage request = %s, want unchanged 5Mi", got.String())
	}
}

func TestReconcile_NonResizableLimitDrift_Skipped(t *testing.T) {
	// Drift on the ephemeral-storage limit only (200Mi -> 300Mi, 50% > 20%
	// threshold); the request recommendation is empty. Still skipped: limits are
	// no more mutable than requests for non-resizable resources.
	profile := readyProfileWithRecs(map[string]ballastv1.ResourceRecommendation{
		"ephemeral-storage": {Limit: "300Mi"},
	})
	pod := resizePod("100m", "200m")
	pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("200Mi")
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
		t.Error("resize should not be called for non-resizable limit drift")
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
		threshold   float64
		wantAtMost  string // upper bound on result (result must be <= recommended and <= wantAtMost)
		wantAtLeast string // result must be > current when drifting upward
	}{
		{
			name: "small move lands inside drift band, snaps to recommended",
			// current=200m, recommended=210m: capped step 205m is within 20% of
			// 210m, so the recommendation is applied exactly
			current: "200m", recommended: "210m", maxPct: 50, threshold: 20,
			wantAtMost: "210m", wantAtLeast: "210m",
		},
		{
			name: "large move, cap applied upward",
			// current=100m, recommended=300m: gap 200m, step 100m → ~200m,
			// still >20% from 300m so no snap
			current: "100m", recommended: "300m", maxPct: 50, threshold: 20,
			wantAtMost: "200m", wantAtLeast: "199m",
		},
		{
			name: "large move, cap applied downward",
			// current=300m, recommended=100m: gap 200m, step 100m → ~200m,
			// still >20% from 100m so no snap
			current: "300m", recommended: "100m", maxPct: 50, threshold: 20,
			wantAtMost: "200m", wantAtLeast: "199m",
		},
		{
			name: "capped step within threshold of recommendation, snaps",
			// current=100m, recommended=140m: gap 40m, step 20m → 120m, which is
			// within 20% of 140m, so the recommendation is applied exactly
			current: "100m", recommended: "140m", maxPct: 50, threshold: 20,
			wantAtMost: "140m", wantAtLeast: "140m",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := resource.MustParse(tc.current)
			rec := resource.MustParse(tc.recommended)
			result := resourceadjuster.CapChange(cur, rec, tc.maxPct, tc.threshold)
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

func TestReconcile_BestEffortPod_QOSPinned_NoResizeNoBlock(t *testing.T) {
	// A pod with no requests or limits anywhere is BestEffort. Adding requests
	// via the resize subresource would change its QoS class to Burstable, which
	// Kubernetes rejects — so no resize is attempted and the pod is not marked
	// blocked (the patch was never issued, let alone failed).
	profile := readyProfile("200m", "400m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-nil",
			Namespace: "default",
			Labels:    map[string]string{validation.LabelMode: validation.ModeResize},
			Annotations: map[string]string{
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
	if resizeCalled {
		t.Error("resize should not be attempted on a BestEffort pod: it cannot succeed")
	}
	var updated corev1.Pod
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-nil"}, &updated); err != nil {
		t.Fatalf("getting pod: %v", err)
	}
	if v, ok := updated.Annotations[resourceadjuster.AnnotationResizeBlocked]; ok {
		t.Errorf("qos_pinned skip should not mark the pod blocked, got %q", v)
	}
}

func TestReconcile_GuaranteedPod_RequestOnlyDrift_QOSPinned(t *testing.T) {
	// A Guaranteed pod (requests == limits for cpu and memory) whose
	// recommendation moves only the cpu request would become Burstable, so the
	// resize is skipped as qos_pinned.
	profile := readyProfileWithRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "200m"},
	})
	pod := guaranteedPod()
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
		t.Error("resize breaking requests==limits on a Guaranteed pod should be skipped")
	}
}

func TestReconcile_GuaranteedPod_CoupledMove_Resized(t *testing.T) {
	// A Guaranteed pod whose recommendation moves the cpu request and limit
	// together (to the same value) stays Guaranteed, so the resize proceeds.
	profile := readyProfileWithRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "200m", Limit: "200m"},
	})
	pod := guaranteedPod()
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
		t.Error("a QoS-preserving resize of a Guaranteed pod should proceed")
	}
}

// guaranteedPod returns a resize-annotated pod with requests == limits for both
// cpu and memory (QoS class Guaranteed).
func guaranteedPod() *corev1.Pod {
	pod := resizePod("100m", "100m")
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
	pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
	return pod
}

func TestReconcile_NilContainerLimits_HandledGracefully(t *testing.T) {
	// Container with requests set but a nil Limits map — adjustments must still
	// be produced and the limit written into a freshly initialized map. The pod
	// is Burstable before and after (a cpu limit alone is not Guaranteed), so
	// the QoS pre-check does not interfere.
	profile := readyProfile("200m", "400m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-nil",
			Namespace: "default",
			Labels:    map[string]string{validation.LabelMode: validation.ModeResize},
			Annotations: map[string]string{
				workloadwatcher.AnnotationProfileRef: "prod",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					// nil Limits
				},
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
		t.Error("resize should be called when the limits map is nil but requests drift")
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
			Labels:    map[string]string{validation.LabelMode: validation.ModeResize},
			Annotations: map[string]string{
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
	if got := updated.Annotations[resourceadjuster.AnnotationResizeBlocked]; got != "resize failed" {
		t.Errorf("expected resize-blocked annotation to carry the failure reason, got %q", got)
	}
}

func TestReconcile_EmptyMaxChangePerCycle_UsesDefault(t *testing.T) {
	// Policy with empty MaxChangePerCycle; the resolver fills it with the
	// canonical default ("50%") before the adjuster sees it.
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
	// All threshold fields empty — falls back to the canonical default.
	behaviors := ballastv1.BehaviorConfig{}
	got := resourceadjuster.ResolveFieldThreshold(behaviors, "cpu", "request")
	if got != ballastv1.DefaultThresholdPercent {
		t.Errorf("expected %v (canonical default), got %v", ballastv1.DefaultThresholdPercent, got)
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

func TestReconcile_ApplyResize_NilLimits_InitializedCorrectly(t *testing.T) {
	// Pod container has requests but a nil Limits map. applyResize must
	// initialize the map before writing. When the current limit is zero,
	// CapChange returns the recommendation directly (no cap).
	profile := readyProfile("200m", "400m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-nil-res",
			Namespace: "default",
			Labels:    map[string]string{validation.LabelMode: validation.ModeResize},
			Annotations: map[string]string{
				workloadwatcher.AnnotationProfileRef: "prod",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					// nil Limits
				},
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
		t.Error("expected last-resize annotation after successful resize with nil limits")
	}
	// request: current=100m, recommended=200m, 50% of the gap → 150m
	gotReq := updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if gotReq.Cmp(resource.MustParse("150m")) != 0 {
		t.Errorf("CPU request: want 150m, got %s", gotReq.String())
	}
	// limit: current=0, recommended=400m → no cap, full recommendation
	gotLim := updated.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	if gotLim.Cmp(resource.MustParse("400m")) != 0 {
		t.Errorf("CPU limit: want 400m, got %s", gotLim.String())
	}
}

func TestPodQOS(t *testing.T) {
	req := func(cpu, mem string) corev1.ResourceList {
		rl := corev1.ResourceList{}
		if cpu != "" {
			rl[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem != "" {
			rl[corev1.ResourceMemory] = resource.MustParse(mem)
		}
		return rl
	}
	cases := []struct {
		name       string
		containers []corev1.Container
		want       corev1.PodQOSClass
	}{
		{
			name:       "no resources anywhere",
			containers: []corev1.Container{{Name: "a"}},
			want:       corev1.PodQOSBestEffort,
		},
		{
			name: "non-qos resources and zero quantities are ignored",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					corev1.ResourceCPU:              resource.MustParse("0"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			}}},
			want: corev1.PodQOSBestEffort,
		},
		{
			name: "request only",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", ""),
			}}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "requests equal limits for cpu and memory",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", "128Mi"),
				Limits:   req("100m", "128Mi"),
			}}},
			want: corev1.PodQOSGuaranteed,
		},
		{
			name: "cpu limit only is not guaranteed",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", ""),
				Limits:   req("100m", ""),
			}}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "request below limit is not guaranteed",
			containers: []corev1.Container{{Name: "a", Resources: corev1.ResourceRequirements{
				Requests: req("100m", "128Mi"),
				Limits:   req("200m", "128Mi"),
			}}},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "init container resources count",
			containers: []corev1.Container{
				{Name: "a"},
				{Name: "init", Resources: corev1.ResourceRequirements{Requests: req("10m", "")}},
			},
			want: corev1.PodQOSBurstable,
		},
		{
			name: "two guaranteed containers aggregate",
			containers: []corev1.Container{
				{Name: "a", Resources: corev1.ResourceRequirements{
					Requests: req("100m", "128Mi"),
					Limits:   req("100m", "128Mi"),
				}},
				{Name: "b", Resources: corev1.ResourceRequirements{
					Requests: req("50m", "64Mi"),
					Limits:   req("50m", "64Mi"),
				}},
			},
			want: corev1.PodQOSGuaranteed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceadjuster.PodQOS(tc.containers); got != tc.want {
				t.Errorf("PodQOS = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCapChange_Memory(t *testing.T) {
	// Memory uses BinarySI (bytes), not milli-units. Large move triggers the cap.
	current := resource.MustParse("100Mi")
	recommended := resource.MustParse("300Mi")
	result := resourceadjuster.CapChange(current, recommended, 50, 20)
	// gap = 200Mi, step = 200Mi * 50% = 100Mi; capped result = 200Mi,
	// still >20% from 300Mi so no snap
	expectedCap := resource.MustParse("200Mi")
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
	result := resourceadjuster.CapChange(current, recommended, 50, 20)
	if result.Cmp(recommended) != 0 {
		t.Errorf("CapChange with current=0: expected %s, got %s", recommended.String(), result.String())
	}
}

func TestCapChange_ConvergesExactly(t *testing.T) {
	// Regression test for the Zeno tail: capping each step at 50% of the
	// remaining gap must still terminate exactly at the recommendation, not
	// park just inside the drift band. Simulates the resize loop (resize only
	// while drift exceeds the threshold) for a badly underprovisioned workload.
	cur := resource.MustParse("15m")
	rec := resource.MustParse("145m")
	steps := 0
	for resourceadjuster.ExceedsDrift(cur, rec, 20) {
		cur = resourceadjuster.CapChange(cur, rec, 50, 20)
		steps++
		if steps > 10 {
			t.Fatalf("did not converge after %d steps; stuck at %s", steps, cur.String())
		}
	}
	if cur.Cmp(rec) != 0 {
		t.Errorf("converged to %s inside the drift band; want exact recommendation %s", cur.String(), rec.String())
	}
	if steps > 4 {
		t.Errorf("took %d steps to converge; want at most 4", steps)
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

// Direct unit coverage of the in-function default: the resolver normally fills
// MaxChangePerCycle before the adjuster sees it, so the Reconcile path can no
// longer reach this branch.
func TestResolveMaxChangePercent(t *testing.T) {
	if got := resourceadjuster.ResolveMaxChangePercent(ballastv1.BehaviorConfig{}); got != ballastv1.DefaultMaxChangePercent {
		t.Errorf("empty behaviors: got %v, want %v", got, ballastv1.DefaultMaxChangePercent)
	}
	behaviors := ballastv1.BehaviorConfig{
		Resize: ballastv1.ResizeConfig{MaxChangePerCycle: "25%"},
	}
	if got := resourceadjuster.ResolveMaxChangePercent(behaviors); got != 25.0 {
		t.Errorf("explicit 25%%: got %v, want 25", got)
	}
}
