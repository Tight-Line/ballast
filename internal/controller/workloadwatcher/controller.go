package workloadwatcher

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/killswitch"
	"github.com/tight-line/ballast/internal/logger"
	"github.com/tight-line/ballast/internal/metrics"
	"github.com/tight-line/ballast/internal/plugin"
	"github.com/tight-line/ballast/internal/store"
)

const (
	AnnotationMeasure    = "ballast.tightlinesoftware.com/measure"
	AnnotationApply      = "ballast.tightlinesoftware.com/apply"
	AnnotationResize     = "ballast.tightlinesoftware.com/resize"
	AnnotationAutoresize = "ballast.tightlinesoftware.com/autoresize"
	AnnotationProfileRef = "ballast.tightlinesoftware.com/profile-ref"

	FinalizerName = "ballast.tightlinesoftware.com/workloadwatcher"

	// ProfileFinalizerName gates WorkloadProfile deletion so that any delete path
	// — the operator's orphan-TTL sweep or a manual `kubectl delete` — routes
	// through the Redis-history purge before the object is released.
	ProfileFinalizerName = "ballast.tightlinesoftware.com/profile-cleanup"

	conditionOrphaned = "Orphaned"

	// requeueTerminating is how long to wait before re-checking a profile that is
	// mid-deletion, so a freshly arriving pod is not bound to a doomed profile.
	requeueTerminating = time.Second

	// requeueKillSwitch is how often a managed pod re-reconciles while the kill
	// switch is active. The kill switch is level state with no deactivation event
	// wired to pods, so without this requeue any enrollment work skipped while it
	// was active (including a one-shot identityLabels fan-out) would wait for the
	// informer resync. Reconciles under the kill switch are read-only, so this is
	// cheap even fleet-wide.
	requeueKillSwitch = time.Minute
)

// errProfileTerminating signals that the target WorkloadProfile is mid-deletion
// (its finalizer is still purging Redis). The pod reconciler translates this into
// a short requeue rather than binding a live pod to a profile about to disappear.
var errProfileTerminating = errors.New("workload profile is terminating")

var behaviorAnnotations = []string{
	AnnotationMeasure,
	AnnotationApply,
	AnnotationResize,
	AnnotationAutoresize,
}

// Controller bundles the PodReconciler and ProfileReconciler.
type Controller struct {
	Pod     *PodReconciler
	Profile *ProfileReconciler
}

// New creates a Controller.
func New(c client.Client, ks *killswitch.KillSwitch, storeClient store.Client, rec *metrics.Recorder) *Controller {
	return &Controller{
		Pod:     &PodReconciler{client: c, ks: ks, rec: rec},
		Profile: &ProfileReconciler{client: c, storeClient: storeClient, rec: rec},
	}
}

// Setup is the single entry point used by both main.go and integration tests.
// It creates the Redis client, wires up the kill switch, and registers all
// controllers with mgr — so both callers exercise the same code path.
func Setup(mgr ctrl.Manager, namespace, redisURL string) error {
	storeClient, err := store.NewClient(redisURL)
	if err != nil { // coverage:ignore - requires a malformed Redis URL
		return fmt.Errorf("creating Redis client: %w", err)
	}
	ks := killswitch.New(mgr.GetClient(), namespace, nil)
	if err := ks.SetupWithManager(mgr); err != nil { // coverage:ignore - requires a malformed manager
		return err
	}
	return New(mgr.GetClient(), ks, storeClient, nil).SetupWithManager(mgr)
}

// SetupWithManager registers both sub-reconcilers with the manager.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	if err := c.Pod.SetupWithManager(mgr); err != nil { // coverage:ignore - requires a malformed manager
		return err
	}
	return c.Profile.SetupWithManager(mgr)
}

// PodReconciler watches pods carrying Ballast behavior annotations and maintains
// WorkloadProfile objects and their activeWorkloads counters.
type PodReconciler struct {
	client client.Client
	ks     *killswitch.KillSwitch
	rec    *metrics.Recorder
}

