package workloadwatcher

import (
	"context"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/killswitch"
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

	conditionOrphaned = "Orphaned"
)

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
		return ctrl.Result{}, nil
	}

	// Already processed: ensure finalizer is present for cleanup, then recalibrate.
	if pod.Annotations[AnnotationProfileRef] != "" {
		profName := pod.Annotations[AnnotationProfileRef]
		if !controllerutil.ContainsFinalizer(pod, FinalizerName) {
			controllerutil.AddFinalizer(pod, FinalizerName)
			if err := r.client.Update(ctx, pod); err != nil { // coverage:ignore - transient API error
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, r.setActiveWorkloads(ctx, profName)
	}

	var cfg ballastv1.BallastConfig
	if err := r.client.Get(ctx, types.NamespacedName{Name: killswitch.BallastConfigName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("BallastConfig not found, skipping pod")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	tupleLabels := ExtractTupleLabels(pod.Labels, cfg.Spec.IdentityLabels)
	selectorLabels := ExtractSelectorLabels(pod.Labels, cfg.Spec.IdentityLabels)
	profName := ProfileName(tupleLabels, cfg.Spec.IdentityLabels)

	if err := r.ensureProfile(ctx, profName, tupleLabels, selectorLabels); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	// Add finalizer before stamping the annotation so delete is always handled
	// even if the annotation stamp fails.
	if !controllerutil.ContainsFinalizer(pod, FinalizerName) {
		controllerutil.AddFinalizer(pod, FinalizerName)
		if err := r.client.Update(ctx, pod); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
		r.rec.PodProcessed(ctx, "created", pod.Namespace, profName)
	}

	if err := r.stampProfileRef(ctx, pod, profName); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.setActiveWorkloads(ctx, profName)
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
		if err := r.setActiveWorkloads(ctx, profName); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
		r.rec.PodProcessed(ctx, "deleted", pod.Namespace, profName)
	}

	controllerutil.RemoveFinalizer(pod, FinalizerName)
	return ctrl.Result{}, r.client.Update(ctx, pod)
}

func (r *PodReconciler) ensureProfile(ctx context.Context, profName string, tupleLabels, selectorLabels map[string]string) error {
	var existing ballastv1.WorkloadProfile
	err := r.client.Get(ctx, types.NamespacedName{Name: profName}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) { // coverage:ignore - transient API error
		return err
	}

	profile := &ballastv1.WorkloadProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profName},
	}
	if err := r.client.Create(ctx, profile); err != nil { // coverage:ignore - transient API error
		if apierrors.IsAlreadyExists(err) { // coverage:ignore - create/create race
			return nil
		}
		return err // coverage:ignore - transient non-AlreadyExists error
	}
	r.rec.WorkloadProfileCreated(ctx, profName)

	// Status is a subresource; must be updated after creation.
	profile.Status.TupleLabels = tupleLabels
	profile.Status.SelectorLabels = selectorLabels
	return r.client.Status().Update(ctx, profile)
}

// setActiveWorkloads counts all pods that hold our finalizer, carry a profileRef
// matching profName, and have no DeletionTimestamp, then writes that count to the
// WorkloadProfile status. This is level-triggered: each call derives the count from
// actual pod state rather than incrementing/decrementing, making every reconcile
// idempotent and self-healing against any prior miscounting.
func (r *PodReconciler) setActiveWorkloads(ctx context.Context, profName string) error {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList); err != nil { // coverage:ignore - transient API error
		return err
	}

	var count int32
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Annotations[AnnotationProfileRef] == profName &&
			controllerutil.ContainsFinalizer(p, FinalizerName) &&
			p.DeletionTimestamp.IsZero() {
			count++
		}
	}

	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, types.NamespacedName{Name: profName}, &profile); err != nil { // coverage:ignore - transient API error
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err // coverage:ignore - transient non-NotFound error
	}
	base := profile.DeepCopy()
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

// SetupWithManager registers the PodReconciler with the manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadwatcher-pod").
		For(&corev1.Pod{}, builder.WithPredicates(predicate.NewPredicateFuncs(HasBallastAnnotationOrFinalizer))).
		Complete(r)
}

// ProfileReconciler watches WorkloadProfile objects and enforces orphan TTL cleanup.
type ProfileReconciler struct {
	client      client.Client
	storeClient store.Client
	rec         *metrics.Recorder
}

// Reconcile checks whether an orphaned profile has exceeded its TTL and, if so,
// purges its Redis data and deletes the profile object.
func (r *ProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, req.NamespacedName, &profile); err != nil { // coverage:ignore - transient API error
		if apierrors.IsNotFound(err) { // coverage:ignore - transient API error
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient non-NotFound error
	}

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

	if err := r.client.Delete(ctx, &profile); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}
	r.rec.WorkloadProfilePurged(ctx, profile.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the ProfileReconciler with the manager.
func (r *ProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadwatcher-profile").
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
