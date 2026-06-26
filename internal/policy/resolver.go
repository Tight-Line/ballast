package policy

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ballastv1 "github.com/tight-line/ballast/api/v1"
)

// Input holds the pod attributes used to evaluate policy selectors.
type Input struct {
	// Namespace is the pod's namespace.
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
}

// policyCandidate is an intermediate match collected during policy resolution.
type policyCandidate struct {
	spec       ballastv1.ClusterResourcePolicySpec
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
	r.log.Info("resolved policy",
		"namespace", in.Namespace,
		"ownerKind", in.OwnerKind,
		"policy", best.name,
		"namespaced", best.namespaced,
		"priority", best.priority,
	)

	return &ResolvedPolicy{
		Spec:       best.spec,
		Name:       best.name,
		Namespaced: best.namespaced,
	}, nil
}

// collectMatches lists all ResourcePolicies and ClusterResourcePolicies that match in.
func (r *Resolver) collectMatches(ctx context.Context, in Input) ([]policyCandidate, error) {
	var matches []policyCandidate

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
			})
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
