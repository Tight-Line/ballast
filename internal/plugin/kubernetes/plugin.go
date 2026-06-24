package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
}

// DefaultOptions returns the production defaults.
func DefaultOptions() Options {
	return Options{
		MaxRPS:     10,
		MaxBackoff: 5 * time.Minute,
	}
}

type backoffEntry struct {
	nextAllowed time.Time
	delay       time.Duration
}

// Plugin implements plugin.MetricsPlugin for the in-cluster Kubernetes metrics API.
//
// Callers (MetricsCollector) are responsible for adding per-profile jitter before
// the first poll cycle to spread the initial burst across the pollInterval window.
type Plugin struct {
	lister     PodMetricsLister
	limiter    *rate.Limiter
	maxBackoff time.Duration

	mu       sync.Mutex
	backoffs map[string]*backoffEntry // keyed by TupleHash of identity labels
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
		backoffs:   make(map[string]*backoffEntry),
	}
}

// Type implements plugin.MetricsPlugin.
func (p *Plugin) Type() string { return Type }

// FetchStats returns one ContainerStats per pod/container/resource for all pods
// matching id.Labels. Only containers listed in PodMetrics.Containers are included;
// the metrics API server omits initContainers and ephemeral containers automatically.
//
// Returns an error without calling the API if the identity is within its backoff window.
func (p *Plugin) FetchStats(ctx context.Context, id plugin.WorkloadIdentity, _ plugin.TimeWindow) ([]plugin.ContainerStats, error) {
	key := store.TupleHash(id.Labels)
	now := time.Now()

	if err := p.checkBackoff(key, now); err != nil {
		return nil, err
	}

	if err := p.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	list, err := p.lister.List(ctx, metav1.ListOptions{
		LabelSelector: buildLabelSelector(id.Labels),
	})
	if err != nil {
		p.recordFailure(key, now)
		return nil, fmt.Errorf("listing pod metrics: %w", err)
	}

	p.resetBackoff(key)
	return collect(list.Items, now), nil
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

// buildLabelSelector converts a label map to a comma-separated key=value selector.
func buildLabelSelector(lbls map[string]string) string {
	keys := make([]string, 0, len(lbls))
	for k := range lbls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + lbls[k]
	}
	return strings.Join(parts, ",")
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
