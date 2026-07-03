/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package resourceadjuster

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/logger"
	"github.com/tight-line/ballast/internal/metrics"
	"github.com/tight-line/ballast/internal/policy"
)

const (
	// AnnotationResizeBlocked carries the error text of the pod's most recent
	// failed resize (truncated to maxBlockedReasonLen), so "why is this pod not
	// being resized" is answerable from the pod itself after the failure Events
	// have expired. Prior to v0.3.11 the value was the literal "true".
	AnnotationResizeBlocked = "ballast.tightlinesoftware.com/resize-blocked"
	// AnnotationResizeBlockedAt is the RFC3339 time of the most recent failed
	// resize. While it is younger than the policy's resize interval the pod is
	// skipped (reason "blocked") instead of retrying a patch that just failed.
	// Both blocked annotations are removed by the next successful resize.
	AnnotationResizeBlockedAt = "ballast.tightlinesoftware.com/resize-blocked-at"
	AnnotationLastResize      = "ballast.tightlinesoftware.com/last-resize"

	// maxBlockedReasonLen caps the error text stored in AnnotationResizeBlocked.
	maxBlockedReasonLen = 256
)

// Reconciler watches WorkloadProfile objects and resizes pods in-place when
// resource drift exceeds the configured threshold.
type Reconciler struct {
	client       client.Client
	ks           *killswitch.KillSwitch
	resolver     *policy.Resolver
	dryRunResize bool
	rec          *metrics.Recorder
	// ResizePod issues the in-place resize patch. Defaults to calling the resize
	// subresource; overridable in tests without a real cluster.
	ResizePod func(ctx context.Context, pod *corev1.Pod, adjustments []ContainerAdjustment) error
}

// New creates a Reconciler.
func New(c client.Client, ks *killswitch.KillSwitch, dryRunResize bool, rec *metrics.Recorder) *Reconciler {
	r := &Reconciler{
		client:       c,
		ks:           ks,
		resolver:     policy.NewResolver(c, ctrl.Log.WithName("resourceadjuster")),
		dryRunResize: dryRunResize,
		rec:          rec,
	}
	r.ResizePod = r.applyResize
	return r
}

// Setup registers the Reconciler with mgr.
func Setup(mgr ctrl.Manager, ks *killswitch.KillSwitch, dryRunResize bool, rec *metrics.Recorder) error {
	return New(mgr.GetClient(), ks, dryRunResize, rec).SetupWithManager(mgr)
}

// SetupWithManager registers the Reconciler with mgr.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("resourceadjuster").
		WithLogConstructor(logger.ControllerLogConstructor(mgr.GetLogger(), "resourceadjuster")).
		For(&ballastv1.WorkloadProfile{}).
		Complete(r)
}

