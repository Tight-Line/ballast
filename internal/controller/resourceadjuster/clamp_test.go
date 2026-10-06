/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package resourceadjuster_test

import (
	"context"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/resourceadjuster"
	"github.com/tight-line/ballast/internal/metrics"
)

// newMetricsRecorder returns a Recorder backed by a Prometheus registry so tests
// can assert on the series the adjuster records.
func newMetricsRecorder(t *testing.T) (*metrics.Recorder, *promclient.Registry) {
	t.Helper()
	reg := promclient.NewRegistry()
	exp, err := promexporter.New(promexporter.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("creating prometheus exporter: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	rec, err := metrics.NewRecorder(provider)
	if err != nil {
		t.Fatalf("creating recorder: %v", err)
	}
	return rec, reg
}

// counterSeries returns the value and labels of the named counter's first series,
// or (0, nil) when the metric has no series.
func counterSeries(t *testing.T, reg *promclient.Registry, name string) (value float64, labels map[string]string) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			return m.GetCounter().GetValue(), labels
		}
	}
	return 0, nil
}

// resourcesPod returns a resize-enrolled pod whose "app" container carries the
// given requests and limits.
func resourcesPod(requests, limits corev1.ResourceList) *corev1.Pod {
	pod := resizePod("100m", "100m")
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{Requests: requests, Limits: limits}
	return pod
}

// rl builds a ResourceList from resource/quantity pairs.
func rl(pairs ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		out[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return out
}

// clampOutcome is what a single reconcile of a clamp scenario produced.
type clampOutcome struct {
	adjustments []resourceadjuster.ContainerAdjustment
	resized     bool
	reg         *promclient.Registry
	events      []corev1.Event
}

// reconcileClamp reconciles one pod against recs and captures the outcome.
func reconcileClamp(t *testing.T, pod *corev1.Pod, recs map[string]ballastv1.ResourceRecommendation) clampOutcome {
	t.Helper()
	return reconcileClampUnder(t, noResizePolicy(), pod, recs)
}

// tightPolicy is noResizePolicy with a 10% resize threshold, so capped 50%
// steps land outside the drift band instead of snapping to the target.
func tightPolicy() *ballastv1.ClusterResourcePolicy {
	p := noResizePolicy()
	p.Spec.Behaviors.Thresholds.Default = "10%"
	p.Spec.Behaviors.Thresholds.Resize.Default = "10%"
	return p
}

// reconcileClampUnder is reconcileClamp under the given policy.
func reconcileClampUnder(t *testing.T, policy *ballastv1.ClusterResourcePolicy, pod *corev1.Pod, recs map[string]ballastv1.ResourceRecommendation) clampOutcome {
	t.Helper()
	profile := readyProfileWithRecs(recs)
	fc := newFakeClient(profile, policy, pod)
	rec, reg := newMetricsRecorder(t)
	r := resourceadjuster.New(fc, inactiveKS(t), false, rec)
	out := clampOutcome{reg: reg}
	r.ResizePod = func(_ context.Context, _ *corev1.Pod, adjs []resourceadjuster.ContainerAdjustment) error {
		out.resized = true
		out.adjustments = adjs
		return nil
	}
	if _, err := doReconcile(t, r, profile.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var events corev1.EventList
	if err := fc.List(context.Background(), &events); err != nil {
		t.Fatalf("listing events: %v", err)
	}
	out.events = events.Items
	return out
}

// request returns the adjusted request for res on the "app" container.
func (o clampOutcome) request(t *testing.T, res corev1.ResourceName) resource.Quantity {
	t.Helper()
	if !o.resized || len(o.adjustments) != 1 {
		t.Fatalf("expected one resize adjustment, got resized=%v %+v", o.resized, o.adjustments)
	}
	return o.adjustments[0].Requests[res]
}

func (o clampOutcome) limit(t *testing.T, res corev1.ResourceName) resource.Quantity {
	t.Helper()
	if !o.resized || len(o.adjustments) != 1 {
		t.Fatalf("expected one resize adjustment, got resized=%v %+v", o.resized, o.adjustments)
	}
	return o.adjustments[0].Limits[res]
}

func (o clampOutcome) hasClampEvent() bool {
	for _, e := range o.events {
		if e.Reason == "RequestClampedToLimit" && e.Type == corev1.EventTypeWarning {
			return true
		}
	}
	return false
}

func assertQuantity(t *testing.T, what string, got resource.Quantity, want string) {
	t.Helper()
	if w := resource.MustParse(want); got.Cmp(w) != 0 {
		t.Errorf("%s = %s, want %s", what, got.String(), want)
	}
}

// assertClamped asserts the clamp was recorded as a metric and a Warning event.
func assertClamped(t *testing.T, o clampOutcome, res string) {
	t.Helper()
	got, labels := counterSeries(t, o.reg, "ballast_recommendation_clamped_total")
	if got != 1 {
		t.Fatalf("ballast_recommendation_clamped_total = %v, want 1", got)
	}
	if labels["container"] != "app" || labels["resource"] != res || labels["phase"] != "resize" ||
		labels["policy"] != "test-policy" || labels["namespace"] != "default" {
		t.Errorf("clamp attrs = %v", labels)
	}
	if !o.hasClampEvent() {
		t.Errorf("expected a RequestClampedToLimit Warning event, got %+v", o.events)
	}
}

func assertNotClamped(t *testing.T, o clampOutcome) {
	t.Helper()
	if got, _ := counterSeries(t, o.reg, "ballast_recommendation_clamped_total"); got != 0 {
		t.Errorf("ballast_recommendation_clamped_total = %v, want 0", got)
	}
	if o.hasClampEvent() {
		t.Error("unexpected RequestClampedToLimit event")
	}
}

func TestReconcile_RequestsOnlyMemoryOverLimit_ClampedToLimit(t *testing.T) {
	// The #119 shape: a requests-only memory recommendation above the
	// container's 128Mi limit. The request is held at the limit, which is left
	// alone. No cpu limit, so the pod stays Burstable at exactly the limit.
	pod := resourcesPod(rl("cpu", "100m", "memory", "100Mi"), rl("memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "149Mi"},
	})
	assertQuantity(t, "memory request", o.request(t, corev1.ResourceMemory), "128Mi")
	assertQuantity(t, "memory limit", o.limit(t, corev1.ResourceMemory), "128Mi")
	assertClamped(t, o, "memory")
}

func TestReconcile_RequestsOnlyCPUOverLimit_ClampedToLimit(t *testing.T) {
	// From 150m, a 50% step toward the clamped 200m target lands within
	// threshold of it, so the request goes straight to the limit.
	pod := resourcesPod(rl("cpu", "150m"), rl("cpu", "200m"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m"},
	})
	assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), "200m")
	assertQuantity(t, "cpu limit", o.limit(t, corev1.ResourceCPU), "200m")
	assertClamped(t, o, "cpu")
}

