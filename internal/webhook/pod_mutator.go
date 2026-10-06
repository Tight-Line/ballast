/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/kube"
	"github.com/tight-line/ballast/internal/metrics"
	"github.com/tight-line/ballast/internal/naming"
	"github.com/tight-line/ballast/internal/policy"
	"github.com/tight-line/ballast/internal/validation"
)

const (
	// annotationApplied prefixes the applied-<resource>-<field> annotations that
	// record each value admission wrote.
	annotationApplied = "ballast.tightlinesoftware.com/applied-"
	// annotationClamped prefixes the clamped-<resource>-request annotation, which
	// records the request admission would have written had it not exceeded the
	// container's limit (#119).
	annotationClamped = "ballast.tightlinesoftware.com/clamped-"
)

// PodMutator implements admission.Handler for pod CREATE requests.
// It validates Ballast annotations, resolves the active WorkloadProfile,
// and patches container resource requests/limits when the profile is ready.
type PodMutator struct {
	client      client.Client
	ks          *killswitch.KillSwitch
	resolver    *policy.Resolver
	dryRunApply bool
	rec         *metrics.Recorder
}

// NewPodMutator creates a PodMutator backed by the given client and kill switch.
func NewPodMutator(c client.Client, ks *killswitch.KillSwitch, dryRunApply bool, rec *metrics.Recorder) *PodMutator {
	log := ctrl.Log.WithName("webhook")
	return &PodMutator{
		client:      c,
		ks:          ks,
		resolver:    policy.NewResolver(c, log),
		dryRunApply: dryRunApply,
		rec:         rec,
	}
}

// SetupWithManager registers the PodMutator handler with the manager's webhook server.
func (m *PodMutator) SetupWithManager(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register("/mutate-v1-pod", &admission.Webhook{Handler: m})
}

// Handle processes a pod admission request.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := ctrl.Log.WithName("webhook").WithValues("pod", req.Name, "namespace", req.Namespace)

	if m.ks.IsActive() {
		log.Info("kill switch active, skipping mutation", "reason", m.ks.Reason())
		m.rec.WebhookMutation(ctx, "kill_switch", req.Namespace, metrics.ProfileID{})
		return admission.Allowed("kill switch active")
	}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding pod: %w", err))
	}

	if err := validation.ValidateMode(pod.Labels); err != nil {
		return admission.Denied(err.Error())
	}

	if !validation.WantsApply(pod.Labels) {
		m.rec.WebhookMutation(ctx, "skipped", req.Namespace, metrics.ProfileID{})
		return admission.Allowed("apply not requested")
	}

	// Resolve policy before looking up the profile: a profile's identity includes
	// the policy governing it, so the policy is part of the profile's name. This
	// is also the value stamped as policy-ref below, so admission resolves exactly
	// once and cannot stamp one policy while applying another's recommendations.
	resolved, err := m.resolver.Resolve(ctx, policy.InputForPod(&pod))
	if err != nil { // coverage:ignore - transient API error listing policy objects
		log.V(1).Info("policy resolution error, allowing without mutation", "err", err)
		m.rec.WebhookMutation(ctx, "not_available", req.Namespace, metrics.ProfileID{})
		return admission.Allowed("policy not available")
	}

	profile, err := m.lookupProfile(ctx, &pod, resolved)
	if err != nil {
		log.V(1).Info("profile resolution error, allowing without mutation", "err", err)
		m.rec.WebhookMutation(ctx, "not_available", req.Namespace, metrics.ProfileID{})
		return admission.Allowed("profile not available")
	}
	if profile == nil {
		m.rec.ApplySkipped(ctx, "no_profile", metrics.ProfileID{}, "", pod.Namespace)
		m.rec.WebhookMutation(ctx, "skipped", req.Namespace, metrics.ProfileID{})
		return admission.Allowed("no profile for workload yet")
	}
	if !profile.Status.MeetsThreshold {
		pid := metrics.ProfileID{Name: profile.Name, Labels: profile.Status.TupleLabels}
		m.rec.ApplySkipped(ctx, "not_ready", pid, "", pod.Namespace)
		m.rec.WebhookMutation(ctx, "skipped", req.Namespace, metrics.ProfileID{})
		return admission.Allowed("profile not ready")
	}

	return m.mutate(ctx, &pod, profile, resolved)
}