// Reconcile handles pod CREATE/UPDATE (stamp and increment) and DELETE (decrement).
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.client.Get(ctx, req.NamespacedName, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	if !pod.DeletionTimestamp.IsZero() {
		return r.handleDelete(ctx, &pod)
	}
	return r.handleCreateUpdate(ctx, &pod)
}

func (r *PodReconciler) handleCreateUpdate(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	if r.ks.IsActive() {
		log.Info("kill switch active, skipping pod",
			"kill_switch", true, "kill_switch_reason", r.ks.Reason())
		return ctrl.Result{RequeueAfter: requeueKillSwitch}, nil
	}

	currentRef := pod.Annotations[AnnotationProfileRef]

	// Desired enrollment is derived from the pod's live annotations, not from the
	// stamp. A pod that no longer carries any behavior annotation must be
	// un-enrolled: drop its profile-ref, remove the finalizer, and recount the
	// profile it is leaving.
	if !hasBehaviorAnnotation(pod) {
		if currentRef != "" || controllerutil.ContainsFinalizer(pod, FinalizerName) {
			return ctrl.Result{}, r.unenroll(ctx, pod, currentRef)
		}
		return ctrl.Result{}, nil
	}

	var cfg ballastv1.BallastConfig
	if err := r.client.Get(ctx, types.NamespacedName{Name: killswitch.BallastConfigName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("BallastConfig not found, skipping pod")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	// The desired profile name is recomputed from the pod's current identity every
	// reconcile, so a change to the pod's labels or to identityLabels migrates the
	// pod to the correct profile instead of trusting a possibly-stale stamp.
	tupleLabels := ExtractTupleLabels(pod.Labels, cfg.Spec.IdentityLabels)
	selectorLabels := ExtractSelectorLabels(pod.Labels, cfg.Spec.IdentityLabels)
	profName := ProfileName(tupleLabels, cfg.Spec.IdentityLabels)

	// Ensure the target profile exists; recreates it if it was deleted while pods
	// still reference it.
	if err := r.ensureProfile(ctx, profName, tupleLabels, selectorLabels); err != nil {
		if errors.Is(err, errProfileTerminating) {
			// The profile is being purged; wait for it to finish, then a later
			// reconcile recreates it fresh and rebinds this pod.
			return ctrl.Result{RequeueAfter: requeueTerminating}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	pid := metrics.ProfileID{Name: profName, Labels: tupleLabels}
	firstEnroll := currentRef == ""
	migrating := currentRef != "" && currentRef != profName

	// Add finalizer before stamping the annotation so delete is always handled
	// even if the annotation stamp fails.
	if !controllerutil.ContainsFinalizer(pod, FinalizerName) {
		base := pod.DeepCopy()
		controllerutil.AddFinalizer(pod, FinalizerName)
		if err := r.client.Patch(ctx, pod, client.MergeFrom(base)); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
	}

	if currentRef != profName {
		if err := r.stampProfileRef(ctx, pod, profName); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
	}

	if firstEnroll {
		r.rec.PodProcessed(ctx, "created", pod.Namespace, pid)
	}
	if migrating {
		r.rec.PodProcessed(ctx, "unenrolled", pod.Namespace, metrics.ProfileID{Name: currentRef})
		r.rec.PodProcessed(ctx, "created", pod.Namespace, pid)
	}

	// Recount the target profile, treating this pod as bound to profName regardless
	// of informer cache read-after-write lag on the stamp we just wrote.
	self := &podEnrollment{namespace: pod.Namespace, name: pod.Name, ref: profName}
	if migrating {
		if err := r.setActiveWorkloads(ctx, profName, self); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
		// Recount the profile the pod just left so it can transition to orphaned.
		return ctrl.Result{}, r.setActiveWorkloads(ctx, currentRef, self)
	}
	return ctrl.Result{}, r.setActiveWorkloads(ctx, profName, self)
}

// unenroll removes a pod from Ballast management: it drops the profile-ref
// annotation and the finalizer, then recounts the profile the pod was leaving so
// that profile can transition to orphaned once its last workload departs.
func (r *PodReconciler) unenroll(ctx context.Context, pod *corev1.Pod, oldRef string) error {
	base := pod.DeepCopy()
	delete(pod.Annotations, AnnotationProfileRef)
	controllerutil.RemoveFinalizer(pod, FinalizerName)
	if err := r.client.Patch(ctx, pod, client.MergeFrom(base)); err != nil { // coverage:ignore - transient API error
		return err
	}
	if oldRef == "" {
		return nil
	}
	r.rec.PodProcessed(ctx, "unenrolled", pod.Namespace, metrics.ProfileID{Name: oldRef})
	return r.setActiveWorkloads(ctx, oldRef, &podEnrollment{namespace: pod.Namespace, name: pod.Name, ref: ""})
}

func (r *PodReconciler) handleDelete(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	// Only recount when our finalizer is present. Without this guard, removing the
	// finalizer triggers a MODIFIED event → second reconcile → second recount while
	// the pod is still in the cache, which would inflate the count by 1.
	if !controllerutil.ContainsFinalizer(pod, FinalizerName) {
		return ctrl.Result{}, nil
	}

	// Kill switch does NOT suppress the recount — accounting must stay correct.
	if profName := pod.Annotations[AnnotationProfileRef]; profName != "" {
		// The pod has DeletionTimestamp set, so setActiveWorkloads excludes it from
		// the live count automatically — no separate decrement needed.
		if err := r.setActiveWorkloads(ctx, profName, nil); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
		// Recover the identity-tuple labels from the profile so the "deleted" event
		// carries the same attributes as "created"; fall back to name-only if the
		// profile has already been purged.
		pid := metrics.ProfileID{Name: profName}
		var profile ballastv1.WorkloadProfile
		if err := r.client.Get(ctx, types.NamespacedName{Name: profName}, &profile); err == nil {
			pid.Labels = profile.Status.TupleLabels
		}
		r.rec.PodProcessed(ctx, "deleted", pod.Namespace, pid)
	}

	base := pod.DeepCopy()
	controllerutil.RemoveFinalizer(pod, FinalizerName)
	return ctrl.Result{}, r.client.Patch(ctx, pod, client.MergeFrom(base))
}

func (r *PodReconciler) ensureProfile(ctx context.Context, profName string, tupleLabels, selectorLabels map[string]string) error {
	var existing ballastv1.WorkloadProfile
	err := r.client.Get(ctx, types.NamespacedName{Name: profName}, &existing)
	if err == nil {
		// A profile mid-deletion is having its Redis history purged by the
		// finalizer. Binding a live pod to it now would race the purge and lose
		// the freshly-recreated history; signal the caller to requeue instead.
		if !existing.DeletionTimestamp.IsZero() {
			return errProfileTerminating
		}
		return r.ensureProfileStatus(ctx, &existing, tupleLabels, selectorLabels)
	}
	if !apierrors.IsNotFound(err) { // coverage:ignore - transient API error
		return err
	}

	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	if err := r.client.Create(ctx, profile); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// The cache said NotFound but the API server has the object: either a
			// concurrent create by another pod's reconcile or a deletion that has
			// not completed server-side. Requeue and re-evaluate against a fresher
			// cache rather than guessing which; binding now could attach the pod
			// to an object mid-purge.
			return errProfileTerminating
		}
		return err // coverage:ignore - transient non-AlreadyExists error
	}
	r.rec.WorkloadProfileCreated(ctx, metrics.ProfileID{Name: profName, Labels: tupleLabels})

	// Status is a subresource; it can only be written after creation.
	return r.ensureProfileStatus(ctx, profile, tupleLabels, selectorLabels)
}

// ensureProfileStatus level-triggers the profile's identity labels: whenever the
// stored status does not match the desired tuple/selector labels, patch it.
// Converging on every reconcile (not only at creation) heals a profile whose
// initial status write was lost — a conflict with the profile reconciler's
// concurrent finalizer back-fill, a crash between create and status write, or a
// profile inherited from an older operator version. A Patch (not Update) is used
// so the write cannot 409 against that finalizer back-fill.
func (r *PodReconciler) ensureProfileStatus(ctx context.Context, profile *ballastv1.WorkloadProfile, tupleLabels, selectorLabels map[string]string) error {
	if maps.Equal(profile.Status.TupleLabels, tupleLabels) &&
		maps.Equal(profile.Status.SelectorLabels, selectorLabels) {
		return nil
	}
	base := profile.DeepCopy()
	profile.Status.TupleLabels = tupleLabels
	profile.Status.SelectorLabels = selectorLabels
	return r.client.Status().Patch(ctx, profile, client.MergeFrom(base))
}

// podEnrollment overrides the reconciled pod's enrollment when recomputing a
// profile's active-workload count, so the count reflects the state just written
// even if the informer cache has not yet caught up (read-after-write lag). A ref
// of "" treats the pod as un-enrolled.
type podEnrollment struct {
	namespace string
	name      string
	ref       string
}

// hasBehaviorAnnotation reports whether the pod carries at least one Ballast
// behavior annotation, i.e. whether it wants to be enrolled.
func hasBehaviorAnnotation(pod *corev1.Pod) bool {
	for _, ann := range behaviorAnnotations {
		if _, ok := pod.Annotations[ann]; ok {
			return true
		}
	}
	return false
}

// countActiveWorkloads counts pods that hold our finalizer, carry a profileRef
// matching profName, and have no DeletionTimestamp. When self is non-nil, the
// matching pod's enrollment is overridden with self.ref so the count is correct
// despite cache lag on a stamp written earlier in the same reconcile.
func countActiveWorkloads(pods []corev1.Pod, profName string, self *podEnrollment) int32 {
	var count int32
	for i := range pods {
		p := &pods[i]
		ref := p.Annotations[AnnotationProfileRef]
		enrolled := controllerutil.ContainsFinalizer(p, FinalizerName)
		if self != nil && p.Namespace == self.namespace && p.Name == self.name {
			ref = self.ref
			enrolled = self.ref != ""
		}
		if ref == profName && enrolled && p.DeletionTimestamp.IsZero() {
			count++
		}
	}
	return count
}

// setWorkloadCount records count on the profile status and maintains the Orphaned
// condition: set when the count reaches zero, removed otherwise.
func setWorkloadCount(profile *ballastv1.WorkloadProfile, count int32) {
	profile.Status.ActiveWorkloads = count
	if count == 0 {
		apimeta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               conditionOrphaned,
			Status:             metav1.ConditionTrue,
			Reason:             "NoActiveWorkloads",
			Message:            "No active workloads for this profile",
			LastTransitionTime: metav1.Now(),
		})
	} else {
		apimeta.RemoveStatusCondition(&profile.Status.Conditions, conditionOrphaned)
	}
}

