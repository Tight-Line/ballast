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

	promclient "github.com/prometheus/client_golang/prometheus"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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
	"github.com/tight-line/ballast/internal/metrics"
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

// testPod returns a minimal pod enrolled at the given mode (unenrolled when mode
// is "") with identity label app=web.
func testPod(name, mode string) *corev1.Pod {
	labels := map[string]string{"app": "web"}
	if mode != "" {
		labels[validation.LabelMode] = mode
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
			},
		},
	}
}

// newMetricsRecorder returns a Recorder backed by a Prometheus registry so tests
// can assert on the series the webhook records.
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

func hasResourcePatch(resp admission.Response) bool {
	for _, p := range resp.Patches {
		if strings.Contains(p.Path, "resources") || strings.Contains(p.Path, "annotations") {
			return true
		}
	}
	return false
}

// policyRefValue extracts the stamped policy-ref from an admission response. It
// handles both patch shapes: a per-key add when the pod already had annotations,
// and a whole-object add of /metadata/annotations when it did not (the common
// case now that enrollment is a label and a minimal pod carries no annotations).
func policyRefValue(resp admission.Response) string {
	const key = "ballast.tightlinesoftware.com/policy-ref"
	for _, p := range resp.Patches {
		switch {
		case strings.Contains(p.Path, "policy-ref"):
			if s, ok := p.Value.(string); ok {
				return s
			}
		case p.Path == "/metadata/annotations":
			if m, ok := p.Value.(map[string]any); ok {
				if s, ok := m[key].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// -- unit tests (fake client) --

func TestPodMutator_KillSwitch(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, activeKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got denied: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches when kill switch active, got %d", len(resp.Patches))
	}
}

func TestPodMutator_InvalidMode(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", "frobnicate")))

	if resp.Allowed {
		t.Error("expected Denied for invalid mode label, got Allowed")
	}
}

func TestPodMutator_NoApplyMode(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeMeasure)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeResize)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeResize)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

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
	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

	if !resp.Allowed {
		t.Fatalf("expected Allowed when identity label missing, got: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches, got %d", len(resp.Patches))
	}
}

func TestPodMutator_PolicyRefStamped(t *testing.T) {
	// ClusterResourcePolicy: policy-ref value is just the policy name (no namespace prefix).
	policy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-policy"},
		Spec:       ballastv1.ClusterResourcePolicySpec{},
	}
	fc := newFakeClient(defaultBallastConfig(), readyProfile(), policy)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	if got := policyRefValue(resp); got != "default-policy" {
		t.Errorf("policy-ref: got %q, want %q", got, "default-policy")
	}
}

func TestPodMutator_PolicyRefNamespaced(t *testing.T) {
	// ResourcePolicy: policy-ref value must include "namespace/name" so observers
	// can distinguish it from a same-named ClusterResourcePolicy.
	policy := &ballastv1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "team-policy", Namespace: "default"},
		Spec:       ballastv1.ResourcePolicySpec{},
	}
	fc := newFakeClient(defaultBallastConfig(), readyProfile(), policy)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	if got := policyRefValue(resp); got != "default/team-policy" {
		t.Errorf("policy-ref: got %q, want %q", got, "default/team-policy")
	}
}

func TestPodMutator_UnmatchedContainer(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, nil)

	// pod has an extra "sidecar" container not in the profile
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "default",
			Labels:    map[string]string{"app": "web", validation.LabelMode: validation.ModeApply},
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

// TestPodMutator_ApplyAppliedMetric asserts a mutation that changes container
// resources records ballast.apply.applied with profile, policy, and namespace
// attributes.
func TestPodMutator_ApplyAppliedMetric(t *testing.T) {
	policy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-policy"},
		Spec:       ballastv1.ClusterResourcePolicySpec{},
	}
	fc := newFakeClient(defaultBallastConfig(), readyProfile(), policy)
	rec, reg := newMetricsRecorder(t)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, rec)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}

	got, labels := counterSeries(t, reg, "ballast_apply_applied_total")
	if got != 1 {
		t.Fatalf("ballast_apply_applied_total = %v, want 1", got)
	}
	if labels["profile"] != "web" || labels["policy"] != "default-policy" || labels["namespace"] != "default" {
		t.Errorf("profile/policy/namespace attrs = %q/%q/%q",
			labels["profile"], labels["policy"], labels["namespace"])
	}
}

// TestPodMutator_AppliesRestartableInitSidecar asserts the webhook patches a
// restartable-init (native sidecar) container on spec.initContainers. The only
// container matching the profile is the sidecar, so a recorded apply proves the
// init-container path ran (#30).
func TestPodMutator_AppliesRestartableInitSidecar(t *testing.T) {
	restartAlways := corev1.ContainerRestartPolicyAlways
	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Status: ballastv1.WorkloadProfileStatus{
			MeetsThreshold: true,
			Containers: []ballastv1.ContainerProfile{{
				Name: "otc",
				Recommendations: map[string]ballastv1.ResourceRecommendation{
					"cpu": {Request: "200m", Limit: "400m"},
				},
			}},
		},
	}
	policy := &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-policy"},
		Spec:       ballastv1.ClusterResourcePolicySpec{},
	}
	fc := newFakeClient(defaultBallastConfig(), profile, policy)
	rec, reg := newMetricsRecorder(t)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, rec)

	pod := testPod("p", validation.ModeApply)
	// The regular "app" container has no recommendation; the profile's "otc"
	// recommendation matches only the restartable-init sidecar.
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "otc", Image: "otc", RestartPolicy: &restartAlways},
	}

	resp := m.Handle(context.Background(), makeRequest(pod))
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}
	if got, _ := counterSeries(t, reg, "ballast_apply_applied_total"); got != 1 {
		t.Fatalf("ballast_apply_applied_total = %v, want 1 (restartable-init sidecar patched)", got)
	}
}

