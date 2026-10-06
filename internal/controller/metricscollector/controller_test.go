package metricscollector_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/metricscollector"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/plugin"
	"github.com/tight-line/ballast/internal/store"
	"github.com/tight-line/ballast/internal/validation"
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

func newMiniredisClient(t *testing.T) (*miniredis.Miniredis, store.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return mr, rc
}

// mockPlugin is a test-only MetricsPlugin that returns pre-configured samples and
// records the identity of the most recent FetchStats call.
type mockPlugin struct {
	typeName string
	samples  []plugin.ContainerStats
	err      error
	lastID   plugin.WorkloadIdentity
}

func (m *mockPlugin) Type() string { return m.typeName }
func (m *mockPlugin) FetchStats(_ context.Context, id plugin.WorkloadIdentity, _ plugin.TimeWindow) ([]plugin.ContainerStats, error) {
	m.lastID = id
	return m.samples, m.err
}

// newReconcilerWithPlugin wires a Reconciler with a fake client, miniredis, and a mock plugin.
func newReconcilerWithPlugin(t *testing.T, fc client.Client, sc store.Client, ks *killswitch.KillSwitch, dryRun bool, p *mockPlugin) *metricscollector.Reconciler {
	t.Helper()
	r := metricscollector.New(fc, sc, ks, dryRun, nil)
	r.PluginGet = func(typeName string) (plugin.MetricsPlugin, bool) {
		if p != nil && typeName == p.typeName {
			return p, true
		}
		return nil, false
	}
	return r
}

func reconcileProfile(t *testing.T, r *metricscollector.Reconciler, name string) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
}

// -- fixture helpers --

func defaultMetricsSource() *ballastv1.MetricsSource {
	return &ballastv1.MetricsSource{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-metrics"},
		Spec: ballastv1.MetricsSourceSpec{
			Type: "kubernetesMetrics",
			Config: ballastv1.MetricsSourceConfig{
				PollInterval:  "60s",
				ReservoirSize: 10000,
			},
		},
	}
}

func defaultPolicy() *ballastv1.ClusterResourcePolicy {
	return &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-defaults"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Metrics: []ballastv1.MetricConfig{
				{Resource: "cpu", Field: "request", Source: "k8s-metrics", Aggregation: "p95", Headroom: "1.2"},
				{Resource: "cpu", Field: "limit", Source: "k8s-metrics", Aggregation: "p99", Headroom: "1.25"},
			},
			Readiness: ballastv1.ReadinessConfig{
				MinDataPoints: 2,
				MinTimeSpan:   "1ms",
				MaxCV:         "99.0",
			},
		},
	}
}

func defaultProfile(tupleLabels map[string]string) *ballastv1.WorkloadProfile {
	return &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status: ballastv1.WorkloadProfileStatus{
			TupleLabels:     tupleLabels,
			SelectorLabels:  tupleLabels,
			PolicyRef:       defaultPolicyRef(),
			MeasurementHash: profileHash(tupleLabels),
		},
	}
}

// defaultPolicyRef references the policy defaultPolicy builds. The collector reads
// its governing policy from status.policyRef rather than resolving one, so every
// profile fixture must record it the way the workloadwatcher would.
func defaultPolicyRef() *ballastv1.PolicyReference {
	return &ballastv1.PolicyReference{
		Kind: ballastv1.KindClusterResourcePolicy,
		Name: "platform-defaults",
	}
}

// profileHash is the Redis key namespace a profile owns, mirroring what the
// workloadwatcher records in status.measurementHash.
func profileHash(tupleLabels map[string]string) string {
	return store.MeasurementHash(tupleLabels, defaultPolicyRef().Key())
}

func cpuSample(container string, milliCores int64, ts time.Time) plugin.ContainerStats {
	return plugin.ContainerStats{
		ContainerName: container,
		Resource:      "cpu",
		Value:         *resource.NewMilliQuantity(milliCores, resource.DecimalSI),
		Timestamp:     ts,
	}
}

// appPod is a pod matched by the {"app": "web"} profile
// fixtures whose only container is "app". The collector measures only
// containers that a matching pod's spec shows, so a test that expects "app"
// samples to be written needs this pod in the fake client. It carries the
// profile-ref annotation for the "web" profile, as the workloadwatcher stamps it.
func appPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-app",
			Namespace:   "default",
			Labels:      map[string]string{"app": "web"},
			Annotations: map[string]string{workloadwatcher.AnnotationProfileRef: "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
}

// siblingPod matches the {"app": "web"} selector but is bound to another
// profile: same identity tuple, different governing policy.
func siblingPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-other",
			Namespace:   "default",
			Labels:      map[string]string{"app": "web"},
			Annotations: map[string]string{workloadwatcher.AnnotationProfileRef: "web-other-policy"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "other"}}},
	}
}

