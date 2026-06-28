/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package resourceadjuster

import (
	"context"
	"fmt"
	"maps"
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
	"github.com/tight-line/ballast/internal/policy"
)

const (
	AnnotationResizeBlocked = "ballast.tightlinesoftware.com/resize-blocked"
	AnnotationLastResize    = "ballast.tightlinesoftware.com/last-resize"

	defaultResizeInterval   = 15 * time.Minute
	defaultThresholdPercent = 20.0
	defaultMaxChangePercent = 50.0
)

// Reconciler watches WorkloadProfile objects and resizes pods in-place when
// resource drift exceeds the configured threshold.
type Reconciler struct {
	client       client.Client
	ks           *killswitch.KillSwitch
	resolver     *policy.Resolver
	dryRunResize bool
	// ResizePod issues the in-place resize patch. Defaults to calling the resize
	// subresource; overridable in tests without a real cluster.
	ResizePod func(ctx context.Context, pod *corev1.Pod, adjustments []ContainerAdjustment) error
}

// New creates a Reconciler.
func New(c client.Client, ks *killswitch.KillSwitch, dryRunResize bool) *Reconciler {
	r := &Reconciler{
		client:       c,
		ks:           ks,
		resolver:     policy.NewResolver(c, ctrl.Log.WithName("resourceadjuster")),
		dryRunResize: dryRunResize,
	}
	r.ResizePod = r.applyResize
	return r
}

// Setup registers the Reconciler with mgr.
func Setup(mgr ctrl.Manager, ks *killswitch.KillSwitch, dryRunResize bool) error {
	return New(mgr.GetClient(), ks, dryRunResize).SetupWithManager(mgr)
}

