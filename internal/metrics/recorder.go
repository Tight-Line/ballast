/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metrics

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Recorder holds all Ballast OTel instruments. All methods are nil-safe: calling
// any method on a nil *Recorder is a no-op, allowing callers to pass nil in tests.
type Recorder struct {
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
	r := &Recorder{}
	r.ksReason.Store("")

	var err error

	if r.samplesCollected, err = m.Int64Counter("ballast.samples.collected",
		metric.WithDescription("Metric samples collected and written to the store"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.fetchErrors, err = m.Int64Counter("ballast.fetch.errors",
		metric.WithDescription("Errors returned by FetchStats for a metrics source"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.profileThresholdMet, err = m.Int64Counter("ballast.profiles.threshold_met",
		metric.WithDescription("WorkloadProfiles that transitioned to meets-threshold"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.podsProcessed, err = m.Int64Counter("ballast.pods.processed",
		metric.WithDescription("Pods processed by the workload watcher (created or deleted)"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.workloadProfilesCreated, err = m.Int64Counter("ballast.workload_profiles.created",
		metric.WithDescription("WorkloadProfile objects created"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.workloadProfilesPurged, err = m.Int64Counter("ballast.workload_profiles.purged",
		metric.WithDescription("WorkloadProfile objects purged after orphan TTL expired"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.resizeApplied, err = m.Int64Counter("ballast.resize.applied",
		metric.WithDescription("In-place resize operations applied successfully"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.resizeFailed, err = m.Int64Counter("ballast.resize.failed",
		metric.WithDescription("In-place resize operations that failed"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.resizeSkipped, err = m.Int64Counter("ballast.resize.skipped",
		metric.WithDescription("Resize evaluations skipped before issuing a patch"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.webhookMutations, err = m.Int64Counter("ballast.webhook.mutations",
		metric.WithDescription("Pod admission webhook invocations and their outcomes"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
		return nil, err
	}

	if r.killSwitchTransitions, err = m.Int64Counter("ballast.kill_switch.transitions",
		metric.WithDescription("Kill switch state transitions (activated or deactivated)"),
	); err != nil { // coverage:ignore - OTel SDK never errors for valid instrument registration
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

// SampleCollected records one metric sample written to the store.
func (r *Recorder) SampleCollected(ctx context.Context, source, resource, container, tupleHash string) {
	if r == nil {
		return
	}
	r.samplesCollected.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source", source),
		attribute.String("resource", resource),
		attribute.String("container", container),
		attribute.String("profile", tupleHash),
	))
}

// FetchError records a FetchStats failure for a metrics source.
func (r *Recorder) FetchError(ctx context.Context, source, tupleHash string) {
	if r == nil {
		return
	}
	r.fetchErrors.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source", source),
		attribute.String("profile", tupleHash),
	))
}

// ProfileThresholdMet records when a WorkloadProfile transitions to meets-threshold.
func (r *Recorder) ProfileThresholdMet(ctx context.Context, profile, policy string) {
	if r == nil {
		return
	}
	r.profileThresholdMet.Add(ctx, 1, metric.WithAttributes(
		attribute.String("profile", profile),
		attribute.String("policy", policy),
	))
}

// PodProcessed records a pod being processed by the workload watcher.
// event is "created" or "deleted".
func (r *Recorder) PodProcessed(ctx context.Context, event, namespace, profile string) {
	if r == nil {
		return
	}
	r.podsProcessed.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event", event),
		attribute.String("namespace", namespace),
		attribute.String("profile", profile),
	))
}

// WorkloadProfileCreated records a new WorkloadProfile being created.
func (r *Recorder) WorkloadProfileCreated(ctx context.Context, profile string) {
	if r == nil {
		return
	}
	r.workloadProfilesCreated.Add(ctx, 1, metric.WithAttributes(
		attribute.String("profile", profile),
	))
}

// WorkloadProfilePurged records a WorkloadProfile being deleted after orphan TTL.
func (r *Recorder) WorkloadProfilePurged(ctx context.Context, profile string) {
	if r == nil {
		return
	}
	r.workloadProfilesPurged.Add(ctx, 1, metric.WithAttributes(
		attribute.String("profile", profile),
	))
}

// ResizeApplied records a successful in-place resize.
func (r *Recorder) ResizeApplied(ctx context.Context, profile, policy, namespace string) {
	if r == nil {
		return
	}
	r.resizeApplied.Add(ctx, 1, metric.WithAttributes(
		attribute.String("profile", profile),
		attribute.String("policy", policy),
		attribute.String("namespace", namespace),
	))
}

// ResizeFailed records a failed in-place resize.
func (r *Recorder) ResizeFailed(ctx context.Context, profile, policy, namespace string) {
	if r == nil {
		return
	}
	r.resizeFailed.Add(ctx, 1, metric.WithAttributes(
		attribute.String("profile", profile),
		attribute.String("policy", policy),
		attribute.String("namespace", namespace),
	))
}

// ResizeSkipped records a resize evaluation that was skipped without issuing a patch.
// reason is one of: cooldown, no_drift, kill_switch, not_ready, no_policy, dry_run.
func (r *Recorder) ResizeSkipped(ctx context.Context, reason, profile string) {
	if r == nil {
		return
	}
	r.resizeSkipped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
		attribute.String("profile", profile),
	))
}

// WebhookMutation records a pod admission webhook invocation.
// result is one of: kill_switch, skipped, not_available, dry_run, mutated.
func (r *Recorder) WebhookMutation(ctx context.Context, result, namespace, profile string) {
	if r == nil {
		return
	}
	r.webhookMutations.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result),
		attribute.String("namespace", namespace),
		attribute.String("profile", profile),
	))
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