// setActiveWorkloads derives the profile's active-workload count from actual pod
// state and writes it to the WorkloadProfile status. This is level-triggered:
// each call recomputes rather than incrementing/decrementing, making every
// reconcile idempotent and self-healing against any prior miscounting.
func (r *PodReconciler) setActiveWorkloads(ctx context.Context, profName string, self *podEnrollment) error {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList); err != nil { // coverage:ignore - transient API error
		return err
	}
	count := countActiveWorkloads(podList.Items, profName, self)

	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, types.NamespacedName{Name: profName}, &profile); err != nil { // coverage:ignore - transient API error
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err // coverage:ignore - transient non-NotFound error
	}
	base := profile.DeepCopy()
	setWorkloadCount(&profile, count)
	return r.client.Status().Patch(ctx, &profile, client.MergeFrom(base))
}

func (r *PodReconciler) stampProfileRef(ctx context.Context, pod *corev1.Pod, profName string) error {
	base := pod.DeepCopy()
	if pod.Annotations == nil { // coverage:ignore - predicate guarantees ≥1 annotation
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[AnnotationProfileRef] = profName
	return r.client.Patch(ctx, pod, client.MergeFrom(base))
}

// HasBallastAnnotationOrFinalizer reports whether obj carries a Ballast behavior
// annotation or holds the workloadwatcher finalizer. Exported so it can be unit-tested
// independently of the controller manager.
func HasBallastAnnotationOrFinalizer(obj client.Object) bool {
	anns := obj.GetAnnotations()
	for _, ann := range behaviorAnnotations {
		if _, ok := anns[ann]; ok {
			return true
		}
	}
	// Admit pods that already hold our finalizer so deletions are processed
	// even after behavior annotations have been removed.
	return slices.Contains(obj.GetFinalizers(), FinalizerName)
}

// podsForProfile maps a WorkloadProfile event to reconcile requests for every pod
// that references it by name, so a deleted profile promptly re-reconciles (and thus
// recreates for) the workloads that still point at it.
func (r *PodReconciler) podsForProfile(ctx context.Context, obj client.Object) []ctrl.Request {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList); err != nil { // coverage:ignore - transient API error
		return nil
	}
	var reqs []ctrl.Request
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Annotations[AnnotationProfileRef] == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
		}
	}
	return reqs
}

