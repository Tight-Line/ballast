package killswitch

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ballastv1 "github.com/tight-line/ballast/api/v1"
)

const (
	ConfigMapName     = "ballast-kill-switch"
	BallastConfigName = "ballast"
)

// KillSwitch watches the emergency ConfigMap and BallastConfig.spec.suspended.
// IsActive returns true if either trigger is live.
type KillSwitch struct {
	client    client.Client
	namespace string

	mu     sync.RWMutex
	active bool
	reason string
}

// New creates a KillSwitch that watches resources in namespace.
func New(c client.Client, namespace string) *KillSwitch {
	return &KillSwitch{client: c, namespace: namespace}
}

// IsActive reports whether the kill switch is currently active.
func (k *KillSwitch) IsActive() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.active
}

// Reason returns a human-readable description of the active trigger(s).
func (k *KillSwitch) Reason() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.reason
}

// Reconcile re-evaluates both kill switch sources and updates internal state.
func (k *KillSwitch) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var cm corev1.ConfigMap
	cmActive := false
	switch err := k.client.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: k.namespace}, &cm); {
	case err == nil:
		cmActive = true
	case apierrors.IsNotFound(err):
		// not active
	default: // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	var cfg ballastv1.BallastConfig
	cfgSuspended := false
	switch err := k.client.Get(ctx, types.NamespacedName{Name: BallastConfigName}, &cfg); {
	case err == nil:
		cfgSuspended = cfg.Spec.Suspended
	case apierrors.IsNotFound(err):
		// not suspended
	default: // coverage:ignore - transient API error
		return ctrl.Result{}, err
	}

	active := cmActive || cfgSuspended

	var reason string
	switch {
	case cmActive && cfgSuspended:
		reason = "ConfigMap " + ConfigMapName + " and BallastConfig.spec.suspended"
	case cmActive:
		reason = "ConfigMap " + ConfigMapName
	case cfgSuspended:
		reason = "BallastConfig.spec.suspended"
	}

	k.mu.Lock()
	changed := k.active != active
	k.active = active
	k.reason = reason
	k.mu.Unlock()

	if changed {
		if active {
			log.Info("Kill switch activated", "reason", reason)
		} else {
			log.Info("Kill switch deactivated")
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the kill switch controller with mgr.
func (k *KillSwitch) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, _ client.Object) []reconcile.Request {
			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{Name: ConfigMapName, Namespace: k.namespace}},
			}
		},
	)

	cmFilter := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetName() == ConfigMapName && obj.GetNamespace() == k.namespace
	})

	bcFilter := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetName() == BallastConfigName
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("killswitch").
		Watches(&corev1.ConfigMap{}, enqueue, builder.WithPredicates(cmFilter)).
		Watches(&ballastv1.BallastConfig{}, enqueue, builder.WithPredicates(bcFilter)).
		Complete(k)
}
