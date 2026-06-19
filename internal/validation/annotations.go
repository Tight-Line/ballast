package validation

import (
	"errors"
	"fmt"
	"strings"
)

const (
	AnnotationMeasure    = "ballast.tightlinesoftware.com/measure"
	AnnotationApply      = "ballast.tightlinesoftware.com/apply"
	AnnotationResize     = "ballast.tightlinesoftware.com/resize"
	AnnotationEvict      = "ballast.tightlinesoftware.com/evict"
	AnnotationAutoresize = "ballast.tightlinesoftware.com/autoresize"
	AnnotationAutomagic  = "ballast.tightlinesoftware.com/automagic"
	AnnotationProfileRef = "ballast.tightlinesoftware.com/profile-ref"
)

// ValidateAnnotations checks that the Ballast annotations on a pod form a valid combination.
// Invalid combinations return a non-nil error with a descriptive message.
//
// Valid rules (full dependency chain: measure -> apply -> resize -> (optionally) evict):
//   - apply requires measure
//   - resize requires apply (which implies measure)
//   - evict requires apply or resize
//   - autoresize and automagic are mutually exclusive with each other
//   - autoresize and automagic are mutually exclusive with apply, resize, and evict
func ValidateAnnotations(annotations map[string]string) error {
	has := func(key string) bool {
		v, ok := annotations[key]
		return ok && strings.EqualFold(v, "true")
	}

	measure := has(AnnotationMeasure)
	apply := has(AnnotationApply)
	resize := has(AnnotationResize)
	evict := has(AnnotationEvict)
	autoresize := has(AnnotationAutoresize)
	automagic := has(AnnotationAutomagic)

	var errs []string

	// autoresize and automagic are mutually exclusive
	if autoresize && automagic {
		errs = append(errs, "autoresize and automagic are mutually exclusive")
	}

	// autoresize/automagic are mutually exclusive with explicit apply/resize/evict
	if autoresize || automagic {
		mode := "autoresize"
		if automagic {
			mode = "automagic"
		}
		if apply {
			errs = append(errs, fmt.Sprintf("%s is mutually exclusive with apply", mode))
		}
		if resize {
			errs = append(errs, fmt.Sprintf("%s is mutually exclusive with resize", mode))
		}
		if evict {
			errs = append(errs, fmt.Sprintf("%s is mutually exclusive with evict", mode))
		}
	}

	// apply requires measure
	if apply && !measure {
		errs = append(errs, "apply requires measure")
	}

	// resize requires apply
	if resize && !apply {
		errs = append(errs, "resize requires apply")
	}

	// evict requires apply or resize
	if evict && !apply && !resize {
		errs = append(errs, "evict requires apply or resize")
	}

	if len(errs) > 0 {
		return errors.New("invalid Ballast annotation combination: " + strings.Join(errs, "; "))
	}
	return nil
}