// -- unit tests --

func TestReconcile_ProfileNotFound(t *testing.T) {
	fc := newFakeClient()
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for not-found profile, got %v", result.RequeueAfter)
	}
}

func TestReconcile_NilSelectorLabels(t *testing.T) {
	// SelectorLabels is written by workloadwatcher in a separate update; if the metrics
	// controller fires first, it must requeue rather than collect from every pod.
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status: ballastv1.WorkloadProfileStatus{
			TupleLabels: map[string]string{"app": "web"},
			// SelectorLabels intentionally nil
		},
	}
	fc := newFakeClient(profile)
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when SelectorLabels is nil")
	}
}

func TestReconcile_NoMatchingPolicy(t *testing.T) {
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(profile)
	// No ClusterResourcePolicy that matches
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when no policy matches")
	}
}

func TestReconcile_KillSwitchActive(t *testing.T) {
	ctx := context.Background()
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile)
	_, sc := newMiniredisClient(t)

	// Seed the profile status via fake client.
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, activeKS(t), false, p)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when kill switch active")
	}

	// No samples should have been written to Redis.
	measurementHash := profileHash(map[string]string{"app": "web"})
	key := store.MetricKey(measurementHash, "app", "cpu")
	count, _ := store.SampleCount(ctx, sc, key)
	if count != 0 {
		t.Errorf("expected 0 Redis samples when kill switch active, got %d", count)
	}
}

func TestReconcile_DryRun(t *testing.T) {
	ctx := context.Background()
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), true, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No samples written.
	measurementHash := profileHash(map[string]string{"app": "web"})
	key := store.MetricKey(measurementHash, "app", "cpu")
	count, _ := store.SampleCount(ctx, sc, key)
	if count != 0 {
		t.Errorf("expected 0 Redis samples in dry-run, got %d", count)
	}

	// Status not updated.
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) != 0 {
		t.Errorf("expected no containers in status during dry-run, got %d", len(got.Status.Containers))
	}
}

func TestReconcile_MetricsSourceNotFound(t *testing.T) {
	// Policy references "missing-source" which doesn't exist.
	policy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-defaults"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Metrics: []ballastv1.MetricConfig{
				{Resource: "cpu", Field: "request", Source: "missing-source", Aggregation: "p95", Headroom: "1.0"},
			},
		},
	}
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(policy, profile)
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should still requeue.
	if result.RequeueAfter == 0 {
		t.Error("expected requeue even when source is missing")
	}
}

func TestReconcile_CollectAndUpdate(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	now := time.Now()
	p := &mockPlugin{
		typeName: "kubernetesMetrics",
		samples: []plugin.ContainerStats{
			cpuSample("app", 100, now.Add(-2*time.Second)),
			cpuSample("app", 200, now.Add(-time.Second)),
			cpuSample("app", 300, now),
		},
	}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Samples should be written to Redis.
	measurementHash := profileHash(tupleLabels)
	key := store.MetricKey(measurementHash, "app", "cpu")
	count, err := store.SampleCount(ctx, sc, key)
	if err != nil {
		t.Fatalf("SampleCount: %v", err)
	}
	if count != 3 {
		t.Errorf("Redis sample count: got %d, want 3", count)
	}

	// WorkloadProfile status should be updated.
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) == 0 {
		t.Fatal("expected containers in status after collection")
	}
	appContainer := got.Status.Containers[0]
	if appContainer.Name != "app" {
		t.Errorf("container name: got %q, want %q", appContainer.Name, "app")
	}
	if len(appContainer.UsageStats) == 0 {
		t.Fatal("expected usage stats for app container")
	}
	if appContainer.UsageStats[0].Samples != 3 {
		t.Errorf("samples: got %d, want 3", appContainer.UsageStats[0].Samples)
	}
}

func TestReconcile_MeasuresOnlyEnrolledPods(t *testing.T) {
	// Collection must require the enrollment (mode) label in addition to the
	// identity tuple, so unenrolled pods that share the tuple are not measured.
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile)
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics"}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := p.lastID.Labels["app"]; got != "web" {
		t.Errorf("FetchStats identity label app = %q, want web", got)
	}
	if got := p.lastID.Labels[validation.LabelMode]; got != plugin.LabelPresent {
		t.Errorf("FetchStats identity must require the mode label: %s = %q, want %q",
			validation.LabelMode, got, plugin.LabelPresent)
	}
}

