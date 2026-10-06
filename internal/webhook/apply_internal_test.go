/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package webhook

import (
	"maps"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/kube"
)

// rl builds a ResourceList from resource/quantity pairs.
func rl(pairs ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		out[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return out
}

// sizedPod returns a pod with one "app" container carrying the given resources.
func sizedPod(requests, limits corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "app",
			Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits},
		}}},
	}
}

// profileFor returns a profile recommending recs for each named container.
func profileFor(recs map[string]map[string]ballastv1.ResourceRecommendation) *ballastv1.WorkloadProfile {
	p := &ballastv1.WorkloadProfile{}
	for _, name := range slices.Sorted(maps.Keys(recs)) {
		p.Status.Containers = append(p.Status.Containers,
			ballastv1.ContainerProfile{Name: name, Recommendations: recs[name]})
	}
	return p
}

func appRecs(recs map[string]ballastv1.ResourceRecommendation) *ballastv1.WorkloadProfile {
	return profileFor(map[string]map[string]ballastv1.ResourceRecommendation{"app": recs})
}

func assertQty(t *testing.T, what string, got resource.Quantity, want string) {
	t.Helper()
	if w := resource.MustParse(want); got.Cmp(w) != 0 {
		t.Errorf("%s = %s, want %s", what, got.String(), want)
	}
}

func assertClamps(t *testing.T, got []kube.RequestClamp, want ...kube.RequestClamp) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("clamps = %+v, want %+v", got, want)
	}
}

func TestApply_RequestsOnlyMemoryOverLimit_Clamped(t *testing.T) {
	pod := sizedPod(rl("memory", "100Mi"), rl("memory", "128Mi"))
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "152508Ki"},
	}))
	c := pod.Spec.Containers[0].Resources
	assertQty(t, "memory request", c.Requests[corev1.ResourceMemory], "128Mi")
	assertQty(t, "memory limit", c.Limits[corev1.ResourceMemory], "128Mi")
	assertClamps(t, res.clamps, kube.RequestClamp{Container: "app", Resource: "memory", Recommended: "152508Ki", Limit: "128Mi", Applied: "128Mi"})
	if got := pod.Annotations[annotationClamped+"memory-request"]; got != "152508Ki" {
		t.Errorf("clamped annotation = %q, want 152508Ki", got)
	}
	if got := pod.Annotations[annotationApplied+"memory-request"]; got != "128Mi" {
		t.Errorf("applied annotation = %q, want the clamped 128Mi", got)
	}
	if !slices.Equal(res.applied, []string{"app"}) {
		t.Errorf("applied = %v, want [app]", res.applied)
	}
}

func TestApply_RequestsOnlyCPUOverLimit_Clamped(t *testing.T) {
	pod := sizedPod(rl("cpu", "100m"), rl("cpu", "200m"))
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m"},
	}))
	assertQty(t, "cpu request", pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU], "200m")
	assertClamps(t, res.clamps, kube.RequestClamp{Container: "app", Resource: "cpu", Recommended: "500m", Limit: "200m", Applied: "200m"})
}

func TestApply_BothFieldsManaged_ComparedToNewLimit(t *testing.T) {
	pod := sizedPod(rl("cpu", "100m"), rl("cpu", "200m"))
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m", Limit: "300m"},
	}))
	c := pod.Spec.Containers[0].Resources
	assertQty(t, "cpu limit", c.Limits[corev1.ResourceCPU], "300m")
	assertQty(t, "cpu request", c.Requests[corev1.ResourceCPU], "300m")
	assertClamps(t, res.clamps, kube.RequestClamp{Container: "app", Resource: "cpu", Recommended: "500m", Limit: "300m", Applied: "300m"})
}

func TestApply_NoLimit_NotClamped(t *testing.T) {
	pod := sizedPod(rl("cpu", "100m"), nil)
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "500m"},
	}))
	assertQty(t, "cpu request", pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU], "500m")
	assertClamps(t, res.clamps)
	if _, ok := pod.Annotations[annotationClamped+"cpu-request"]; ok {
		t.Error("unexpected clamped annotation with no limit set")
	}
}

