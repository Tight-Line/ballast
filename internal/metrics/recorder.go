/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metrics

import (
	"context"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ProfileID identifies the WorkloadProfile a metric is about. Name is the readable
// profile name; Labels is the identity-tuple label map (the same keys configured in
// ballastConfig.identityLabels). Both are optional: a zero ProfileID (e.g. for a
// webhook call that never resolved a profile) contributes no profile attributes.
type ProfileID struct {
	Name   string
	Labels map[string]string
}

// ProfileSnapshot is the per-profile state the profiles gauge observes.
type ProfileSnapshot struct {
	Labels map[string]string
	Ready  bool
}

// ProfileLister supplies the current set of WorkloadProfiles to the profiles gauge
// callback. It is implemented outside this package (against the controller cache) so
// internal/metrics stays free of Kubernetes client dependencies.
type ProfileLister interface {
	ListProfiles(ctx context.Context) ([]ProfileSnapshot, error)
}

// Recorder holds all Ballast OTel instruments. All methods are nil-safe: calling
// any method on a nil *Recorder is a no-op, allowing callers to pass nil in tests.
type Recorder struct {
	meter metric.Meter

	samplesCollected        metric.Int64Counter
	fetchErrors             metric.Int64Counter
	profileThresholdMet     metric.Int64Counter
	podsProcessed           metric.Int64Counter
	workloadProfilesCreated metric.Int64Counter
	workloadProfilesPurged  metric.Int64Counter
	resizeApplied           metric.Int64Counter
	resizeFailed            metric.Int64Counter
	resizeSkipped           metric.Int64Counter
	webhookMutations        metric.Int64Counter
	killSwitchTransitions   metric.Int64Counter

	ksActive atomic.Bool
	ksReason atomic.Value // string
}

// NewRecorder creates a Recorder using the given MeterProvider. Returns an error
// if any instrument registration fails.
func NewRecorder(provider metric.MeterProvider) (*Recorder, error) {
	m := provider.Meter("ballast")
	r := &Recorder{meter: m}
	r.ksReason.Store("")

	var err error
	// counter registers an Int64Counter, latching the first error so callers can
	// register every instrument in a flat list and check err once at the end.
	counter := func(name, desc string) metric.Int64Counter {
		if err != nil { // coverage:ignore - short-circuits once a prior registration has failed
			return nil
		}
		var c metric.Int64Counter
		c, err = m.Int64Counter(name, metric.WithDescription(desc))
		return c
	}

	r.samplesCollected = counter("ballast.samples.collected",
		"Metric samples collected and written to the store")
	r.fetchErrors = counter("ballast.fetch.errors",
		"Errors returned by FetchStats for a metrics source")
	r.profileThresholdMet = counter("ballast.profiles.threshold_met",
		"WorkloadProfiles that transitioned to meets-threshold")
	r.podsProcessed = counter("ballast.pods.processed",
		"Pods processed by the workload watcher (created or deleted)")
	r.workloadProfilesCreated = counter("ballast.workload_profiles.created",
		"WorkloadProfile objects created")
	r.workloadProfilesPurged = counter("ballast.workload_profiles.purged",
		"WorkloadProfile objects purged after orphan TTL expired")
	r.resizeApplied = counter("ballast.resize.applied",
		"In-place resize operations applied successfully")
	r.resizeFailed = counter("ballast.resize.failed",
		"In-place resize operations that failed")
	r.resizeSkipped = counter("ballast.resize.skipped",
		"Resize evaluations skipped before issuing a patch")
	r.webhookMutations = counter("ballast.webhook.mutations",
		"Pod admission webhook invocations and their outcomes")
	r.killSwitchTransitions = counter("ballast.kill_switch.transitions",
		"Kill switch state transitions (activated or deactivated)")
	if err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	ksGauge, err := m.Int64ObservableGauge("ballast.kill_switch.active",
		metric.WithDescription("1 when the kill switch is active, 0 otherwise"),
	)
	if err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if _, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		val := int64(0)
		if r.ksActive.Load() {
			val = 1
		}
		o.ObserveInt64(ksGauge, val,
			metric.WithAttributes(attribute.String("reason", r.ksReason.Load().(string))))
		return nil
	}, ksGauge); err != nil { // coverage:ignore - OTel SDK never errors for valid callback registration
		return nil, err
	}

	return r, nil
}

// RegisterProfileGauge registers the ballast.profiles observable gauge, which emits a
// value of 1 per WorkloadProfile carrying its identity-tuple attributes plus a state
// attribute ("accruing" until the profile meets its threshold, then "ready"). Dashboards
// aggregate it: total = count, by business unit = count grouped by that attribute, etc.
// The lister reads from the controller cache at collection time, so the gauge is always
// a fresh snapshot with no counters to drift. Nil-safe; call once during setup.
func (r *Recorder) RegisterProfileGauge(lister ProfileLister) error {
	if r == nil {
		return nil
	}
	gauge, err := r.meter.Int64ObservableGauge("ballast.profiles",
		metric.WithDescription("WorkloadProfiles by identity tuple and readiness state (1 per profile)"),
	)
	if err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return err
	}
	_, err = r.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		snaps, lerr := lister.ListProfiles(ctx)
		if lerr != nil {
			return lerr
		}
		for _, s := range snaps {
			state := "accruing"
			if s.Ready {
				state = "ready"
			}
			attrs := append(profileAttrs(ProfileID{Labels: s.Labels}), attribute.String("state", state))
			o.ObserveInt64(gauge, 1, metric.WithAttributes(attrs...))
		}
		return nil
	}, gauge)
	return err
}