// mutate builds the patched pod and returns a JSON-patch admission response.
func (m *PodMutator) mutate(ctx context.Context, pod *corev1.Pod, profile *ballastv1.WorkloadProfile, resolved *policy.ResolvedPolicy) admission.Response {
	log := ctrl.Log.WithName("webhook")

	modifiedPod := pod.DeepCopy()
	// Enrollment lives in a label, so an enrolled pod may carry no annotations.
	// The apply and policy-ref paths below write annotations, so ensure the map
	// exists before they run.
	if modifiedPod.Annotations == nil {
		modifiedPod.Annotations = make(map[string]string)
	}
	policyRef := stampPolicyRef(modifiedPod, resolved)

	result := applyRecommendations(modifiedPod, profile)
	log.Info("applying resource recommendations", "dry_run", m.dryRunApply, "containers", result.applied)
	if len(result.pinned) > 0 {
		log.Info("dropped recommendations that would change the pod's QoS class; keeping the author's values",
			"pod", pod.Name, "namespace", pod.Namespace, "profile", profile.Name, "containers", result.pinned)
	}

	pid := metrics.ProfileID{Name: profile.Name, Labels: profile.Status.TupleLabels}

	// Exactly one apply.* outcome per admission that requested apply: no_change
	// (or qos_pinned, when recommendations existed but all were dropped to keep
	// the QoS class) wins over dry_run (nothing would have changed either way),
	// and applied is recorded only when a real patch changed resources.
	applied := result.applied
	switch {
	case len(applied) == 0 && len(result.pinned) > 0:
		m.rec.ApplySkipped(ctx, "qos_pinned", pid, policyRef, pod.Namespace)
	case len(applied) == 0:
		m.rec.ApplySkipped(ctx, "no_change", pid, policyRef, pod.Namespace)
	case m.dryRunApply:
		m.rec.ApplySkipped(ctx, "dry_run", pid, policyRef, pod.Namespace)
	default:
		m.rec.ApplyApplied(ctx, pid, policyRef, pod.Namespace)
	}

	if m.dryRunApply {
		m.rec.WebhookMutation(ctx, "dry_run", pod.Namespace, pid)
		return admission.Allowed("dry-run: apply suppressed")
	}

	// The pod does not exist yet, so there is nothing to attach an Event to; the
	// clamped-<resource>-request annotation carries the finding on the pod.
	for _, cl := range result.clamps {
		log.Info("recommended request exceeds the container limit; clamping request to the limit",
			"pod", pod.Name, "namespace", pod.Namespace, "profile", profile.Name,
			"container", cl.Container, "resource", cl.Resource,
			"recommended", cl.Recommended, "limit", cl.Limit, "phase", "admission")
		m.rec.RecommendationClamped(ctx, pid, cl.Container, cl.Resource, policyRef, pod.Namespace, "admission")
	}

	m.rec.WebhookMutation(ctx, "mutated", pod.Namespace, pid)
	return patchResponse(pod, modifiedPod)
}

// stampPolicyRef records the governing policy on modifiedPod and returns the
// stamped value ("" when no policy matched).
//
// This is admission-time resolution; the workloadwatcher refreshes the annotation
// afterwards, because the policy set can change while the pod runs.
func stampPolicyRef(modifiedPod *corev1.Pod, resolved *policy.ResolvedPolicy) string {
	if resolved == nil {
		return ""
	}
	ref := policy.PodAnnotationValue(resolved.Ref)
	modifiedPod.Annotations[validation.AnnotationPolicyRef] = ref
	return ref
}

