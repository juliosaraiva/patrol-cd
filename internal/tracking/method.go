package tracking

import (
	"fmt"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Method is a resource tracking method (application.resourceTrackingMethod). It is an alias
// of [v1alpha1.TrackingMethod] so settings values can be passed without conversion.
type Method = v1alpha1.TrackingMethod

// Tracking methods.
const (
	// MethodAnnotation tracks resources with the tracking-id annotation (the default).
	MethodAnnotation = v1alpha1.TrackingMethodAnnotation
	// MethodLabel tracks resources with the instance label.
	MethodLabel = v1alpha1.TrackingMethodLabel
	// MethodAnnotationAndLabel tracks with the tracking-id annotation and also writes the
	// (truncated) instance label for other tools.
	MethodAnnotationAndLabel = v1alpha1.TrackingMethodAnnotationAndLabel
)

// EffectiveMethod returns the method m behaves as: MethodLabel and MethodAnnotationAndLabel
// as given, MethodAnnotation for everything else (including the empty value, which is the
// default, and unknown values, which Argo CD also treats as annotation tracking).
func EffectiveMethod(m Method) Method {
	switch m {
	case MethodLabel, MethodAnnotationAndLabel:
		return m
	default:
		return MethodAnnotation
	}
}

// UsesAnnotation reports whether the tracking-id annotation is authoritative for ownership
// under method m (every method except label).
func UsesAnnotation(m Method) bool { return EffectiveMethod(m) != MethodLabel }

// ValidateMethod returns an error when m is neither empty nor one of the three tracking
// methods. Settings loaders should call it; the tracker itself treats unknown values as
// annotation tracking.
func ValidateMethod(m Method) error {
	switch m {
	case "", MethodAnnotation, MethodLabel, MethodAnnotationAndLabel:
		return nil
	default:
		return fmt.Errorf("invalid resource tracking method %q: must be one of %q, %q or %q",
			string(m), string(MethodAnnotation), string(MethodLabel), string(MethodAnnotationAndLabel))
	}
}
