package policy_test

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ballastv1 "github.com/tight-line/ballast/api/v1"
	"github.com/tight-line/ballast/internal/policy"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ballastv1.AddToScheme(s); err != nil {
		t.Fatalf("adding ballastv1 to scheme: %v", err)
	}
	return s
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		Build()
}

func clusterPolicy(name string, priority int32, sel ballastv1.PolicySelector) *ballastv1.ClusterResourcePolicy {
	return &ballastv1.ClusterResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ballastv1.ClusterResourcePolicySpec{
			Priority: priority,
			Selector: sel,
		},
	}
}

func namespacedPolicy(namespace, name string, priority int32, sel ballastv1.PolicySelector) *ballastv1.ResourcePolicy {
	return &ballastv1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: ballastv1.ResourcePolicySpec{
			Priority: priority,
			Selector: sel,
		},
	}
}

func assertPolicy(t *testing.T, got *policy.ResolvedPolicy, err error, wantPolicy string, wantErr bool) {
	t.Helper()
	if wantErr {
		if err == nil {
			t.Error("expected error, got nil")
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wantPolicy == "" {
		if got != nil {
			t.Errorf("expected nil, got policy %q", got.Name)
		}
		return
	}
	if got == nil {
		t.Fatalf("expected policy %q, got nil", wantPolicy)
	}
	if got.Name != wantPolicy {
		t.Errorf("got policy %q, want %q", got.Name, wantPolicy)
	}
}

func TestResolve(t *testing.T) {
	const ns = "team-prod"

	baseInput := policy.Input{
		Namespace: ns,
		OwnerKind: "Deployment",
		Labels:    map[string]string{"app": "billing", "tier": "backend"},
		Annotations: map[string]string{
			"my.org/business-unit": "payments",
		},
	}

	tests := []struct {
		name       string
		objs       []client.Object
		input      policy.Input
		wantPolicy string // expected policy Name; "" means nil result
		wantErr    bool
	}{
		{
			name:       "no policies — nil",
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "empty selector matches everything",
			objs: []client.Object{
				clusterPolicy("default", 0, ballastv1.PolicySelector{}),
			},
			input:      baseInput,
			wantPolicy: "default",
		},
		// kinds
		{
			name: "kinds match",
			objs: []client.Object{
				clusterPolicy("dep-only", 0, ballastv1.PolicySelector{Kinds: []string{"Deployment"}}),
			},
			input:      baseInput,
			wantPolicy: "dep-only",
		},
		{
			name: "kinds mismatch",
			objs: []client.Object{
				clusterPolicy("sts-only", 0, ballastv1.PolicySelector{Kinds: []string{"StatefulSet"}}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		// namespace include
		{
			name: "namespace include exact match",
			objs: []client.Object{
				clusterPolicy("ns-exact", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Include: []string{ns}},
				}),
			},
			input:      baseInput,
			wantPolicy: "ns-exact",
		},
		{
			name: "namespace include exact no match",
			objs: []client.Object{
				clusterPolicy("ns-exact", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Include: []string{"other-ns"}},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "namespace include regex match",
			objs: []client.Object{
				clusterPolicy("ns-regex", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Include: []string{`/.*-prod/`}},
				}),
			},
			input:      baseInput,
			wantPolicy: "ns-regex",
		},
		{
			name: "namespace include regex no match",
			objs: []client.Object{
				clusterPolicy("ns-regex", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Include: []string{`/.*-dev/`}},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "namespace include list — second pattern matches",
			objs: []client.Object{
				clusterPolicy("ns-list", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Include: []string{"staging", ns}},
				}),
			},
			input:      baseInput,
			wantPolicy: "ns-list",
		},
		// namespace exclude
		{
			name: "namespace exclude exact",
			objs: []client.Object{
				clusterPolicy("exclude-exact", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Exclude: []string{ns}},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "namespace exclude regex",
			objs: []client.Object{
				clusterPolicy("exclude-regex", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Exclude: []string{`/.*-prod/`}},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "namespace not in exclude — passes",
			objs: []client.Object{
				clusterPolicy("exclude-other", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Exclude: []string{"kube-system"}},
				}),
			},
			input:      baseInput,
			wantPolicy: "exclude-other",
		},
		{
			name: "namespace matches both include and exclude — excluded",
			objs: []client.Object{
				clusterPolicy("both", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{
						Include: []string{ns},
						Exclude: []string{ns},
					},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		// annotation matching
		{
			name: "annotation exact match",
			objs: []client.Object{
				clusterPolicy("ann-exact", 0, ballastv1.PolicySelector{
					Annotations: map[string]string{"my.org/business-unit": "payments"},
				}),
			},
			input:      baseInput,
			wantPolicy: "ann-exact",
		},
		{
			name: "annotation exact mismatch",
			objs: []client.Object{
				clusterPolicy("ann-mismatch", 0, ballastv1.PolicySelector{
					Annotations: map[string]string{"my.org/business-unit": "engineering"},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "annotation key missing from pod",
			objs: []client.Object{
				clusterPolicy("ann-missing", 0, ballastv1.PolicySelector{
					Annotations: map[string]string{"my.org/cost-center": "cc-123"},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "annotation regex match",
			objs: []client.Object{
				clusterPolicy("ann-regex", 0, ballastv1.PolicySelector{
					Annotations: map[string]string{"my.org/business-unit": `/pay.*/`},
				}),
			},
			input:      baseInput,
			wantPolicy: "ann-regex",
		},
		{
			name: "annotation regex no match",
			objs: []client.Object{
				clusterPolicy("ann-regex-miss", 0, ballastv1.PolicySelector{
					Annotations: map[string]string{"my.org/business-unit": `/eng.*/`},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		// labelSelector
		{
			name: "labelSelector matches",
			objs: []client.Object{
				clusterPolicy("ls-match", 0, ballastv1.PolicySelector{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"tier": "backend"},
					},
				}),
			},
			input:      baseInput,
			wantPolicy: "ls-match",
		},
		{
			name: "labelSelector no match",
			objs: []client.Object{
				clusterPolicy("ls-nomatch", 0, ballastv1.PolicySelector{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"tier": "frontend"},
					},
				}),
			},
			input:      baseInput,
			wantPolicy: "",
		},
		{
			name: "invalid labelSelector expression",
			objs: []client.Object{
				clusterPolicy("bad-ls", 0, ballastv1.PolicySelector{
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "foo", Operator: "InvalidOp", Values: []string{"bar"}},
						},
					},
				}),
			},
			input:   baseInput,
			wantErr: true,
		},
		// precedence
		{
			name: "ResourcePolicy beats ClusterResourcePolicy regardless of priority",
			objs: []client.Object{
				clusterPolicy("cluster-high", 1000, ballastv1.PolicySelector{}),
				namespacedPolicy(ns, "ns-low", 0, ballastv1.PolicySelector{}),
			},
			input:      baseInput,
			wantPolicy: "ns-low",
		},
		{
			name: "higher priority wins within ClusterResourcePolicy",
			objs: []client.Object{
				clusterPolicy("low", 10, ballastv1.PolicySelector{}),
				clusterPolicy("high", 100, ballastv1.PolicySelector{}),
			},
			input:      baseInput,
			wantPolicy: "high",
		},
		{
			name: "alphabetical tiebreak for ClusterResourcePolicy",
			objs: []client.Object{
				clusterPolicy("beta-policy", 50, ballastv1.PolicySelector{}),
				clusterPolicy("alpha-policy", 50, ballastv1.PolicySelector{}),
			},
			input:      baseInput,
			wantPolicy: "alpha-policy",
		},
		{
			name: "alphabetical tiebreak for ResourcePolicy",
			objs: []client.Object{
				namespacedPolicy(ns, "zz-policy", 50, ballastv1.PolicySelector{}),
				namespacedPolicy(ns, "aa-policy", 50, ballastv1.PolicySelector{}),
			},
			input:      baseInput,
			wantPolicy: "aa-policy",
		},
		// ResourcePolicy error path
		{
			name: "invalid regex in ResourcePolicy namespace include",
			objs: []client.Object{
				namespacedPolicy(ns, "bad-rp", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Include: []string{`/[invalid/`}},
				}),
			},
			input:   baseInput,
			wantErr: true,
		},
		// error cases
		{
			name: "invalid regex in namespace include",
			objs: []client.Object{
				clusterPolicy("bad-include", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Include: []string{`/[invalid/`}},
				}),
			},
			input:   baseInput,
			wantErr: true,
		},
		{
			name: "invalid regex in namespace exclude",
			objs: []client.Object{
				clusterPolicy("bad-exclude", 0, ballastv1.PolicySelector{
					Namespaces: ballastv1.NamespaceSelector{Exclude: []string{`/[invalid/`}},
				}),
			},
			input:   baseInput,
			wantErr: true,
		},
		{
			name: "invalid regex in annotation",
			objs: []client.Object{
				clusterPolicy("bad-ann", 0, ballastv1.PolicySelector{
					Annotations: map[string]string{"my.org/business-unit": `/[invalid/`},
				}),
			},
			input:   baseInput,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, tc.objs...)
			r := policy.NewResolver(c, logr.Discard())
			got, err := r.Resolve(context.Background(), tc.input)
			assertPolicy(t, got, err, tc.wantPolicy, tc.wantErr)
		})
	}
}
