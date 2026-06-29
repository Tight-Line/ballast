package metricscollector_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/tight-line/ballast/internal/controller/metricscollector"
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

func inactiveKS(t *testing.T) *killswitch.KillSwitch {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	ks := killswitch.New(fc, "ballast-system")
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
	ks := killswitch.New(fc, "ballast-system")
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

// mockPlugin is a test-only MetricsPlugin that returns pre-configured samples.
type mockPlugin struct {
	typeName string
	samples  []plugin.ContainerStats
	err      error
}

func (m *mockPlugin) Type() string { return m.typeName }
func (m *mockPlugin) FetchStats(_ context.Context, _ plugin.WorkloadIdentity, _ plugin.TimeWindow) ([]plugin.ContainerStats, error) {
	return m.samples, m.err
}

// newReconcilerWithPlugin wires a Reconciler with a fake client, miniredis, and a mock plugin.
func newReconcilerWithPlugin(t *testing.T, fc client.Client, sc store.Client, ks *killswitch.KillSwitch, dryRun bool, p *mockPlugin) *metricscollector.Reconciler {
	t.Helper()
	r := metricscollector.New(fc, sc, ks, dryRun)
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

func defaultBallastConfig() *ballastv1.BallastConfig {
	return &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec: ballastv1.BallastConfigSpec{
			IdentityLabels:  []string{"app"},
			OrphanTTL:       "168h",
			RetentionWindow: "168h",
		},
	}
}

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
			TupleLabels:    tupleLabels,
			SelectorLabels: tupleLabels,
		},
	}
}

func cpuSample(container string, milliCores int64, ts time.Time) plugin.ContainerStats {
	return plugin.ContainerStats{
		ContainerName: container,
		Resource:      "cpu",
		Value:         *resource.NewMilliQuantity(milliCores, resource.DecimalSI),
		Timestamp:     ts,
	}
}

// -- unit tests --

func TestReconcile_ProfileNotFound(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig())
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

func TestReconcile_NoBallastConfig(t *testing.T) {
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(profile) // no BallastConfig
	_, sc := newMiniredisClient(t)
	r := newReconcilerWithPlugin(t, fc, sc, inactiveKS(t), false, nil)

	result, err := reconcileProfile(t, r, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when BallastConfig is absent")
	}
}

func TestReconcile_NoMatchingPolicy(t *testing.T) {
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(defaultBallastConfig(), profile)
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
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), defaultMetricsSource(), profile)
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
	tupleHash := store.TupleHash(map[string]string{"app": "web"})
	key := store.MetricKey(tupleHash, "app", "cpu")
	count, _ := store.SampleCount(ctx, sc, key)
	if count != 0 {
		t.Errorf("expected 0 Redis samples when kill switch active, got %d", count)
	}
}

func TestReconcile_DryRun(t *testing.T) {
	ctx := context.Background()
	profile := defaultProfile(map[string]string{"app": "web"})
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), defaultMetricsSource(), profile)
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
	tupleHash := store.TupleHash(map[string]string{"app": "web"})
	key := store.MetricKey(tupleHash, "app", "cpu")
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
	fc := newFakeClient(defaultBallastConfig(), policy, profile)
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
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), defaultMetricsSource(), profile)
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
	tupleHash := store.TupleHash(tupleLabels)
	key := store.MetricKey(tupleHash, "app", "cpu")
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
	fc := newFakeClient(defaultBallastConfig(), policy, defaultMetricsSource(), profile)
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
	if len(got.Status.Containers) > 0 && got.Status.Containers[0].Recommendations != nil {
		t.Error("expected no recommendations when readiness not met")
	}
}

func TestReconcile_ReadinessMet_RecommendationsPopulated(t *testing.T) {
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), defaultMetricsSource(), profile)
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
	// should still be present in the status (merged from existing profile).
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), defaultMetricsSource(), profile)

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
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), src, profile)
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
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), defaultMetricsSource(), profile)
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
	fc := newFakeClient(defaultBallastConfig(), badPolicy, defaultMetricsSource(), profile)
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
	fc := newFakeClient(cfg, defaultPolicy(), defaultMetricsSource(), profile)
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
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), src, profile)
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
	fc := newFakeClient(defaultBallastConfig(), memPolicy, defaultMetricsSource(), profile)
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

func TestReconcile_DuplicateContainerMerge(t *testing.T) {
	// Existing profile has app/cpu AND new FetchStats also returns app/cpu.
	// mergeContainerSets calls appendUnique twice for the same pair; second call returns early.
	ctx := context.Background()
	tupleLabels := map[string]string{"app": "web"}
	profile := defaultProfile(tupleLabels)
	fc := newFakeClient(defaultBallastConfig(), defaultPolicy(), defaultMetricsSource(), profile)
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

	ks := killswitch.New(mgr.GetClient(), "default")
	if err := ks.SetupWithManager(mgr); err != nil {
		t.Fatalf("ks.SetupWithManager: %v", err)
	}
	if err := metricscollector.Setup(mgr, ks, sc, false); err != nil {
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
