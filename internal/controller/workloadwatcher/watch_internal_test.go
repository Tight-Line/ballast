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
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/validation"
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

	mkNamed := func(name string, labels ...string) *ballastv1.BallastConfig {
		return &ballastv1.BallastConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       ballastv1.BallastConfigSpec{IdentityLabels: labels},
		}
	}
	mk := func(labels ...string) *ballastv1.BallastConfig {
		return mkNamed(killswitch.BallastConfigName, labels...)
	}

	// Creation of the canonical config is admitted: pods reconciled while it was
	// absent were skipped, and a delete + re-apply never fires the update path.
	if !p.Create(event.CreateEvent{Object: mk("app")}) {
		t.Error("create of the canonical BallastConfig should be admitted")
	}
	if p.Create(event.CreateEvent{Object: mkNamed("stray", "app")}) {
		t.Error("create of a non-canonical BallastConfig should not be admitted")
	}
	if p.Delete(event.DeleteEvent{}) {
		t.Error("delete should not be admitted")
	}
	if p.Generic(event.GenericEvent{}) {
		t.Error("generic should not be admitted")
	}

	if p.Update(event.UpdateEvent{ObjectOld: mk("app"), ObjectNew: mk("app")}) {
		t.Error("update with unchanged identityLabels should not be admitted")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: mk("app"), ObjectNew: mk("app", "tier")}) {
		t.Error("update with changed identityLabels should be admitted")
	}
	if p.Update(event.UpdateEvent{ObjectOld: mkNamed("stray", "app"), ObjectNew: mkNamed("stray", "app", "tier")}) {
		t.Error("update of a non-canonical BallastConfig should not be admitted")
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
	fc := fake.NewClientBuilder().
		WithScheme(internalScheme()).
		WithIndex(&corev1.Pod{}, PodProfileRefField, PodProfileRefIndexer).
		WithObjects(match, other).
		Build()
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
	enrolled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "a", Namespace: "default",
		Labels: map[string]string{validation.LabelMode: validation.ModeMeasure},
	}}
	unmanaged := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"}}
	fc := fake.NewClientBuilder().WithScheme(internalScheme()).
		WithObjects(managed, enrolled, unmanaged).Build()
	r := &PodReconciler{client: fc}

	reqs := r.podsForConfig(context.Background(), &ballastv1.BallastConfig{})
	if len(reqs) != 2 {
		t.Errorf("podsForConfig: got %d requests, want 2 (managed + enrolled)", len(reqs))
	}
}

// enrolledPod builds a pod carrying the given profile-ref and our finalizer, as
// countActiveWorkloads expects index-served list items to look.
func enrolledPod(name, ref string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default",
		Annotations: map[string]string{AnnotationProfileRef: ref},
		Finalizers:  []string{FinalizerName},
	}}
}

// TestCountActiveWorkloads pins the self-override semantics against an
// index-served pod list, which reflects the cached (possibly pre-stamp)
// profile-ref annotation:
//   - a pod newly stamped in the same reconcile is still indexed under its old
//     ref, so it is absent from the new profile's list and must be inserted;
//   - the same pod does appear in the old profile's list and must be excluded.
//
// A refactor that drops the insert half silently undercounts the target profile
// after every migration and first enrollment.
func TestCountActiveWorkloads(t *testing.T) {
	self := &podEnrollment{namespace: "default", name: "migrating", ref: "new"}

	tests := []struct {
		name     string
		pods     []corev1.Pod
		profName string
		self     *podEnrollment
		want     int32
	}{
		{
			name:     "self absent from target profile's indexed list is inserted",
			pods:     []corev1.Pod{enrolledPod("other", "new")},
			profName: "new",
			self:     self,
			want:     2,
		},
		{
			name:     "self present under old ref is excluded from the old profile",
			pods:     []corev1.Pod{enrolledPod("migrating", "old"), enrolledPod("stays", "old")},
			profName: "old",
			self:     self,
			want:     1,
		},
		{
			name:     "self present under target ref is not double-counted",
			pods:     []corev1.Pod{enrolledPod("migrating", "new")},
			profName: "new",
			self:     self,
			want:     1,
		},
		{
			name:     "un-enrolling self (empty ref) is excluded and never inserted",
			pods:     []corev1.Pod{enrolledPod("migrating", "old")},
			profName: "old",
			self:     &podEnrollment{namespace: "default", name: "migrating", ref: ""},
			want:     0,
		},
		{
			name: "nil self counts only live finalized pods",
			pods: []corev1.Pod{
				enrolledPod("live", "web"),
				func() corev1.Pod {
					p := enrolledPod("terminating", "web")
					now := metav1.Now()
					p.DeletionTimestamp = &now
					return p
				}(),
				{ObjectMeta: metav1.ObjectMeta{
					Name: "no-finalizer", Namespace: "default",
					Annotations: map[string]string{AnnotationProfileRef: "web"},
				}},
			},
			profName: "web",
			self:     nil,
			want:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countActiveWorkloads(tt.pods, tt.profName, tt.self); got != tt.want {
				t.Errorf("countActiveWorkloads(%q) = %d, want %d", tt.profName, got, tt.want)
			}
		})
	}
}