// podsForConfig maps a BallastConfig change to reconcile requests for every managed
// pod, so an identityLabels change promptly migrates each pod to its new profile.
func (r *PodReconciler) podsForConfig(ctx context.Context, _ client.Object) []ctrl.Request {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList); err != nil { // coverage:ignore - transient API error
		return nil
	}
	var reqs []ctrl.Request
	for i := range podList.Items {
		p := &podList.Items[i]
		if hasBehaviorAnnotation(p) || controllerutil.ContainsFinalizer(p, FinalizerName) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
		}
	}
	return reqs
}

// profileDeleted admits only WorkloadProfile delete events. Status writes happen on
// every pod change, so admitting updates here would enqueue the referencing pods on
// each write and amplify work without cause.
func profileDeleted() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// identityLabelsChanged admits only the canonical BallastConfig events that can
// change enrollment outcomes: creation (pods reconciled while the config was
// absent were skipped, and a delete + re-apply with different identityLabels never
// fires the update path) and updates that change the identity label set — the
// single field that alters profile names. The name filter matches the killswitch's
// own BallastConfig watch, so a stray non-canonical object cannot fan out
// reconciles for the whole fleet.
func identityLabelsChanged() predicate.Predicate {
	canonical := func(obj client.Object) bool { return obj.GetName() == killswitch.BallastConfigName }
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return canonical(e.Object) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldCfg, ok1 := e.ObjectOld.(*ballastv1.BallastConfig)
			newCfg, ok2 := e.ObjectNew.(*ballastv1.BallastConfig)
			if !ok1 || !ok2 || !canonical(e.ObjectNew) {
				return false
			}
			return !slices.Equal(oldCfg.Spec.IdentityLabels, newCfg.Spec.IdentityLabels)
		},
	}
}