func TestReconcile_ExcludesInitAndEphemeralContainers(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	restartAlways := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-abc",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
			// "init-db" is a run-to-completion init container (excluded).
			// "sidecar" is a restartable-init native sidecar (restartPolicy:
			// Always): it runs the whole pod lifetime and IS measured (#30).
			InitContainers: []corev1.Container{
				{Name: "init-db"},
				{Name: "sidecar", RestartPolicy: &restartAlways},
			},
			EphemeralContainers: []corev1.EphemeralContainer{
				{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger"}},
			},
		},
	}
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, pod)
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	now := time.Now()
	// The plugin reports samples for every container the metrics API sees. The
	// collector must drop the run-once init and ephemeral containers but keep the
	// app container and the restartable-init sidecar.
	p := &mockPlugin{
		typeName: "kubernetesMetrics",
		samples: []plugin.ContainerStats{
			cpuSample("app", 100, now.Add(-2*time.Second)),
			cpuSample("app", 200, now.Add(-time.Second)),
			cpuSample("app", 300, now),
			cpuSample("sidecar", 110, now.Add(-2*time.Second)),
			cpuSample("sidecar", 210, now.Add(-time.Second)),
			cpuSample("sidecar", 310, now),
			cpuSample("init-db", 500, now),
			cpuSample("debugger", 500, now),
		},
	}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	measurementHash := profileHash(tupleLabels)
	for _, name := range []string{"app", "sidecar"} {
		key := store.MetricKey(measurementHash, name, "cpu")
		if count, err := store.SampleCount(ctx, sc, key); err != nil {
			t.Fatalf("SampleCount(%s): %v", name, err)
		} else if count != 3 {
			t.Errorf("%s sample count: got %d, want 3 (should be measured)", name, count)
		}
	}

	for _, name := range []string{"init-db", "debugger"} {
		key := store.MetricKey(measurementHash, name, "cpu")
		count, err := store.SampleCount(ctx, sc, key)
		if err != nil {
			t.Fatalf("SampleCount(%s): %v", name, err)
		}
		if count != 0 {
			t.Errorf("%s sample count: got %d, want 0 (should be excluded from measurement)", name, count)
		}
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	gotNames := map[string]bool{}
	for _, cp := range got.Status.Containers {
		gotNames[cp.Name] = true
	}
	if len(got.Status.Containers) != 2 || !gotNames["app"] || !gotNames["sidecar"] {
		t.Fatalf("status containers: got %d %v, want [app sidecar]", len(got.Status.Containers), gotNames)
	}
}

func TestReconcile_ExclusionScopedBySelector(t *testing.T) {
	ctx := context.Background()
	// The selector requires "role" to be absent. A pod carrying role=batch is
	// returned by the server-side app=web filter but rejected client-side, so its
	// containers must not make anything measurable for this profile.
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status: ballastv1.WorkloadProfileStatus{
			TupleLabels:     map[string]string{"app": "web"},
			SelectorLabels:  map[string]string{"app": "web", "role": plugin.LabelAbsent},
			PolicyRef:       defaultPolicyRef(),
			MeasurementHash: profileHash(map[string]string{"app": "web"}),
		},
	}
	matchingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "web"}},
			InitContainers: []corev1.Container{{Name: "web-init"}},
		},
	}
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default", Labels: map[string]string{"app": "web", "role": "batch"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "batch"}}},
	}
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, matchingPod, otherPod)
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	now := time.Now()
	p := &mockPlugin{
		typeName: "kubernetesMetrics",
		samples: []plugin.ContainerStats{
			cpuSample("web", 100, now),
			cpuSample("web-init", 100, now),
			cpuSample("batch", 100, now),
		},
	}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	measurementHash := profileHash(profile.Status.TupleLabels)
	for name, want := range map[string]int64{
		"web":      1, // regular container of a matching pod
		"web-init": 0, // run-to-completion init container of a matching pod
		"batch":    0, // regular container, but only on a pod the selector rejects
	} {
		if count, err := store.SampleCount(ctx, sc, store.MetricKey(measurementHash, name, "cpu")); err != nil {
			t.Fatalf("SampleCount(%s): %v", name, err)
		} else if count != want {
			t.Errorf("%s: got %d samples, want %d", name, count, want)
		}
	}
}

// TestReconcile_UnknownContainerNotMeasured covers the pod-cache race from #59:
// the metrics source can report a freshly injected container before the pod
// cache shows it. Inclusion is an allowlist of names the matching pod specs
// show, so such a container is dropped rather than admitted by default.
func TestReconcile_UnknownContainerNotMeasured(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	now := time.Now()
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 100, now),
		cpuSample("injected", 100, now),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	measurementHash := profileHash(tupleLabels)
	if count, _ := store.SampleCount(ctx, sc, store.MetricKey(measurementHash, "injected", "cpu")); count != 0 {
		t.Errorf("injected: got %d samples, want 0 (not in any matching pod spec)", count)
	}
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) != 1 || got.Status.Containers[0].Name != "app" {
		t.Errorf("status containers = %+v, want only app", got.Status.Containers)
	}
}

