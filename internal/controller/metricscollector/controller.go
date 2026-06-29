/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metricscollector

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/metrics"
	"github.com/tight-line/ballast/internal/plugin"
	"github.com/tight-line/ballast/internal/policy"
	"github.com/tight-line/ballast/internal/stats"
	"github.com/tight-line/ballast/internal/store"
)

const (
	defaultPollInterval = 60 * time.Second
)

// Reconciler collects metrics for each WorkloadProfile and updates its status.
type Reconciler struct {
	client        client.Client
	storeClient   store.Client
	ks            *killswitch.KillSwitch
	resolver      *policy.Resolver
	dryRunMeasure bool
	// PluginGet is the registry lookup. Defaults to plugin.Get; overridable in tests
	// without touching global plugin registry state.
	PluginGet func(string) (plugin.MetricsPlugin, bool)
	rec       *metrics.Recorder
}

// New creates a Reconciler with the given dependencies.
func New(c client.Client, sc store.Client, ks *killswitch.KillSwitch, dryRunMeasure bool, rec *metrics.Recorder) *Reconciler {
	return &Reconciler{
		client:        c,
		storeClient:   sc,
		ks:            ks,
		resolver:      policy.NewResolver(c, ctrl.Log.WithName("metricscollector")),
		dryRunMeasure: dryRunMeasure,
		PluginGet:     plugin.Get,
		rec:           rec,
	}
}

// Setup wires a Reconciler into a running manager using a pre-registered KillSwitch.
// The KillSwitch must already be registered with the same manager (e.g. by workloadwatcher.Setup)
// so it is not double-registered.
func Setup(mgr ctrl.Manager, ks *killswitch.KillSwitch, sc store.Client, dryRunMeasure bool, rec *metrics.Recorder) error {
	return New(mgr.GetClient(), sc, ks, dryRunMeasure, rec).SetupWithManager(mgr)
}

