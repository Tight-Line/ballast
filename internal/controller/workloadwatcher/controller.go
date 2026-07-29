package workloadwatcher

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
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
	"github.com/tight-line/ballast/internal/naming"
	"github.com/tight-line/ballast/internal/plugin"
	"github.com/tight-line/ballast/internal/policy"
	"github.com/tight-line/ballast/internal/store"
	"github.com/tight-line/ballast/internal/validation"
)

const (
	// AnnotationProfileRef records the WorkloadProfile a pod is bound to. Enrollment
	// itself is driven by the validation.LabelMode label; this is operator output.
	AnnotationProfileRef = "ballast.tightlinesoftware.com/profile-ref"

	FinalizerName = "ballast.tightlinesoftware.com/workloadwatcher"

	// PodProfileRefField is the cache index key for looking up pods by their
	// profile-ref annotation. It is registered in PodReconciler.SetupWithManager;
	// test harnesses using a fake client must register it with
	// WithIndex(&corev1.Pod{}, PodProfileRefField, PodProfileRefIndexer).
	// Without it, every count recomputation would list and deep-copy every pod
	// in the cluster on each reconcile.
	PodProfileRefField = ".metadata.annotations.profile-ref"

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

// Controller bundles the PodReconciler and ProfileReconciler.
type Controller struct {
	Pod     *PodReconciler
	Profile *ProfileReconciler
}

// New creates a Controller.
func New(c client.Client, ks *killswitch.KillSwitch, storeClient store.Client, rec *metrics.Recorder) *Controller {
	return &Controller{
		Pod: &PodReconciler{
			client:   c,
			ks:       ks,
			rec:      rec,
			resolver: policy.NewResolver(c, ctrl.Log.WithName("workloadwatcher-pod")),
		},
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
//
// It is also the only component that resolves policy for a pod outside admission,
// and the only one positioned to: a pod reconcile has the namespace, the full
// label set, the annotations, and the owner kind that policy selectors are
// written against, none of which survive into the cluster-scoped WorkloadProfile
// the other controllers work from. The resolved policy is recorded on the
// profile's status.policyRef for those controllers to read.
type PodReconciler struct {
	client   client.Client
	ks       *killswitch.KillSwitch
	rec      *metrics.Recorder
	resolver *policy.Resolver
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

	// Desired enrollment is derived from the pod's live mode label, not from the
	// stamp. A pod that no longer carries a Ballast mode label must be
	// un-enrolled: drop its profile-ref, remove the finalizer, and recount the
	// profile it is leaving.
	if !isEnrolled(pod) {
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

	// The desired identity is recomputed from the pod's current state every
	// reconcile, so a change to the pod's labels, to identityLabels, or to the
	// policy set migrates the pod to the correct profile instead of trusting a
	// possibly-stale stamp.
	id, err := r.identityFor(ctx, pod, cfg.Spec.IdentityLabels)
	if err != nil { // coverage:ignore - transient API error listing policy objects
		return ctrl.Result{}, err
	}
	profName := id.name

	// Ensure the target profile exists; recreates it if it was deleted while pods
	// still reference it.
	if err := r.ensureProfile(ctx, id); err != nil {
		if errors.Is(err, errProfileTerminating) {
			// The profile is being purged; wait for it to finish, then a later
			// reconcile recreates it fresh and rebinds this pod.
			return ctrl.Result{RequeueAfter: requeueTerminating}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	pid := metrics.ProfileID{Name: profName, Labels: id.tupleLabels}
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

	if err := r.stampRefs(ctx, pod, id); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
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

// profileIdentity is everything that distinguishes one WorkloadProfile: the pods
// it pools, the policy governing them, and the Redis namespace holding their
// samples.
type profileIdentity struct {
	name            string
	tupleLabels     map[string]string
	selectorLabels  map[string]string
	policyRef       *ballastv1.PolicyReference
	measurementHash string
}

// identityFor derives the WorkloadProfile identity a pod belongs to: its label
// tuple plus the policy governing it.
//
// The policy belongs in identity because a profile holds exactly one set of
// recommendations per container, and the policy is what decides the metrics
// sources, poll cadence, tracked resources, aggregation, and headroom that
// produce them. Pods resolving to different policies cannot share one answer, so
// they cannot share one profile.
func (r *PodReconciler) identityFor(ctx context.Context, pod *corev1.Pod, identityLabels []string) (id profileIdentity, err error) {
	resolved, err := r.resolver.Resolve(ctx, policy.InputForPod(pod))
	if err != nil { // coverage:ignore - transient API error listing policy objects
		return profileIdentity{}, err
	}

	id = profileIdentity{
		tupleLabels:    ExtractTupleLabels(pod.Labels, identityLabels),
		selectorLabels: ExtractSelectorLabels(pod.Labels, identityLabels),
	}

	// A pod matching no policy still gets a profile, so it stays counted and
	// visible; the metrics collector has no policy to measure with and skips it.
	// That profile's identity carries the NoPolicy token, so as soon as a policy
	// does match, the pod migrates to a new profile and the empty one orphans.
	discriminator := naming.NoPolicy
	var policyKey string
	if resolved != nil {
		ref := resolved.Ref
		id.policyRef = &ref
		discriminator = naming.PolicyDiscriminator(ref.Kind, ref.Namespace, ref.Name)
		policyKey = ref.Key()
	}

	id.name = naming.ProfileName(id.tupleLabels, identityLabels, discriminator)
	id.measurementHash = store.MeasurementHash(id.tupleLabels, policyKey)
	return id, nil
}

func (r *PodReconciler) ensureProfile(ctx context.Context, id profileIdentity) error {
	var existing ballastv1.WorkloadProfile
	err := r.client.Get(ctx, types.NamespacedName{Name: id.name}, &existing)
	if err == nil {
		// A profile mid-deletion is having its Redis history purged by the
		// finalizer. Binding a live pod to it now would race the purge and lose
		// the freshly-recreated history; signal the caller to requeue instead.
		if !existing.DeletionTimestamp.IsZero() {
			return errProfileTerminating
		}
		return r.ensureProfileStatus(ctx, &existing, id)
	}
	if !apierrors.IsNotFound(err) { // coverage:ignore - transient API error
		return err
	}

	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: id.name},
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
	r.rec.WorkloadProfileCreated(ctx, metrics.ProfileID{Name: id.name, Labels: id.tupleLabels})

	// Status is a subresource; it can only be written after creation.
	return r.ensureProfileStatus(ctx, profile, id)
}

// ensureProfileStatus level-triggers the profile's identity: whenever the stored
// status does not match the desired tuple labels, selector labels, policy
// reference, or measurement hash, patch it. Converging on every reconcile (not
// only at creation) heals a profile whose initial status write was lost — a
// conflict with the profile reconciler's concurrent finalizer back-fill, a crash
// between create and status write, or a profile inherited from an older operator
// version that recorded no policy reference at all. A Patch (not Update) is used
// so the write cannot 409 against that finalizer back-fill.
func (r *PodReconciler) ensureProfileStatus(ctx context.Context, profile *ballastv1.WorkloadProfile, id profileIdentity) error {
	if maps.Equal(profile.Status.TupleLabels, id.tupleLabels) &&
		maps.Equal(profile.Status.SelectorLabels, id.selectorLabels) &&
		samePolicyRef(profile.Status.PolicyRef, id.policyRef) &&
		profile.Status.MeasurementHash == id.measurementHash {
		return nil
	}
	base := profile.DeepCopy()
	profile.Status.TupleLabels = id.tupleLabels
	profile.Status.SelectorLabels = id.selectorLabels
	profile.Status.PolicyRef = id.policyRef
	profile.Status.MeasurementHash = id.measurementHash
	return r.client.Status().Patch(ctx, profile, client.MergeFrom(base))
}

// samePolicyRef compares two policy references, treating "no policy matched"
// (nil) as distinct from every reference.
func samePolicyRef(a, b *ballastv1.PolicyReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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

// isEnrolled reports whether the pod carries a recognized Ballast mode label,
// i.e. whether it wants to be enrolled.
func isEnrolled(pod *corev1.Pod) bool {
	return validation.IsEnrolled(pod.Labels)
}

// countActiveWorkloads counts pods that hold our finalizer, carry a profileRef
// matching profName, and have no DeletionTimestamp. When self is non-nil, the
// matching pod's enrollment is overridden with self.ref so the count is correct
// despite cache lag on a stamp written earlier in the same reconcile.
//
// pods comes from the profile-ref index, which reflects the *cached* annotation.
// A pod stamped with profName earlier in this same reconcile is usually still
// indexed under its previous ref, so it is absent from the list entirely; the
// override can only exclude pods, never admit them. The insert below closes that
// half: when self claims profName but did not appear, count it in by hand. No
// DeletionTimestamp check is needed there because handleCreateUpdate (the only
// caller passing a non-nil self) never runs for terminating pods.
func countActiveWorkloads(pods []corev1.Pod, profName string, self *podEnrollment) int32 {
	var count int32
	seenSelf := false
	for i := range pods {
		p := &pods[i]
		ref := p.Annotations[AnnotationProfileRef]
		enrolled := controllerutil.ContainsFinalizer(p, FinalizerName)
		if self != nil && p.Namespace == self.namespace && p.Name == self.name {
			seenSelf = true
			ref = self.ref
			enrolled = self.ref != ""
		}
		if ref == profName && enrolled && p.DeletionTimestamp.IsZero() {
			count++
		}
	}
	if self != nil && !seenSelf && self.ref == profName {
		count++
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
// reconcile idempotent and self-healing against any prior miscounting. The list
// is served by the profile-ref index, so its cost scales with the profile's
// membership, not the cluster's pod count.
func (r *PodReconciler) setActiveWorkloads(ctx context.Context, profName string, self *podEnrollment) error {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList, client.MatchingFields{PodProfileRefField: profName}); err != nil { // coverage:ignore - transient API error
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

// stampRefs writes the profile-ref and policy-ref annotations in a single patch,
// and is a no-op when both already hold their desired values.
//
// policy-ref is refreshed here rather than left as the webhook wrote it. The
// webhook stamps the policy it resolved at admission; after a policy is created,
// edited, or deleted, that annotation would otherwise keep advertising a policy
// that no longer governs the pod, which is a trap for anyone debugging from it.
func (r *PodReconciler) stampRefs(ctx context.Context, pod *corev1.Pod, id profileIdentity) error {
	var policyRef string
	if id.policyRef != nil {
		policyRef = policy.PodAnnotationValue(*id.policyRef)
	}
	if pod.Annotations[AnnotationProfileRef] == id.name &&
		pod.Annotations[validation.AnnotationPolicyRef] == policyRef {
		return nil
	}

	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[AnnotationProfileRef] = id.name
	if policyRef == "" {
		delete(pod.Annotations, validation.AnnotationPolicyRef)
	} else {
		pod.Annotations[validation.AnnotationPolicyRef] = policyRef
	}
	return r.client.Patch(ctx, pod, client.MergeFrom(base))
}

// HasBallastModeOrFinalizer reports whether obj carries a Ballast mode label or
// holds the workloadwatcher finalizer. Exported so it can be unit-tested
// independently of the controller manager.
func HasBallastModeOrFinalizer(obj client.Object) bool {
	if validation.IsEnrolled(obj.GetLabels()) {
		return true
	}
	// Admit pods that already hold our finalizer so deletions are processed
	// even after the mode label has been removed.
	return slices.Contains(obj.GetFinalizers(), FinalizerName)
}

// PodProfileRefIndexer extracts the profile-ref annotation as the index key for
// PodProfileRefField. Pods without the annotation are not indexed. Exported so
// tests can register the same index on a fake client.
func PodProfileRefIndexer(obj client.Object) []string {
	if ref := obj.GetAnnotations()[AnnotationProfileRef]; ref != "" {
		return []string{ref}
	}
	return nil
}

// podsForProfile maps a WorkloadProfile event to reconcile requests for every pod
// that references it by name (served by the profile-ref index), so a deleted
// profile promptly re-reconciles (and thus recreates for) the workloads that
// still point at it.
func (r *PodReconciler) podsForProfile(ctx context.Context, obj client.Object) []ctrl.Request {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList, client.MatchingFields{PodProfileRefField: obj.GetName()}); err != nil { // coverage:ignore - transient API error
		return nil
	}
	var reqs []ctrl.Request
	for i := range podList.Items {
		p := &podList.Items[i]
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
	}
	return reqs
}

// allManagedPods maps a cluster-wide configuration change to reconcile requests
// for every managed pod, so the change promptly migrates each pod to its correct
// profile. It backs both the BallastConfig watch (identityLabels changes rename
// every profile) and the policy watches (a policy change can move any pod to a
// different policy, and therefore a different profile).
//
// The fan-out is deliberately indiscriminate. The set of pods a policy event
// affects is not "the pods this policy matches": deleting a policy affects the
// pods that matched the spec that no longer exists, narrowing a selector affects
// the pods that stopped matching, and because precedence is cluster-wide, a new
// high-priority policy can flip pods that never matched anything before.
// Computing that set exactly would need both the old and new spec plus a
// re-evaluation against every other policy. Enqueueing everything instead costs
// one cache read and one resolve per pod, writes nothing unless a pod's identity
// actually changed, and only happens on human-initiated policy edits.
func (r *PodReconciler) allManagedPods(ctx context.Context, _ client.Object) []ctrl.Request {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList); err != nil { // coverage:ignore - transient API error
		return nil
	}
	var reqs []ctrl.Request
	for i := range podList.Items {
		p := &podList.Items[i]
		if isEnrolled(p) || controllerutil.ContainsFinalizer(p, FinalizerName) {
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

// policyResolutionChanged admits only the policy events that can change which
// policy governs a pod: creation, deletion, and updates that touch the selector
// or the priority.
//
// Every other spec field (metrics sources, aggregation, headroom, thresholds,
// cadence) is read live from the policy object by the metrics collector and the
// resource adjuster on their next cycle, so those edits take effect without any
// profile churn. Treating them as identity changes would re-key measurement
// history and force a fresh accrual for nothing — the sample already recorded
// does not change meaning because the headroom applied to it did.
func policyResolutionChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSpec, ok1 := policySpecOf(e.ObjectOld)
			newSpec, ok2 := policySpecOf(e.ObjectNew)
			if !ok1 || !ok2 {
				// Not a policy object we recognize; admit it rather than silently
				// dropping an event that might matter.
				return true
			}
			return oldSpec.Priority != newSpec.Priority ||
				!reflect.DeepEqual(oldSpec.Selector, newSpec.Selector)
		},
	}
}

// policySpecOf extracts the shared policy spec from either policy kind.
// ResourcePolicySpec is a type alias for ClusterResourcePolicySpec, so one
// pointer type serves both.
func policySpecOf(obj client.Object) (*ballastv1.ClusterResourcePolicySpec, bool) {
	switch p := obj.(type) {
	case *ballastv1.ClusterResourcePolicy:
		return &p.Spec, true
	case *ballastv1.ResourcePolicy:
		return &p.Spec, true
	default:
		return nil, false
	}
}

// SetupWithManager registers the PodReconciler with the manager. Beyond watching
// pods, it watches WorkloadProfile deletions (to promptly recreate profiles still
// referenced by live pods), BallastConfig identityLabels changes (to promptly
// migrate pods to their new profiles), and both policy kinds (so a policy applied
// to a running cluster takes effect without waiting for pod churn). It also
// registers the profile-ref pod index on the manager's shared cache, which serves
// both this reconciler's and the ProfileReconciler's count lookups.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &corev1.Pod{}, PodProfileRefField, PodProfileRefIndexer,
	); err != nil { // coverage:ignore - fails only on duplicate index registration
		return fmt.Errorf("registering pod profile-ref index: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadwatcher-pod").
		WithLogConstructor(logger.ControllerLogConstructor(mgr.GetLogger(), "workloadwatcher-pod")).
		For(&corev1.Pod{}, builder.WithPredicates(predicate.NewPredicateFuncs(HasBallastModeOrFinalizer))).
		Watches(&ballastv1.WorkloadProfile{},
			handler.EnqueueRequestsFromMapFunc(r.podsForProfile),
			builder.WithPredicates(profileDeleted())).
		Watches(&ballastv1.BallastConfig{},
			handler.EnqueueRequestsFromMapFunc(r.allManagedPods),
			builder.WithPredicates(identityLabelsChanged())).
		Watches(&ballastv1.ClusterResourcePolicy{},
			handler.EnqueueRequestsFromMapFunc(r.allManagedPods),
			builder.WithPredicates(policyResolutionChanged())).
		Watches(&ballastv1.ResourcePolicy{},
			handler.EnqueueRequestsFromMapFunc(r.allManagedPods),
			builder.WithPredicates(policyResolutionChanged())).
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
// another profile reconcile) on the steady-state path. The indexed list matters
// here even more than on the pod side: this runs on *every* profile event,
// including the metrics collector's once-per-poll status writes, so an
// unindexed list would deep-copy the whole cluster's pods several times a
// second, forever.
func (r *ProfileReconciler) recountActiveWorkloads(ctx context.Context, profile *ballastv1.WorkloadProfile) error {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList, client.MatchingFields{PodProfileRefField: profile.Name}); err != nil { // coverage:ignore - transient API error
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

	// The profile's own measurement hash, not a hash of its tuple: profiles that
	// share a tuple but resolve to different policies each own a separate key
	// namespace, and purging by tuple would delete a live sibling's history.
	hash := profile.Status.MeasurementHash
	if hash == "" {
		// A profile from a release that predates measurement hashes (or one whose
		// status write was lost) holds its samples under the bare tuple hash.
		// Falling back keeps those keys from being stranded in Redis when the
		// profile ages out, which is how every profile inherited across the
		// upgrade is cleaned up.
		hash = store.TupleHash(profile.Status.TupleLabels)
	}
	keys, err := store.AllKeysForHash(ctx, r.storeClient, hash)
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