// SetupWithManager registers the PodReconciler with the manager. Beyond watching
// pods, it watches WorkloadProfile deletions (to promptly recreate profiles still
// referenced by live pods) and BallastConfig identityLabels changes (to promptly
// migrate pods to their new profiles).
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadwatcher-pod").
		WithLogConstructor(logger.ControllerLogConstructor(mgr.GetLogger(), "workloadwatcher-pod")).
		For(&corev1.Pod{}, builder.WithPredicates(predicate.NewPredicateFuncs(HasBallastAnnotationOrFinalizer))).
		Watches(&ballastv1.WorkloadProfile{},
			handler.EnqueueRequestsFromMapFunc(r.podsForProfile),
			builder.WithPredicates(profileDeleted())).
		Watches(&ballastv1.BallastConfig{},
			handler.EnqueueRequestsFromMapFunc(r.podsForConfig),
			builder.WithPredicates(identityLabelsChanged())).
		Complete(r)
}

// ProfileReconciler watches WorkloadProfile objects and enforces orphan TTL cleanup.
type ProfileReconciler struct {
	client      client.Client
	storeClient store.Client
	rec         *metrics.Recorder
}

// Reconcile enforces the profile lifecycle. It runs the Redis-history purge for
// profiles that are being deleted (the finalizer path), ensures the cleanup
// finalizer is present on live profiles, and deletes profiles that have been
// orphaned past their TTL. Cleanup itself lives entirely in the finalizer, so it
// runs regardless of whether the delete was triggered by the TTL sweep or by an
// operator running `kubectl delete`.
func (r *ProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, req.NamespacedName, &profile); err != nil { // coverage:ignore - transient API error
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient non-NotFound error
	}

	// Deletion in progress: run the finalizer (purge Redis, release the object).
	if !profile.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &profile)
	}

	// Ensure the cleanup finalizer is present so every future deletion routes
	// through finalize. This also back-fills the finalizer onto profiles created
	// by an older operator version on their first reconcile after upgrade.
	if !controllerutil.ContainsFinalizer(&profile, ProfileFinalizerName) {
		base := profile.DeepCopy()
		controllerutil.AddFinalizer(&profile, ProfileFinalizerName)
		if err := r.client.Patch(ctx, &profile, client.MergeFrom(base)); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
	}

	// Correctness backstop for counts: recompute activeWorkloads from live pod
	// state. The pod reconciler's recounts are the prompt path, but they fire only
	// for profiles some pod still references; if the trailing recount of a
	// migration or un-enrollment is lost (transient API error, operator crash), no
	// pod names the old profile anymore and no pod event will ever recount it.
	// Recounting here — on every profile event and on resync — guarantees such a
	// profile still converges to zero, orphans, and ages out.
	if err := r.recountActiveWorkloads(ctx, &profile); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	// Orphan-TTL policy decides *when* to delete; the finalizer decides *how* to clean up.
	cond := apimeta.FindStatusCondition(profile.Status.Conditions, conditionOrphaned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	var cfg ballastv1.BallastConfig
	if err := r.client.Get(ctx, types.NamespacedName{Name: killswitch.BallastConfigName}, &cfg); err != nil { // coverage:ignore - transient API error
		if apierrors.IsNotFound(err) { // coverage:ignore - transient API error
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient non-NotFound error
	}

	ttl, err := time.ParseDuration(cfg.Spec.OrphanTTL)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("parsing orphanTTL %q: %w", cfg.Spec.OrphanTTL, err)
	}

	age := time.Since(cond.LastTransitionTime.Time)
	if age < ttl {
		return ctrl.Result{RequeueAfter: ttl - age}, nil
	}

	// Deleting only sets the DeletionTimestamp; the finalizer runs on the next
	// reconcile and performs the Redis purge before the object is removed.
	if err := r.client.Delete(ctx, &profile); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// recountActiveWorkloads level-triggers profile.Status.ActiveWorkloads from live
// pod state, writing only when the stored count or Orphaned condition disagrees.
// The write-on-change guard keeps this from generating a status event (and thus
// another profile reconcile) on the steady-state path.
func (r *ProfileReconciler) recountActiveWorkloads(ctx context.Context, profile *ballastv1.WorkloadProfile) error {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList); err != nil { // coverage:ignore - transient API error
		return err
	}
	count := countActiveWorkloads(podList.Items, profile.Name, nil)

	cond := apimeta.FindStatusCondition(profile.Status.Conditions, conditionOrphaned)
	orphaned := cond != nil && cond.Status == metav1.ConditionTrue
	if profile.Status.ActiveWorkloads == count && orphaned == (count == 0) {
		return nil
	}

	base := profile.DeepCopy()
	setWorkloadCount(profile, count)
	return r.client.Status().Patch(ctx, profile, client.MergeFrom(base))
}

