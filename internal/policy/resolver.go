package policy

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ballastv1 "github.com/tight-line/ballast/api/v1"
)

// Input holds the pod attributes used to evaluate policy selectors.
//
// Every field must describe a single real pod. Resolution is a per-pod question,
// and answering it from a partially-filled Input silently changes the answer: an
// empty Namespace excludes every ResourcePolicy, an empty OwnerKind matches only
// policies with no Kinds, and absent Annotations match only policies with no
// annotation selector. Callers with a pod should build the Input with InputForPod
// rather than assembling it field by field.
type Input struct {
	// Namespace is the pod's namespace. Empty means "no namespace is known", and
	// no ResourcePolicy can match, since a namespace-scoped policy cannot be
	// established as the more specific match for a workload whose namespace is
	// unknown.
	Namespace string
	// OwnerKind is the pre-resolved top-level owner kind (e.g. "Deployment", "StatefulSet").
	// Callers walk ownerReferences to resolve this before calling Resolve.
	// Empty string means no owner (standalone pod); only policies with empty Kinds match.
	OwnerKind string
	// Labels are the pod's labels.
	Labels map[string]string
	// Annotations are the pod's annotations.
	Annotations map[string]string
}

// ResolvedPolicy is the result of a successful policy resolution.
type ResolvedPolicy struct {
	// Spec is the effective policy configuration. ResourcePolicySpec is a type alias for
	// ClusterResourcePolicySpec, so this holds the spec regardless of which kind matched.
	Spec ballastv1.ClusterResourcePolicySpec
	// Name is the policy object name; used to stamp ballast.tightlinesoftware.com/policy-ref.
	Name string
	// Namespaced is true for a ResourcePolicy, false for a ClusterResourcePolicy.
	Namespaced bool
	// Ref identifies the policy object, for recording on a WorkloadProfile's
	// status.policyRef and for loading the same policy again via Load.
	Ref ballastv1.PolicyReference
}

// PodAnnotationValue returns the value stamped into a pod's policy-ref
// annotation: "namespace/name" for a ResourcePolicy, and the bare name for a
// ClusterResourcePolicy, which has no namespace.
func PodAnnotationValue(ref ballastv1.PolicyReference) string {
	if ref.Namespace != "" {
		return ref.Namespace + "/" + ref.Name
	}
	return ref.Name
}

// policyCandidate is an intermediate match collected during policy resolution.
type policyCandidate struct {
	spec       ballastv1.ClusterResourcePolicySpec
	ref        ballastv1.PolicyReference
	name       string
	priority   int32
	namespaced bool
}

// Resolver selects the single effective policy for a given pod.
type Resolver struct {
	client client.Client
	log    logr.Logger
}

// NewResolver creates a Resolver backed by the given controller-runtime client.
func NewResolver(c client.Client, log logr.Logger) *Resolver {
	return &Resolver{client: c, log: log}
}

// Resolve returns the effective policy for the given pod, or nil if no policy matches.
//
// Precedence rules:
//   - ResourcePolicy (namespace-scoped) beats ClusterResourcePolicy regardless of priority.
//   - Within the same class, higher Priority wins.
//   - Equal priority ties break alphabetically by policy name.
//
// The first rule holds only because in.Namespace names the pod's own namespace:
// "namespaced" is shorthand for "more specific to this workload". An Input with
// no namespace has no such candidates to rank, because collectMatches admits no
// ResourcePolicy in that case.
func (r *Resolver) Resolve(ctx context.Context, in Input) (*ResolvedPolicy, error) {
	matches, err := r.collectMatches(ctx, in)
	if err != nil {
		return nil, err
	}

	if len(matches) == 0 {
		return nil, nil
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].namespaced != matches[j].namespaced {
			return matches[i].namespaced
		}
		if matches[i].priority != matches[j].priority {
			return matches[i].priority > matches[j].priority
		}
		return matches[i].name < matches[j].name
	})

	best := matches[0]
	r.log.V(1).Info("resolved policy",
		"namespace", in.Namespace,
		"ownerKind", in.OwnerKind,
		"policy", best.name,
		"namespaced", best.namespaced,
		"priority", best.priority,
	)

	// best.spec is a value copy of the cached object's spec, so filling its
	// unset fields here cannot mutate the informer cache (ApplyDefaults only
	// writes value fields and nil maps). Defaulting at resolve time, rather
	// than via CRD schema defaults, keeps sparse policies tracking the running
	// release's defaults instead of freezing whatever was current when the
	// object was written.
	best.spec.ApplyDefaults()

	return &ResolvedPolicy{
		Spec:       best.spec,
		Name:       best.name,
		Namespaced: best.namespaced,
		Ref:        best.ref,
	}, nil
}