// SetupWithManager registers the Reconciler with mgr.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("metricscollector").
		For(&ballastv1.WorkloadProfile{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile is the main entry point for a WorkloadProfile reconcile cycle.
//
// Steps (in order):
//  1. Load the WorkloadProfile — not-found is silently ignored; transient errors requeue.
//  2. Load BallastConfig to get the retention window — not-found requeues after the default
//     interval; parse errors fall back to the 7-day default.
//  3. Resolve the matching policy — no match requeues after the default interval.
//  4. Load MetricsSources referenced by the policy to determine poll interval and plugins.
//  5. If the kill switch is active, log at warn and requeue without collecting data.
//  6. Call collectAllSamples to fetch from each plugin and write to Redis
//     (skipped per-sample when --dry-run-measure is set).
//  7. Build container profiles: query Redis, compute stats, evaluate readiness, build
//     recommendations for each (container, resource) pair.
//  8. If --dry-run-measure, log what would be written and return without updating status.
//  9. Patch WorkloadProfile status with updated container profiles and meetsThreshold.
//
// The return value always sets RequeueAfter to the minimum poll interval across all sources.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, req.NamespacedName, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	// SelectorLabels is written by the workloadwatcher in a separate Status().Update()
	// after the profile is created. If the metrics collector fires before that write
	// completes, selectorLabels would be nil and filterPods would return every pod in
	// the cluster. Requeue briefly to let the workloadwatcher finish.
	if profile.Status.SelectorLabels == nil {
		log.Info("selectorLabels not yet set, requeueing", "profile", profile.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	resolved, err := r.resolver.Resolve(ctx, policy.Input{Labels: profile.Status.TupleLabels})
	if err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}
	if resolved == nil {
		log.Info("no policy matches profile, skipping", "profile", profile.Name)
		return ctrl.Result{RequeueAfter: defaultPollInterval}, nil
	}

	sources, pollInterval := r.loadSources(ctx, resolved.Spec.Metrics)

	if r.ks.IsActive() {
		log.Info("kill switch active, skipping metrics collection",
			"kill_switch", true, "kill_switch_reason", r.ks.Reason(), "profile", profile.Name)
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	now := time.Now()
	tupleHash := store.TupleHash(profile.Status.TupleLabels)

	observed, err := r.collectAllSamples(ctx, tupleHash, profile.Status.SelectorLabels, now, sources)
	if err != nil { // coverage:ignore - Redis error
		return ctrl.Result{}, err
	}

	resourcesInPolicy := policyResourceMap(resolved.Spec.Metrics)
	containers := mergeContainerSets(observed, profile.Status.Containers, resourcesInPolicy)
	containerProfiles, allReady := r.buildContainerProfiles(
		ctx, tupleHash, containers, resourcesInPolicy, resolved.Spec, now.UnixMilli())

	if r.dryRunMeasure {
		log.Info("dry-run: would update WorkloadProfile status",
			"dry_run", true, "profile", profile.Name, "meetsThreshold", allReady)
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	if allReady && !profile.Status.MeetsThreshold {
		r.rec.ProfileThresholdMet(ctx, profile.Name, resolved.Name)
	}
	base := profile.DeepCopy()
	profile.Status.Containers = containerProfiles
	profile.Status.MeetsThreshold = allReady
	if err := r.client.Status().Patch(ctx, &profile, client.MergeFrom(base)); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// collectAllSamples iterates over each loaded MetricsSource, looks up its plugin,
// calls FetchStats, and writes the resulting samples to Redis.
//
// Steps (in order):
//  1. For each source, look up the plugin by type name. Unknown types are logged and skipped.
//  2. Delegate to collectFromSource, which calls FetchStats and writes samples.
//     FetchStats failures are non-fatal (logged and skipped per source).
//     Redis write failures are fatal and return an error.
//  3. Accumulate observed (container, resource) pairs across all sources.
//
// Returns the union of (container, resource) pairs seen across all sources.
func (r *Reconciler) collectAllSamples(
	ctx context.Context,
	tupleHash string,
	selectorLabels map[string]string,
	now time.Time,
	sources map[string]*ballastv1.MetricsSource,
) (map[string]map[string]struct{}, error) {
	log := ctrl.LoggerFrom(ctx)
	observed := make(map[string]map[string]struct{})

	for sourceName, ms := range sources {
		p, ok := r.PluginGet(ms.Spec.Type)
		if !ok {
			log.Info("no plugin registered for source type, skipping",
				"source", sourceName, "type", ms.Spec.Type)
			continue
		}

		additional, err := r.collectFromSource(ctx, tupleHash, selectorLabels, now, sourceName, ms, p)
		if err != nil { // coverage:ignore - Redis error
			return nil, err
		}
		mergeObserved(observed, additional)
	}

	return observed, nil
}

// collectFromSource calls FetchStats on a single plugin and writes the resulting samples to Redis.
//
// Steps (in order):
//  1. Call FetchStats — on error, log at Error and return an empty result (non-fatal so other
//     sources are still attempted).
//  2. For each ContainerStats: record the (container, resource) pair as observed.
//  3. If --dry-run-measure, log the sample and skip writing.
//  4. Otherwise, delegate to writeSample (AddSample → ExpireOlderThan → EnforceReservoirCap).
//     Redis write failures are fatal and propagate to the caller.
//
// Returns the (container, resource) pairs observed from this source's FetchStats results.
func (r *Reconciler) collectFromSource(
	ctx context.Context,
	tupleHash string,
	selectorLabels map[string]string,
	now time.Time,
	sourceName string,
	ms *ballastv1.MetricsSource,
	p plugin.MetricsPlugin,
) (map[string]map[string]struct{}, error) {
	log := ctrl.LoggerFrom(ctx)
	observed := make(map[string]map[string]struct{})

	samples, err := p.FetchStats(ctx,
		plugin.WorkloadIdentity{Labels: selectorLabels},
		plugin.TimeWindow{End: now})
	if err != nil {
		log.Error(err, "FetchStats failed", "source", sourceName)
		r.rec.FetchError(ctx, sourceName, tupleHash)
		return observed, nil
	}

	for _, s := range samples {
		markObserved(observed, s.ContainerName, s.Resource)

		if r.dryRunMeasure {
			log.Info("dry-run: would write sample",
				"dry_run", true, "container", s.ContainerName,
				"resource", s.Resource, "value", s.Value.String())
			continue
		}

		if err := r.writeSample(ctx, tupleHash, ms, s); err != nil { // coverage:ignore - Redis error
			return nil, err
		}
	}

	return observed, nil
}

// writeSample persists a single ContainerStats entry to Redis.
func (r *Reconciler) writeSample(
	ctx context.Context,
	tupleHash string,
	ms *ballastv1.MetricsSource,
	s plugin.ContainerStats,
) error {
	key := store.MetricKey(tupleHash, s.ContainerName, s.Resource)
	valueStr := quantityToStoreValue(s.Resource, s.Value)
	if err := store.AddSample(ctx, r.storeClient, key, s.Timestamp.UnixMilli(), valueStr, ms.Spec.Config.ReservoirSize); err != nil { // coverage:ignore - Redis error
		return fmt.Errorf("adding sample for %s: %w", key, err)
	}
	return nil
}

// loadSources fetches the MetricsSource CRD for each unique source name referenced in the
// policy metrics. It also computes the minimum poll interval across all loaded sources.
//
// Missing sources are logged at Warn (misconfigured policy reference); transient errors
// are logged at Error. In either case the controller continues with any sources that did
// load. Returns the loaded sources and the minimum poll interval found, falling back to
// defaultPollInterval if no sources loaded successfully.
func (r *Reconciler) loadSources(ctx context.Context, metrics []ballastv1.MetricConfig) (map[string]*ballastv1.MetricsSource, time.Duration) {
	sources := make(map[string]*ballastv1.MetricsSource)
	pollInterval := defaultPollInterval

	for _, m := range metrics {
		if _, seen := sources[m.Source]; seen {
			continue
		}
		ms := r.tryLoadSource(ctx, m.Source)
		if ms == nil {
			continue
		}
		sources[m.Source] = ms
		pollInterval = minPollInterval(ms, pollInterval)
	}

	return sources, pollInterval
}

// tryLoadSource attempts to load a MetricsSource by name.
// Not-found is logged at Warn (misconfigured policy reference). Other errors are logged at Error.
// Returns nil in either failure case.
func (r *Reconciler) tryLoadSource(ctx context.Context, name string) *ballastv1.MetricsSource {
	log := ctrl.LoggerFrom(ctx)
	var ms ballastv1.MetricsSource
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("MetricsSource not found; check policy configuration", "source", name)
		} else { // coverage:ignore - transient API error
			log.Error(err, "failed to load MetricsSource", "source", name)
		}
		return nil
	}
	return &ms
}

// buildContainerProfiles builds a ContainerProfile for each tracked container by
// querying Redis for each policy-referenced resource.
//
// Steps (in order):
//  1. Return immediately (nil, false) if no containers are tracked.
//  2. For each container (in sorted order), delegate to buildContainerProfile.
//  3. If any container reports not-ready, allReady is set false.
//
// Returns the slice of profiles and a single meetsThreshold bool (true only when
// at least one container was processed and all are ready).
func (r *Reconciler) buildContainerProfiles(
	ctx context.Context,
	tupleHash string,
	containers map[string][]string,
	resourcesInPolicy map[string][]ballastv1.MetricConfig,
	policySpec ballastv1.ClusterResourcePolicySpec,
	nowMs int64,
) ([]ballastv1.ContainerProfile, bool) {
	if len(containers) == 0 {
		return nil, false
	}

	allReady := true
	var containerProfiles []ballastv1.ContainerProfile

	for _, containerName := range sortedKeys(containers) {
		cp, ready := r.buildContainerProfile(ctx, tupleHash, containerName,
			containers[containerName], resourcesInPolicy, policySpec, nowMs)
		if !ready {
			allReady = false
		}
		containerProfiles = append(containerProfiles, cp)
	}

	return containerProfiles, allReady && len(containerProfiles) > 0
}

// buildContainerProfile queries Redis for each policy-referenced resource of a single container,
// computes its stats and recommendations, and assembles the ContainerProfile.
//
// Steps (in order):
//  1. For each resource (sorted) that appears in the policy:
//     a. Delegate to processResourceStats to query Redis and compute stats.
//     b. On error: log at Error and mark this container not-ready; continue to the next resource.
//     c. Append the returned ContainerUsageStats to the profile.
//     d. If not ready: mark not-ready and skip recommendations.
//     e. If ready: merge returned recommendations into the profile's Recommendations map.
//  2. Return the assembled profile and whether all tracked resources were ready.
func (r *Reconciler) buildContainerProfile(
	ctx context.Context,
	tupleHash, containerName string,
	resources []string,
	resourcesInPolicy map[string][]ballastv1.MetricConfig,
	policySpec ballastv1.ClusterResourcePolicySpec,
	nowMs int64,
) (ballastv1.ContainerProfile, bool) {
	log := ctrl.LoggerFrom(ctx)
	cp := ballastv1.ContainerProfile{Name: containerName}
	allReady := true

	for _, resourceName := range sortedStringSlice(resources) {
		metricsForResource := resourcesInPolicy[resourceName]
		if len(metricsForResource) == 0 { // coverage:ignore - defensive guard, cannot occur in practice
			// mergeContainerSets filters to policy resources, so this should never fire.
			log.Info("resource tracked but not found in policy map; skipping",
				"container", containerName, "resource", resourceName)
			continue
		}

		usageStats, recs, ready, err := r.processResourceStats(
			ctx, tupleHash, containerName, resourceName, metricsForResource, policySpec, nowMs)
		if err != nil { // coverage:ignore - Redis error
			log.Error(err, "processResourceStats failed",
				"container", containerName, "resource", resourceName)
			allReady = false
			continue
		}

		cp.UsageStats = append(cp.UsageStats, usageStats)
		if !ready {
			allReady = false
			continue
		}

		if cp.Recommendations == nil {
			cp.Recommendations = make(map[string]ballastv1.ResourceRecommendation)
		}
		maps.Copy(cp.Recommendations, recs)
	}

	return cp, allReady
}

// processResourceStats queries Redis for a single (container, resource) key, computes
// statistical aggregates, evaluates readiness against the policy, and computes
// recommendations for all metric entries referencing this resource.
//
// Steps (in order):
//  1. QueryAll to retrieve all stored samples.
//     Redis errors are returned as fatal.
//  2. Parse member strings to int64 and sort ascending for percentile computation.
//  3. ComputeStats (p50/p95/p99/max/mean/stddev/cv).
//  4. FirstSeenMs to get the wall-clock time the first sample was stored.
//     Redis errors are returned as fatal. lastMs is set to nowMs.
//  5. EvaluateReadiness against the policy readiness config.
//  6. Build and return a ContainerUsageStats for the caller to append.
//  7. If not ready: return (usageStats, nil recommendations, false, nil).
//  8. If ready: computeAllRecommendations for all metric entries for this resource.
func (r *Reconciler) processResourceStats(
	ctx context.Context,
	tupleHash, containerName, resourceName string,
	metricsForResource []ballastv1.MetricConfig,
	policySpec ballastv1.ClusterResourcePolicySpec,
	nowMs int64,
) (containerStats ballastv1.ContainerUsageStats, resourceRecs map[string]ballastv1.ResourceRecommendation, meetsReadiness bool, err error) {
	key := store.MetricKey(tupleHash, containerName, resourceName)

	vals, err := store.QueryAll(ctx, r.storeClient, key)
	if err != nil { // coverage:ignore - Redis error
		return ballastv1.ContainerUsageStats{}, nil, false, fmt.Errorf("QueryAll %s: %w", key, err)
	}

	s := store.ComputeStats(parseValues(vals))

	firstMs, err := store.FirstSeenMs(ctx, r.storeClient, key)
	if err != nil { // coverage:ignore - Redis error
		return ballastv1.ContainerUsageStats{}, nil, false, fmt.Errorf("FirstSeenMs %s: %w", key, err)
	}

	ready := stats.EvaluateReadiness(s, firstMs, nowMs, policySpec.Readiness)
	sourceName := metricsForResource[0].Source
	usageStats := buildUsageStats(resourceName, sourceName, s, firstMs, nowMs)

	if !ready {
		return usageStats, nil, false, nil
	}

	recs := computeAllRecommendations(ctx, s, resourceName, metricsForResource)
	return usageStats, recs, true, nil
}

// computeAllRecommendations computes a ResourceRecommendation for resourceName by
// iterating over all metric entries for that resource. For each entry it calls
// ComputeRecommendation and sets the Request or Limit field accordingly.
// ComputeRecommendation failures (e.g. unknown aggregation in the policy) are logged at
// Error and skipped — the recommendation for that field is simply left unset.
func computeAllRecommendations(ctx context.Context, s store.Stats, resourceName string, metrics []ballastv1.MetricConfig) map[string]ballastv1.ResourceRecommendation {
	log := ctrl.LoggerFrom(ctx)
	recs := make(map[string]ballastv1.ResourceRecommendation)
	rec := recs[resourceName]

	for _, m := range metrics {
		q, err := stats.ComputeRecommendation(s, m)
		if err != nil {
			log.Error(err, "ComputeRecommendation failed; check policy metric configuration",
				"resource", resourceName, "field", m.Field, "aggregation", m.Aggregation)
			continue
		}
		if m.Field == "request" {
			rec.Request = q.String()
		} else {
			rec.Limit = q.String()
		}
	}

	recs[resourceName] = rec
	return recs
}

// buildUsageStats assembles a ContainerUsageStats from precomputed Stats and time range.
func buildUsageStats(resourceName, sourceName string, s store.Stats, firstMs, lastMs int64) ballastv1.ContainerUsageStats {
	now := metav1.Now()
	return ballastv1.ContainerUsageStats{
		Resource:    resourceName,
		Source:      sourceName,
		Samples:     int64(s.Count),
		TimeSpan:    formatDuration(lastMs - firstMs),
		P50:         formatResourceValue(resourceName, s.P50),
		P95:         formatResourceValue(resourceName, s.P95),
		P99:         formatResourceValue(resourceName, s.P99),
		Mean:        formatResourceValue(resourceName, s.Mean),
		StdDev:      formatResourceValue(resourceName, s.StdDev),
		CV:          fmt.Sprintf("%.3f", s.CV),
		LastUpdated: &now,
	}
}

// parseValues converts a slice of list values to a sorted int64 slice for ComputeStats.
// Unparseable entries indicate corrupted Redis data and are discarded; this is a
// defensive guard and should not occur under normal operation.
func parseValues(vals []string) []int64 {
	values := make([]int64, 0, len(vals))
	for _, v := range vals {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			values = append(values, n)
		}
	}
	slices.Sort(values)
	return values
}

// minPollInterval returns the smaller of current and the MetricsSource's configured pollInterval.
// If the source's interval is unparseable or non-positive, current is returned unchanged.
func minPollInterval(ms *ballastv1.MetricsSource, current time.Duration) time.Duration {
	d, err := time.ParseDuration(ms.Spec.Config.PollInterval)
	if err != nil || d <= 0 || d >= current {
		return current
	}
	return d
}

// quantityToStoreValue converts a resource.Quantity to an integer string for Redis storage.
// CPU is stored as millicores; everything else as bytes.
func quantityToStoreValue(resourceName string, q resource.Quantity) string {
	if resourceName == "cpu" {
		return strconv.FormatInt(q.MilliValue(), 10)
	}
	return strconv.FormatInt(q.Value(), 10)
}

// formatResourceValue converts a raw float64 value to a display string.
// CPU values are in millicores (rendered as "Xm"). Memory/storage values are
// auto-scaled: >=1Gi → "X.XXGi", >=1Mi → "X.XXMi", otherwise "X.XXKi".
func formatResourceValue(resourceName string, v float64) string {
	if resourceName == "cpu" {
		return resource.NewMilliQuantity(int64(v), resource.DecimalSI).String()
	}
	const (
		ki = 1024.0
		mi = 1024.0 * 1024.0
		gi = 1024.0 * 1024.0 * 1024.0
	)
	switch {
	case v >= gi:
		return fmt.Sprintf("%.2fGi", v/gi)
	case v >= mi:
		return fmt.Sprintf("%.2fMi", v/mi)
	default:
		return fmt.Sprintf("%.2fKi", v/ki)
	}
}

// formatDuration converts a millisecond span to a rounded-to-second duration string.
// Returns "0s" for non-positive spans.
func formatDuration(ms int64) string {
	if ms <= 0 { // coverage:ignore - defensive guard for zero/same-millisecond first+last timestamps
		return "0s"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

// policyResourceMap returns a map from resource name to the list of MetricConfigs
// that reference it. Used to look up which metrics to compute for a given resource.
func policyResourceMap(metrics []ballastv1.MetricConfig) map[string][]ballastv1.MetricConfig {
	result := make(map[string][]ballastv1.MetricConfig)
	for _, m := range metrics {
		result[m.Resource] = append(result[m.Resource], m)
	}
	return result
}

// mergeContainerSets returns a container→resources map combining containers from the current
// FetchStats cycle with containers already present in the profile status. Only resources
// referenced in the policy are included, ensuring stale resources don't accumulate.
func mergeContainerSets(
	current map[string]map[string]struct{},
	existing []ballastv1.ContainerProfile,
	resourcesInPolicy map[string][]ballastv1.MetricConfig,
) map[string][]string {
	result := make(map[string][]string)

	for containerName, resourceSet := range current {
		for resourceName := range resourceSet {
			if _, inPolicy := resourcesInPolicy[resourceName]; inPolicy {
				result[containerName] = appendUnique(result[containerName], resourceName)
			}
		}
	}

	for _, cp := range existing {
		for _, us := range cp.UsageStats {
			if _, inPolicy := resourcesInPolicy[us.Resource]; inPolicy {
				result[cp.Name] = appendUnique(result[cp.Name], us.Resource)
			}
		}
	}

	return result
}

// markObserved records that (container, resource) was seen in a FetchStats cycle.
func markObserved(observed map[string]map[string]struct{}, container, res string) {
	if observed[container] == nil {
		observed[container] = make(map[string]struct{})
	}
	observed[container][res] = struct{}{}
}

// mergeObserved adds all entries from src into dst.
func mergeObserved(dst, src map[string]map[string]struct{}) {
	for container, resources := range src {
		for res := range resources {
			markObserved(dst, container, res)
		}
	}
}

func appendUnique(slice []string, s string) []string {
	if slices.Contains(slice, s) {
		return slice
	}
	return append(slice, s)
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringSlice(ss []string) []string {
	sorted := make([]string, len(ss))
	copy(sorted, ss)
	sort.Strings(sorted)
	return sorted
}
