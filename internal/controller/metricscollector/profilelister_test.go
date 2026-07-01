/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metricscollector_test

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/controller/metricscollector"
)

func TestProfileLister_ListProfiles(t *testing.T) {
	ready := &ballastv1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: "ready"}}
	ready.Status.TupleLabels = map[string]string{"example.com/business-unit": "payments"}
	ready.Status.MeetsThreshold = true

	accruing := &ballastv1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: "accruing"}}
	accruing.Status.TupleLabels = map[string]string{"example.com/business-unit": "search"}
	accruing.Status.MeetsThreshold = false

	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&ballastv1.WorkloadProfile{}).
		WithObjects(ready, accruing).
		Build()

	snaps, err := metricscollector.NewProfileLister(fc).ListProfiles(context.Background())
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshot count = %d, want 2", len(snaps))
	}

	byBU := make(map[string]bool) // business-unit -> Ready
	for _, s := range snaps {
		byBU[s.Labels["example.com/business-unit"]] = s.Ready
	}
	if !byBU["payments"] {
		t.Errorf("payments profile Ready = false, want true")
	}
	if byBU["search"] {
		t.Errorf("search profile Ready = true, want false")
	}
}

func TestProfileLister_ListError(t *testing.T) {
	boom := errors.New("list boom")
	fc := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).
		Build()

	if _, err := metricscollector.NewProfileLister(fc).ListProfiles(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("ListProfiles error = %v, want wrapped %v", err, boom)
	}
}
