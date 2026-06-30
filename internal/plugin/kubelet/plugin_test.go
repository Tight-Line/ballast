package kubelet_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/tight-line/ballast/internal/plugin"
	kplugin "github.com/tight-line/ballast/internal/plugin/kubelet"
)

// ---- test doubles ----

type fakeNodeLister struct {
	nodes []corev1.Node
	err   error
}

func (f *fakeNodeLister) List(_ context.Context, _ metav1.ListOptions) (*corev1.NodeList, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &corev1.NodeList{Items: f.nodes}, nil
}

type fakePodLister struct {
	pods []corev1.Pod
	err  error
}

func (f *fakePodLister) List(_ context.Context, _ metav1.ListOptions) (*corev1.PodList, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &corev1.PodList{Items: f.pods}, nil
}

type fakeSummaryFetcher struct {
	// summaries maps nodeName -> summary to return (nil = not found)
	summaries map[string]*kplugin.NodeSummary
	err       error
}

func (f *fakeSummaryFetcher) Fetch(_ context.Context, nodeName string) (*kplugin.NodeSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.summaries[nodeName]
	if !ok {
		return &kplugin.NodeSummary{}, nil
	}
	return s, nil
}

// ---- helpers ----

func node(name string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func pod(namespace, name string, labels map[string]string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels}}
}

func usedBytes(n uint64) *uint64 { return &n }

func newPlugin(nodes kplugin.NodeLister, pods kplugin.PodLister, fetcher kplugin.SummaryFetcher) *kplugin.Plugin {
	return kplugin.NewWithDeps(nodes, pods, fetcher,
		kplugin.Options{MaxBackoff: 10 * time.Second}, // CacheTTL=0 disables caching
		logr.Discard())
}

// ---- tests ----

func TestType(t *testing.T) {
	p := newPlugin(&fakeNodeLister{}, &fakePodLister{}, &fakeSummaryFetcher{})
	if got := p.Type(); got != "kubeletSummary" {
		t.Errorf("Type() = %q, want kubeletSummary", got)
	}
}

func TestFetchStats_NoNodes(t *testing.T) {
	p := newPlugin(&fakeNodeLister{}, &fakePodLister{}, &fakeSummaryFetcher{})
	stats, err := p.FetchStats(context.Background(), plugin.WorkloadIdentity{Labels: map[string]string{"app": "x"}}, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty stats, got %d", len(stats))
	}
}

func TestFetchStats_NoMatchingPods(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "nginx-abc", map[string]string{"app": "nginx"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "nginx-abc"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(1024)},
				Containers:       []kplugin.ContainerRef{{Name: "nginx"}}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(),
		plugin.WorkloadIdentity{Labels: map[string]string{"app": "redis"}},
		plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats for non-matching selector, got %d", len(stats))
	}
}

func TestFetchStats_SingleContainerPod(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(512 * 1024 * 1024)},
				Containers:       []kplugin.ContainerRef{{Name: "app"}}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(),
		plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}},
		plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat entry, got %d", len(stats))
	}
	s := stats[0]
	if s.ContainerName != "app" {
		t.Errorf("ContainerName = %q, want app", s.ContainerName)
	}
	if s.Resource != "ephemeral-storage" {
		t.Errorf("Resource = %q, want ephemeral-storage", s.Resource)
	}
	wantBytes := int64(512 * 1024 * 1024)
	if s.Value.Value() != wantBytes {
		t.Errorf("Value = %d, want %d", s.Value.Value(), wantBytes)
	}
}

func TestFetchStats_MultiContainerPod_DistributesEvenly(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
	}}
	// 3 containers, 300 bytes total -> 100 bytes each.
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(300)},
				Containers: []kplugin.ContainerRef{
					{Name: "app"}, {Name: "sidecar"}, {Name: "init"},
				}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(),
		plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}},
		plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("expected 3 stat entries (one per container), got %d", len(stats))
	}
	for _, s := range stats {
		if s.Value.Value() != 100 {
			t.Errorf("container %q: Value = %d, want 100", s.ContainerName, s.Value.Value())
		}
		if s.Resource != "ephemeral-storage" {
			t.Errorf("container %q: Resource = %q, want ephemeral-storage", s.ContainerName, s.Resource)
		}
	}
}