// profileAttrs converts a ProfileID into OTel attributes: the readable profile name
// (when set) plus one attribute per identity-tuple label. Each label's attribute key is
// the segment after the last '/', sanitized to [a-z0-9_]; if two labels would sanitize to
// the same key, the colliding ones fall back to their sanitized fully-qualified key.
func profileAttrs(id ProfileID) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(id.Labels)+1)
	if id.Name != "" {
		attrs = append(attrs, attribute.String("profile", id.Name))
	}
	// Count sanitized suffixes so colliding keys can fall back to the full key.
	counts := make(map[string]int, len(id.Labels))
	for k := range id.Labels {
		counts[sanitizeAttrKey(labelSuffix(k))]++
	}
	for k, v := range id.Labels {
		name := sanitizeAttrKey(labelSuffix(k))
		if counts[name] > 1 {
			name = sanitizeAttrKey(k)
		}
		attrs = append(attrs, attribute.String(name, v))
	}
	return attrs
}

// labelSuffix returns the portion of a label key after the last '/', or the whole key
// when there is no '/'.
func labelSuffix(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// sanitizeAttrKey lowercases s and replaces every character outside [a-z0-9_] with '_'.
func sanitizeAttrKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// SampleCollected records one metric sample written to the store.
func (r *Recorder) SampleCollected(ctx context.Context, source, resource, container string, id ProfileID) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id),
		attribute.String("source", source),
		attribute.String("resource", resource),
		attribute.String("container", container),
	)
	r.samplesCollected.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// FetchError records a FetchStats failure for a metrics source.
func (r *Recorder) FetchError(ctx context.Context, source string, id ProfileID) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id), attribute.String("source", source))
	r.fetchErrors.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// ProfileThresholdMet records when a WorkloadProfile transitions to meets-threshold.
func (r *Recorder) ProfileThresholdMet(ctx context.Context, id ProfileID, policy string) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id), attribute.String("policy", policy))
	r.profileThresholdMet.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// PodProcessed records a pod being processed by the workload watcher.
// event is "created" or "deleted".
func (r *Recorder) PodProcessed(ctx context.Context, event, namespace string, id ProfileID) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id),
		attribute.String("event", event),
		attribute.String("namespace", namespace),
	)
	r.podsProcessed.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// WorkloadProfileCreated records a new WorkloadProfile being created.
func (r *Recorder) WorkloadProfileCreated(ctx context.Context, id ProfileID) {
	if r == nil {
		return
	}
	r.workloadProfilesCreated.Add(ctx, 1, metric.WithAttributes(profileAttrs(id)...))
}

// WorkloadProfilePurged records a WorkloadProfile being deleted after orphan TTL.
func (r *Recorder) WorkloadProfilePurged(ctx context.Context, id ProfileID) {
	if r == nil {
		return
	}
	r.workloadProfilesPurged.Add(ctx, 1, metric.WithAttributes(profileAttrs(id)...))
}

// ResizeApplied records a successful in-place resize.
func (r *Recorder) ResizeApplied(ctx context.Context, id ProfileID, policy, namespace string) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id),
		attribute.String("policy", policy),
		attribute.String("namespace", namespace),
	)
	r.resizeApplied.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// ResizeFailed records a failed in-place resize.
func (r *Recorder) ResizeFailed(ctx context.Context, id ProfileID, policy, namespace string) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id),
		attribute.String("policy", policy),
		attribute.String("namespace", namespace),
	)
	r.resizeFailed.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// ResizeSkipped records a resize evaluation that was skipped without issuing a patch.
// reason is one of: cooldown, no_drift, kill_switch, not_ready, no_policy, dry_run.
func (r *Recorder) ResizeSkipped(ctx context.Context, reason string, id ProfileID) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id), attribute.String("reason", reason))
	r.resizeSkipped.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// WebhookMutation records a pod admission webhook invocation.
// result is one of: kill_switch, skipped, not_available, dry_run, mutated.
func (r *Recorder) WebhookMutation(ctx context.Context, result, namespace string, id ProfileID) {
	if r == nil {
		return
	}
	attrs := append(profileAttrs(id),
		attribute.String("result", result),
		attribute.String("namespace", namespace),
	)
	r.webhookMutations.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// KillSwitchTransition records a kill switch state change.
// transition is "activated" or "deactivated".
func (r *Recorder) KillSwitchTransition(ctx context.Context, transition string) {
	if r == nil {
		return
	}
	r.killSwitchTransitions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("transition", transition),
	))
}

// SetKillSwitchActive updates the kill switch gauge state. Call this on every
// reconcile (not just transitions) to keep the gauge current.
func (r *Recorder) SetKillSwitchActive(active bool, reason string) {
	if r == nil {
		return
	}
	r.ksActive.Store(active)
	r.ksReason.Store(reason)
}
