package workloadwatcher

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	ballastv1 "github.com/tight-line/ballast/api/v1"
)

// These tests live in the internal package so they can exercise the unexported
// watch predicates and map functions directly, covering every closure branch.

func internalScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = ballastv1.AddToScheme(s)
	return s
}

func TestProfileDeletedPredicate(t *testing.T) {
	p := profileDeleted()
	if p.Create(event.CreateEvent{}) {
		t.Error("create should not be admitted")
	}
	if p.Update(event.UpdateEvent{}) {
		t.Error("update should not be admitted")
	}
	if p.Generic(event.GenericEvent{}) {
		t.Error("generic should not be admitted")
	}
	if !p.Delete(event.DeleteEvent{}) {
		t.Error("delete should be admitted")
	}
}

func TestIdentityLabelsChangedPredicate(t *testing.T) {
	p := identityLabelsChanged()
	if p.Create(event.CreateEvent{}) {
		t.Error("create should not be admitted")
	}
	if p.Delete(event.DeleteEvent{}) {
		t.Error("delete should not be admitted")
	}
	if p.Generic(event.GenericEvent{}) {
		t.Error("generic should not be admitted")
	}

	mk := func(labels ...string) *ballastv1.BallastConfig {
		return &ballastv1.BallastConfig{Spec: ballastv1.BallastConfigSpec{IdentityLabels: labels}}
	}
	if p.Update(event.UpdateEvent{ObjectOld: mk("app"), ObjectNew: mk("app")}) {
		t.Error("update with unchanged identityLabels should not be admitted")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: mk("app"), ObjectNew: mk("app", "tier")}) {
		t.Error("update with changed identityLabels should be admitted")
	}
	// Non-BallastConfig objects → type assertion fails → not admitted.
	if p.Update(event.UpdateEvent{ObjectOld: &corev1.Pod{}, ObjectNew: &corev1.Pod{}}) {
		t.Error("update with non-BallastConfig objects should not be admitted")
	}
}

func TestPodsForProfile(t *testing.T) {
	match := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "m", Namespace: "default",
		Annotations: map[string]string{AnnotationProfileRef: "web"},
	}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "o", Namespace: "default",
		Annotations: map[string]string{AnnotationProfileRef: "db"},
	}}
	fc := fake.NewClientBuilder().WithScheme(internalScheme()).WithObjects(match, other).Build()
	r := &PodReconciler{client: fc}

	reqs := r.podsForProfile(context.Background(), &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
	})
	if len(reqs) != 1 || reqs[0].Name != "m" {
		t.Errorf("podsForProfile: got %v, want a single request for pod m", reqs)
	}
}

func TestPodsForConfig(t *testing.T) {
	managed := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "m", Namespace: "default",
		Finalizers: []string{FinalizerName},
	}}
	annotated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "a", Namespace: "default",
		Annotations: map[string]string{AnnotationMeasure: "true"},
	}}
	unmanaged := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"}}
	fc := fake.NewClientBuilder().WithScheme(internalScheme()).
		WithObjects(managed, annotated, unmanaged).Build()
	r := &PodReconciler{client: fc}

	reqs := r.podsForConfig(context.Background(), &ballastv1.BallastConfig{})
	if len(reqs) != 2 {
		t.Errorf("podsForConfig: got %d requests, want 2 (managed + annotated)", len(reqs))
	}
}
