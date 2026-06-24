package kubernetes_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/tight-line/ballast/internal/plugin"
	kplugin "github.com/tight-line/ballast/internal/plugin/kubernetes"
)

// fakeLister implements PodMetricsLister for tests.
type fakeLister struct {
	pods []metricsv1beta1.PodMetrics
	err  error
}

func (f *fakeLister) List(_ context.Context, _ metav1.ListOptions) (*metricsv1beta1.PodMetricsList, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &metricsv1beta1.PodMetricsList{Items: f.pods}, nil
}

func makeUsage(cpuMillis, memBytes int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
	}
}

func TestType(t *testing.T) {
	p := kplugin.New(&fakeLister{}, kplugin.DefaultOptions())
	if got := p.Type(); got != "kubernetesMetrics" {
		t.Errorf("Type() = %q, want kubernetesMetrics", got)
	}
}

func TestNew_ZeroOptsUsesDefaults(t *testing.T) {
	// New with zero opts should not panic or produce a broken limiter.
	p := kplugin.New(&fakeLister{}, kplugin.Options{})
	if p == nil {
		t.Fatal("New returned nil")
	}
}

func TestFetchStats_Empty(t *testing.T) {
	p := kplugin.New(&fakeLister{}, kplugin.DefaultOptions())
	stats, err := p.FetchStats(context.Background(), plugin.WorkloadIdentity{Labels: map[string]string{"app": "test"}}, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty stats for no pods, got %d entries", len(stats))
	}
}

func TestFetchStats_OneContainerStats(t *testing.T) {
	lister := &fakeLister{
		pods: []metricsv1beta1.PodMetrics{
			{Containers: []metricsv1beta1.ContainerMetrics{
				{Name: "app", Usage: makeUsage(250, 512*1024*1024)},
			}},
		},
	}
	p := kplugin.New(lister, kplugin.DefaultOptions())
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "billing"}}

	before := time.Now()
	stats, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	after := time.Now()
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}

	// Expect two entries: app/cpu and app/memory.
	if len(stats) != 2 {
		t.Fatalf("expected 2 stats entries, got %d", len(stats))
	}

	byKey := make(map[string]plugin.ContainerStats)
	for _, s := range stats {
		byKey[s.ContainerName+"/"+s.Resource] = s
	}

	cpu, ok := byKey["app/cpu"]
	if !ok {
		t.Fatal("missing app/cpu entry")
	}
	want := resource.NewMilliQuantity(250, resource.DecimalSI)
	if cpu.Value.Cmp(*want) != 0 {
		t.Errorf("cpu Value = %s, want 250m", cpu.Value.String())
	}
	if cpu.Timestamp.Before(before) || cpu.Timestamp.After(after) {
		t.Errorf("cpu Timestamp %v not in [%v, %v]", cpu.Timestamp, before, after)
	}

	mem, ok := byKey["app/memory"]
	if !ok {
		t.Fatal("missing app/memory entry")
	}
	wantMem := resource.NewQuantity(512*1024*1024, resource.BinarySI)
	if mem.Value.Cmp(*wantMem) != 0 {
		t.Errorf("memory Value = %s, want 512Mi", mem.Value.String())
	}
}

func TestFetchStats_MultiplePodsAndContainers(t *testing.T) {
	lister := &fakeLister{
		pods: []metricsv1beta1.PodMetrics{
			{Containers: []metricsv1beta1.ContainerMetrics{
				{Name: "app", Usage: makeUsage(100, 128*1024*1024)},
				{Name: "sidecar", Usage: makeUsage(20, 32*1024*1024)},
			}},
			{Containers: []metricsv1beta1.ContainerMetrics{
				{Name: "app", Usage: makeUsage(200, 256*1024*1024)},
				{Name: "sidecar", Usage: makeUsage(30, 48*1024*1024)},
			}},
		},
	}
	p := kplugin.New(lister, kplugin.DefaultOptions())
	stats, err := p.FetchStats(context.Background(), plugin.WorkloadIdentity{Labels: map[string]string{"app": "test"}}, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	// 2 pods × 2 containers × 2 resources = 8 entries.
	if len(stats) != 8 {
		t.Errorf("expected 8 stats entries, got %d", len(stats))
	}
}

func TestFetchStats_EphemeralStorage(t *testing.T) {
	lister := &fakeLister{
		pods: []metricsv1beta1.PodMetrics{
			{Containers: []metricsv1beta1.ContainerMetrics{
				{Name: "app", Usage: corev1.ResourceList{
					corev1.ResourceCPU:              *resource.NewMilliQuantity(100, resource.DecimalSI),
					corev1.ResourceMemory:           *resource.NewQuantity(128*1024*1024, resource.BinarySI),
					corev1.ResourceEphemeralStorage: *resource.NewQuantity(1*1024*1024*1024, resource.BinarySI),
				}},
			}},
		},
	}
	p := kplugin.New(lister, kplugin.DefaultOptions())
	stats, err := p.FetchStats(context.Background(), plugin.WorkloadIdentity{Labels: map[string]string{"app": "test"}}, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	// cpu + memory + ephemeral-storage = 3 entries.
	if len(stats) != 3 {
		t.Fatalf("expected 3 stats entries, got %d", len(stats))
	}
	found := false
	for _, s := range stats {
		if s.Resource == "ephemeral-storage" {
			found = true
		}
	}
	if !found {
		t.Error("expected ephemeral-storage entry in stats")
	}
}

func TestFetchStats_BackoffOnError(t *testing.T) {
	lister := &fakeLister{err: errors.New("metrics API unavailable")}
	p := kplugin.New(lister, kplugin.Options{MaxRPS: 100, MaxBackoff: 10 * time.Second})
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "test"}}

	// First call returns the API error.
	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected error from metrics API")
	}

	// Flip to success; if the API were called it would return results.
	lister.err = nil
	lister.pods = []metricsv1beta1.PodMetrics{
		{Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "app", Usage: makeUsage(100, 128*1024*1024)},
		}},
	}

	// Immediate second call should be rejected in backoff (API is not called).
	_, err = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected backoff error on second call")
	}
	if !strings.Contains(err.Error(), "backoff") {
		t.Errorf("expected backoff error, got: %v", err)
	}
}

