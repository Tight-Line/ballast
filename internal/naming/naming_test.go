package naming_test

import (
	"strings"
	"testing"

	"github.com/tight-line/ballast/internal/naming"
)

func TestPolicyDiscriminator_Deterministic(t *testing.T) {
	a := naming.PolicyDiscriminator("ClusterResourcePolicy", "", "fleet")
	b := naming.PolicyDiscriminator("ClusterResourcePolicy", "", "fleet")
	if a != b {
		t.Fatalf("not deterministic: %q != %q", a, b)
	}
}

func TestPolicyDiscriminator_ReadablePrefix(t *testing.T) {
	got := naming.PolicyDiscriminator("ClusterResourcePolicy", "", "fleet-defaults")
	if !strings.HasPrefix(got, "fleet-defaults-") {
		t.Errorf("discriminator %q should start with the policy name", got)
	}
	// name + dash + 8 hex characters.
	if len(got) != len("fleet-defaults")+1+8 {
		t.Errorf("discriminator %q has unexpected length %d", got, len(got))
	}
}

// Two ResourcePolicies in different namespaces may share a name while being
// entirely different policies. If the token were derived from the name alone they
// would collide, and two distinct profiles would contend for one object name.
func TestPolicyDiscriminator_DistinguishesNamespace(t *testing.T) {
	a := naming.PolicyDiscriminator("ResourcePolicy", "team-a", "defaults")
	b := naming.PolicyDiscriminator("ResourcePolicy", "team-b", "defaults")
	if a == b {
		t.Fatalf("same token %q for policies in different namespaces", a)
	}
	if !strings.HasPrefix(a, "defaults-") || !strings.HasPrefix(b, "defaults-") {
		t.Errorf("both tokens should stay readable: %q, %q", a, b)
	}
}

// A cluster-scoped and a namespace-scoped policy of the same name are also
// different policies.
func TestPolicyDiscriminator_DistinguishesKind(t *testing.T) {
	a := naming.PolicyDiscriminator("ClusterResourcePolicy", "", "defaults")
	b := naming.PolicyDiscriminator("ResourcePolicy", "", "defaults")
	if a == b {
		t.Fatalf("same token %q for different kinds", a)
	}
}

func TestPolicyDiscriminator_LongNameTruncated(t *testing.T) {
	long := strings.Repeat("a", 80)
	got := naming.PolicyDiscriminator("ClusterResourcePolicy", "", long)
	if len(got) != 24+1+8 {
		t.Errorf("discriminator %q length = %d, want readable part capped at 24", got, len(got))
	}
	// Truncation must not cost uniqueness: the hash covers the full name.
	other := naming.PolicyDiscriminator("ClusterResourcePolicy", "", long+"-different")
	if got == other {
		t.Error("two long names sharing a prefix produced the same token")
	}
}

func TestPolicyDiscriminator_UnsanitizableName(t *testing.T) {
	got := naming.PolicyDiscriminator("ClusterResourcePolicy", "", "...")
	if !strings.HasPrefix(got, "policy-") {
		t.Errorf("discriminator %q should fall back to a readable prefix", got)
	}
}

func TestProfileName_JoinsTupleInIdentityOrder(t *testing.T) {
	tuple := map[string]string{"app": "checkout", "component": "server"}
	got := naming.ProfileName(tuple, []string{"app", "component"}, "fleet-abc12345")
	if want := "checkout--server--fleet-abc12345"; got != want {
		t.Errorf("ProfileName = %q, want %q", got, want)
	}

	// identityLabels order drives the name, not map iteration order.
	got = naming.ProfileName(tuple, []string{"component", "app"}, "fleet-abc12345")
	if want := "server--checkout--fleet-abc12345"; got != want {
		t.Errorf("ProfileName = %q, want %q", got, want)
	}
}

func TestProfileName_SkipsAbsentIdentityLabels(t *testing.T) {
	got := naming.ProfileName(map[string]string{"app": "checkout"}, []string{"app", "missing"}, "x-1")
	if want := "checkout--x-1"; got != want {
		t.Errorf("ProfileName = %q, want %q", got, want)
	}
}

func TestProfileName_EmptyDiscriminator(t *testing.T) {
	got := naming.ProfileName(map[string]string{"app": "checkout"}, []string{"app"}, "")
	if want := "checkout"; got != want {
		t.Errorf("ProfileName = %q, want %q", got, want)
	}
}

func TestProfileName_SanitizesValues(t *testing.T) {
	got := naming.ProfileName(map[string]string{"app": "My_App.v2"}, []string{"app"}, "")
	if want := "my-app-v2"; got != want {
		t.Errorf("ProfileName = %q, want %q", got, want)
	}
}

// Truncation must stay inside the Kubernetes name limit and must not merge two
// distinct tuples into one profile. The discriminator's hash covers only the
// policy, so a tuple hash is appended when the readable part is cut.
func TestProfileName_TruncatesWithinLimitAndStaysUnique(t *testing.T) {
	long := strings.Repeat("a", 200)
	other := strings.Repeat("a", 199) + "b"
	labels := []string{"one", "two"}

	first := naming.ProfileName(map[string]string{"one": long, "two": long}, labels, "fleet-abc12345")
	second := naming.ProfileName(map[string]string{"one": long, "two": other}, labels, "fleet-abc12345")

	for _, name := range []string{first, second} {
		if len(name) > 253 {
			t.Errorf("name length %d exceeds 253: %q", len(name), name)
		}
	}
	if first == second {
		t.Error("two different long tuples produced the same profile name")
	}
	if !strings.HasSuffix(first, "--fleet-abc12345") {
		t.Errorf("discriminator must survive truncation, got %q", first)
	}
}

func TestProfileName_TruncatesWithoutDiscriminator(t *testing.T) {
	long := strings.Repeat("a", 300)
	got := naming.ProfileName(map[string]string{"one": long}, []string{"one"}, "")
	if len(got) > 253 {
		t.Errorf("name length %d exceeds 253", len(got))
	}
	other := naming.ProfileName(map[string]string{"one": long + "b"}, []string{"one"}, "")
	if got == other {
		t.Error("two different long tuples produced the same profile name")
	}
}

func TestSanitizeSegment(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Web", "web"},
		{"my_app.v2", "my-app-v2"},
		{"-leading-and-trailing-", "leading-and-trailing"},
		{"...", ""},
		{"a1-b2", "a1-b2"},
	}
	for _, tc := range tests {
		if got := naming.SanitizeSegment(tc.in); got != tc.want {
			t.Errorf("SanitizeSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