// finalize purges the profile's Redis history and removes the cleanup finalizer,
// allowing the API server to complete the deletion.
func (r *ProfileReconciler) finalize(ctx context.Context, profile *ballastv1.WorkloadProfile) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(profile, ProfileFinalizerName) {
		return ctrl.Result{}, nil
	}

	tupleHash := store.TupleHash(profile.Status.TupleLabels)
	keys, err := store.AllKeysForHash(ctx, r.storeClient, tupleHash)
	if err != nil { // coverage:ignore - requires a broken Redis instance
		return ctrl.Result{}, err
	}
	for _, key := range keys {
		if err := store.DeleteKey(ctx, r.storeClient, key); err != nil { // coverage:ignore - requires a broken Redis instance
			return ctrl.Result{}, err
		}
	}
	r.rec.WorkloadProfilePurged(ctx, metrics.ProfileID{Name: profile.Name, Labels: profile.Status.TupleLabels})

	base := profile.DeepCopy()
	controllerutil.RemoveFinalizer(profile, ProfileFinalizerName)
	return ctrl.Result{}, r.client.Patch(ctx, profile, client.MergeFrom(base)) // coverage:ignore - transient API error on the patch itself
}

// SetupWithManager registers the ProfileReconciler with the manager.
func (r *ProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadwatcher-profile").
		WithLogConstructor(logger.ControllerLogConstructor(mgr.GetLogger(), "workloadwatcher-profile")).
		For(&ballastv1.WorkloadProfile{}).
		Complete(r)
}

