package policystatus_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/policystatus"
	"github.com/tight-line/ballast/internal/naming"
)

func newFakeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = ballastv1.AddToScheme(s)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&ballastv1.ClusterResourcePolicy{}, &ballastv1.ResourcePolicy{}).
		WithObjects(objs...).
		Build()
}

func TestReconcile_ClusterPolicy_PublishesDiscriminator(t *testing.T) {
	ctx := context.Background()
	crp := &ballastv1.ClusterResourcePolicy{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}}
	fc := newFakeClient(crp)

	if _, err := policystatus.NewCluster(fc).Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "fleet"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.ClusterResourcePolicy
	if err := fc.Get(ctx, types.NamespacedName{Name: "fleet"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := naming.PolicyDiscriminator(ballastv1.KindClusterResourcePolicy, "", "fleet")
	if got.Status.ProfileDiscriminator != want {
		t.Errorf("profileDiscriminator = %q, want %q", got.Status.ProfileDiscriminator, want)
	}
}

// The token must distinguish the kind, so a same-named ResourcePolicy publishes a
// different value than the ClusterResourcePolicy above.
func TestReconcile_ResourcePolicy_PublishesDiscriminator(t *testing.T) {
	ctx := context.Background()
	rp := &ballastv1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "fleet"},
	}
	fc := newFakeClient(rp)

	if _, err := policystatus.NewNamespaced(fc).Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "fleet"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got ballastv1.ResourcePolicy
	if err := fc.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "fleet"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := naming.PolicyDiscriminator(ballastv1.KindResourcePolicy, "team-a", "fleet")
	if got.Status.ProfileDiscriminator != want {
		t.Errorf("profileDiscriminator = %q, want %q", got.Status.ProfileDiscriminator, want)
	}
	clusterToken := naming.PolicyDiscriminator(ballastv1.KindClusterResourcePolicy, "", "fleet")
	if got.Status.ProfileDiscriminator == clusterToken {
		t.Error("a namespaced policy must not share a token with a same-named cluster policy")
	}
}

// The token is a pure function of the object's identity, so a second reconcile has
// nothing to write. Without this guard every policy event would generate a status
// write, and each write another event.
func TestReconcile_AlreadyPublished_NoWrite(t *testing.T) {
	ctx := context.Background()
	crp := &ballastv1.ClusterResourcePolicy{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}}
	fc := newFakeClient(crp)
	r := policystatus.NewCluster(fc)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "fleet"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	var first ballastv1.ClusterResourcePolicy
	if err := fc.Get(ctx, req.NamespacedName, &first); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	var second ballastv1.ClusterResourcePolicy
	if err := fc.Get(ctx, req.NamespacedName, &second); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("status rewritten on a no-op reconcile: %s -> %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}

func TestReconcile_NotFound(t *testing.T) {
	fc := newFakeClient()

	for name, r := range map[string]*policystatus.Reconciler{
		"cluster":    policystatus.NewCluster(fc),
		"namespaced": policystatus.NewNamespaced(fc),
	} {
		result, err := r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "gone"},
		})
		if err != nil {
			t.Errorf("%s: Reconcile of a deleted policy should not error: %v", name, err)
		}
		if result.RequeueAfter != 0 {
			t.Errorf("%s: unexpected requeue %v", name, result.RequeueAfter)
		}
	}
}

// TestSetupWithManager registers both reconcilers against a real API server and
// asserts the discriminator is published end to end.
func TestSetupWithManager(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = ballastv1.AddToScheme(s)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 s,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if err := policystatus.NewCluster(mgr.GetClient()).SetupWithManager(mgr); err != nil {
		t.Fatalf("cluster SetupWithManager: %v", err)
	}
	if err := policystatus.NewNamespaced(mgr.GetClient()).SetupWithManager(mgr); err != nil {
		t.Fatalf("namespaced SetupWithManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}
	c := mgr.GetClient()

	crp := &ballastv1.ClusterResourcePolicy{ObjectMeta: metav1.ObjectMeta{Name: "fleet"}}
	if err := c.Create(ctx, crp); err != nil {
		t.Fatalf("create ClusterResourcePolicy: %v", err)
	}

	want := naming.PolicyDiscriminator(ballastv1.KindClusterResourcePolicy, "", "fleet")
	deadline := time.Now().Add(20 * time.Second)
	for {
		var got ballastv1.ClusterResourcePolicy
		if err := c.Get(ctx, types.NamespacedName{Name: "fleet"}, &got); err == nil &&
			got.Status.ProfileDiscriminator == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for profileDiscriminator %q", want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