// TestReconcile_PodListError_SkipsCycle pins the fail-closed behavior from #59:
// when the pod List fails the collector cannot tell measurable containers from
// excluded ones, so it writes no samples, leaves status alone, and requeues on
// the normal poll interval instead of measuring everything the source reports.
func TestReconcile_PodListError_SkipsCycle(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	inner := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod())
	if err := inner.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	fc := interceptor.NewClient(inner.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return errors.New("pod list boom")
			}
			return c.List(ctx, list, opts...)
		},
	})

	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 100, time.Now()),
		cpuSample("init-db", 100, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("RequeueAfter = %v, want the 60s poll interval", result.RequeueAfter)
	}

	measurementHash := profileHash(tupleLabels)
	for _, name := range []string{"app", "init-db"} {
		if count, _ := store.SampleCount(ctx, sc, store.MetricKey(measurementHash, name, "cpu")); count != 0 {
			t.Errorf("%s: got %d samples, want 0 when the pod list fails", name, count)
		}
	}
	var got ballastv1.WorkloadProfile
	if err := inner.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) != 0 || len(got.Status.Conditions) != 0 {
		t.Errorf("status was written on a skipped cycle: containers=%+v conditions=%+v",
			got.Status.Containers, got.Status.Conditions)
	}
}

// TestReconcile_ExcludedStatusContainerEvicted reproduces the pinned profile from
// #59: a run-to-completion init container that an earlier fail-open cycle let
// into status with a single sample kept failing readiness forever, holding the
// profile in Accruing. The collector must evict it from status, purge its
// stored series, and let the profile reach Sufficient on the real container.
func TestReconcile_ExcludedStatusContainerEvicted(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	pod := appPod()
	pod.Spec.InitContainers = []corev1.Container{{Name: "init-db"}}
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, pod)

	_, sc := newMiniredisClient(t)
	now := time.Now()
	measurementHash := profileHash(tupleLabels)
	strayKey := store.MetricKey(measurementHash, "init-db", "cpu")
	if err := store.AddSample(ctx, sc, strayKey, now.Add(-time.Hour).UnixMilli(), "500", 0); err != nil {
		t.Fatalf("seeding stray sample: %v", err)
	}
	profile.Status.State = ballastv1.WorkloadProfileStateAccruing
	profile.Status.Containers = []ballastv1.ContainerProfile{
		{Name: "app", UsageStats: []ballastv1.ContainerUsageStats{{Resource: "cpu", Source: "k8s-metrics", Samples: 0}}},
		{Name: "init-db", UsageStats: []ballastv1.ContainerUsageStats{{Resource: "cpu", Source: "k8s-metrics", Samples: 1}}},
	}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, now.Add(-10*time.Millisecond)),
		cpuSample("app", 400, now),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) != 1 || got.Status.Containers[0].Name != "app" {
		t.Fatalf("status containers = %+v, want only app after eviction", got.Status.Containers)
	}
	if got.Status.State != ballastv1.WorkloadProfileStateSufficient || !got.Status.MeetsThreshold {
		t.Errorf("state = %q meetsThreshold = %v, want Sufficient/true once the stray container is evicted",
			got.Status.State, got.Status.MeetsThreshold)
	}
	if count, _ := store.SampleCount(ctx, sc, strayKey); count != 0 {
		t.Errorf("stray series still holds %d samples, want it purged", count)
	}
	if ms, _ := store.FirstSeenMs(ctx, sc, strayKey); ms != 0 {
		t.Errorf("stray first_seen still %d, want it purged", ms)
	}
}

// TestReconcile_SiblingPodsDoNotEvict covers profiles that share an identity
// tuple under different policies. This profile is scaled to zero while a
// sibling profile's pods, with different container names, still match the
// selector. Only the profile's own pods (by profile-ref) say which containers
// it runs, so none of its status entries or stored series may be touched.
func TestReconcile_SiblingPodsDoNotEvict(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, siblingPod())

	_, sc := newMiniredisClient(t)
	key := store.MetricKey(profileHash(tupleLabels), "app", "cpu")
	if err := store.AddSample(ctx, sc, key, time.Now().UnixMilli(), "200", 0); err != nil {
		t.Fatalf("seeding sample: %v", err)
	}
	profile.Status.Containers = []ballastv1.ContainerProfile{
		{Name: "app", UsageStats: []ballastv1.ContainerUsageStats{{Resource: "cpu", Source: "k8s-metrics", Samples: 1}}},
	}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, &mockPlugin{typeName: "kubernetesMetrics"})
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) != 1 || got.Status.Containers[0].Name != "app" {
		t.Errorf("status containers = %+v, want app kept while only sibling pods exist", got.Status.Containers)
	}
	if count, _ := store.SampleCount(ctx, sc, key); count != 1 {
		t.Errorf("app series holds %d samples, want 1 (not purged)", count)
	}
}