func TestReconcile_BothFieldsManaged_ComparedToNewLimit(t *testing.T) {
	// The limit moves 200m -> 300m in this cycle, so the request is bounded by
	// 300m, not the old 200m.
	pod := resourcesPod(rl("cpu", "200m"), rl("cpu", "200m"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m", Limit: "300m"},
	})
	assertQuantity(t, "cpu limit", o.limit(t, corev1.ResourceCPU), "300m")
	assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), "300m")
	assertClamped(t, o, "cpu")
}

func TestReconcile_NoLimit_RequestNotClamped(t *testing.T) {
	pod := resourcesPod(rl("cpu", "100m"), nil)
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m"},
	})
	// 50% of the 100m -> 500m gap.
	assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), "300m")
	assertNotClamped(t, o)
}

func TestReconcile_ClampWouldPromoteToGuaranteed_StepsDownOneUnit(t *testing.T) {
	// cpu is already at its limit; memory is the last unequal pair. Clamping
	// memory to exactly 128Mi would make the pod Guaranteed, which in-place
	// resize forbids, so the request lands one byte under the limit.
	pod := resourcesPod(rl("cpu", "100m", "memory", "100Mi"), rl("cpu", "100m", "memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "149Mi"},
	})
	assertQuantity(t, "memory request", o.request(t, corev1.ResourceMemory), "134217727")
	assertQuantity(t, "memory limit", o.limit(t, corev1.ResourceMemory), "128Mi")
	assertClamped(t, o, "memory")
}

func TestReconcile_CPUClampWouldPromoteToGuaranteed_StepsDownOneMillicore(t *testing.T) {
	pod := resourcesPod(rl("cpu", "150m", "memory", "128Mi"), rl("cpu", "200m", "memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m"},
	})
	assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), "199m")
	assertClamped(t, o, "cpu")
}