func TestApply_ClampWouldPromoteToGuaranteed_StepsDownOneUnit(t *testing.T) {
	// cpu already equals its limit; memory is the last unequal pair, so clamping
	// it to exactly 128Mi would promote the pod to Guaranteed. It lands one byte
	// under instead. ephemeral-storage is clamped too, but it does not affect
	// the QoS class, so it stays at exactly its limit.
	pod := sizedPod(
		rl("cpu", "100m", "memory", "100Mi", "ephemeral-storage", "500Mi"),
		rl("cpu", "100m", "memory", "128Mi", "ephemeral-storage", "1Gi"))
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"memory":            {Request: "149Mi"},
		"ephemeral-storage": {Request: "2Gi"},
	}))
	c := pod.Spec.Containers[0].Resources
	assertQty(t, "memory request", c.Requests[corev1.ResourceMemory], "134217727")
	assertQty(t, "ephemeral-storage request", c.Requests[corev1.ResourceEphemeralStorage], "1Gi")
	if got := kube.PodQOS(nil, pod.Spec.Containers); got != corev1.PodQOSBurstable {
		t.Errorf("QoS = %s, want Burstable", got)
	}
	if got := pod.Annotations[annotationApplied+"memory-request"]; got != "134217727" {
		t.Errorf("applied memory annotation = %q, want the stepped-down value", got)
	}
	if len(res.clamps) != 2 {
		t.Errorf("clamps = %+v, want ephemeral-storage and memory", res.clamps)
	}
}

func TestApply_GuaranteedPod_RecommendationAboveLimit_StaysGuaranteed(t *testing.T) {
	pod := sizedPod(rl("cpu", "100m", "memory", "128Mi"), rl("cpu", "100m", "memory", "128Mi"))
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"memory": {Request: "149Mi"},
	}))
	assertQty(t, "memory request", pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory], "128Mi")
	if got := kube.PodQOS(nil, pod.Spec.Containers); got != corev1.PodQOSGuaranteed {
		t.Errorf("QoS = %s, want Guaranteed", got)
	}
	assertClamps(t, res.clamps, kube.RequestClamp{Container: "app", Resource: "memory", Recommended: "149Mi", Limit: "128Mi", Applied: "128Mi"})
}

func TestApply_GuaranteedPod_LowerRequestsOnly_LeftAlone(t *testing.T) {
	pod := sizedPod(rl("cpu", "100m", "memory", "128Mi"), rl("cpu", "100m", "memory", "128Mi"))
	before := pod.DeepCopy()
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu":               {Request: "50m"},
		"ephemeral-storage": {Request: "1Gi"},
	}))
	if len(res.applied) != 0 || !slices.Equal(res.pinned, []string{"app"}) {
		t.Errorf("applied/pinned = %v/%v, want none/[app]", res.applied, res.pinned)
	}
	if pod.Spec.Containers[0].Resources.Requests.Cpu().Cmp(*before.Spec.Containers[0].Resources.Requests.Cpu()) != 0 ||
		len(pod.Spec.Containers[0].Resources.Requests) != len(before.Spec.Containers[0].Resources.Requests) {
		t.Errorf("container resources changed: %+v", pod.Spec.Containers[0].Resources)
	}
	if len(pod.Annotations) != 0 {
		t.Errorf("annotations written for a pinned container: %v", pod.Annotations)
	}
}

func TestApply_BestEffortPod_PromotedToBurstable(t *testing.T) {
	pod := sizedPod(nil, nil)
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu":    {Request: "200m"},
		"memory": {Request: "128Mi", Limit: "256Mi"},
	}))
	if got := kube.PodQOS(nil, pod.Spec.Containers); got != corev1.PodQOSBurstable {
		t.Errorf("QoS = %s, want Burstable", got)
	}
	if !slices.Equal(res.applied, []string{"app"}) || len(res.pinned) != 0 {
		t.Errorf("applied/pinned = %v/%v, want [app]/none", res.applied, res.pinned)
	}
}