// TestReconcile_ObservedContainerNotEvicted: the measurement selector folds
// same-tuple pods together, so a sibling profile's container can be measured
// into this profile. While it is being observed it must not be evicted, or
// every cycle would purge the samples it had just written.
func TestReconcile_ObservedContainerNotEvicted(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod(), siblingPod())
	profile.Status.Containers = []ballastv1.ContainerProfile{
		{Name: "other", UsageStats: []ballastv1.ContainerUsageStats{{Resource: "cpu", Source: "k8s-metrics", Samples: 0}}},
	}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("other", 100, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if count, _ := store.SampleCount(ctx, sc, store.MetricKey(profileHash(tupleLabels), "other", "cpu")); count != 1 {
		t.Errorf("other series holds %d samples, want the 1 just written", count)
	}
}

// TestReconcile_NothingObservedDoesNotEvict: a container that is in status but
// not (yet) on any pod stamped with this profile's profile-ref, during a cycle
// in which the metrics source returned nothing, must be kept. That combination
// is a rollout racing the workloadwatcher plus a failed fetch, not evidence
// that the container is gone.
func TestReconcile_NothingObservedDoesNotEvict(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod())

	_, sc := newMiniredisClient(t)
	key := store.MetricKey(profileHash(tupleLabels), "sidecar", "cpu")
	if err := store.AddSample(ctx, sc, key, time.Now().UnixMilli(), "50", 0); err != nil {
		t.Fatalf("seeding sample: %v", err)
	}
	profile.Status.Containers = []ballastv1.ContainerProfile{
		{Name: "sidecar", UsageStats: []ballastv1.ContainerUsageStats{{Resource: "cpu", Source: "k8s-metrics", Samples: 1}}},
	}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	p := &mockPlugin{typeName: "kubernetesMetrics", err: errors.New("metrics API unavailable")}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) != 1 || got.Status.Containers[0].Name != "sidecar" {
		t.Errorf("status containers = %+v, want sidecar kept when nothing was observed", got.Status.Containers)
	}
	if count, _ := store.SampleCount(ctx, sc, key); count != 1 {
		t.Errorf("sidecar series holds %d samples, want 1 (not purged)", count)
	}
}

func TestReconcile_ReadinessNotMet(t *testing.T) {
	ctx := context.Background()
	// Policy requires 100 data points — we'll only send 1.
	policy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-defaults"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Metrics: []ballastv1.MetricConfig{
				{Resource: "cpu", Field: "request", Source: "k8s-metrics", Aggregation: "p95", Headroom: "1.2"},
			},
			Readiness: ballastv1.ReadinessConfig{
				MinDataPoints: 100,
				MinTimeSpan:   "1ms",
				MaxCV:         "99.0",
			},
		},
	}
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(policy, defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if got.Status.MeetsThreshold {
		t.Error("expected meetsThreshold=false when minDataPoints not met")
	}
	if got.Status.State != ballastv1.WorkloadProfileStateAccruing {
		t.Errorf("state = %q, want Accruing while threshold not met", got.Status.State)
	}
	// Ready is collection health, not history sufficiency: the cpu sample was
	// collected fine, so the profile is Ready even while still accruing.
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition = %+v, want status True while accruing with healthy collection", cond)
	}
	if len(got.Status.Containers) > 0 && got.Status.Containers[0].Recommendations != nil {
		t.Error("expected no recommendations when readiness not met")
	}
}

// TestReconcile_NoSamples_ReadyFalse pins the health semantics of the Ready
// condition: when a policy resource produces no samples in the collection
// cycle, the profile is not Ready, independent of meetsThreshold.
func TestReconcile_NoSamples_ReadyFalse(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile)
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	// The plugin returns no samples at all: the policy tracks cpu, so cpu is missing.
	p := &mockPlugin{typeName: "kubernetesMetrics"}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition = %+v, want status False when no samples were collected", cond)
	}
	if cond.Reason != "MissingSamples" || !strings.Contains(cond.Message, "cpu") {
		t.Errorf("Ready condition reason/message = %q/%q, want MissingSamples naming cpu", cond.Reason, cond.Message)
	}
	if got.Status.State != ballastv1.WorkloadProfileStateAccruing {
		t.Errorf("state = %q, want Accruing with no history", got.Status.State)
	}
}

