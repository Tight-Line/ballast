package killswitch_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/killswitch"
)

const testNamespace = "ballast-system"

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = ballastv1.AddToScheme(s)
	return s
}

func reconcileOnce(t *testing.T, ks *killswitch.KillSwitch) {
	t.Helper()
	_, err := ks.Reconcile(context.Background(), reconcile.Request{})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
}

// --- unit tests (fake client) ---

func TestKillSwitch_Inactive(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	ks := killswitch.New(fc, testNamespace, nil)
	reconcileOnce(t, ks)
	if ks.IsActive() {
		t.Error("expected kill switch inactive when neither trigger is set")
	}
	if ks.Reason() != "" {
		t.Errorf("expected empty reason, got %q", ks.Reason())
	}
}

func TestKillSwitch_ConfigMapPresent(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: killswitch.ConfigMapName, Namespace: testNamespace,
	}}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()
	ks := killswitch.New(fc, testNamespace, nil)
	reconcileOnce(t, ks)
	if !ks.IsActive() {
		t.Error("expected kill switch active when ConfigMap present")
	}
	want := "ConfigMap " + killswitch.ConfigMapName
	if ks.Reason() != want {
		t.Errorf("reason: got %q, want %q", ks.Reason(), want)
	}
}

func TestKillSwitch_ConfigMapAbsent(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	ks := killswitch.New(fc, testNamespace, nil)
	reconcileOnce(t, ks)
	if ks.IsActive() {
		t.Error("expected kill switch inactive when ConfigMap absent")
	}
}

func TestKillSwitch_BallastConfigSuspendedTrue(t *testing.T) {
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app"}, Suspended: true},
	}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cfg).Build()
	ks := killswitch.New(fc, testNamespace, nil)
	reconcileOnce(t, ks)
	if !ks.IsActive() {
		t.Error("expected kill switch active when BallastConfig.spec.suspended=true")
	}
	if ks.Reason() != "BallastConfig.spec.suspended" {
		t.Errorf("unexpected reason: %q", ks.Reason())
	}
}

func TestKillSwitch_BallastConfigSuspendedFalse(t *testing.T) {
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app"}, Suspended: false},
	}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cfg).Build()
	ks := killswitch.New(fc, testNamespace, nil)
	reconcileOnce(t, ks)
	if ks.IsActive() {
		t.Error("expected kill switch inactive when BallastConfig.spec.suspended=false")
	}
}

func TestKillSwitch_BothActive(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: killswitch.ConfigMapName, Namespace: testNamespace,
	}}
	cfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app"}, Suspended: true},
	}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm, cfg).Build()
	ks := killswitch.New(fc, testNamespace, nil)
	reconcileOnce(t, ks)
	if !ks.IsActive() {
		t.Error("expected kill switch active when both triggers set")
	}
	want := "ConfigMap " + killswitch.ConfigMapName + " and BallastConfig.spec.suspended"
	if ks.Reason() != want {
		t.Errorf("reason: got %q, want %q", ks.Reason(), want)
	}
}

func TestKillSwitch_Deactivation(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: killswitch.ConfigMapName, Namespace: testNamespace,
	}}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cm).Build()
	ks := killswitch.New(fc, testNamespace, nil)

	reconcileOnce(t, ks)
	if !ks.IsActive() {
		t.Fatal("expected active after ConfigMap created")
	}

	if err := fc.Delete(ctx, cm); err != nil {
		t.Fatalf("Delete ConfigMap: %v", err)
	}
	reconcileOnce(t, ks)
	if ks.IsActive() {
		t.Error("expected inactive after ConfigMap deleted")
	}
}

func TestKillSwitch_NoChangeOnRepeatReconcile(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	ks := killswitch.New(fc, testNamespace, nil)
	reconcileOnce(t, ks)
	reconcileOnce(t, ks) // second reconcile with same state — exercises changed=false path
	if ks.IsActive() {
		t.Error("expected still inactive")
	}
}

// --- envtest test: exercises SetupWithManager, watches, and predicates ---

func TestKillSwitch_SetupWithManager(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	scheme := newScheme()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	ks := killswitch.New(mgr.GetClient(), testNamespace, nil)
	if err := ks.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgrErr := make(chan error, 1)
	go func() { mgrErr <- mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	c := mgr.GetClient()

	// Create the namespace so ConfigMap creation succeeds.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	// Create kill-switch ConfigMap; reconcile should activate.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: killswitch.ConfigMapName, Namespace: testNamespace,
	}}
	if err := c.Create(ctx, cm); err != nil {
		t.Fatalf("create ConfigMap: %v", err)
	}
	waitFor(t, ks.IsActive, true, "kill switch to activate after ConfigMap creation")

	// Create a BallastConfig (exercises bcFilter predicate).
	ballastCfg := &ballastv1.BallastConfig{
		ObjectMeta: metav1.ObjectMeta{Name: killswitch.BallastConfigName},
		Spec:       ballastv1.BallastConfigSpec{IdentityLabels: []string{"app"}},
	}
	if err := c.Create(ctx, ballastCfg); err != nil {
		t.Fatalf("create BallastConfig: %v", err)
	}

	// Delete ConfigMap; reconcile should deactivate (BallastConfig.suspended=false).
	if err := c.Delete(ctx, cm); err != nil {
		t.Fatalf("delete ConfigMap: %v", err)
	}
	waitFor(t, ks.IsActive, false, "kill switch to deactivate after ConfigMap deletion")
}

// waitFor polls fn until it returns want, or the test fails after 10 s.
func waitFor(t *testing.T, fn func() bool, want bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("timed out waiting for %s", desc)
}