// Load returns the policy named by ref with defaults applied, or nil when the
// object no longer exists.
//
// Callers holding a WorkloadProfile use this instead of Resolve: the profile's
// status.policyRef was written by the workloadwatcher from a full per-pod Input,
// whereas a profile on its own supplies neither a namespace nor the pod labels
// outside its identity tuple, so re-resolving from it would reach a different
// answer than admission did.
func (r *Resolver) Load(ctx context.Context, ref ballastv1.PolicyReference) (*ResolvedPolicy, error) {
	var spec ballastv1.ClusterResourcePolicySpec

	switch ref.Kind {
	case ballastv1.KindResourcePolicy:
		var rp ballastv1.ResourcePolicy
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &rp); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("getting ResourcePolicy %s/%s: %w", ref.Namespace, ref.Name, err) // coverage:ignore - transient API error
		}
		spec = rp.Spec
	case ballastv1.KindClusterResourcePolicy:
		var crp ballastv1.ClusterResourcePolicy
		if err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name}, &crp); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("getting ClusterResourcePolicy %s: %w", ref.Name, err) // coverage:ignore - transient API error
		}
		spec = crp.Spec
	default:
		return nil, fmt.Errorf("unknown policy kind %q in reference %s", ref.Kind, ref.Key())
	}

	// Defaulting happens here for the same reason it happens in Resolve: sparse
	// policies should track the running release's defaults rather than whatever
	// was current when the object was written.
	spec.ApplyDefaults()

	return &ResolvedPolicy{
		Spec:       spec,
		Name:       ref.Name,
		Namespaced: ref.Kind == ballastv1.KindResourcePolicy,
		Ref:        ref,
	}, nil
}

// collectMatches lists all ResourcePolicies and ClusterResourcePolicies that match in.
//
// ResourcePolicies are listed only when in.Namespace is set. client.InNamespace("")
// means *all namespaces*, so listing unconditionally would make every
// ResourcePolicy in the cluster a candidate for an Input with no namespace, and
// the scope-before-priority rule in Resolve would then hand any one of them
// precedence over every ClusterResourcePolicy.
func (r *Resolver) collectMatches(ctx context.Context, in Input) ([]policyCandidate, error) {
	var matches []policyCandidate

	if in.Namespace != "" {
		var rpList ballastv1.ResourcePolicyList
		if err := r.client.List(ctx, &rpList, client.InNamespace(in.Namespace)); err != nil { // coverage:ignore - client List failure requires envtest
			return nil, fmt.Errorf("listing ResourcePolicies in %s: %w", in.Namespace, err)
		}
		for _, rp := range rpList.Items {
			ok, err := r.matchesSelector(in, rp.Spec.Selector)
			if err != nil {
				return nil, fmt.Errorf("evaluating ResourcePolicy %s/%s: %w", in.Namespace, rp.Name, err)
			}
			if ok {
				matches = append(matches, policyCandidate{
					spec:       rp.Spec,
					name:       rp.Name,
					priority:   rp.Spec.Priority,
					namespaced: true,
					ref: ballastv1.PolicyReference{
						Kind:      ballastv1.KindResourcePolicy,
						Namespace: in.Namespace,
						Name:      rp.Name,
					},
				})
			}
		}
	}

	var crpList ballastv1.ClusterResourcePolicyList
	if err := r.client.List(ctx, &crpList); err != nil { // coverage:ignore - client List failure requires envtest
		return nil, fmt.Errorf("listing ClusterResourcePolicies: %w", err)
	}
	for _, crp := range crpList.Items {
		ok, err := r.matchesSelector(in, crp.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("evaluating ClusterResourcePolicy %s: %w", crp.Name, err)
		}
		if ok {
			matches = append(matches, policyCandidate{
				spec:       crp.Spec,
				name:       crp.Name,
				priority:   crp.Spec.Priority,
				namespaced: false,
				ref: ballastv1.PolicyReference{
					Kind: ballastv1.KindClusterResourcePolicy,
					Name: crp.Name,
				},
			})
		}
	}

	return matches, nil
}