func TestReconcile_ReadinessMet_RecommendationsPopulated(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	now := time.Now()
	// Two samples with a tiny time spread — policy requires minDataPoints=2, minTimeSpan=1ms.
	p := &mockPlugin{
		typeName: "kubernetesMetrics",
		samples: []plugin.ContainerStats{
			cpuSample("app", 200, now.Add(-10*time.Millisecond)),
			cpuSample("app", 400, now),
		},
	}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}

	if !got.Status.MeetsThreshold {
		t.Error("expected meetsThreshold=true")
	}
	if got.Status.State != ballastv1.WorkloadProfileStateSufficient {
		t.Errorf("state = %q, want Sufficient when threshold met", got.Status.State)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition = %+v, want status True when threshold met", cond)
	}
	if len(got.Status.Containers) == 0 {
		t.Fatal("expected containers in status")
	}
	cpuRec, ok := got.Status.Containers[0].Recommendations["cpu"]
	if !ok {
		t.Fatal("expected cpu recommendation")
	}
	if cpuRec.Request == "" {
		t.Error("expected cpu request recommendation to be populated")
	}
	if cpuRec.Limit == "" {
		t.Error("expected cpu limit recommendation to be populated")
	}
}

func TestReconcile_ExistingContainersPreserved(t *testing.T) {
	// If FetchStats returns no samples, existing container stats from a prior cycle
	// should still be present in the status (merged from existing profile). No pod
	// matches here either (a workload scaled to zero), and that alone must not
	// evict the container: an empty pod list says nothing about what it runs.
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile)

	// Pre-populate profile with a prior cycle's container stats.
	profile.Status.Containers = []ballastv1.ContainerProfile{{
		Name: "app",
		UsageStats: []ballastv1.ContainerUsageStats{
			{Resource: "cpu", Source: "k8s-metrics", Samples: 5},
		},
	}}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}

	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: nil} // no new samples
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	// The app container should still appear (merged from existing status).
	found := false
	for _, cp := range got.Status.Containers {
		if cp.Name == "app" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'app' container to be preserved from previous cycle")
	}
}

func TestReconcile_PluginNotFound(t *testing.T) {
	// MetricsSource type has no registered plugin — collectAllSamples skips it.
	src := &ballastv1.MetricsSource{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-metrics"},
		Spec: ballastv1.MetricsSourceSpec{
			Type:   "unknownType",
			Config: ballastv1.MetricsSourceConfig{PollInterval: "60s"},
		},
	}
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(defaultPolicy(), src, profile)
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics"} // type mismatch with source
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue even when plugin not found")
	}
}

func TestReconcile_FetchStatsError(t *testing.T) {
	// Plugin returns an error from FetchStats — collectFromSource logs and continues.
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile)
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", err: errors.New("metrics unavailable")}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue even when FetchStats fails")
	}
}

func TestReconcile_BadAggregation(t *testing.T) {
	// Policy uses an unknown aggregation — ComputeRecommendation fails, field left empty.
	ctx := context.Background()
	badPolicy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-defaults"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Metrics: []ballastv1.MetricConfig{
				{Resource: "cpu", Field: "request", Source: "k8s-metrics", Aggregation: "badagg", Headroom: "1.0"},
			},
			Readiness: ballastv1.ReadinessConfig{MinDataPoints: 2, MinTimeSpan: "1ms", MaxCV: "99.0"},
		},
	}
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(badPolicy, defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	now := time.Now()
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, now.Add(-10*time.Millisecond)),
		cpuSample("app", 300, now),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.Containers) > 0 {
		if recs := got.Status.Containers[0].Recommendations; recs != nil {
			if cpuRec, ok := recs["cpu"]; ok && cpuRec.Request != "" {
				t.Error("expected empty recommendation when aggregation is invalid")
			}
		}
	}
}

func TestReconcile_InvalidRetentionWindow(t *testing.T) {
	// BallastConfig with an unparseable RetentionWindow falls back to 168h default.
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec:       ballastv1.BallastConfigSpec{RetentionWindow: "not-a-duration"},
	}
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(cfg, defaultPolicy(), defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue with invalid retention window")
	}
}

func TestReconcile_ShortPollInterval(t *testing.T) {
	// MetricsSource PollInterval shorter than defaultPollInterval — minPollInterval returns it.
	src := &ballastv1.MetricsSource{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-metrics"},
		Spec: ballastv1.MetricsSourceSpec{
			Type:   "kubernetesMetrics",
			Config: ballastv1.MetricsSourceConfig{PollInterval: "30s", ReservoirSize: 10000},
		},
	}
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(defaultPolicy(), src, profile, appPod())
	if err := fc.Status().Update(context.Background(), profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s requeue interval, got %v", result.RequeueAfter)
	}
}

