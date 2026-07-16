/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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
	"github.com/tight-line/ballast/internal/policy"
	"github.com/tight-line/ballast/internal/validation"
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

	profile, err := m.lookupProfile(ctx, &pod)
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

	return m.mutate(ctx, &pod, profile)
}

// mutate builds the patched pod and returns a JSON-patch admission response.
func (m *PodMutator) mutate(ctx context.Context, pod *corev1.Pod, profile *ballastv1.WorkloadProfile) admission.Response {
	log := ctrl.Log.WithName("webhook")

	modifiedPod := pod.DeepCopy()
	// Enrollment lives in a label, so an enrolled pod may carry no annotations.
	// The apply and policy-ref paths below write annotations, so ensure the map
	// exists before they run.
	if modifiedPod.Annotations == nil {
		modifiedPod.Annotations = make(map[string]string)
	}
	policyRef := m.stampPolicyRef(ctx, pod, modifiedPod)

	applied := applyRecommendations(modifiedPod, profile)
	log.Info("applying resource recommendations", "dry_run", m.dryRunApply, "containers", applied)

	pid := metrics.ProfileID{Name: profile.Name, Labels: profile.Status.TupleLabels}

	// Exactly one apply.* outcome per admission that requested apply: no_change
	// wins over dry_run (nothing would have changed either way), and applied is
	// recorded only when a real patch changed resources.
	switch {
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

	m.rec.WebhookMutation(ctx, "mutated", pod.Namespace, pid)
	return patchResponse(pod, modifiedPod)
}

// stampPolicyRef resolves the active policy and stamps its name onto modifiedPod,
// returning the stamped ref ("" when no policy resolved). Policy resolution
// failures are non-fatal — the admission proceeds without a policy-ref.
func (m *PodMutator) stampPolicyRef(ctx context.Context, pod, modifiedPod *corev1.Pod) string {
	resolved, err := m.resolver.Resolve(ctx, policy.Input{
		Namespace:   pod.Namespace,
		OwnerKind:   directOwnerKind(pod),
		Labels:      pod.Labels,
		Annotations: pod.Annotations,
	})
	if err != nil { // coverage:ignore - transient API error listing policy objects
		ctrl.Log.WithName("webhook").V(1).Info("policy resolution error, skipping policy-ref", "err", err)
		return ""
	}
	if resolved == nil {
		return ""
	}
	ref := resolved.Name
	if resolved.Namespaced {
		ref = pod.Namespace + "/" + resolved.Name
	}
	modifiedPod.Annotations[validation.AnnotationPolicyRef] = ref
	return ref
}

// lookupProfile resolves the WorkloadProfile for the given pod.
// Returns (nil, nil) when no profile exists yet — normal for new workloads.
func (m *PodMutator) lookupProfile(ctx context.Context, pod *corev1.Pod) (*ballastv1.WorkloadProfile, error) {
	var cfg ballastv1.BallastConfig
	if err := m.client.Get(ctx, types.NamespacedName{Name: killswitch.BallastConfigName}, &cfg); err != nil {
		return nil, fmt.Errorf("getting BallastConfig: %w", err)
	}

	tupleLabels := workloadwatcher.ExtractTupleLabels(pod.Labels, cfg.Spec.IdentityLabels)

	var wp ballastv1.WorkloadProfile
	if err := m.client.Get(ctx, types.NamespacedName{Name: workloadwatcher.ProfileName(tupleLabels, cfg.Spec.IdentityLabels)}, &wp); err != nil {
		return nil, nil //nolint:nilerr // not-found is expected for new workloads
	}

	return &wp, nil
}

// applyRecommendations patches container resources and records applied-* annotations.
// Returns the names of containers that received patches.
func applyRecommendations(pod *corev1.Pod, profile *ballastv1.WorkloadProfile) []string {
	byName := containerProfilesByName(profile)
	var applied []string
	for i := range pod.Spec.Containers {
		if patchContainerResources(&pod.Spec.Containers[i], byName, pod.Annotations) {
			applied = append(applied, pod.Spec.Containers[i].Name)
		}
	}
	// Restartable-init "native sidecars" are first-class right-sizing targets and
	// are patched on spec.initContainers just like regular containers (#30).
	for i := range pod.Spec.InitContainers {
		if kube.IsRestartableInit(pod.Spec.InitContainers[i]) &&
			patchContainerResources(&pod.Spec.InitContainers[i], byName, pod.Annotations) {
			applied = append(applied, pod.Spec.InitContainers[i].Name)
		}
	}
	return applied
}

func containerProfilesByName(profile *ballastv1.WorkloadProfile) map[string]ballastv1.ContainerProfile {
	m := make(map[string]ballastv1.ContainerProfile, len(profile.Status.Containers))
	for _, cp := range profile.Status.Containers {
		m[cp.Name] = cp
	}
	return m
}

func patchContainerResources(c *corev1.Container, byName map[string]ballastv1.ContainerProfile, ann map[string]string) bool {
	cp, ok := byName[c.Name]
	if !ok || len(cp.Recommendations) == 0 {
		return false
	}
	if c.Resources.Requests == nil {
		c.Resources.Requests = make(corev1.ResourceList)
	}
	if c.Resources.Limits == nil {
		c.Resources.Limits = make(corev1.ResourceList)
	}
	for res, rec := range cp.Recommendations {
		applyResourceField(c.Resources.Requests, ann, res, "request", rec.Request)
		applyResourceField(c.Resources.Limits, ann, res, "limit", rec.Limit)
	}
	return true
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
	ann["ballast.tightlinesoftware.com/applied-"+res+"-"+field] = val
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

// directOwnerKind returns the Kind of the first controller ownerReference on the pod,
// or empty string if none is set.
func directOwnerKind(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind
		}
	}
	return ""
}