// assertSkipped asserts the reconcile issued no resize and recorded exactly one
// ballast.resize.skipped with the given reason.
func assertSkipped(t *testing.T, o clampOutcome, reason string) {
	t.Helper()
	if o.resized {
		t.Errorf("expected no resize, got %+v", o.adjustments)
	}
	got, labels := counterSeries(t, o.reg, "ballast_resize_skipped_total")
	if got != 1 || labels["reason"] != reason {
		t.Errorf("ballast_resize_skipped_total = %v (reason=%q), want 1 with reason=%s", got, labels["reason"], reason)
	}
}

func TestReconcile_GuaranteedPod_RecommendationAboveLimit_ClampedAtLimit(t *testing.T) {
	// A Guaranteed pod whose recommendation exceeds its limit clamps to exactly
	// the limit, which is where its request already sits: no resize, it stays
	// Guaranteed, and the skip says the limit is what holds it back.
	o := reconcileClamp(t, guaranteedPod(), map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "149Mi"},
	})
	assertSkipped(t, o, "clamped_at_limit")
	assertNotClamped(t, o)
}

func TestReconcile_AlreadyClampedPod_ClampedAtLimit(t *testing.T) {
	// A Burstable pod already held one byte under its memory limit by an earlier
	// clamp does not resize every interval; it reports the steady state as
	// clamped_at_limit, with no new clamp counted or event emitted.
	pod := resourcesPod(rl("cpu", "100m", "memory", "134217727"), rl("cpu", "100m", "memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "149Mi"},
	})
	assertSkipped(t, o, "clamped_at_limit")
	assertNotClamped(t, o)
}

func TestReconcile_NoDriftWithinLimit_NoDrift(t *testing.T) {
	// A recommendation under the limit that the request already matches is
	// plain no_drift, not clamped_at_limit.
	pod := resourcesPod(rl("memory", "100Mi"), rl("memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "100Mi"},
	})
	assertSkipped(t, o, "no_drift")
}

func TestReconcile_DriftAlongsideClampedAtLimit_Resized(t *testing.T) {
	// cpu drifts and is resized; memory is already held at its limit. The pod's
	// outcome is the resize, and the held memory request is left alone.
	pod := resourcesPod(rl("cpu", "100m", "memory", "128Mi"), rl("memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"cpu":    {Request: "500m"},
		"memory": {Request: "149Mi"},
	})
	assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), "300m")
	assertQuantity(t, "memory request", o.request(t, corev1.ResourceMemory), "128Mi")
	if got, _ := counterSeries(t, o.reg, "ballast_resize_skipped_total"); got != 0 {
		t.Errorf("ballast_resize_skipped_total = %v, want 0 when the pod is resized", got)
	}
	assertNotClamped(t, o)
}

func TestReconcile_NotResizableOutranksClampedAtLimit(t *testing.T) {
	// The only drift is on ephemeral-storage, which cannot be resized in place,
	// while memory is held at its limit. Pending drift outranks the steady
	// state, so the skip is not_resizable.
	pod := resourcesPod(rl("memory", "128Mi", "ephemeral-storage", "5Mi"), rl("memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"memory":            {Request: "149Mi"},
		"ephemeral-storage": {Request: "50Mi"},
	})
	assertSkipped(t, o, "not_resizable")
}

func TestReconcile_LimitLoweredBelowCurrentRequest_RequestHeldAtLimit(t *testing.T) {
	// A limit-only recommendation lowers the limit (256Mi -> 178Mi capped)
	// below the current 200Mi request. The request is held at the new limit
	// so the patch stays valid.
	pod := resourcesPod(rl("memory", "200Mi"), rl("memory", "256Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"memory": {Limit: "100Mi"},
	})
	lim := o.limit(t, corev1.ResourceMemory)
	if req := o.request(t, corev1.ResourceMemory); req.Cmp(lim) != 0 {
		t.Errorf("memory request = %s, want it held at the new limit %s", req.String(), lim.String())
	}
	assertClamped(t, o, "memory")
}