func TestReconcile_MemoryMetric(t *testing.T) {
	// Non-CPU metric exercises quantityToStoreValue and formatResourceValue memory paths.
	ctx := context.Background()
	memPolicy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-defaults"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Metrics: []ballastv1.MetricConfig{
				{Resource: "memory", Field: "request", Source: "k8s-metrics", Aggregation: "p95", Headroom: "1.1"},
			},
			Readiness: ballastv1.ReadinessConfig{MinDataPoints: 2, MinTimeSpan: "1ms", MaxCV: "99.0"},
		},
	}
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(memPolicy, defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	now := time.Now()
	p := &mockPlugin{
		typeName: "kubernetesMetrics",
		samples: []plugin.ContainerStats{
			{ContainerName: "app", Resource: "memory", Value: *resource.NewQuantity(128*1024*1024, resource.BinarySI), Timestamp: now.Add(-10 * time.Millisecond)},
			{ContainerName: "app", Resource: "memory", Value: *resource.NewQuantity(256*1024*1024, resource.BinarySI), Timestamp: now},
		},
	}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Status.MeetsThreshold {
		t.Error("expected meetsThreshold=true")
	}
	if got.Status.State != ballastv1.WorkloadProfileStateSufficient {
		t.Errorf("state = %q, want Sufficient when threshold met", got.Status.State)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition = %+v, want status True when threshold met", cond)
	}
	if len(got.Status.Containers) == 0 {
		t.Fatal("expected containers in status")
	}
	memRec, ok := got.Status.Containers[0].Recommendations["memory"]
	if !ok {
		t.Fatal("expected memory recommendation")
	}
	if memRec.Request == "" {
		t.Error("expected memory request to be populated")
	}
}

func TestReconcile_MemoryMetric_GiScale(t *testing.T) {
	// Exercises the >=1Gi branch in formatResourceValue.
	ctx := context.Background()
	memPolicy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-defaults"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Metrics:   []ballastv1.MetricConfig{{Resource: "memory", Field: "request", Source: "k8s-metrics", Aggregation: "p95", Headroom: "1.0"}},
			Readiness: ballastv1.ReadinessConfig{MinDataPoints: 2, MinTimeSpan: "1ms", MaxCV: "99.0"},
		},
	}
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(memPolicy, defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	now := time.Now()
	p := &mockPlugin{
		typeName: "kubernetesMetrics",
		samples: []plugin.ContainerStats{
			{ContainerName: "app", Resource: "memory", Value: *resource.NewQuantity(2*1024*1024*1024, resource.BinarySI), Timestamp: now.Add(-10 * time.Millisecond)},
			{ContainerName: "app", Resource: "memory", Value: *resource.NewQuantity(3*1024*1024*1024, resource.BinarySI), Timestamp: now},
		},
	}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.Containers) == 0 || len(got.Status.Containers[0].UsageStats) == 0 {
		t.Fatal("expected container usage stats")
	}
	p50 := got.Status.Containers[0].UsageStats[0].P50
	if len(p50) == 0 || p50[len(p50)-2:] != "Gi" {
		t.Errorf("expected Gi suffix in P50 %q", p50)
	}
}

func TestReconcile_MemoryMetric_KiScale(t *testing.T) {
	// Exercises the <1Mi (Ki) branch in formatResourceValue.
	ctx := context.Background()
	memPolicy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-defaults"},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Metrics:   []ballastv1.MetricConfig{{Resource: "memory", Field: "request", Source: "k8s-metrics", Aggregation: "p95", Headroom: "1.0"}},
			Readiness: ballastv1.ReadinessConfig{MinDataPoints: 2, MinTimeSpan: "1ms", MaxCV: "99.0"},
		},
	}
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(memPolicy, defaultMetricsSource(), profile, appPod())
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	now := time.Now()
	p := &mockPlugin{
		typeName: "kubernetesMetrics",
		samples: []plugin.ContainerStats{
			{ContainerName: "app", Resource: "memory", Value: *resource.NewQuantity(512*1024, resource.BinarySI), Timestamp: now.Add(-10 * time.Millisecond)},
			{ContainerName: "app", Resource: "memory", Value: *resource.NewQuantity(768*1024, resource.BinarySI), Timestamp: now},
		},
	}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)
	if _, err := reconcileProfile(t, r, "web"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.Containers) == 0 || len(got.Status.Containers[0].UsageStats) == 0 {
		t.Fatal("expected container usage stats")
	}
	p50 := got.Status.Containers[0].UsageStats[0].P50
	if len(p50) == 0 || p50[len(p50)-2:] != "Ki" {
		t.Errorf("expected Ki suffix in P50 %q", p50)
	}
}