func TestFetchStats_MultiplePodsAcrossNodes(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1"), node("n2")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
		pod("default", "web-2", map[string]string{"app": "web"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(1000)},
				Containers:       []kplugin.ContainerRef{{Name: "app"}}},
		}},
		"n2": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-2"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(2000)},
				Containers:       []kplugin.ContainerRef{{Name: "app"}}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(),
		plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}},
		plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	// 2 pods, 1 container each = 2 entries.
	if len(stats) != 2 {
		t.Fatalf("expected 2 stat entries, got %d", len(stats))
	}
}

func TestFetchStats_NilEphemeralStorage_Skipped(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			// EphemeralStorage is nil: should be skipped.
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
				Containers: []kplugin.ContainerRef{{Name: "app"}}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(),
		plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}},
		plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 entries for pod with no ephemeral-storage, got %d", len(stats))
	}
}

func TestFetchStats_LabelAbsentFiltering(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		// Matches: has name=nginx, no component label.
		pod("default", "nginx-1", map[string]string{"app.kubernetes.io/name": "nginx"}),
		// Non-match: has the component label that should be absent.
		pod("default", "nginx-2", map[string]string{
			"app.kubernetes.io/name":      "nginx",
			"app.kubernetes.io/component": "server",
		}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "nginx-1"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(100)},
				Containers:       []kplugin.ContainerRef{{Name: "nginx"}}},
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "nginx-2"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(200)},
				Containers:       []kplugin.ContainerRef{{Name: "server"}}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(), plugin.WorkloadIdentity{Labels: map[string]string{
		"app.kubernetes.io/name":      "nginx",
		"app.kubernetes.io/component": plugin.LabelAbsent,
	}}, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat entry (nginx-1 only), got %d", len(stats))
	}
	if stats[0].ContainerName != "nginx" {
		t.Errorf("unexpected container %q; expected nginx", stats[0].ContainerName)
	}
}

func TestFetchStats_EmptyLabels_ReturnsAll(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "a", map[string]string{"app": "a"}),
		pod("default", "b", map[string]string{"app": "b"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "a"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(10)},
				Containers:       []kplugin.ContainerRef{{Name: "a"}}},
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "b"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(20)},
				Containers:       []kplugin.ContainerRef{{Name: "b"}}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(),
		plugin.WorkloadIdentity{Labels: map[string]string{}},
		plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	if len(stats) != 2 {
		t.Errorf("expected 2 entries for empty selector, got %d", len(stats))
	}
}

func TestFetchStats_NodeListError_Backoff(t *testing.T) {
	nodes := &fakeNodeLister{err: errors.New("node API down")}
	pods := &fakePodLister{}
	fetcher := &fakeSummaryFetcher{}
	p := newPlugin(nodes, pods, fetcher)
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "x"}}

	// First call: node list fails.
	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected error from node list failure")
	}

	// Nodes are back; immediate second call should be blocked by backoff.
	nodes.err = nil
	nodes.nodes = []corev1.Node{node("n1")}
	_, err = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected backoff error on second call")
	}
	if !strings.Contains(err.Error(), "backoff") {
		t.Errorf("expected backoff error, got: %v", err)
	}
}

func TestFetchStats_NodeFetchError_NoCache_Backoff(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{}
	fetcher := &fakeSummaryFetcher{err: errors.New("kubelet unreachable")}
	p := newPlugin(nodes, pods, fetcher)
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "x"}}

	// First call: fetch fails, no cache available.
	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected error when node fetch fails and no cache exists")
	}

	// Immediate retry should be rejected by backoff.
	fetcher.err = nil
	_, err = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected backoff error")
	}
	if !strings.Contains(err.Error(), "backoff") {
		t.Errorf("expected backoff error, got: %v", err)
	}
}

func TestFetchStats_NodeFetchError_StaleCache_ReturnsStaleDontBackoff(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(999)},
				Containers:       []kplugin.ContainerRef{{Name: "app"}}},
		}},
	}}
	// CacheTTL=50ms: stale zone is [50ms, 100ms). We sleep 60ms to land in that
	// window reliably without overshooting 2*CacheTTL.
	p := kplugin.NewWithDeps(nodes, pods, fetcher,
		kplugin.Options{MaxBackoff: 10 * time.Second, CacheTTL: 50 * time.Millisecond},
		logr.Discard())
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}}

	// Populate the cache with a successful call.
	stats, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("initial call: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat, got %d", len(stats))
	}

	// Wait for cache to expire into the stale-but-usable zone (age > CacheTTL, < 2*CacheTTL).
	time.Sleep(60 * time.Millisecond)

	// Make the fetcher fail.
	fetcher.err = errors.New("node unavailable")
	fetcher.summaries = nil

	// Second call: fetch fails but cache is stale (not very stale); stale data returned.
	stats, err = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected stale cache to be used on fetch error, got error: %v", err)
	}
	if len(stats) != 1 {
		t.Errorf("expected 1 stale stat entry, got %d", len(stats))
	}
}

