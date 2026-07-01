package kubelet

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/tight-line/ballast/internal/plugin"
	"github.com/tight-line/ballast/internal/store"
)

const Type = "kubeletSummary"

// Options configures the Plugin.
type Options struct {
	// MaxBackoff is the ceiling for exponential backoff on node fetch errors.
	MaxBackoff time.Duration
	// CacheTTL is how long a node's summary is cached before a refresh is attempted.
	// Entries older than 2*CacheTTL are dropped with a warning rather than returned as
	// stale data. Zero disables caching (useful in tests).
	CacheTTL time.Duration
}

// DefaultOptions returns the production defaults.
func DefaultOptions() Options {
	return Options{
		MaxBackoff: 5 * time.Minute,
		CacheTTL:   55 * time.Second,
	}
}

// NodeLister lists cluster nodes.
type NodeLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error)
}

// PodLister lists pods across all namespaces (used to resolve pod labels for filtering).
type PodLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error)
}

// SummaryFetcher fetches the kubelet /stats/summary for a single node via the API
// server proxy.
type SummaryFetcher interface {
	Fetch(ctx context.Context, nodeName string) (*NodeSummary, error)
}

// NodeSummary is the minimal subset of the kubelet /stats/summary response we use.
type NodeSummary struct {
	Pods []PodSummary `json:"pods"`
}

// PodSummary holds the pod-level data from the kubelet summary.
type PodSummary struct {
	PodRef           PodRef         `json:"podRef"`
	EphemeralStorage *StorageStats  `json:"ephemeral-storage,omitempty"`
	Containers       []ContainerRef `json:"containers"`
}

// PodRef identifies a pod by name and namespace.
type PodRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// StorageStats holds ephemeral storage usage.
type StorageStats struct {
	UsedBytes *uint64 `json:"usedBytes,omitempty"`
}

// ContainerRef identifies a container by name.
type ContainerRef struct {
	Name string `json:"name"`
}

type backoffEntry struct {
	nextAllowed time.Time
	delay       time.Duration
}

// nodeCacheEntry holds the per-node summary with a mutex that serializes concurrent
// refreshes for the same node without blocking other nodes.
type nodeCacheEntry struct {
	mu sync.Mutex
	// fetchTime is when pods were last fetched successfully; it drives the staleness
	// gate (serve fresh, fall back to stale, or skip).
	fetchTime time.Time
	// lastAttempt is when a fetch was last attempted (success or failure). It rate-gates
	// retries so a failing kubelet is probed at most once per CacheTTL per node, no matter
	// how many workload identities scrape in a cycle.
	lastAttempt time.Time
	pods        []PodSummary
}

type podKey struct {
	Namespace, Name string
}

// Plugin implements plugin.MetricsPlugin for the kubelet Summary API, providing
// ephemeral-storage measurements.
//
// The kubelet Summary API reports ephemeral storage at the pod level, not per
// container. This plugin attributes pod-level usage evenly across containers;
// single-container pods receive the full value. This is a known limitation of the
// data source.
type Plugin struct {
	nodes   NodeLister
	pods    PodLister
	fetcher SummaryFetcher
	opts    Options
	log     logr.Logger

	// per-node summary cache; mu serializes map writes, per-entry mu serializes fetches
	nodeCacheMu sync.Mutex
	nodeCache   map[string]*nodeCacheEntry

	// cluster-wide pod label cache (keyed by namespace/name)
	podCacheMu   sync.RWMutex
	podCacheTime time.Time
	podLabels    map[podKey]map[string]string

	// per-workload backoff
	backoffMu sync.Mutex
	backoffs  map[string]*backoffEntry
}

// restSummaryFetcher fetches node summaries via the Kubernetes API server proxy.
type restSummaryFetcher struct {
	client rest.Interface
}

func (f *restSummaryFetcher) Fetch(ctx context.Context, nodeName string) (*NodeSummary, error) {
	data, err := f.client.Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy").
		Suffix("stats/summary").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy request to %s: %w", nodeName, err)
	}
	var s NodeSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decoding summary from %s: %w", nodeName, err)
	}
	return &s, nil
}

// NewRestSummaryFetcher wraps a Kubernetes REST client as a SummaryFetcher.
// Exposed for testing with a fake httptest server.
func NewRestSummaryFetcher(client rest.Interface) SummaryFetcher {
	return &restSummaryFetcher{client: client}
}