func TestReconcile_DuplicateContainerMerge(t *testing.T) {
	// Existing profile has app/cpu AND new FetchStats also returns app/cpu.
	// mergeContainerSets calls appendUnique twice for the same pair; second call returns early.
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultPolicy(), defaultMetricsSource(), profile, appPod())
	profile.Status.Containers = []ballastv1.ContainerProfile{{
		Name:       "app",
		UsageStats: []ballastv1.ContainerUsageStats{{Resource: "cpu", Source: "k8s-metrics", Samples: 5}},
	}}
	if err := fc.Status().Update(ctx, profile); err != nil {
		t.Fatalf("status update: %v", err)
	}
	_, sc := newMiniredisClient(t)
	p := &mockPlugin{typeName: "kubernetesMetrics", samples: []plugin.ContainerStats{
		cpuSample("app", 200, time.Now()),
	}}
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, p)

	_, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got ballastv1.WorkloadProfile
	if err := fc.Get(ctx, types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	found := false
	for _, cp := range got.Status.Containers {
		if cp.Name == "app" {
			found = true
		}
	}
	if !found {
		t.Error("expected app container in status after duplicate merge")
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

	mr := miniredis.RunT(t)
	sc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = sc.Close() })

	ks := killswitch.New(mgr.GetClient(), "default", nil)
	if err := ks.SetupWithManager(mgr); err != nil {
		t.Fatalf("ks.SetupWithManager: %v", err)
	}
	if err := metricscollector.Setup(mgr, ks, sc, false, nil); err != nil {
		t.Fatalf("metricscollector.Setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgrErr := make(chan error, 1)
	go func() { mgrErr <- mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	c := mgr.GetClient()

	// Create a WorkloadProfile — the controller should reconcile it without error.
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
	}
	if err := c.Create(ctx, profile); err != nil {
		t.Fatalf("create WorkloadProfile: %v", err)
	}

	// Verify the profile is reachable — the controller reconciles but returns early
	// (no BallastConfig) without error.
	waitForProfileExists(t, ctx, c, "web")
}

func waitForProfileExists(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var p ballastv1.WorkloadProfile
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &p); err == nil {
			return
		} else if !apierrors.IsNotFound(err) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Errorf("timed out waiting for WorkloadProfile %q", name)
}

// A profile whose pods match no policy has nothing to measure with: the policy is
// what names the metrics sources, so the collector skips rather than guessing.
func TestReconcile_NoPolicyRef_Skipped(t *testing.T) {
	profile := defaultProfile(map[string]string{"app": "web"})
	profile.Status.PolicyRef = nil
	fc := newFakeClient(profile, defaultMetricsSource(), defaultPolicy())
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected a requeue so the profile is revisited once a policy matches")
	}

	var got ballastv1.WorkloadProfile
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "web"}, &got); err != nil {
		t.Fatalf("Get profile: %v", err)
	}
	if len(got.Status.Containers) != 0 {
		t.Error("no containers should be recorded without a policy")
	}
}

// The referenced policy can be deleted between the workloadwatcher recording it and
// this reconcile. Skipping is correct: the policy watch is already migrating those
// pods to a profile under whatever policy now governs them.
func TestReconcile_PolicyRefDangling_Skipped(t *testing.T) {
	profile := defaultProfile(map[string]string{"app": "web"})
	profile.Status.PolicyRef = &ballastv1.PolicyReference{
		Kind: ballastv1.KindClusterResourcePolicy,
		Name: "deleted-policy",
	}
	fc := newFakeClient(profile, defaultMetricsSource())
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected a requeue while the profile awaits migration")
	}
}

// measurementHash names the Redis key namespace the profile owns. Writing samples
// before it is set would file them under a key belonging to no profile, where
// nothing would ever read or purge them.
func TestReconcile_NoMeasurementHash_Requeues(t *testing.T) {
	profile := defaultProfile(map[string]string{"app": "web"})
	profile.Status.MeasurementHash = ""
	fc := newFakeClient(profile, defaultMetricsSource(), defaultPolicy())
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected a requeue until the workloadwatcher back-fills the hash")
	}
}

// A policyRef the resolver cannot interpret is surfaced as an error rather than
// treated as "no policy": silently skipping would leave the profile accruing
// nothing with no signal as to why.
func TestReconcile_PolicyRefUnknownKind_Errors(t *testing.T) {
	profile := defaultProfile(map[string]string{"app": "web"})
	profile.Status.PolicyRef = &ballastv1.PolicyReference{Kind: "Nonsense", Name: "x"}
	fc := newFakeClient(profile, defaultMetricsSource())
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	if _, err := reconcileProfile(t, r, "web"); err == nil {
		t.Fatal("expected an error for an uninterpretable policyRef")
	}
}