func TestFetchStats_VeryStaleCache_SkipsNode(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(999)},
				Containers:       []kplugin.ContainerRef{{Name: "app"}}},
		}},
	}}
	// Very short CacheTTL: 1ms means 2*CacheTTL = 2ms.
	p := kplugin.NewWithDeps(nodes, pods, fetcher,
		kplugin.Options{MaxBackoff: 10 * time.Second, CacheTTL: time.Millisecond},
		logr.Discard())
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}}

	// Populate cache.
	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("initial call: %v", err)
	}

	// Wait for cache to become very stale (> 2*CacheTTL).
	time.Sleep(10 * time.Millisecond)

	// Make the fetcher fail so the stale cache is all we have.
	fetcher.err = errors.New("node gone")
	fetcher.summaries = nil

	// Second call: node is very stale, so it should be skipped entirely (no data, no error).
	stats, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected skipped node (no error), got: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats when node is skipped due to very stale cache, got %d", len(stats))
	}
}

func TestFetchStats_BackoffReset(t *testing.T) {
	nodes := &fakeNodeLister{err: errors.New("fail")}
	pods := &fakePodLister{}
	fetcher := &fakeSummaryFetcher{}
	// Very short backoff so we can test recovery.
	p := kplugin.NewWithDeps(nodes, pods, fetcher,
		kplugin.Options{MaxBackoff: time.Millisecond},
		logr.Discard())
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "x"}}

	// Trigger backoff.
	_, _ = p.FetchStats(context.Background(), id, plugin.TimeWindow{})

	// Wait for backoff to expire, then recover.
	time.Sleep(5 * time.Millisecond)
	nodes.err = nil
	nodes.nodes = []corev1.Node{node("n1")}

	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected success after backoff expired: %v", err)
	}

	// Subsequent call should also succeed (backoff was cleared).
	_, err = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected continued success after backoff reset: %v", err)
	}
}

func TestFetchStats_CacheHit_NoExtraFetch(t *testing.T) {
	var fetchCount int
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
	}}
	countingFetcher := &countingFetcher{
		inner: &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
			"n1": {Pods: []kplugin.PodSummary{
				{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
					EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(1)},
					Containers:       []kplugin.ContainerRef{{Name: "app"}}},
			}},
		}},
		count: &fetchCount,
	}
	p := kplugin.NewWithDeps(nodes, pods, countingFetcher,
		kplugin.Options{MaxBackoff: time.Minute, CacheTTL: time.Hour},
		logr.Discard())
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}}

	for i := range 5 {
		_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if fetchCount != 1 {
		t.Errorf("expected 1 actual fetch for 5 calls within CacheTTL, got %d", fetchCount)
	}
}

func TestFetchStats_PodLabelError_Backoff(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{err: errors.New("pod API down")}
	fetcher := &fakeSummaryFetcher{}
	p := newPlugin(nodes, pods, fetcher)
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "x"}}

	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected error when pod list fails")
	}

	// Immediate retry should be blocked by backoff.
	pods.err = nil
	_, err = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err == nil {
		t.Fatal("expected backoff error on immediate retry")
	}
	if !strings.Contains(err.Error(), "backoff") {
		t.Errorf("expected backoff error, got: %v", err)
	}
}

// countingFetcher wraps a SummaryFetcher and counts Fetch calls.
type countingFetcher struct {
	inner kplugin.SummaryFetcher
	count *int
}

func (c *countingFetcher) Fetch(ctx context.Context, nodeName string) (*kplugin.NodeSummary, error) {
	*c.count++
	return c.inner.Fetch(ctx, nodeName)
}

// ---- httptest-based tests for restSummaryFetcher and New ----

func newTestRESTClient(t *testing.T, handler http.Handler) (cfg *rest.Config, cleanup func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	cfg = &rest.Config{Host: srv.URL}
	return cfg, srv.Close
}