// lookupProfile resolves the WorkloadProfile holding this pod's recommendations,
// which is identified by the pod's label tuple together with the policy governing
// it. Returns (nil, nil) when no such profile exists yet — normal for new
// workloads, and also the case immediately after a policy change, until the
// workloadwatcher creates the profile for the new policy.
func (m *PodMutator) lookupProfile(ctx context.Context, pod *corev1.Pod, resolved *policy.ResolvedPolicy) (*ballastv1.WorkloadProfile, error) {
	var cfg ballastv1.BallastConfig
	if err := m.client.Get(ctx, types.NamespacedName{Name: killswitch.BallastConfigName}, &cfg); err != nil {
		return nil, fmt.Errorf("getting BallastConfig: %w", err)
	}

	tupleLabels := workloadwatcher.ExtractTupleLabels(pod.Labels, cfg.Spec.IdentityLabels)

	discriminator := naming.NoPolicy
	if resolved != nil {
		discriminator = naming.PolicyDiscriminator(resolved.Ref.Kind, resolved.Ref.Namespace, resolved.Ref.Name)
	}

	var wp ballastv1.WorkloadProfile
	name := naming.ProfileName(tupleLabels, cfg.Spec.IdentityLabels, discriminator)
	if err := m.client.Get(ctx, types.NamespacedName{Name: name}, &wp); err != nil {
		return nil, nil //nolint:nilerr // not-found is expected for new workloads
	}

	return &wp, nil
}

// applyResult reports what applyRecommendations did to a pod.
type applyResult struct {
	// applied names the containers that received patches.
	applied []string
	// pinned names the containers whose recommendations were dropped because
	// they changed the pod's QoS class.
	pinned []string
	// clamps lists the requests held at their container's limit.
	clamps []kube.RequestClamp
}

// containerPatch is one container's recommendations applied to a copy of it,
// held back until the pod-level QoS check decides whether it is kept.
type containerPatch struct {
	target      *corev1.Container
	patched     corev1.Container
	annotations map[string]string
	clamps      []kube.RequestClamp
}

// applyRecommendations patches container resources and records applied-* (and
// clamped-*) annotations.
//
// The pod's QoS class may change only from BestEffort to Burstable, which is how
// an unsized pod gets sized. A request clamped to its limit that would promote
// the pod to Guaranteed is stepped one unit below the limit instead. Any other
// class change drops the recommendations of the containers whose own class they
// change, keeping the author's values; if that still leaves the pod in a
// different class, no container is patched.
func applyRecommendations(pod *corev1.Pod, profile *ballastv1.WorkloadProfile) applyResult {
	byName := containerProfilesByName(profile)
	var patches []*containerPatch
	for i := range pod.Spec.Containers {
		if p := buildContainerPatch(&pod.Spec.Containers[i], byName); p != nil {
			patches = append(patches, p)
		}
	}
	// Restartable-init "native sidecars" are first-class right-sizing targets and
	// are patched on spec.initContainers just like regular containers (#30).
	for i := range pod.Spec.InitContainers {
		if !kube.IsRestartableInit(pod.Spec.InitContainers[i]) {
			continue
		}
		if p := buildContainerPatch(&pod.Spec.InitContainers[i], byName); p != nil {
			patches = append(patches, p)
		}
	}

	original := kube.PodQOS(withPatches(pod, nil))
	if original != corev1.PodQOSGuaranteed && kube.PodQOS(withPatches(pod, patches)) == corev1.PodQOSGuaranteed {
		stepDownClamps(patches)
	}

	var result applyResult
	if !admissionQOSAllowed(original, kube.PodQOS(withPatches(pod, patches))) {
		var kept []*containerPatch
		for _, p := range patches {
			if admissionQOSAllowed(kube.PodQOS([]corev1.Container{*p.target}), kube.PodQOS([]corev1.Container{p.patched})) {
				kept = append(kept, p)
			} else {
				result.pinned = append(result.pinned, p.target.Name)
			}
		}
		if !admissionQOSAllowed(original, kube.PodQOS(withPatches(pod, kept))) {
			kept, result.pinned = nil, nil
			for _, p := range patches {
				result.pinned = append(result.pinned, p.target.Name)
			}
		}
		patches = kept
	}

	for _, p := range patches {
		*p.target = p.patched
		maps.Copy(pod.Annotations, p.annotations)
		result.applied = append(result.applied, p.target.Name)
		result.clamps = append(result.clamps, p.clamps...)
	}
	return result
}

// admissionQOSAllowed reports whether admission may move a pod from QoS class
// from to class to: unchanged, or BestEffort to Burstable.
func admissionQOSAllowed(from, to corev1.PodQOSClass) bool {
	return from == to || (from == corev1.PodQOSBestEffort && to == corev1.PodQOSBurstable)
}

