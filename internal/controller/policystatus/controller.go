// Package policystatus publishes each policy's profile discriminator on the
// policy's own status.
//
// A WorkloadProfile's identity includes the policy governing it, so profile names
// carry a token derived from the policy's kind, namespace, and name. That token is
// a hash, and nobody should have to compute one by hand to answer "which profiles
// does this policy govern?". Recording it on the policy closes the loop:
//
//	kubectl get workloadprofiles | grep "$(kubectl get crp fleet \
//	  -o jsonpath='{.status.profileDiscriminator}')"
//
// It is written to status rather than to a label because it is operator-derived
// data about a user-owned object, and the operator has no business mutating the
// spec or metadata of objects its users wrote.
package policystatus

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/logger"
	"github.com/tight-line/ballast/internal/naming"
)

// Reconciler maintains status.profileDiscriminator on one policy kind. Both kinds
// share this implementation: ResourcePolicyStatus is a type alias for
// ClusterResourcePolicyStatus, so one pointer serves either object.
type Reconciler struct {
	client client.Client
	kind   string
}

// NewCluster creates a Reconciler for ClusterResourcePolicy objects.
func NewCluster(c client.Client) *Reconciler {
	return &Reconciler{client: c, kind: ballastv1.KindClusterResourcePolicy}
}

// NewNamespaced creates a Reconciler for ResourcePolicy objects.
func NewNamespaced(c client.Client) *Reconciler {
	return &Reconciler{client: c, kind: ballastv1.KindResourcePolicy}
}

// Reconcile writes the policy's discriminator to its status, and is a no-op once
// the stored value agrees. The token is a pure function of the object's identity,
// so it changes only if the object is replaced under a different name.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj, status, err := r.fetch(ctx, req)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // coverage:ignore - transient API error
	}

	want := naming.PolicyDiscriminator(r.kind, req.Namespace, req.Name)
	if status.ProfileDiscriminator == want {
		return ctrl.Result{}, nil
	}

	base := obj.DeepCopyObject().(client.Object) //nolint:errcheck,forcetypeassert // DeepCopyObject of a client.Object is one
	status.ProfileDiscriminator = want
	return ctrl.Result{}, r.client.Status().Patch(ctx, obj, client.MergeFrom(base))
}

// fetch loads the policy named by req and returns it alongside a pointer to its
// status, so the caller can read and write the status without caring which kind
// it holds.
func (r *Reconciler) fetch(ctx context.Context, req ctrl.Request) (client.Object, *ballastv1.ClusterResourcePolicyStatus, error) {
	if r.kind == ballastv1.KindResourcePolicy {
		var rp ballastv1.ResourcePolicy
		if err := r.client.Get(ctx, req.NamespacedName, &rp); err != nil {
			return nil, nil, err
		}
		return &rp, &rp.Status, nil
	}

	var crp ballastv1.ClusterResourcePolicy
	if err := r.client.Get(ctx, req.NamespacedName, &crp); err != nil {
		return nil, nil, err
	}
	return &crp, &crp.Status, nil
}

// SetupWithManager registers the Reconciler for its policy kind.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	name := "policystatus-cluster"
	var obj client.Object = &ballastv1.ClusterResourcePolicy{}
	if r.kind == ballastv1.KindResourcePolicy {
		name = "policystatus-namespaced"
		obj = &ballastv1.ResourcePolicy{}
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithLogConstructor(logger.ControllerLogConstructor(mgr.GetLogger(), name)).
		For(obj).
		Complete(r); err != nil { // coverage:ignore - requires a malformed manager
		return fmt.Errorf("registering %s controller: %w", name, err)
	}
	return nil
}