// New constructs a Plugin from a controller-runtime rest.Config.
func New(restConfig *rest.Config, opts Options) (*Plugin, error) {
	cs, err := kubernetes.NewForConfig(restConfig)
	if err != nil { // coverage:ignore - kubernetes.NewForConfig only fails for invalid TLS config, not testable without a broken cert
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	return NewWithDeps(
		cs.CoreV1().Nodes(),
		cs.CoreV1().Pods(""),
		&restSummaryFetcher{client: cs.CoreV1().RESTClient()},
		opts,
		ctrl.Log.WithName("plugin.kubeletSummary"),
	), nil
}

// NewWithDeps constructs a Plugin with explicit dependencies (intended for testing).
func NewWithDeps(nodes NodeLister, pods PodLister, fetcher SummaryFetcher, opts Options, log logr.Logger) *Plugin {
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultOptions().MaxBackoff
	}
	return &Plugin{
		nodes:     nodes,
		pods:      pods,
		fetcher:   fetcher,
		opts:      opts,
		log:       log,
		nodeCache: make(map[string]*nodeCacheEntry),
		podLabels: make(map[podKey]map[string]string),
		backoffs:  make(map[string]*backoffEntry),
	}
}

// Type implements plugin.MetricsPlugin.
func (p *Plugin) Type() string { return Type }

