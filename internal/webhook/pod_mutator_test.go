/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package webhook_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/validation"
	"github.com/tight-line/ballast/internal/webhook"
)

// -- scheme & client helpers --

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = ballastv1.AddToScheme(s)
	return s
}

func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&ballastv1.WorkloadProfile{}).
		WithObjects(objs...).
		Build()
}

func inactiveKS(t *testing.T) *killswitch.KillSwitch {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	ks := killswitch.New(fc, "ballast-system", nil)
	if _, err := ks.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("ks.Reconcile: %v", err)
	}
	return ks
}

func activeKS(t *testing.T) *killswitch.KillSwitch {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: killswitch.ConfigMapName, Namespace: "ballast-system",
	}}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()
	ks := killswitch.New(fc, "ballast-system", nil)
	if _, err := ks.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("ks.Reconcile: %v", err)
	}
	return ks
}

func defaultBallastConfig() *ballastv1.BallastConfig {
	return &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app"}},
	}
}

// readyProfile returns a WorkloadProfile named "web" (matching pod label app=web)
// with cpu+memory recommendations and meetsThreshold=true.
func readyProfile() *ballastv1.WorkloadProfile {
	return &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status: ballastv1.WorkloadProfileStatus{
			MeetsThreshold: true,
			Containers: []ballastv1.ContainerProfile{
				{
					Name: "app",
					Recommendations: map[string]ballastv1.ResourceRecommendation{
						"cpu":    {Request: "200m", Limit: "400m"},
						"memory": {Request: "128Mi", Limit: "256Mi"},
					},
				},
			},
		},
	}
}

func notReadyProfile() *ballastv1.WorkloadProfile {
	return &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status:     ballastv1.WorkloadProfileStatus{MeetsThreshold: false},
	}
}

// makeRequest builds a synthetic pod CREATE admission request.
func makeRequest(pod *corev1.Pod) admission.Request {
	raw, _ := json.Marshal(pod)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
}

// testPod returns a minimal pod with the given annotations and label app=web.
func testPod(name string, anns map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: anns,
			Labels:      map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
			},
		},
	}
}

func hasResourcePatch(resp admission.Response) bool {
	for _, p := range resp.Patches {
		if strings.Contains(p.Path, "resources") || strings.Contains(p.Path, "annotations") {
			return true
		}
	}
	return false
}

// -- unit tests (fake client) --

func TestPodMutator_KillSwitch(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, activeKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got denied: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches when kill switch active, got %d", len(resp.Patches))
	}
}

func TestPodMutator_InvalidAnnotations(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationApply: "true", // missing measure
	})))

	if resp.Allowed {
		t.Error("expected Denied for invalid annotations, got Allowed")
	}
}

func TestPodMutator_NoApplyAnnotation(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed for measure-only pod, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches, got %d", len(resp.Patches))
	}
}

func TestPodMutator_DryRunApply(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), true /* dryRunApply */, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed in dry-run, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches in dry-run mode, got %d", len(resp.Patches))
	}
}

func TestPodMutator_SuccessfulPatch(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	if !hasResourcePatch(resp) {
		t.Errorf("expected resource or annotation patches, got: %v", resp.Patches)
	}
}

func TestPodMutator_ProfileNotReady(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), notReadyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed when profile not ready, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches when profile not ready, got %d", len(resp.Patches))
	}
}

func TestPodMutator_ProfileNotFound(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig()) // no profile object
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed when profile not found, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches when profile not found, got %d", len(resp.Patches))
	}
}

func TestPodMutator_Autoresize_BelowThreshold(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), notReadyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationAutoresize: "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed (measure-only) for autoresize below threshold, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches below threshold, got %d", len(resp.Patches))
	}
}

func TestPodMutator_Autoresize_AboveThreshold(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationAutoresize: "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed with patch for autoresize above threshold, got: %s", resp.Result.Message)
	}
	if !hasResourcePatch(resp) {
		t.Errorf("expected patches for autoresize above threshold, got: %v", resp.Patches)
	}
}

func TestPodMutator_MalformedRequest(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: []byte("not-valid-json")},
		},
	}
	resp := m.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected error response for malformed JSON, got Allowed")
	}
}

func TestPodMutator_BallastConfigMissing(t *testing.T) {
	fc := newFakeClient() // no BallastConfig
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed when BallastConfig missing, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches, got %d", len(resp.Patches))
	}
}

func TestPodMutator_MissingIdentityLabel(t *testing.T) {
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app", "env"}},
	}
	fc := newFakeClient(cfg)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	// pod only has "app", missing "env"
	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed when identity label missing, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches, got %d", len(resp.Patches))
	}
}

func TestPodMutator_PolicyRefStamped(t *testing.T) {
	policy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-policy"},
		Spec:       ballastv1.ClusterResourcePolicySpec{},
	}
	fc := newFakeClient(defaultBallastConfig(), readyProfile(), policy)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	found := false
	for _, p := range resp.Patches {
		if strings.Contains(p.Path, "policy-ref") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected policy-ref annotation patch, patches were: %v", resp.Patches)
	}
}

func TestPodMutator_UnmatchedContainer(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	// pod has an extra "sidecar" container not in the profile
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "p",
			Namespace:   "default",
			Annotations: map[string]string{validation.AnnotationMeasure: "true", validation.AnnotationApply: "true"},
			Labels:      map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{Name: "sidecar", Image: "envoy"},
			},
		},
	}
	resp := m.Handle(context.Background(), makeRequest(pod))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	if !hasResourcePatch(resp) {
		t.Errorf("expected patches for matched container, got none")
	}
}

func TestPodMutator_EmptyRecommendationField(t *testing.T) {
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status: ballastv1.WorkloadProfileStatus{
			MeetsThreshold: true,
			Containers: []ballastv1.ContainerProfile{
				{
					Name: "app",
					Recommendations: map[string]ballastv1.ResourceRecommendation{
						"cpu": {Request: "200m", Limit: ""}, // empty Limit
					},
				},
			},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	if !hasResourcePatch(resp) {
		t.Errorf("expected patch for non-empty Request field, got none")
	}
}

func TestPodMutator_InvalidQuantity(t *testing.T) {
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status: ballastv1.WorkloadProfileStatus{
			MeetsThreshold: true,
			Containers: []ballastv1.ContainerProfile{
				{
					Name: "app",
					Recommendations: map[string]ballastv1.ResourceRecommendation{
						"cpu": {Request: "not-a-quantity", Limit: "not-a-quantity"},
					},
				},
			},
		},
	}
	fc := newFakeClient(defaultBallastConfig(), profile)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", map[string]string{
		validation.AnnotationMeasure: "true",
		validation.AnnotationApply:   "true",
	})))

	// webhook allows the pod even when recommendations can't be parsed
	if !resp.Allowed {
		t.Fatalf("expected Allowed even with invalid quantity, got: %s", resp.Result.Message)
	}
}

func TestPodMutator_OwnerReference(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	isController := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "default",
			Annotations: map[string]string{
				validation.AnnotationMeasure: "true",
				validation.AnnotationApply:   "true",
			},
			Labels: map[string]string{"app": "web"},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "web-rs", Controller: &isController},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	resp := m.Handle(context.Background(), makeRequest(pod))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	if !hasResourcePatch(resp) {
		t.Errorf("expected patches for pod with owner reference, got none")
	}
}

// -- envtest integration test --

func TestPodMutator_SetupWithManager(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 newScheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	ks := killswitch.New(mgr.GetClient(), "ballast-system", nil)
	webhook.NewPodMutator(mgr.GetClient(), ks, false, nil).SetupWithManager(mgr)
}