func TestRestSummaryFetcher_Success(t *testing.T) {
	want := kplugin.NodeSummary{Pods: []kplugin.PodSummary{
		{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
			EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(512)},
			Containers:       []kplugin.ContainerRef{{Name: "app"}}},
	}}
	cfg, cleanup := newTestRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer cleanup()

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("NewForConfig: %v", err)
	}
	fetcher := kplugin.NewRestSummaryFetcher(cs.CoreV1().RESTClient())
	got, err := fetcher.Fetch(context.Background(), "node1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Pods) != 1 {
		t.Errorf("expected 1 pod, got %d", len(got.Pods))
	}
	if got.Pods[0].PodRef.Name != "web-1" {
		t.Errorf("pod name = %q, want web-1", got.Pods[0].PodRef.Name)
	}
}

func TestRestSummaryFetcher_HTTPError(t *testing.T) {
	cfg, cleanup := newTestRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer cleanup()

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("NewForConfig: %v", err)
	}
	fetcher := kplugin.NewRestSummaryFetcher(cs.CoreV1().RESTClient())
	_, err = fetcher.Fetch(context.Background(), "node1")
	if err == nil {
		t.Fatal("expected error from HTTP 500")
	}
}

func TestRestSummaryFetcher_InvalidJSON(t *testing.T) {
	cfg, cleanup := newTestRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer cleanup()

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("NewForConfig: %v", err)
	}
	fetcher := kplugin.NewRestSummaryFetcher(cs.CoreV1().RESTClient())
	_, err = fetcher.Fetch(context.Background(), "node1")
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestNew_ReturnsPlugin(t *testing.T) {
	cfg, cleanup := newTestRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	p, err := kplugin.New(cfg, kplugin.DefaultOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("New returned nil")
	}
	if p.Type() != "kubeletSummary" {
		t.Errorf("Type() = %q, want kubeletSummary", p.Type())
	}
}

func TestDefaultOptions(t *testing.T) {
	opts := kplugin.DefaultOptions()
	if opts.MaxBackoff <= 0 {
		t.Errorf("DefaultOptions().MaxBackoff = %v, want > 0", opts.MaxBackoff)
	}
	if opts.CacheTTL <= 0 {
		t.Errorf("DefaultOptions().CacheTTL = %v, want > 0", opts.CacheTTL)
	}
}

func TestNewWithDeps_ZeroMaxBackoffUsesDefault(t *testing.T) {
	p := kplugin.NewWithDeps(&fakeNodeLister{}, &fakePodLister{}, &fakeSummaryFetcher{},
		kplugin.Options{MaxBackoff: 0}, // zero: should fall back to DefaultOptions
		logr.Discard())
	if p == nil {
		t.Fatal("NewWithDeps returned nil")
	}
}

func TestCollectStats_PodWithNoContainersSkipped(t *testing.T) {
	nodes := &fakeNodeLister{nodes: []corev1.Node{node("n1")}}
	pods := &fakePodLister{pods: []corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
	}}
	fetcher := &fakeSummaryFetcher{summaries: map[string]*kplugin.NodeSummary{
		"n1": {Pods: []kplugin.PodSummary{
			// Has ephemeral storage reported but zero containers listed.
			{PodRef: kplugin.PodRef{Namespace: "default", Name: "web-1"},
				EphemeralStorage: &kplugin.StorageStats{UsedBytes: usedBytes(1024)},
				Containers:       []kplugin.ContainerRef{}},
		}},
	}}
	p := newPlugin(nodes, pods, fetcher)
	stats, err := p.FetchStats(context.Background(),
		plugin.WorkloadIdentity{Labels: map[string]string{"app": "web"}},
		plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("FetchStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats for pod with no containers, got %d", len(stats))
	}
}

func TestFetchStats_BackoffDoubles(t *testing.T) {
	// MaxBackoff=3ms: delays go 1ms → 2ms → 3ms (capped).
	nodes := &fakeNodeLister{err: errors.New("fail")}
	p := kplugin.NewWithDeps(nodes, &fakePodLister{}, &fakeSummaryFetcher{},
		kplugin.Options{MaxBackoff: 3 * time.Millisecond},
		logr.Discard())
	id := plugin.WorkloadIdentity{Labels: map[string]string{"app": "x"}}

	// First failure: nextDelay(0, 3ms) = 1ms.
	_, _ = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	time.Sleep(5 * time.Millisecond) // wait for 1ms backoff to clear

	// Second failure: nextDelay(1ms, 3ms) = 2ms — exercises the doubling branch.
	_, _ = p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	time.Sleep(5 * time.Millisecond) // wait for 2ms backoff to clear

	// Recovery: backoff expired, node healthy.
	nodes.err = nil
	_, err := p.FetchStats(context.Background(), id, plugin.TimeWindow{})
	if err != nil {
		t.Fatalf("expected success after doubled backoff expired: %v", err)
	}
}
