package validation

import (
	"errors"
	"strings"
)

const (
	AnnotationMeasure    = "ballast.tightlinesoftware.com/measure"
	AnnotationApply      = "ballast.tightlinesoftware.com/apply"
	AnnotationResize     = "ballast.tightlinesoftware.com/resize"
	AnnotationAutoresize = "ballast.tightlinesoftware.com/autoresize"
	AnnotationProfileRef = "ballast.tightlinesoftware.com/profile-ref"
	AnnotationPolicyRef  = "ballast.tightlinesoftware.com/policy-ref"
)

// ValidateAnnotations checks that the Ballast annotations on a pod form a valid combination.
// Invalid combinations return a non-nil error with a descriptive message.
//
// Valid rules:
//   - apply requires measure
//   - resize requires apply (which implies measure)
//   - autoresize is mutually exclusive with apply and resize
func ValidateAnnotations(annotations map[string]string) error {
	has := func(key string) bool {
		v, ok := annotations[key]
		return ok && strings.EqualFold(v, "true")
	}

	measure := has(AnnotationMeasure)
	apply := has(AnnotationApply)
	resize := has(AnnotationResize)
	autoresize := has(AnnotationAutoresize)

	var errs []string

	errs = append(errs, autoresizeConflicts(autoresize, apply, resize)...)

	// apply requires measure
	if apply && !measure {
		errs = append(errs, "apply requires measure")
	}

	// resize requires apply
	if resize && !apply {
		errs = append(errs, "resize requires apply")
	}

	if len(errs) > 0 {
		return errors.New("invalid Ballast annotation combination: " + strings.Join(errs, "; "))
	}
	return nil
}

// autoresizeConflicts returns errors when autoresize is combined with explicit action annotations.
func autoresizeConflicts(autoresize, apply, resize bool) []string {
	if !autoresize {
		return nil
	}
	var errs []string
	if apply {
		errs = append(errs, "autoresize is mutually exclusive with apply")
	}
	if resize {
		errs = append(errs, "autoresize is mutually exclusive with resize")
	}
	return errs
}