func TestApply_BestEffortPod_ClampToGuaranteed_Allowed(t *testing.T) {
	// A policy whose own request formula exceeds its limit formula clamps an
	// unsized pod straight to Guaranteed. A BestEffort pod carries no class
	// intent, so that is allowed: no step-down.
	pod := sizedPod(nil, nil)
	applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu":    {Request: "300m", Limit: "200m"},
		"memory": {Request: "300Mi", Limit: "256Mi"},
	}))
	c := pod.Spec.Containers[0].Resources
	assertQty(t, "cpu request", c.Requests[corev1.ResourceCPU], "200m")
	if got := kube.PodQOS(nil, pod.Spec.Containers); got != corev1.PodQOSGuaranteed {
		t.Errorf("QoS = %s, want Guaranteed", got)
	}
}

func TestApply_BestEffortPod_EqualRecommendations_Guaranteed(t *testing.T) {
	// Request and limit recommendations that come out equal size an unsized pod
	// as Guaranteed rather than leaving it unsized.
	pod := sizedPod(nil, nil)
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu":    {Request: "200m", Limit: "200m"},
		"memory": {Request: "256Mi", Limit: "256Mi"},
	}))
	if !slices.Equal(res.applied, []string{"app"}) || len(res.pinned) != 0 {
		t.Errorf("applied/pinned = %v/%v, want [app]/none", res.applied, res.pinned)
	}
	if got := kube.PodQOS(nil, pod.Spec.Containers); got != corev1.PodQOSGuaranteed {
		t.Errorf("QoS = %s, want Guaranteed", got)
	}
}

func TestApply_RecommendationLandsOnLimit_StepsDownToStayBurstable(t *testing.T) {
	// memory already sits at its limit (an earlier clamp); the cpu
	// recommendation lands exactly on the cpu limit. That is not a clamp, but it
	// would still promote the Burstable pod to Guaranteed, so the cpu request
	// Ballast writes goes one millicore under. The author's memory request is
	// left alone.
	pod := sizedPod(rl("cpu", "100m", "memory", "128Mi"), rl("cpu", "200m", "memory", "128Mi"))
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "200m"},
	}))
	c := pod.Spec.Containers[0].Resources
	assertQty(t, "cpu request", c.Requests[corev1.ResourceCPU], "199m")
	assertQty(t, "memory request", c.Requests[corev1.ResourceMemory], "128Mi")
	if len(res.pinned) != 0 {
		t.Errorf("pinned = %v, want none", res.pinned)
	}
	if got := pod.Annotations[annotationApplied+"cpu-request"]; got != "199m" {
		t.Errorf("applied cpu annotation = %q, want 199m", got)
	}
}

func TestApply_PodLevelResources_DecideClass(t *testing.T) {
	// With pod-level resources the class comes from pod.Spec.Resources, so a
	// requests-only recommendation on a container cannot change it and is
	// applied as is.
	pod := sizedPod(rl("cpu", "100m", "memory", "128Mi"), rl("cpu", "100m", "memory", "128Mi"))
	pod.Spec.Resources = &corev1.ResourceRequirements{
		Requests: rl("cpu", "200m", "memory", "256Mi"),
		Limits:   rl("cpu", "200m", "memory", "256Mi"),
	}
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"cpu": {Request: "50m"},
	}))
	if !slices.Equal(res.applied, []string{"app"}) || len(res.pinned) != 0 {
		t.Errorf("applied/pinned = %v/%v, want [app]/none", res.applied, res.pinned)
	}
	assertQty(t, "cpu request", pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU], "50m")
}

func TestApply_LimitOnlyBelowAuthorRequest_ReportsAuthorRequest(t *testing.T) {
	// A limit-only recommendation below the author's request: the request is
	// held at the new limit, and the clamp reports the author's request as the
	// value that would otherwise have been written.
	pod := sizedPod(rl("memory", "200Mi"), rl("memory", "256Mi"))
	res := applyRecommendations(pod, appRecs(map[string]ballastv1.ResourceRecommendation{
		"memory": {Limit: "150Mi"},
	}))
	assertClamps(t, res.clamps, kube.RequestClamp{Container: "app", Resource: "memory", Recommended: "200Mi", Limit: "150Mi", Applied: "150Mi"})
}