// ExtractTupleLabels returns a map of identityLabel key -> pod label value.
// Keys absent from podLabels are assigned a placeholder derived from the key
// (e.g. "app.kubernetes.io/component" -> "nocomponent") so the WorkloadProfile
// name remains meaningful without a real value.
func ExtractTupleLabels(podLabels map[string]string, identityLabels []string) map[string]string {
	out := make(map[string]string, len(identityLabels))
	for _, k := range identityLabels {
		v, ok := podLabels[k]
		if !ok {
			v = missingLabelPlaceholder(k)
		}
		out[k] = v
	}
	return out
}

// ExtractSelectorLabels returns a map used to query pods from the metrics API.
// Keys present in podLabels carry their real value. Keys absent from podLabels
// carry plugin.LabelAbsent ("--missing--"), which the metrics plugin translates
// to a Kubernetes "!key" requirement so the selector excludes pods that have a
// different value for that label (e.g. component=server) rather than matching them.
func ExtractSelectorLabels(podLabels map[string]string, identityLabels []string) map[string]string {
	out := make(map[string]string, len(identityLabels))
	for _, k := range identityLabels {
		v, ok := podLabels[k]
		if !ok {
			v = plugin.LabelAbsent
		}
		out[k] = v
	}
	return out
}

// missingLabelPlaceholder derives a human-readable sentinel for an absent label.
// It takes the segment after the last '/', strips non-letter characters, lowercases
// the result, and prepends "no".
//
// Examples:
//
//	"app.kubernetes.io/component" -> "nocomponent"
//	"foo.bar.baz"                 -> "nofoobarbaz"
//	"app"                         -> "noapp"
func missingLabelPlaceholder(key string) string {
	seg := key
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		seg = key[i+1:]
	}
	clean := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, seg)
	return "no" + clean
}

// ProfileName derives a deterministic Kubernetes-safe name from a label tuple.
// Values are joined with "--" in identityLabels order. Each value is sanitized
// to lowercase alphanumeric-and-dash.
func ProfileName(tupleLabels map[string]string, identityLabels []string) string {
	var parts []string
	for _, k := range identityLabels {
		if v, ok := tupleLabels[k]; ok {
			parts = append(parts, sanitizeName(v))
		}
	}
	name := strings.Join(parts, "--")
	if len(name) > 253 { // coverage:ignore - triggered only with extremely long label values
		name = name[:253]
	}
	return name
}

// sanitizeName converts a string to a lowercase DNS-label-safe segment.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