func TestReconcile_RecommendationAboveLoweredLimit_RequestHeldAtLimit(t *testing.T) {
	// The request recommendation (190Mi) exceeds the lowered limit, and the
	// clamped target is within threshold of the current 200Mi request, so only
	// the final bound moves the request down to the limit.
	pod := resourcesPod(rl("memory", "200Mi"), rl("memory", "256Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "190Mi", Limit: "100Mi"},
	})
	lim := o.limit(t, corev1.ResourceMemory)
	if req := o.request(t, corev1.ResourceMemory); req.Cmp(lim) != 0 {
		t.Errorf("memory request = %s, want it held at the new limit %s", req.String(), lim.String())
	}
	assertClamped(t, o, "memory")
}

func TestReconcile_ClampReport_CarriesRecommendation(t *testing.T) {
	// The event message names the recommended request, the limit, and the
	// request actually written: here a capped step toward the clamped target,
	// not the limit itself.
	pod := resourcesPod(rl("cpu", "100m"), rl("cpu", "200m"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m"},
	})
	want := "container app: recommended cpu request 500m exceeds its limit 200m; request set to 150m, held at or below the limit"
	for _, e := range o.events {
		if e.Reason == "RequestClampedToLimit" && e.Message == want {
			return
		}
	}
	t.Errorf("no RequestClampedToLimit event with message %q in %+v", want, o.events)
}

func TestReconcile_WholeCoreLimits_CappedStepsInMillicores(t *testing.T) {
	// Limits (and recommendations) written in whole cores print without an
	// "m" suffix. Capped steps must still be computed in millicores, not
	// truncated to whole cores.
	cases := []struct {
		name                string
		request, limit, rec string
		wantRequest         string
	}{
		// 600m toward the 1-core limit: 50% of the gap is 800m (was truncated to 0).
		{"600m toward a 1-core limit", "600m", "1", "1500m", "800m"},
		// 1500m toward the 2-core limit: 1750m (was truncated down to 1).
		{"1500m toward a 2-core limit", "1500m", "2", "3", "1750m"},
		// 1 core toward the 2-core limit: 1500m (was truncated back to 1, a
		// no-op resize every interval).
		{"1 core toward a 2-core limit", "1", "2", "3", "1500m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := resourcesPod(rl("cpu", tc.request), rl("cpu", tc.limit))
			o := reconcileClampUnder(t, tightPolicy(), pod, map[string]ballastv1.ResourceRecommendation{
				"cpu": {Request: tc.rec},
			})
			assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), tc.wantRequest)
			assertQuantity(t, "cpu limit", o.limit(t, corev1.ResourceCPU), tc.limit)
			assertClamped(t, o, "cpu")
		})
	}
}

func TestReconcile_WholeCoreRecommendation_NoLimit_CappedInMillicores(t *testing.T) {
	// "2000m" parses and prints as "2"; the capped step is still 1300m.
	pod := resourcesPod(rl("cpu", "600m"), nil)
	o := reconcileClampUnder(t, tightPolicy(), pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "2000m"},
	})
	assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), "1300m")
	assertNotClamped(t, o)
}

func TestReconcile_CappedStepRoundsToCurrent_NoResize(t *testing.T) {
	// At 3m with a 2m recommendation, half the gap is 2.5m, which rounds back
	// to 3m for both the request and the limit. That is not a change, so no
	// no-op resize is issued.
	pod := resourcesPod(rl("cpu", "3m"), rl("cpu", "3m"))
	o := reconcileClampUnder(t, tightPolicy(), pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "2m", Limit: "2m"},
	})
	assertSkipped(t, o, "no_drift")
}

func TestReconcile_RecommendationLandsOnLimit_StepsDownToStayBurstable(t *testing.T) {
	// memory already sits at its limit (an earlier clamp); the cpu
	// recommendation lands exactly on the cpu limit. That is not a clamp, but it
	// would promote the Burstable pod to Guaranteed, which in-place resize
	// forbids, so the cpu request goes one millicore under instead of the pod
	// being qos_pinned every interval. Memory, which this resize does not
	// move, is left alone.
	pod := resourcesPod(rl("cpu", "150m", "memory", "128Mi"), rl("cpu", "200m", "memory", "128Mi"))
	o := reconcileClamp(t, pod, map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "200m"},
	})
	assertQuantity(t, "cpu request", o.request(t, corev1.ResourceCPU), "199m")
	assertQuantity(t, "memory request", o.request(t, corev1.ResourceMemory), "128Mi")
	assertNotClamped(t, o)
}