func (r *Resolver) matchesSelector(in Input, sel ballastv1.PolicySelector) (bool, error) {
	if len(sel.Kinds) > 0 && !slices.Contains(sel.Kinds, in.OwnerKind) {
		return false, nil
	}

	nsOk, err := r.matchesNamespaceSelector(in.Namespace, sel.Namespaces)
	if err != nil {
		return false, err
	}
	if !nsOk {
		return false, nil
	}

	annOk, err := matchesAnnotations(in.Annotations, sel.Annotations)
	if err != nil {
		return false, err
	}
	if !annOk {
		return false, nil
	}

	if sel.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(sel.LabelSelector)
		if err != nil {
			return false, fmt.Errorf("parsing labelSelector: %w", err)
		}
		if !selector.Matches(labels.Set(in.Labels)) {
			return false, nil
		}
	}

	return true, nil
}

func (r *Resolver) matchesNamespaceSelector(namespace string, sel ballastv1.NamespaceSelector) (bool, error) {
	excluded := false
	for _, pattern := range sel.Exclude {
		matched, err := matchesPattern(namespace, pattern)
		if err != nil {
			return false, fmt.Errorf("namespace exclude pattern %q: %w", pattern, err)
		}
		if matched {
			excluded = true
			break
		}
	}

	included := len(sel.Include) == 0
	for _, pattern := range sel.Include {
		matched, err := matchesPattern(namespace, pattern)
		if err != nil {
			return false, fmt.Errorf("namespace include pattern %q: %w", pattern, err)
		}
		if matched {
			included = true
			break
		}
	}

	if included && excluded {
		r.log.Info("namespace matches both include and exclude; treating as excluded",
			"namespace", namespace,
		)
		return false, nil
	}

	return included && !excluded, nil
}

// matchesAnnotations returns true when podAnnotations satisfies every pattern in selectorAnnotations.
func matchesAnnotations(podAnnotations, selectorAnnotations map[string]string) (bool, error) {
	for key, pattern := range selectorAnnotations {
		val, ok := podAnnotations[key]
		if !ok {
			return false, nil
		}
		matched, err := matchesPattern(val, pattern)
		if err != nil {
			return false, fmt.Errorf("annotation key %q pattern %q: %w", key, pattern, err)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

// matchesPattern returns true if s matches the pattern.
// Patterns wrapped in forward slashes (e.g. /.*-prod/) are treated as full-string
// regular expressions, anchored at both ends. All other patterns are exact string matches.
func matchesPattern(s, pattern string) (bool, error) {
	if len(pattern) >= 2 && pattern[0] == '/' && pattern[len(pattern)-1] == '/' {
		inner := pattern[1 : len(pattern)-1]
		re, err := regexp.Compile(`^(?:` + inner + `)$`)
		if err != nil {
			return false, fmt.Errorf("invalid regex %q: %w", pattern, err)
		}
		return re.MatchString(s), nil
	}
	return s == pattern, nil
}
