/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package crdapply_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	crdmanifests "github.com/tight-line/ballast/config/crd/bases"
	"github.com/tight-line/ballast/internal/crdapply"
)

// newEnvtestClient starts a bare envtest control plane (no CRDs pre-installed,
// unlike the controller suites) and returns a client against it.
func newEnvtestClient(t *testing.T) client.Client {
	t.Helper()
	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apiextensions to scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestApply_InstallsAndUpgradesCRDs(t *testing.T) {
	ctx := context.Background()
	c := newEnvtestClient(t)

	// First apply: creates every embedded CRD on the virgin control plane.
	if err := crdapply.Apply(ctx, c, crdmanifests.FS, logr.Discard()); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	key := types.NamespacedName{Name: "workloadprofiles.ballast.tightlinesoftware.com"}
	if err := c.Get(ctx, key, &crd); err != nil {
		t.Fatalf("get workloadprofiles CRD: %v", err)
	}

	// The applied schema is current: the State printer column exists (the
	// exact field Helm's install-only crds/ handling failed to deliver).
	var cols []string
	for _, col := range crd.Spec.Versions[0].AdditionalPrinterColumns {
		cols = append(cols, col.Name)
	}
	if !slices.Contains(cols, "State") {
		t.Errorf("printer columns = %v, want State present", cols)
	}

	// Ownership: the spec is managed by our field manager.
	var managers []string
	for _, mf := range crd.ManagedFields {
		managers = append(managers, mf.Manager)
	}
	if !slices.Contains(managers, crdapply.FieldManager) {
		t.Errorf("field managers = %v, want %s present", managers, crdapply.FieldManager)
	}

	// Second apply: idempotent upgrade path, converges without error or
	// spurious generation bumps.
	genBefore := crd.Generation
	if err := crdapply.Apply(ctx, c, crdmanifests.FS, logr.Discard()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if err := c.Get(ctx, key, &crd); err != nil {
		t.Fatalf("re-get workloadprofiles CRD: %v", err)
	}
	if crd.Generation != genBefore {
		t.Errorf("generation changed on no-op re-apply: %d -> %d", genBefore, crd.Generation)
	}
}

func TestApply_RejectsBadInput(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		fsys    fstest.MapFS
		wantErr string
	}{
		{
			name:    "empty FS",
			fsys:    fstest.MapFS{},
			wantErr: "no CRD manifests",
		},
		{
			name: "malformed yaml",
			fsys: fstest.MapFS{
				"bad.yaml": &fstest.MapFile{Data: []byte("{{ not yaml")},
			},
			wantErr: "parsing bad.yaml",
		},
		{
			name: "not a CRD",
			fsys: fstest.MapFS{
				"sneaky.yaml": &fstest.MapFile{Data: []byte(
					"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: sneaky\n")},
			},
			wantErr: "expected a CustomResourceDefinition",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No API calls happen before validation fails, so no client is needed.
			err := crdapply.Apply(ctx, nil, tt.fsys, logr.Discard())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Apply error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestApply_SurfacesApplyErrors(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("apiserver on fire")
	c := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return wantErr
			},
		}).
		Build()

	err := crdapply.Apply(ctx, c, crdmanifests.FS, logr.Discard())
	if !errors.Is(err, wantErr) {
		t.Errorf("Apply error = %v, want wrapping %v", err, wantErr)
	}
}
