// Package naming derives the deterministic Kubernetes object names Ballast uses
// for WorkloadProfiles, along with the policy tokens those names embed. It is a
// leaf package so that the webhook and the workloadwatcher, which must agree on
// every profile name exactly, can share one implementation.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

const (
	// maxNameLength is the Kubernetes object-name limit. A WorkloadProfile name
	// is only ever stored in object names and annotations, never in a label
	// value, so the full 253 characters are available rather than the
	// 63-character ceiling that would apply to a label.
	maxNameLength = 253

	// shortHashLength is the width, in hex characters, of the hash suffixes
	// appended to a name segment. Eight characters (32 bits) is ample for
	// disambiguating the handful of policies and tuples in one cluster while
	// staying short enough to read in kubectl output.
	shortHashLength = 8

	// maxPolicySegment bounds the readable part of a policy discriminator so a
	// verbose policy name cannot crowd the identity tuple out of the name.
	maxPolicySegment = 24

	// NoPolicy is the discriminator for a profile whose pods currently match no
	// policy. Such a profile still accrues nothing and is skipped by the metrics
	// collector, but it exists so the pods are tracked and so the profile's
	// identity changes (and it orphans) the moment a policy starts matching.
	NoPolicy = "nopolicy"

	// segmentSeparator joins the identity-tuple values to each other and the
	// tuple to the policy discriminator.
	segmentSeparator = "--"
)

// PolicyDiscriminator returns the stable token identifying one policy within a
// WorkloadProfile name, in the form "<readable-name>-<hash>".
//
// The hash covers kind, namespace, and name rather than the name alone, because
// two ResourcePolicies in different namespaces may share a name while being
// entirely different policies. Hashing the name alone would give them the same
// token, and two distinct profiles would then contend for one object name.
//
// The token is a pure function of the policy's identity and never of the
// workload's, so it is identical across every profile that resolves to this
// policy. That is what makes it meaningful to publish on the policy's own status
// as status.profileDiscriminator.
func PolicyDiscriminator(kind, namespace, name string) string {
	readable := SanitizeSegment(name)
	if len(readable) > maxPolicySegment {
		readable = strings.Trim(readable[:maxPolicySegment], "-")
	}
	if readable == "" {
		// Sanitization can empty a segment (a name of only non-alphanumerics).
		// The hash still disambiguates; this only keeps the token from starting
		// with a dash.
		readable = "policy"
	}
	return readable + "-" + shortHash(kind+"/"+namespace+"/"+name)
}

// ProfileName derives a deterministic, DNS-safe WorkloadProfile name from an
// identity tuple and the discriminator of the policy governing that tuple.
// Tuple values are joined in identityLabels order and the discriminator is
// appended last, so a name reads as "<tuple>--<policy>-<hash>".
//
// An empty discriminator produces a tuple-only name; callers that resolve policy
// pass NoPolicy rather than "" when nothing matched, so a bare tuple name means
// "policy was not considered", not "no policy matched".
func ProfileName(tupleLabels map[string]string, identityLabels []string, discriminator string) string {
	parts := make([]string, 0, len(identityLabels))
	for _, k := range identityLabels {
		if v, ok := tupleLabels[k]; ok {
			parts = append(parts, SanitizeSegment(v))
		}
	}
	tuple := strings.Join(parts, segmentSeparator)

	var suffix string
	if discriminator != "" {
		suffix = segmentSeparator + discriminator
	}

	if budget := maxNameLength - len(suffix); len(tuple) > budget {
		// Truncating the readable tuple can map two distinct tuples onto one
		// name, which would silently merge two workloads into one profile. The
		// hash of the full tuple is appended so the truncated form stays unique;
		// the discriminator's own hash covers only the policy and cannot serve
		// here. Only pathological label values reach this path.
		suffix = "-" + shortHash(tuple) + suffix
		tuple = strings.Trim(tuple[:maxNameLength-len(suffix)], "-")
	}
	return tuple + suffix
}

// SanitizeSegment converts s to a lowercase DNS-label-safe segment: letters,
// digits, and dashes survive, everything else becomes a dash, and leading and
// trailing dashes are trimmed.
func SanitizeSegment(s string) string {
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

// shortHash returns the first shortHashLength hex characters of the SHA-256 of s.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:shortHashLength]
}