// TestPodMutator_ApplyAppliedMetric_AnnotationOnlyMutation asserts a mutation whose
// patch touches no container resources (no container matches the profile) reports
// result=mutated but does not record ballast.apply.applied.
func TestPodMutator_ApplyAppliedMetric_AnnotationOnlyMutation(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	rec, reg := newMetricsRecorder(t)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, rec)

	// The pod's only container is absent from the profile, so the patch carries
	// no resource changes.
	pod := testPod("p", validation.ModeApply)
	pod.Spec.Containers = []corev1.Container{{Name: "sidecar", Image: "envoy"}}

	resp := m.Handle(context.Background(), makeRequest(pod))
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}

	// Prove the mutate path ran: the webhook counted the invocation as mutated.
	mutations, mutationLabels := counterSeries(t, reg, "ballast_webhook_mutations_total")
	if mutations != 1 || mutationLabels["result"] != "mutated" {
		t.Fatalf("ballast_webhook_mutations_total = %v (result=%q), want 1 with result=mutated",
			mutations, mutationLabels["result"])
	}
	if got, _ := counterSeries(t, reg, "ballast_apply_applied_total"); got != 0 {
		t.Errorf("ballast_apply_applied_total = %v, want 0 when no resources changed", got)
	}
	skipped, skipLabels := counterSeries(t, reg, "ballast_apply_skipped_total")
	if skipped != 1 || skipLabels["reason"] != "no_change" {
		t.Errorf("ballast_apply_skipped_total = %v (reason=%q), want 1 with reason=no_change",
			skipped, skipLabels["reason"])
	}
}

// TestPodMutator_ApplySkippedMetric_NotReady asserts a pod that requests apply
// against a below-threshold profile records ballast.apply.skipped{reason=not_ready}
// carrying the profile attributes.
func TestPodMutator_ApplySkippedMetric_NotReady(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), notReadyProfile())
	rec, reg := newMetricsRecorder(t)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, rec)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}

	got, labels := counterSeries(t, reg, "ballast_apply_skipped_total")
	if got != 1 || labels["reason"] != "not_ready" {
		t.Fatalf("ballast_apply_skipped_total = %v (reason=%q), want 1 with reason=not_ready",
			got, labels["reason"])
	}
	if labels["profile"] != "web" || labels["namespace"] != "default" {
		t.Errorf("profile/namespace attrs = %q/%q", labels["profile"], labels["namespace"])
	}
}

// TestPodMutator_ApplySkippedMetric_NoProfile asserts a pod that requests apply
// before any profile exists records ballast.apply.skipped{reason=no_profile}
// without profile attributes.
func TestPodMutator_ApplySkippedMetric_NoProfile(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig())
	rec, reg := newMetricsRecorder(t)
	m := webhook.NewPodMutator(fc, inactiveKS(t), false, rec)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}

	got, labels := counterSeries(t, reg, "ballast_apply_skipped_total")
	if got != 1 || labels["reason"] != "no_profile" {
		t.Fatalf("ballast_apply_skipped_total = %v (reason=%q), want 1 with reason=no_profile",
			got, labels["reason"])
	}
	if _, ok := labels["profile"]; ok {
		t.Errorf("profile attr present on no_profile skip: %q", labels["profile"])
	}
}

// TestPodMutator_ApplySkippedMetric_DryRun asserts a suppressed apply that would
// have changed resources records reason=dry_run and no apply.applied.
func TestPodMutator_ApplySkippedMetric_DryRun(t *testing.T) {
	fc := newFakeClient(defaultBallastConfig(), readyProfile())
	rec, reg := newMetricsRecorder(t)
	m := webhook.NewPodMutator(fc, inactiveKS(t), true /* dryRunApply */, rec)

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %s", resp.Result.Message)
	}

	got, labels := counterSeries(t, reg, "ballast_apply_skipped_total")
	if got != 1 || labels["reason"] != "dry_run" {
		t.Fatalf("ballast_apply_skipped_total = %v (reason=%q), want 1 with reason=dry_run",
			got, labels["reason"])
	}
	if applied, _ := counterSeries(t, reg, "ballast_apply_applied_total"); applied != 0 {
		t.Errorf("ballast_apply_applied_total = %v, want 0 in dry-run", applied)
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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

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

	resp := m.Handle(context.Background(), makeRequest(testPod("p", validation.ModeApply)))

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
			Labels:    map[string]string{"app": "web", validation.LabelMode: validation.ModeApply},
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
