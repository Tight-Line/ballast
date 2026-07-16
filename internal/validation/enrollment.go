package validation

import (
	"fmt"
	"strings"
)

const (
	// LabelMode is the single enrollment label. Workloads opt in by setting it on
	// their pod template; its value selects a rung on the behavior ladder, and each
	// rung implies the ones below it. A label (rather than an annotation) is what
	// lets the API server filter pods server-side, so Ballast's informer and
	// admission webhook only ever see enrolled pods even in very large clusters.
	LabelMode = "ballast.tightlinesoftware.com/mode"

	// ModeMeasure collects utilization history only; nothing is patched or resized.
	ModeMeasure = "measure"
	// ModeApply adds admission-time request/limit patching. Implies measure.
	ModeApply = "apply"
	// ModeResize adds in-place resize of running pods. Implies apply (and measure).
	ModeResize = "resize"

	// AnnotationProfileRef records the WorkloadProfile a pod is bound to. It is an
	// output the operator stamps, not an enrollment input, so it stays an annotation.
	AnnotationProfileRef = "ballast.tightlinesoftware.com/profile-ref"
	// AnnotationPolicyRef records the policy resolved for a pod at admission time.
	// Like profile-ref it is operator output and stays an annotation.
	AnnotationPolicyRef = "ballast.tightlinesoftware.com/policy-ref"
)

// modes lists the recognized enrollment modes, lowest rung first.
var modes = []string{ModeMeasure, ModeApply, ModeResize}

// Mode returns the enrollment mode from a pod's labels, or "" when the pod
// carries no Ballast mode label.
func Mode(labels map[string]string) string {
	return labels[LabelMode]
}

// IsEnrolled reports whether labels carry a Ballast mode label with a recognized
// value. A pod with no mode label, or with an unrecognized value, is not enrolled.
func IsEnrolled(labels map[string]string) bool {
	switch Mode(labels) {
	case ModeMeasure, ModeApply, ModeResize:
		return true
	default:
		return false
	}
}

// WantsApply reports whether the mode enables admission-time request/limit
// patching. True for apply and resize (resize implies apply).
func WantsApply(labels map[string]string) bool {
	switch Mode(labels) {
	case ModeApply, ModeResize:
		return true
	default:
		return false
	}
}

// WantsResize reports whether the mode enables in-place resize of running pods.
func WantsResize(labels map[string]string) bool {
	return Mode(labels) == ModeResize
}

// ValidateMode checks the enrollment label. A pod with no mode label is simply
// not enrolled and is valid (returns nil). A pod that carries the label with an
// unrecognized value is rejected, since it opted in but named no valid behavior.
func ValidateMode(labels map[string]string) error {
	v, ok := labels[LabelMode]
	if !ok {
		return nil
	}
	if IsEnrolled(labels) {
		return nil
	}
	return fmt.Errorf("invalid Ballast %s label %q: must be one of %s",
		LabelMode, v, strings.Join(modes, ", "))
}