// SetupWithManager registers the Reconciler with mgr.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("resourceadjuster").
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

	if r.ks.IsActive() {
		log.Info("kill switch active, skipping resize",
			"kill_switch", true, "kill_switch_reason", r.ks.Reason(), "profile", profile.Name)
		return ctrl.Result{RequeueAfter: defaultResizeInterval}, nil
	}

	if !profile.Status.MeetsThreshold {
		log.Info("profile does not meet threshold, skipping resize", "profile", profile.Name)
		return ctrl.Result{RequeueAfter: defaultResizeInterval}, nil
	}

	resolved, err := r.resolver.Resolve(ctx, policy.Input{Labels: profile.Status.TupleLabels})
	if err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}
	if resolved == nil {
		log.Info("no policy matches profile, skipping resize", "profile", profile.Name)
		return ctrl.Result{RequeueAfter: defaultResizeInterval}, nil
	}

	interval := ParseResizeInterval(resolved.Spec.Behaviors.Resize)

	pods, err := r.listResizePods(ctx, profile.Name)
	if err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	for i := range pods {
		if err := r.reconcilePod(ctx, &pods[i], &profile, resolved.Spec.Behaviors); err != nil { // coverage:ignore - transient API error
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
func (r *Reconciler) reconcilePod(ctx context.Context, pod *corev1.Pod, profile *ballastv1.WorkloadProfile, behaviors ballastv1.BehaviorConfig) error {
	log := ctrl.LoggerFrom(ctx)

	recsByName := containerRecsByName(profile)
	adjustments := computeAdjustments(pod, recsByName, behaviors)
	if len(adjustments) == 0 {
		return nil
	}

	logFields := []any{"profile", profile.Name, "pod", pod.Name, "namespace", pod.Namespace}

	if r.dryRunResize {
		log.Info("dry-run: would resize pod", append(logFields, "dry_run", true)...)
		return nil
	}

	if err := r.ResizePod(ctx, pod, adjustments); err != nil {
		log.Error(err, "resize failed, marking pod blocked", logFields...)
		r.emitEvent(ctx, pod, corev1.EventTypeWarning, "ResizeBlocked",
			fmt.Sprintf("in-place resize failed: %v", err))
		return r.stampPodAnnotation(ctx, pod, AnnotationResizeBlocked, "true")
	}

	log.Info("resize applied", logFields...)
	r.emitEvent(ctx, pod, corev1.EventTypeNormal, "Resized", "in-place resize applied by Ballast")
	return r.stampPodAnnotation(ctx, pod, AnnotationLastResize, metav1.Now().UTC().Format(time.RFC3339))
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
// beyond threshold for at least one field.
func computeAdjustments(
	pod *corev1.Pod,
	recsByName map[string]map[string]ballastv1.ResourceRecommendation,
	behaviors ballastv1.BehaviorConfig,
) []ContainerAdjustment {
	maxChange := resolveMaxChangePercent(behaviors)
	var result []ContainerAdjustment

	for _, c := range pod.Spec.Containers {
		recs, ok := recsByName[c.Name]
		if !ok || len(recs) == 0 {
			continue
		}

		adj := ContainerAdjustment{
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

		drifted := false
		for res, rec := range recs {
			resName := corev1.ResourceName(res)

			if rec.Request != "" {
				threshold := ResolveFieldThreshold(behaviors, res, "request")
				if recommended, err := resource.ParseQuantity(rec.Request); err == nil {
					current := currentValue(c.Resources.Requests, resName)
					if ExceedsDrift(current, recommended, threshold) {
						drifted = true
						adj.Requests[resName] = CapChange(current, recommended, maxChange)
					}
				}
			}

			if rec.Limit != "" {
				threshold := ResolveFieldThreshold(behaviors, res, "limit")
				if recommended, err := resource.ParseQuantity(rec.Limit); err == nil {
					current := currentValue(c.Resources.Limits, resName)
					if ExceedsDrift(current, recommended, threshold) {
						drifted = true
						adj.Limits[resName] = CapChange(current, recommended, maxChange)
					}
				}
			}
		}

		if drifted {
			result = append(result, adj)
		}
	}

	return result
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
	return defaultThresholdPercent
}

// resolveMaxChangePercent parses the maxChangePerCycle value from the policy.
func resolveMaxChangePercent(behaviors ballastv1.BehaviorConfig) float64 {
	if v := parsePercent(behaviors.Resize.MaxChangePerCycle); v > 0 {
		return v
	}
	return defaultMaxChangePercent
}

// exceedsDrift returns true when |recommended - current| / current > threshold%.
// If current is zero and recommended is nonzero, drift is treated as infinite (always resize).
func ExceedsDrift(current, recommended resource.Quantity, thresholdPct float64) bool {
	cur := current.AsApproximateFloat64()
	rec := recommended.AsApproximateFloat64()
	if cur == 0 {
		return rec > 0
	}
	drift := (rec - cur) / cur
	if drift < 0 {
		drift = -drift
	}
	return drift > thresholdPct/100.0
}

// capChange applies maxChangePerCycle: if the move from current to recommended
// exceeds maxChangePct% of current, cap it at that fraction toward recommended.
func CapChange(current, recommended resource.Quantity, maxChangePct float64) resource.Quantity {
	cur := current.AsApproximateFloat64()
	rec := recommended.AsApproximateFloat64()

	if cur == 0 {
		return recommended
	}

	maxDelta := cur * (maxChangePct / 100.0)
	delta := rec - cur
	if delta < 0 {
		delta = -delta
	}
	if delta <= maxDelta {
		return recommended
	}

	var capped float64
	if rec > cur {
		capped = cur + maxDelta
	} else {
		capped = cur - maxDelta
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

// applyResize patches the pod via the resize subresource.
func (r *Reconciler) applyResize(ctx context.Context, pod *corev1.Pod, adjustments []ContainerAdjustment) error {
	modified := pod.DeepCopy()
	for i := range modified.Spec.Containers {
		for _, adj := range adjustments {
			if modified.Spec.Containers[i].Name != adj.Name {
				continue
			}
			if modified.Spec.Containers[i].Resources.Requests == nil {
				modified.Spec.Containers[i].Resources.Requests = make(corev1.ResourceList)
			}
			if modified.Spec.Containers[i].Resources.Limits == nil {
				modified.Spec.Containers[i].Resources.Limits = make(corev1.ResourceList)
			}
			maps.Copy(modified.Spec.Containers[i].Resources.Requests, adj.Requests)
			maps.Copy(modified.Spec.Containers[i].Resources.Limits, adj.Limits)
		}
	}
	return r.client.SubResource("resize").Patch(ctx, modified, client.MergeFrom(pod))
}

// stampPodAnnotation patches a single annotation onto the pod.
func (r *Reconciler) stampPodAnnotation(ctx context.Context, pod *corev1.Pod, key, value string) error {
	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[key] = value
	return r.client.Patch(ctx, pod, client.MergeFrom(base))
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
	return defaultResizeInterval
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