// withPatches returns the pod's init and regular containers, with each patched
// container substituted for its target.
func withPatches(pod *corev1.Pod, patches []*containerPatch) []corev1.Container {
	byTarget := make(map[*corev1.Container]*corev1.Container, len(patches))
	for _, p := range patches {
		byTarget[p.target] = &p.patched
	}
	out := make([]corev1.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range list {
			if patched, ok := byTarget[&list[i]]; ok {
				out = append(out, *patched)
			} else {
				out = append(out, list[i])
			}
		}
	}
	return out
}

// stepDownClamps moves every cpu/memory request clamped to its limit one unit
// below the limit, so the clamp does not promote the pod to Guaranteed.
func stepDownClamps(patches []*containerPatch) {
	for _, p := range patches {
		for _, cl := range p.clamps {
			res := corev1.ResourceName(cl.Resource)
			if !kube.IsQOSResource(res) {
				continue
			}
			stepped := kube.StepDown(p.patched.Resources.Limits[res], res)
			p.patched.Resources.Requests[res] = stepped
			p.annotations[annotationApplied+cl.Resource+"-request"] = stepped.String()
		}
	}
}

func containerProfilesByName(profile *ballastv1.WorkloadProfile) map[string]ballastv1.ContainerProfile {
	m := make(map[string]ballastv1.ContainerProfile, len(profile.Status.Containers))
	for _, cp := range profile.Status.Containers {
		m[cp.Name] = cp
	}
	return m
}

// buildContainerPatch applies c's recommendations to a copy of c, or returns nil
// when the profile has none for it. A request above the resulting limit (the
// recommended limit, else the container's own) is clamped to that limit and
// recorded in a clamped-<resource>-request annotation holding the pre-clamp value.
func buildContainerPatch(c *corev1.Container, byName map[string]ballastv1.ContainerProfile) *containerPatch {
	cp, ok := byName[c.Name]
	if !ok || len(cp.Recommendations) == 0 {
		return nil
	}
	p := &containerPatch{target: c, patched: *c.DeepCopy(), annotations: make(map[string]string)}
	rr := &p.patched.Resources
	if rr.Requests == nil {
		rr.Requests = make(corev1.ResourceList)
	}
	if rr.Limits == nil {
		rr.Limits = make(corev1.ResourceList)
	}
	// Sorted so clamps are reported in a stable order.
	for _, res := range slices.Sorted(maps.Keys(cp.Recommendations)) {
		rec := cp.Recommendations[res]
		applyResourceField(rr.Requests, p.annotations, res, "request", rec.Request)
		applyResourceField(rr.Limits, p.annotations, res, "limit", rec.Limit)

		name := corev1.ResourceName(res)
		req, hasReq := rr.Requests[name]
		limit, hasLimit := rr.Limits[name]
		if !hasReq || !hasLimit || req.Cmp(limit) <= 0 {
			continue
		}
		rr.Requests[name] = limit
		p.annotations[annotationApplied+res+"-request"] = limit.String()
		p.annotations[annotationClamped+res+"-request"] = req.String()
		p.clamps = append(p.clamps, kube.RequestClamp{
			Container:   c.Name,
			Resource:    res,
			Recommended: req.String(),
			Limit:       limit.String(),
		})
	}
	return p
}

func applyResourceField(list corev1.ResourceList, ann map[string]string, res, field, val string) {
	if val == "" {
		return
	}
	qty, err := resource.ParseQuantity(val)
	if err != nil {
		return
	}
	list[corev1.ResourceName(res)] = qty
	ann[annotationApplied+res+"-"+field] = val
}

// patchResponse computes a JSON-patch response from original → modified pod.
func patchResponse(original, modified *corev1.Pod) admission.Response {
	originalJSON, err := json.Marshal(original)
	if err != nil { // coverage:ignore - json.Marshal of corev1.Pod cannot fail
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshaling original pod: %w", err))
	}
	modifiedJSON, err := json.Marshal(modified)
	if err != nil { // coverage:ignore - json.Marshal of corev1.Pod cannot fail
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshaling modified pod: %w", err))
	}
	return admission.PatchResponseFromRaw(originalJSON, modifiedJSON)
}