func TestApply_OnlyOffendingContainerDropped(t *testing.T) {
	// A Guaranteed pod with two containers: "app" gets a requests-only
	// recommendation that would break requests == limits, "otc" gets a coupled
	// move that keeps it Guaranteed. Only app's recommendations are dropped.
	pod := sizedPod(rl("cpu", "100m", "memory", "128Mi"), rl("cpu", "100m", "memory", "128Mi"))
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "otc",
		Resources: corev1.ResourceRequirements{
			Requests: rl("cpu", "50m", "memory", "64Mi"),
			Limits:   rl("cpu", "50m", "memory", "64Mi"),
		},
	})
	res := applyRecommendations(pod, profileFor(map[string]map[string]ballastv1.ResourceRecommendation{
		"app": {"cpu": {Request: "50m"}},
		"otc": {"cpu": {Request: "80m", Limit: "80m"}},
	}))
	if !slices.Equal(res.applied, []string{"otc"}) || !slices.Equal(res.pinned, []string{"app"}) {
		t.Errorf("applied/pinned = %v/%v, want [otc]/[app]", res.applied, res.pinned)
	}
	assertQty(t, "app cpu request", pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU], "100m")
	assertQty(t, "otc cpu request", pod.Spec.Containers[1].Resources.Requests[corev1.ResourceCPU], "80m")
	if got := kube.PodQOS(nil, pod.Spec.Containers); got != corev1.PodQOSGuaranteed {
		t.Errorf("QoS = %s, want Guaranteed", got)
	}
}

func TestApply_InitContainers_OnlyRestartableClamped(t *testing.T) {
	// A restartable-init sidecar is patched (and clamped) like a regular
	// container; a run-to-completion init container is never patched, even
	// with a recommendation under its name.
	always := corev1.ContainerRestartPolicyAlways
	pod := sizedPod(rl("cpu", "100m"), nil)
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "migrate", Resources: corev1.ResourceRequirements{Requests: rl("cpu", "10m")}},
		{Name: "otc", RestartPolicy: &always, Resources: corev1.ResourceRequirements{
			Requests: rl("memory", "32Mi"), Limits: rl("memory", "64Mi"),
		}},
	}
	res := applyRecommendations(pod, profileFor(map[string]map[string]ballastv1.ResourceRecommendation{
		"migrate": {"cpu": {Request: "500m"}},
		"otc":     {"memory": {Request: "100Mi"}},
	}))
	if !slices.Equal(res.applied, []string{"otc"}) {
		t.Errorf("applied = %v, want [otc]", res.applied)
	}
	assertQty(t, "migrate cpu request", pod.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU], "10m")
	assertQty(t, "otc memory request", pod.Spec.InitContainers[1].Resources.Requests[corev1.ResourceMemory], "64Mi")
	assertClamps(t, res.clamps, kube.RequestClamp{Container: "otc", Resource: "memory", Recommended: "100Mi", Limit: "64Mi", Applied: "64Mi"})
}

func TestApply_PerContainerDropInsufficient_PodLeftUnpatched(t *testing.T) {
	// The pod's class comes from aggregate requests and limits, so a spec can be
	// Guaranteed as a whole while neither container is on its own ("app" asks
	// for more cpu than its limit, "otc" less). Lowering otc's memory request
	// changes the pod's class without changing otc's own, so dropping offending
	// containers cannot help and the whole pod is left unpatched.
	pod := sizedPod(rl("cpu", "150m", "memory", "128Mi"), rl("cpu", "100m", "memory", "128Mi"))
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name: "otc",
		Resources: corev1.ResourceRequirements{
			Requests: rl("cpu", "50m", "memory", "64Mi"),
			Limits:   rl("cpu", "100m", "memory", "64Mi"),
		},
	})
	before := pod.DeepCopy()
	res := applyRecommendations(pod, profileFor(map[string]map[string]ballastv1.ResourceRecommendation{
		"app": {"ephemeral-storage": {Request: "1Gi"}},
		"otc": {"memory": {Request: "32Mi"}},
	}))
	if len(res.applied) != 0 || !slices.Equal(res.pinned, []string{"app", "otc"}) {
		t.Errorf("applied/pinned = %v/%v, want none/[app otc]", res.applied, res.pinned)
	}
	assertQty(t, "otc memory request", pod.Spec.Containers[1].Resources.Requests[corev1.ResourceMemory],
		before.Spec.Containers[1].Resources.Requests.Memory().String())
}