// Reconcile evaluates drift for each pod associated with the profile and issues
// in-place resize patches when drift exceeds the configured threshold.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, req.NamespacedName, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	pid := metrics.ProfileID{Name: profile.Name, Labels: profile.Status.TupleLabels}

	if r.ks.IsActive() {
		log.Info("kill switch active, skipping resize",
			"kill_switch", true, "kill_switch_reason", r.ks.Reason(), "profile", profile.Name)
		r.rec.ResizeSkipped(ctx, "kill_switch", pid, "", "")
		return ctrl.Result{RequeueAfter: ballastv1.DefaultResizeIntervalDuration}, nil
	}

	if !profile.Status.MeetsThreshold {
		log.V(1).Info("profile does not meet threshold, skipping resize", "profile", profile.Name)
		r.rec.ResizeSkipped(ctx, "not_ready", pid, "", "")
		return ctrl.Result{RequeueAfter: ballastv1.DefaultResizeIntervalDuration}, nil
	}

	resolved, err := r.resolver.Resolve(ctx, policy.Input{Labels: profile.Status.TupleLabels})
	if err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}
	if resolved == nil {
		log.Info("no policy matches profile, skipping resize", "profile", profile.Name)
		r.rec.ResizeSkipped(ctx, "no_policy", pid, "", "")
		return ctrl.Result{RequeueAfter: ballastv1.DefaultResizeIntervalDuration}, nil
	}

	interval := ParseResizeInterval(resolved.Spec.Behaviors.Resize)

	pods, err := r.listResizePods(ctx, profile.Name)
	if err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	for i := range pods {
		if err := r.reconcilePod(ctx, &pods[i], &profile, resolved.Spec.Behaviors, interval, resolved.Name); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

// listResizePods returns all pods that reference this profile and have the
// resize or autoresize annotation.
func (r *Reconciler) listResizePods(ctx context.Context, profileName string) ([]corev1.Pod, error) {
	var allPods corev1.PodList
	if err := r.client.List(ctx, &allPods); err != nil { // coverage:ignore - transient API error
		return nil, err
	}

	var result []corev1.Pod
	for _, pod := range allPods.Items {
		if pod.Annotations[workloadwatcher.AnnotationProfileRef] != profileName {
			continue
		}
		if !wantsResize(pod.Annotations) {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		result = append(result, pod)
	}
	return result, nil
}

// wantsResize reports whether a pod has opted into in-place resize.
func wantsResize(ann map[string]string) bool {
	return ann[workloadwatcher.AnnotationResize] == "true" ||
		ann[workloadwatcher.AnnotationAutoresize] == "true"
}

// reconcilePod evaluates drift for one pod and issues a resize if needed.
func (r *Reconciler) reconcilePod(ctx context.Context, pod *corev1.Pod, profile *ballastv1.WorkloadProfile, behaviors ballastv1.BehaviorConfig, interval time.Duration, policyName string) error {
	log := ctrl.LoggerFrom(ctx)

	pid := metrics.ProfileID{Name: profile.Name, Labels: profile.Status.TupleLabels}

	if at, ok := pod.Annotations[AnnotationResizeBlockedAt]; ok {
		if t, err := time.Parse(time.RFC3339, at); err == nil && time.Since(t) < interval {
			log.V(1).Info("resize recently failed, backing off",
				"pod", pod.Name, "namespace", pod.Namespace,
				"blocked_reason", pod.Annotations[AnnotationResizeBlocked],
				"next_attempt", t.Add(interval))
			r.rec.ResizeSkipped(ctx, "blocked", pid, policyName, pod.Namespace)
			return nil
		}
	}

	if last, ok := pod.Annotations[AnnotationLastResize]; ok {
		if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < interval {
			log.V(1).Info("resize cooldown active, skipping", "pod", pod.Name, "namespace", pod.Namespace, "next_resize", t.Add(interval))
			r.rec.ResizeSkipped(ctx, "cooldown", pid, policyName, pod.Namespace)
			return nil
		}
	}

	recsByName := containerRecsByName(profile)
	adjustments, notResizable := computeAdjustments(pod, recsByName, behaviors)
	if len(notResizable) > 0 {
		log.Info("excluding drifted resources the resize subresource cannot mutate",
			"pod", pod.Name, "namespace", pod.Namespace, "resources", notResizable)
	}
	if len(adjustments) == 0 {
		// The skip reason describes the whole pod: not_resizable when the only
		// actionable drift was on resources in-place resize cannot touch.
		reason := "no_drift"
		if len(notResizable) > 0 {
			reason = "not_resizable"
		}
		r.rec.ResizeSkipped(ctx, reason, pid, policyName, pod.Namespace)
		return nil
	}

	logFields := []any{"profile", profile.Name, "pod", pod.Name, "namespace", pod.Namespace}

	// The resize subresource may not change the pod's QoS class (fixed at
	// creation). Detect that before patching: attempting it would fail every
	// evaluation forever, since the class can only change on pod recreation.
	all := append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...)
	adjusted := append(slices.Clone(pod.Spec.InitContainers), adjustedContainers(pod.Spec.Containers, adjustments)...)
	if cur, next := PodQOS(all), PodQOS(adjusted); cur != next {
		log.Info("resize would change pod QoS class, which Kubernetes forbids; skipping",
			append(logFields, "qos_current", string(cur), "qos_after", string(next))...)
		r.rec.ResizeSkipped(ctx, "qos_pinned", pid, policyName, pod.Namespace)
		return nil
	}

	if r.dryRunResize {
		log.Info("dry-run: would resize pod", append(logFields, "dry_run", true)...)
		r.rec.ResizeSkipped(ctx, "dry_run", pid, policyName, pod.Namespace)
		return nil
	}

	if err := r.ResizePod(ctx, pod, adjustments); err != nil {
		log.Error(err, "resize failed, marking pod blocked", logFields...)
		r.rec.ResizeFailed(ctx, pid, policyName, pod.Namespace)
		r.emitEvent(ctx, pod, corev1.EventTypeWarning, "ResizeBlocked",
			fmt.Sprintf("in-place resize failed: %v", err))
		return r.patchPodAnnotations(ctx, pod, map[string]string{
			AnnotationResizeBlocked:   truncate(err.Error(), maxBlockedReasonLen),
			AnnotationResizeBlockedAt: metav1.Now().UTC().Format(time.RFC3339),
		}, nil)
	}

	log.Info("resize applied", logFields...)
	r.rec.ResizeApplied(ctx, pid, policyName, pod.Namespace)
	r.emitEvent(ctx, pod, corev1.EventTypeNormal, "Resized", "in-place resize applied by Ballast")
	return r.patchPodAnnotations(ctx, pod,
		map[string]string{AnnotationLastResize: metav1.Now().UTC().Format(time.RFC3339)},
		[]string{AnnotationResizeBlocked, AnnotationResizeBlockedAt})
}

// containerRecsByName indexes profile recommendations by container name.
func containerRecsByName(profile *ballastv1.WorkloadProfile) map[string]map[string]ballastv1.ResourceRecommendation {
	m := make(map[string]map[string]ballastv1.ResourceRecommendation)
	for _, cp := range profile.Status.Containers {
		m[cp.Name] = cp.Recommendations
	}
	return m
}

// ContainerAdjustment captures the target resources for one container after
// drift detection and maxChangePerCycle capping.
type ContainerAdjustment struct {
	Name     string
	Requests corev1.ResourceList
	Limits   corev1.ResourceList
}

// computeAdjustments returns one adjustment per container that has drifted
// beyond threshold for at least one resizable field, plus the sorted, deduplicated
// names of drifted resources that were excluded because the resize subresource
// cannot mutate them.
func computeAdjustments(
	pod *corev1.Pod,
	recsByName map[string]map[string]ballastv1.ResourceRecommendation,
	behaviors ballastv1.BehaviorConfig,
) (result []ContainerAdjustment, notResizable []string) {
	maxChange := ResolveMaxChangePercent(behaviors)

	for _, c := range pod.Spec.Containers {
		recs, ok := recsByName[c.Name]
		if !ok || len(recs) == 0 {
			continue
		}
		adj, drifted, skipped := computeContainerAdjustment(c, recs, behaviors, maxChange)
		if drifted {
			result = append(result, adj)
		}
		notResizable = append(notResizable, skipped...)
	}

	slices.Sort(notResizable)
	return result, slices.Compact(notResizable)
}

// computeContainerAdjustment evaluates every recommended field for one container
// and returns the capped adjustment, whether any resizable field drifted beyond
// threshold, and the drifted resources that were excluded as not resizable.
// Recommendations for resources the resize subresource cannot mutate (anything
// other than cpu and memory) are applied only at admission time by the webhook.
func computeContainerAdjustment(
	c corev1.Container,
	recs map[string]ballastv1.ResourceRecommendation,
	behaviors ballastv1.BehaviorConfig,
	maxChange float64,
) (adj ContainerAdjustment, drifted bool, notResizable []string) {
	adj = ContainerAdjustment{
		Name:     c.Name,
		Requests: c.Resources.Requests.DeepCopy(),
		Limits:   c.Resources.Limits.DeepCopy(),
	}
	if adj.Requests == nil {
		adj.Requests = make(corev1.ResourceList)
	}
	if adj.Limits == nil {
		adj.Limits = make(corev1.ResourceList)
	}

	for res, rec := range recs {
		resName := corev1.ResourceName(res)
		if !resizableResource(resName) {
			if fieldDrifts(rec.Request, ResolveFieldThreshold(behaviors, res, "request"),
				currentValue(c.Resources.Requests, resName)) ||
				fieldDrifts(rec.Limit, ResolveFieldThreshold(behaviors, res, "limit"),
					currentValue(c.Resources.Limits, resName)) {
				notResizable = append(notResizable, res)
			}
			continue
		}
		if evaluateField(adj.Requests, resName, rec.Request,
			ResolveFieldThreshold(behaviors, res, "request"), maxChange,
			currentValue(c.Resources.Requests, resName)) {
			drifted = true
		}
		if evaluateField(adj.Limits, resName, rec.Limit,
			ResolveFieldThreshold(behaviors, res, "limit"), maxChange,
			currentValue(c.Resources.Limits, resName)) {
			drifted = true
		}
	}
	return adj, drifted, notResizable
}

// resizableResource reports whether the pod resize subresource can mutate the
// given resource. Kubernetes in-place resize (KEP-1287) allows only cpu and
// memory; a patch touching any other resource is rejected by the API server
// with "only cpu and memory resources are mutable".
func resizableResource(res corev1.ResourceName) bool {
	return res == corev1.ResourceCPU || res == corev1.ResourceMemory
}

// fieldDrifts reports whether a recommended value parses and drifts beyond
// threshold relative to current, without recording an adjustment. An empty
// recValue fails to parse and is treated as "no recommendation".
func fieldDrifts(recValue string, threshold float64, current resource.Quantity) bool {
	recommended, err := resource.ParseQuantity(recValue)
	return err == nil && ExceedsDrift(current, recommended, threshold)
}

// evaluateField records the capped target for one field in dst when the recommended
// value parses and drifts beyond threshold; it returns whether the field drifted.
// An empty recValue parses to an error and is treated as "no recommendation".
func evaluateField(
	dst corev1.ResourceList,
	resName corev1.ResourceName,
	recValue string,
	threshold, maxChange float64,
	current resource.Quantity,
) bool {
	recommended, err := resource.ParseQuantity(recValue)
	if err != nil || !ExceedsDrift(current, recommended, threshold) {
		return false
	}
	dst[resName] = CapChange(current, recommended, maxChange, threshold)
	return true
}

// resolveFieldThreshold looks up a per-field threshold using the coalesce order:
// resourceThresholds[res][field] -> resize.default -> thresholds.default
func ResolveFieldThreshold(behaviors ballastv1.BehaviorConfig, res, field string) float64 {
	if rt := behaviors.Thresholds.Resize.ResourceThresholds; rt != nil {
		if ft, ok := rt[res]; ok {
			var s string
			if field == "request" {
				s = ft.Request
			} else {
				s = ft.Limit
			}
			if v := parsePercent(s); v > 0 {
				return v
			}
		}
	}
	if v := parsePercent(behaviors.Thresholds.Resize.Default); v > 0 {
		return v
	}
	if v := parsePercent(behaviors.Thresholds.Default); v > 0 {
		return v
	}
	return ballastv1.DefaultThresholdPercent
}

// ResolveMaxChangePercent parses the maxChangePerCycle value from the policy.
func ResolveMaxChangePercent(behaviors ballastv1.BehaviorConfig) float64 {
	if v := parsePercent(behaviors.Resize.MaxChangePerCycle); v > 0 {
		return v
	}
	return ballastv1.DefaultMaxChangePercent
}

// exceedsDrift returns true when |recommended - current| / current > threshold%.
// If current is zero and recommended is nonzero, drift is treated as infinite (always resize).
func ExceedsDrift(current, recommended resource.Quantity, thresholdPct float64) bool {
	return exceedsDriftFloat(current.AsApproximateFloat64(), recommended.AsApproximateFloat64(), thresholdPct)
}

// exceedsDriftFloat is ExceedsDrift on raw float values.
func exceedsDriftFloat(cur, rec, thresholdPct float64) bool {
	if cur == 0 {
		return rec > 0
	}
	drift := (rec - cur) / cur
	if drift < 0 {
		drift = -drift
	}
	return drift > thresholdPct/100.0
}

// CapChange applies maxChangePerCycle: a single cycle moves at most
// maxChangePct% of the gap between current and recommended. When the capped
// step would land within thresholdPct of the recommendation (so the next
// cycle's drift check would not fire), the recommendation is applied exactly
// instead of parking the value just inside the drift band forever.
func CapChange(current, recommended resource.Quantity, maxChangePct, thresholdPct float64) resource.Quantity {
	cur := current.AsApproximateFloat64()
	rec := recommended.AsApproximateFloat64()

	if cur == 0 {
		return recommended
	}

	gap := rec - cur
	if gap < 0 {
		gap = -gap
	}
	step := gap * (maxChangePct / 100.0)

	var capped float64
	if rec > cur {
		capped = cur + step
	} else {
		capped = cur - step
	}
	if !exceedsDriftFloat(capped, rec, thresholdPct) {
		return recommended
	}

	if strings.HasSuffix(recommended.String(), "m") {
		return *resource.NewMilliQuantity(int64(capped*1000), resource.DecimalSI)
	}
	return *resource.NewQuantity(int64(capped), resource.BinarySI)
}

// currentValue returns the current value for a resource from a ResourceList,
// returning zero if absent.
func currentValue(list corev1.ResourceList, name corev1.ResourceName) resource.Quantity {
	if v, ok := list[name]; ok {
		return v
	}
	return resource.MustParse("0")
}

// adjustedContainers returns a deep copy of containers with adjustments merged
// into the matching containers' requests and limits.
func adjustedContainers(containers []corev1.Container, adjustments []ContainerAdjustment) []corev1.Container {
	out := make([]corev1.Container, len(containers))
	for i := range containers {
		out[i] = *containers[i].DeepCopy()
		for _, adj := range adjustments {
			if out[i].Name != adj.Name {
				continue
			}
			if out[i].Resources.Requests == nil {
				out[i].Resources.Requests = make(corev1.ResourceList)
			}
			if out[i].Resources.Limits == nil {
				out[i].Resources.Limits = make(corev1.ResourceList)
			}
			maps.Copy(out[i].Resources.Requests, adj.Requests)
			maps.Copy(out[i].Resources.Limits, adj.Limits)
		}
	}
	return out
}

// PodQOS computes the QoS class Kubernetes assigns to a pod built from the
// given containers (pass regular and init containers together), following the
// upstream GetPodQOS algorithm over cpu and memory, the only QoS-relevant
// resources: BestEffort when no container sets any cpu/memory request or
// limit, Guaranteed when every container sets both cpu and memory limits and
// aggregate requests equal aggregate limits, Burstable otherwise.
func PodQOS(containers []corev1.Container) corev1.PodQOSClass {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	isGuaranteed := true
	for _, c := range containers {
		for name, q := range c.Resources.Requests {
			if !resizableResource(name) || q.IsZero() {
				continue
			}
			addQuantity(requests, name, q)
		}
		qosLimits := 0
		for name, q := range c.Resources.Limits {
			if !resizableResource(name) || q.IsZero() {
				continue
			}
			qosLimits++
			addQuantity(limits, name, q)
		}
		if qosLimits != 2 { // both cpu and memory
			isGuaranteed = false
		}
	}
	if len(requests) == 0 && len(limits) == 0 {
		return corev1.PodQOSBestEffort
	}
	if isGuaranteed {
		for name, req := range requests {
			if lim, ok := limits[name]; !ok || lim.Cmp(req) != 0 {
				isGuaranteed = false
				break
			}
		}
	}
	if isGuaranteed && len(requests) == len(limits) {
		return corev1.PodQOSGuaranteed
	}
	return corev1.PodQOSBurstable
}

// addQuantity adds q to the running total for name in list.
func addQuantity(list corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	total := q.DeepCopy()
	if cur, ok := list[name]; ok {
		total.Add(cur)
	}
	list[name] = total
}

// applyResize patches the pod via the resize subresource.
func (r *Reconciler) applyResize(ctx context.Context, pod *corev1.Pod, adjustments []ContainerAdjustment) error {
	modified := pod.DeepCopy()
	modified.Spec.Containers = adjustedContainers(pod.Spec.Containers, adjustments)
	return r.client.SubResource("resize").Patch(ctx, modified, client.MergeFrom(pod))
}

// patchPodAnnotations patches pod annotations: entries in set are added or
// replaced, keys in remove are deleted.
func (r *Reconciler) patchPodAnnotations(ctx context.Context, pod *corev1.Pod, set map[string]string, remove []string) error {
	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	maps.Copy(pod.Annotations, set)
	for _, k := range remove {
		delete(pod.Annotations, k)
	}
	return r.client.Patch(ctx, pod, client.MergeFrom(base))
}

// truncate shortens s to at most n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// emitEvent creates a Kubernetes Event on the pod.
func (r *Reconciler) emitEvent(ctx context.Context, pod *corev1.Pod, eventType, reason, message string) {
	log := ctrl.LoggerFrom(ctx)
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%s.%d", pod.Name, strings.ToLower(reason), now.UnixNano()),
			Namespace: pod.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       pod.Name,
			Namespace:  pod.Namespace,
			UID:        pod.UID,
		},
		Type:           eventType,
		Reason:         reason,
		Message:        message,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source:         corev1.EventSource{Component: "ballast"},
	}
	if err := r.client.Create(ctx, event); err != nil { // coverage:ignore - non-fatal; requires a broken client to trigger
		log.Error(err, "failed to emit event", "reason", reason)
	}
}

// parseResizeInterval parses the resize interval from the policy, falling back to the default.
func ParseResizeInterval(resize ballastv1.ResizeConfig) time.Duration {
	if d, err := time.ParseDuration(resize.Interval); err == nil && d > 0 {
		return d
	}
	return ballastv1.DefaultResizeIntervalDuration
}

// parsePercent parses a "20%" style string and returns the float64 value (20.0).
func parsePercent(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}
