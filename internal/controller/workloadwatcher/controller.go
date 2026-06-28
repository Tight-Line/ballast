package workloadwatcher

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
func New(c client.Client, ks *killswitch.KillSwitch, storeClient store.Client) *Controller {
	return &Controller{
		Pod:     &PodReconciler{client: c, ks: ks},
		Profile: &ProfileReconciler{client: c, storeClient: storeClient},
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
	ks := killswitch.New(mgr.GetClient(), namespace)
	if err := ks.SetupWithManager(mgr); err != nil { // coverage:ignore - requires a malformed manager
		return err
	}
	return New(mgr.GetClient(), ks, storeClient).SetupWithManager(mgr)
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

	// Already processed: ensure finalizer is present for cleanup, then stop.
	if pod.Annotations[AnnotationProfileRef] != "" {
		if !controllerutil.ContainsFinalizer(pod, FinalizerName) {
			controllerutil.AddFinalizer(pod, FinalizerName)
			return ctrl.Result{}, r.client.Update(ctx, pod)
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

	tupleLabels, err := ExtractTupleLabels(pod.Labels, cfg.Spec.IdentityLabels)
	if err != nil {
		log.Info("pod missing required identity labels, skipping",
			"pod", pod.Name, "namespace", pod.Namespace, "error", err.Error())
		return ctrl.Result{}, nil
	}

	profName := ProfileName(tupleLabels)

	if err := r.ensureProfile(ctx, profName, tupleLabels); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	if err := r.incrementActiveWorkloads(ctx, profName); err != nil { // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	// Add finalizer before stamping the annotation so delete is always handled
	// even if the annotation stamp fails.
	if !controllerutil.ContainsFinalizer(pod, FinalizerName) {
		controllerutil.AddFinalizer(pod, FinalizerName)
		if err := r.client.Update(ctx, pod); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, r.stampProfileRef(ctx, pod, profName)
}

func (r *PodReconciler) handleDelete(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	// Kill switch does NOT suppress decrement — accounting must stay correct.
	if profName := pod.Annotations[AnnotationProfileRef]; profName != "" {
		if err := r.decrementActiveWorkloads(ctx, profName); err != nil { // coverage:ignore - transient API error
			return ctrl.Result{}, err
		}
	}

	if controllerutil.ContainsFinalizer(pod, FinalizerName) {
		controllerutil.RemoveFinalizer(pod, FinalizerName)
		return ctrl.Result{}, r.client.Update(ctx, pod)
	}
	return ctrl.Result{}, nil
}

func (r *PodReconciler) ensureProfile(ctx context.Context, profName string, tupleLabels map[string]string) error {
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

	// Status is a subresource; must be updated after creation.
	profile.Status.TupleLabels = tupleLabels
	return r.client.Status().Update(ctx, profile)
}

func (r *PodReconciler) incrementActiveWorkloads(ctx context.Context, profName string) error {
	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, types.NamespacedName{Name: profName}, &profile); err != nil { // coverage:ignore - transient API error
		return err
	}
	base := profile.DeepCopy()
	profile.Status.ActiveWorkloads++
	apimeta.RemoveStatusCondition(&profile.Status.Conditions, conditionOrphaned)
	return r.client.Status().Patch(ctx, &profile, client.MergeFrom(base))
}

func (r *PodReconciler) decrementActiveWorkloads(ctx context.Context, profName string) error {
	var profile ballastv1.WorkloadProfile
	if err := r.client.Get(ctx, types.NamespacedName{Name: profName}, &profile); err != nil { // coverage:ignore - transient API error
		if apierrors.IsNotFound(err) { // coverage:ignore - transient API error
			return nil
		}
		return err // coverage:ignore - transient non-NotFound error
	}
	base := profile.DeepCopy()
	if profile.Status.ActiveWorkloads > 0 {
		profile.Status.ActiveWorkloads--
	}
	if profile.Status.ActiveWorkloads == 0 {
		apimeta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               conditionOrphaned,
			Status:             metav1.ConditionTrue,
			Reason:             "NoActiveWorkloads",
			Message:            "No active workloads for this profile",
			LastTransitionTime: metav1.Now(),
		})
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

	return ctrl.Result{}, r.client.Delete(ctx, &profile)
}

// SetupWithManager registers the ProfileReconciler with the manager.
func (r *ProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadwatcher-profile").
		For(&ballastv1.WorkloadProfile{}).
		Complete(r)
}

// ExtractTupleLabels returns a map of identityLabel key -> pod label value.
// Returns an error listing any keys absent from podLabels.
func ExtractTupleLabels(podLabels map[string]string, identityLabels []string) (map[string]string, error) {
	out := make(map[string]string, len(identityLabels))
	var missing []string
	for _, k := range identityLabels {
		v, ok := podLabels[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		out[k] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing identity labels: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// ProfileName derives a deterministic Kubernetes-safe name from a label tuple.
// Keys are sorted alphabetically and joined with values as "key--value" pairs
// separated by "--". Each segment is sanitized to lowercase alphanumeric-and-dash.
func ProfileName(tupleLabels map[string]string) string {
	keys := make([]string, 0, len(tupleLabels))
	for k := range tupleLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, sanitizeName(k), sanitizeName(tupleLabels[k]))
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
