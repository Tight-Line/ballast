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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/workloadwatcher"
	"github.com/tight-line/ballast/internal/killswitch"
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
}

// NewPodMutator creates a PodMutator backed by the given client and kill switch.
func NewPodMutator(c client.Client, ks *killswitch.KillSwitch, dryRunApply bool) *PodMutator {
	log := ctrl.Log.WithName("webhook")
	return &PodMutator{
		client:      c,
		ks:          ks,
		resolver:    policy.NewResolver(c, log),
		dryRunApply: dryRunApply,
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
		return admission.Allowed("kill switch active")
	}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding pod: %w", err))
	}

	if err := validation.ValidateAnnotations(pod.Annotations); err != nil {
		return admission.Denied(err.Error())
	}

	profile, ok, err := m.resolveApplyProfile(ctx, &pod)
	if err != nil {
		log.V(1).Info("profile resolution error, allowing without mutation", "err", err)
		return admission.Allowed("profile not available")
	}
	if !ok {
		return admission.Allowed("apply not active or profile not ready")
	}

	return m.mutate(ctx, &pod, profile)
}

// resolveApplyProfile returns the WorkloadProfile to use for patching, and true if apply
// should proceed. Returns (nil, false, nil) when apply is not requested or the profile is
// not ready — both are normal non-error states.
func (m *PodMutator) resolveApplyProfile(ctx context.Context, pod *corev1.Pod) (*ballastv1.WorkloadProfile, bool, error) {
	if !wantsApply(pod.Annotations) {
		return nil, false, nil
	}
	p, err := m.lookupProfile(ctx, pod)
	if err != nil {
		return nil, false, err
	}
	if p == nil || !p.Status.MeetsThreshold {
		return nil, false, nil
	}
	return p, true, nil
}

// wantsApply reports whether the pod carries any annotation that requests resource application.
func wantsApply(ann map[string]string) bool {
	for _, key := range []string{
		validation.AnnotationApply,
		validation.AnnotationAutoresize,
	} {
		if v, ok := ann[key]; ok && strings.EqualFold(v, "true") {
			return true
		}
	}
	return false
}

// mutate builds the patched pod and returns a JSON-patch admission response.
func (m *PodMutator) mutate(ctx context.Context, pod *corev1.Pod, profile *ballastv1.WorkloadProfile) admission.Response {
	log := ctrl.Log.WithName("webhook")

	modifiedPod := pod.DeepCopy()
	m.stampPolicyRef(ctx, pod, modifiedPod)

	applied := applyRecommendations(modifiedPod, profile)
	log.Info("applying resource recommendations", "dry_run", m.dryRunApply, "containers", applied)

	if m.dryRunApply {
		return admission.Allowed("dry-run: apply suppressed")
	}

	return patchResponse(pod, modifiedPod)
}

// stampPolicyRef resolves the active policy and stamps its name onto modifiedPod.
// Policy resolution failures are non-fatal — the admission proceeds without a policy-ref.
func (m *PodMutator) stampPolicyRef(ctx context.Context, pod, modifiedPod *corev1.Pod) {
	resolved, err := m.resolver.Resolve(ctx, policy.Input{
		Namespace:   pod.Namespace,
		OwnerKind:   directOwnerKind(pod),
		Labels:      pod.Labels,
		Annotations: pod.Annotations,
	})
	if err != nil { // coverage:ignore - transient API error listing policy objects
		ctrl.Log.WithName("webhook").V(1).Info("policy resolution error, skipping policy-ref", "err", err)
		return
	}
	if resolved != nil {
		modifiedPod.Annotations[validation.AnnotationPolicyRef] = resolved.Name
	}
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
	if err := m.client.Get(ctx, types.NamespacedName{Name: workloadwatcher.ProfileName(tupleLabels)}, &wp); err != nil {
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