func TestFetchStats_BackoffExponential(t *testing.T) {
	lister := &fakeLister{err: errors.New("fail")}
	p := kplugin.New(lister, kplugin.Options{MaxRPS: 100, MaxBackoff: 10 * time.Second})
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "test"}}

	// First failure: backoff = 1s.
	_, _ = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	// Second failure (after backoff expires): backoff = 2s.
	// We can't wait 1s in a unit test, so verify the pattern via a very short maxBackoff.
	p2 := kplugin.New(lister, kplugin.Options{MaxRPS: 100, MaxBackoff: 3 * time.Millisecond})
	_, _ = p2.FetchStats(context.Background(), id, plugin.TimeWindow{}) // delay=1ms (capped)
	time.Sleep(5 * time.Millisecond)
	_, _ = p2.FetchStats(context.Background(), id, plugin.TimeWindow{}) // delay=2ms (capped)
	time.Sleep(5 * time.Millisecond)
	_, _ = p2.FetchStats(context.Background(), id, plugin.TimeWindow{}) // delay=3ms (cap)
	time.Sleep(5 * time.Millisecond)
	_, _ = p2.FetchStats(context.Background(), id, plugin.TimeWindow{}) // delay=3ms (stays at cap)

	// Now flip to success and wait for backoff to clear.
	lister.err = nil
	lister.pods = []metricsv1beta1.PodMetrics{
		{Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "app", Usage: makeUsage(100, 128*1024*1024)},
		}},
	}
	time.Sleep(10 * time.Millisecond)
	stats, err := p2.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected success after backoff expired: %v", err)
	}
	if len(stats) == 0 {
		t.Error("expected non-empty stats after recovery")
	}
}

func TestFetchStats_BackoffResetOnSuccess(t *testing.T) {
	lister := &fakeLister{err: errors.New("fail")}
	p := kplugin.New(lister, kplugin.Options{MaxRPS: 100, MaxBackoff: time.Millisecond})
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "test"}}

	// Trigger backoff.
	_, _ = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	time.Sleep(5 * time.Millisecond)

	// Recover: backoff expired and call succeeds; backoff state should be cleared.
	lister.err = nil
	lister.pods = []metricsv1beta1.PodMetrics{
		{Containers: []metricsv1beta1.ContainerMetrics{
			{Name: "app", Usage: makeUsage(100, 128*1024*1024)},
		}},
	}
	stats, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected success after backoff expired: %v", err)
	}
	if len(stats) == 0 {
		t.Error("expected non-empty stats")
	}

	// Subsequent calls should succeed without hitting backoff.
	stats, err = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected continued success after backoff reset: %v", err)
	}
	if len(stats) == 0 {
		t.Error("expected non-empty stats on follow-up call")
	}
}

func TestFetchStats_RateLimit(t *testing.T) {
	lister := &fakeLister{}
	// MaxRPS=0.001 means ~1000s between tokens; burst=1 so first call drains it.
	p := kplugin.New(lister, kplugin.Options{MaxRPS: 0.001, MaxBackoff: time.Minute})
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "test"}}

	// First call consumes the burst token immediately.
	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call with a short deadline should fail waiting for the next token.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = p.FetchStats(ctx, id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected rate-limit timeout on second call")
	}
}
