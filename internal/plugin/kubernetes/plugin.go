package kubernetes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/tight-line/ballast/internal/plugin"
	"github.com/tight-line/ballast/internal/store"
)

const Type = "kubernetesMetrics"

// PodMetricsLister is the subset of the metrics API client used by this plugin.
// It matches the interface of metricsClient.MetricsV1beta1().PodMetricses(""),
// enabling test doubles without importing the full metrics clientset.
type PodMetricsLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*metricsv1beta1.PodMetricsList, error)
}

// Options configures the Plugin.
type Options struct {
	// MaxRPS is the maximum metrics API requests per second (shared across all FetchStats calls).
	MaxRPS float64
	// MaxBackoff is the ceiling for exponential backoff on API errors.
	MaxBackoff time.Duration
	// CacheTTL is how long the full pod metrics list is cached before the next refresh.
	// All FetchStats calls within the TTL share one API call; filtering is done client-side.
	// Zero disables caching (useful in tests).
	CacheTTL time.Duration
}

// DefaultOptions returns the production defaults.
func DefaultOptions() Options {
	return Options{
		MaxRPS:     10,
		MaxBackoff: 5 * time.Minute,
		CacheTTL:   55 * time.Second,
	}
}

type backoffEntry struct {
	nextAllowed time.Time
	delay       time.Duration
}

// Plugin implements plugin.MetricsPlugin for the in-cluster Kubernetes metrics API.
//
// The metrics.k8s.io API does not filter by label selector server-side, so this
// plugin fetches all pod metrics once per CacheTTL and filters client-side.
// Concurrent FetchStats calls share the cached list; the first caller to find the
// cache stale refreshes it while others block on cacheMu.
type Plugin struct {
	lister     PodMetricsLister
	limiter    *rate.Limiter
	maxBackoff time.Duration
	cacheTTL   time.Duration

	mu       sync.Mutex
	backoffs map[string]*backoffEntry // keyed by TupleHash of identity labels

	cacheMu    sync.Mutex
	cacheTime  time.Time
	cacheItems []metricsv1beta1.PodMetrics
}

// New constructs a Plugin. Register it with plugin.Register after construction.
// Zero values in opts fall back to DefaultOptions.
func New(lister PodMetricsLister, opts Options) *Plugin {
	if opts.MaxRPS <= 0 {
		opts.MaxRPS = DefaultOptions().MaxRPS
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultOptions().MaxBackoff
	}
	return &Plugin{
		lister:     lister,
		limiter:    rate.NewLimiter(rate.Limit(opts.MaxRPS), 1),
		maxBackoff: opts.MaxBackoff,
		cacheTTL:   opts.CacheTTL,
		backoffs:   make(map[string]*backoffEntry),
	}
}

// Type implements plugin.MetricsPlugin.
func (p *Plugin) Type() string { return Type }

// FetchStats returns one ContainerStats per pod/container/resource for all pods
// matching id.Labels. The full pod metrics list is fetched at most once per
// CacheTTL; filtering against id.Labels is done client-side because the
// metrics.k8s.io API ignores label selectors.
//
// Returns an error without calling the API if the identity is within its backoff window.
func (p *Plugin) FetchStats(ctx context.Context, id plugin.WorkloadIdentity, _ plugin.TimeWindow) ([]plugin.ContainerStats, error) {
	key := store.TupleHash(id.Labels)
	now := time.Now()

	if err := p.checkBackoff(key, now); err != nil {
		return nil, err
	}

	items, err := p.cachedList(ctx, now)
	if err != nil {
		p.recordFailure(key, now)
		return nil, err
	}

	p.resetBackoff(key)
	return collect(filterPods(items, id.Labels), now), nil
}

// cachedList returns the cached pod metrics list, refreshing if stale.
// The rate limiter applies only to actual API calls, not cache hits.
// On error, cacheTime is still updated to prevent a thundering herd of retries
// from concurrent FetchStats callers; per-workload backoff handles retry pacing.
func (p *Plugin) cachedList(ctx context.Context, now time.Time) ([]metricsv1beta1.PodMetrics, error) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if p.cacheTTL > 0 && now.Sub(p.cacheTime) < p.cacheTTL {
		return p.cacheItems, nil
	}

	if err := p.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	list, err := p.lister.List(ctx, metav1.ListOptions{})
	p.cacheTime = now
	if err != nil {
		p.cacheItems = nil
		return nil, fmt.Errorf("listing pod metrics: %w", err)
	}

	p.cacheItems = list.Items
	return list.Items, nil
}

func (p *Plugin) checkBackoff(key string, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	be, ok := p.backoffs[key]
	if !ok {
		return nil
	}
	if now.Before(be.nextAllowed) {
		return fmt.Errorf("metrics API in backoff until %s: skipping cycle", be.nextAllowed.Format(time.RFC3339))
	}
	return nil
}

func (p *Plugin) recordFailure(key string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	be, ok := p.backoffs[key]
	if !ok {
		be = &backoffEntry{}
		p.backoffs[key] = be
	}
	be.delay = nextDelay(be.delay, p.maxBackoff)
	be.nextAllowed = now.Add(be.delay)
}

func (p *Plugin) resetBackoff(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.backoffs, key)
}

// nextDelay doubles the current delay toward maxBackoff, starting from 1s.
func nextDelay(current, maxBackoff time.Duration) time.Duration {
	d := time.Second
	if current > 0 {
		d = current * 2
	}
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// filterPods returns only the entries whose labels satisfy all requirements in
// selectorLabels. Values equal to plugin.LabelAbsent require the key to be
// absent from the pod; all other values require an exact match. This client-side
// filter is necessary because metrics.k8s.io ignores label selectors server-side.
func filterPods(pods []metricsv1beta1.PodMetrics, selectorLabels map[string]string) []metricsv1beta1.PodMetrics {
	if len(selectorLabels) == 0 {
		return pods
	}
	out := make([]metricsv1beta1.PodMetrics, 0, len(pods))
	for i := range pods {
		if plugin.MatchesSelector(pods[i].Labels, selectorLabels) {
			out = append(out, pods[i])
		}
	}
	return out
}

// collect emits one ContainerStats per pod/container/resource combination.
// All resources present in the Usage map are included (cpu, memory, ephemeral-storage).
func collect(pods []metricsv1beta1.PodMetrics, now time.Time) []plugin.ContainerStats {
	var stats []plugin.ContainerStats
	for i := range pods {
		for _, c := range pods[i].Containers {
			for res, qty := range c.Usage {
				stats = append(stats, plugin.ContainerStats{
					ContainerName: c.Name,
					Resource:      string(res),
					Value:         qty,
					Timestamp:     now,
				})
			}
		}
	}
	return stats
}