// FetchStats returns ephemeral-storage ContainerStats for all pods matching id.Labels.
// It fans out to all nodes in parallel, using per-node TTL caches, and backs off the
// workload identity on node fetch errors.
func (p *Plugin) FetchStats(ctx context.Context, id plugin.WorkloadIdentity, _ plugin.TimeWindow) ([]plugin.ContainerStats, error) {
	key := store.TupleHash(id.Labels)
	now := time.Now()

	if err := p.checkBackoff(key, now); err != nil {
		return nil, err
	}

	if err := p.refreshPodLabels(ctx, now); err != nil {
		p.recordFailure(key, now)
		return nil, fmt.Errorf("refreshing pod labels: %w", err)
	}

	nodeList, err := p.nodes.List(ctx, metav1.ListOptions{})
	if err != nil {
		p.recordFailure(key, now)
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	type nodeResult struct {
		pods []PodSummary
		err  error
	}

	results := make([]nodeResult, len(nodeList.Items))
	var wg sync.WaitGroup
	for i, node := range nodeList.Items {
		wg.Add(1)
		go func(idx int, nodeName string) {
			defer wg.Done()
			pods, err := p.nodeData(ctx, nodeName, now)
			results[idx] = nodeResult{pods: pods, err: err}
		}(i, node.Name)
	}
	wg.Wait()

	var allPods []PodSummary
	for i, r := range results {
		if r.err != nil {
			p.log.Error(r.err, "failed to fetch node summary", "node", nodeList.Items[i].Name)
			p.recordFailure(key, now)
			return nil, fmt.Errorf("node %s: %w", nodeList.Items[i].Name, r.err)
		}
		allPods = append(allPods, r.pods...)
	}

	p.resetBackoff(key)

	matching := p.filterPodsByLabels(allPods, id.Labels)
	return collectStats(matching, now), nil
}

// nodeData returns the pod summaries for nodeName, using the cache when fresh.
// When the cache is fresh (age < CacheTTL) it is served directly. Otherwise a refresh
// is attempted, rate-gated to at most once per CacheTTL per node so a failing kubelet
// is not re-hit once per workload every scrape. When a refresh is unavailable (it
// failed or the gate is closed) the staleness gate applies: cached pods are served
// while age < 2*CacheTTL, and the node is skipped (nil, nil) once age >= 2*CacheTTL.
// A recovered node heals within one CacheTTL of the kubelet coming back.
// Returns a non-nil error only when there is no usable data at all.
func (p *Plugin) nodeData(ctx context.Context, nodeName string, now time.Time) ([]PodSummary, error) {
	// Get or lazily create the entry; nodeCacheMu only protects the map, not the
	// fetch — each entry carries its own mutex to serialize per-node refreshes.
	p.nodeCacheMu.Lock()
	entry, ok := p.nodeCache[nodeName]
	if !ok {
		entry = &nodeCacheEntry{}
		p.nodeCache[nodeName] = entry
	}
	p.nodeCacheMu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if p.opts.CacheTTL > 0 && !entry.fetchTime.IsZero() && now.Sub(entry.fetchTime) < p.opts.CacheTTL {
		return entry.pods, nil
	}

	// Rate-gate refresh attempts: probe a node at most once per CacheTTL, whether the
	// last attempt succeeded or failed. Without this, a failing kubelet would be re-hit
	// once per enrolled workload every scrape, since the per-node cache is shared across
	// workload identities but FetchStats runs per identity.
	if p.opts.CacheTTL > 0 && !entry.lastAttempt.IsZero() && now.Sub(entry.lastAttempt) < p.opts.CacheTTL {
		return p.cachedOrSkip(entry, nodeName, now), nil
	}

	// Cache is stale (or absent) and the retry gate is open: attempt a refresh. The
	// staleness gate only governs the fallback path, so it can never block a re-fetch.
	entry.lastAttempt = now
	s, err := p.fetcher.Fetch(ctx, nodeName)
	if err != nil {
		if entry.fetchTime.IsZero() {
			return nil, err
		}
		p.log.Error(err, "node summary fetch failed; using stale data", "node", nodeName)
		return p.cachedOrSkip(entry, nodeName, now), nil
	}

	entry.fetchTime = now
	entry.pods = s.Pods
	return s.Pods, nil
}

// cachedOrSkip decides what to serve when a fresh fetch is unavailable (it failed, or was
// suppressed by the retry gate). It serves the last successful pods while their age is
// under 2*CacheTTL, and skips the node (nil) once age reaches 2*CacheTTL. With no prior
// success there is nothing to serve, so it skips silently.
func (p *Plugin) cachedOrSkip(entry *nodeCacheEntry, nodeName string, now time.Time) []PodSummary {
	if entry.fetchTime.IsZero() {
		return nil
	}
	if age := now.Sub(entry.fetchTime); age >= 2*p.opts.CacheTTL {
		p.log.Info("skipping node: summary too stale; metrics for this node will be missing",
			"node", nodeName, "age", age.Round(time.Second), "limit", 2*p.opts.CacheTTL)
		return nil
	}
	return entry.pods
}

// refreshPodLabels fetches all pods cluster-wide and rebuilds the label map when the
// cache is stale. The cache timestamp is always updated on completion (even on error)
// to prevent concurrent callers from all retrying simultaneously.
func (p *Plugin) refreshPodLabels(ctx context.Context, now time.Time) error {
	p.podCacheMu.Lock()
	defer p.podCacheMu.Unlock()

	if p.opts.CacheTTL > 0 && !p.podCacheTime.IsZero() && now.Sub(p.podCacheTime) < p.opts.CacheTTL {
		return nil
	}

	list, err := p.pods.List(ctx, metav1.ListOptions{})
	p.podCacheTime = now // always stamp, even on error, to avoid thundering herd
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	labels := make(map[podKey]map[string]string, len(list.Items))
	for _, pod := range list.Items {
		labels[podKey{Namespace: pod.Namespace, Name: pod.Name}] = pod.Labels
	}
	p.podLabels = labels
	return nil
}

// filterPodsByLabels returns pod summaries whose labels (resolved from the pod label
// cache) satisfy all requirements in selectorLabels, using the same matching logic as
// the kubernetesMetrics plugin.
func (p *Plugin) filterPodsByLabels(pods []PodSummary, selectorLabels map[string]string) []PodSummary {
	if len(selectorLabels) == 0 {
		return pods
	}
	p.podCacheMu.RLock()
	defer p.podCacheMu.RUnlock()

	out := make([]PodSummary, 0, len(pods))
	for _, pod := range pods {
		podLabels := p.podLabels[podKey{Namespace: pod.PodRef.Namespace, Name: pod.PodRef.Name}]
		if plugin.MatchesSelector(podLabels, selectorLabels) {
			out = append(out, pod)
		}
	}
	return out
}

// collectStats builds ContainerStats entries from matching pod summaries.
// Pod-level ephemeral storage is distributed evenly across containers.
func collectStats(pods []PodSummary, now time.Time) []plugin.ContainerStats {
	var stats []plugin.ContainerStats
	for _, pod := range pods {
		if pod.EphemeralStorage == nil || pod.EphemeralStorage.UsedBytes == nil {
			continue
		}
		if len(pod.Containers) == 0 {
			continue
		}
		perContainer := int64(*pod.EphemeralStorage.UsedBytes) / int64(len(pod.Containers))
		qty := resource.NewQuantity(perContainer, resource.BinarySI)
		for _, c := range pod.Containers {
			stats = append(stats, plugin.ContainerStats{
				ContainerName: c.Name,
				Resource:      "ephemeral-storage",
				Value:         *qty,
				Timestamp:     now,
			})
		}
	}
	return stats
}

func (p *Plugin) checkBackoff(key string, now time.Time) error {
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	be, ok := p.backoffs[key]
	if !ok {
		return nil
	}
	if now.Before(be.nextAllowed) {
		return fmt.Errorf("kubelet summary in backoff until %s: skipping cycle", be.nextAllowed.Format(time.RFC3339))
	}
	return nil
}

func (p *Plugin) recordFailure(key string, now time.Time) {
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	be, ok := p.backoffs[key]
	if !ok {
		be = &backoffEntry{}
		p.backoffs[key] = be
	}
	be.delay = nextDelay(be.delay, p.opts.MaxBackoff)
	be.nextAllowed = now.Add(be.delay)
}

func (p *Plugin) resetBackoff(key string) {
	p.backoffMu.Lock()
	defer p.backoffMu.Unlock()
	delete(p.backoffs, key)
}

func nextDelay(current, maxBackoff time.Duration) time.Duration {
	d := time.Second
	if current > 0 {
		d = current * 2
	}
	if maxBackoff > 0 && d > maxBackoff {
		return maxBackoff
	}
	return d
}
