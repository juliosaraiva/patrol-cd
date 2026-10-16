package compare

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// maxConditionMessage is the metav1.Condition message limit enforced by the API server.
const maxConditionMessage = 32768

// maxMergedMessages bounds how many messages of one condition type are joined.
const maxMergedMessages = 20

// comparisonConditionTypes are the condition types a comparison is authoritative for
// (upstream SetConditions evaluated types of CompareAppState).
var comparisonConditionTypes = []string{
	v1alpha1.ApplicationConditionComparisonError,
	v1alpha1.ApplicationConditionSharedResourceWarning,
	v1alpha1.ApplicationConditionRepeatedResourceWarning,
	v1alpha1.ApplicationConditionExcludedResourceWarning,
	ConditionResourcesTruncated,
}

func newCondition(t, reason, message string) Condition {
	return Condition{Type: t, Reason: reason, Message: message}
}

func comparisonError(reason, message string) Condition {
	return newCondition(v1alpha1.ApplicationConditionComparisonError, reason, message)
}

// MergeConditions folds the conditions found by a comparison into existing status
// conditions, one metav1.Condition (status True) per type:
//
//   - messages of conditions sharing a type are joined with "; " (first maxMergedMessages,
//     then a count), and bounded to the API server limit;
//   - an existing condition of a found type is replaced, keeping its lastTransitionTime when
//     it was already True;
//   - an existing condition of an evaluated type that was not found is removed (resolved);
//   - other existing conditions are kept unchanged.
//
// existing is not modified.
func MergeConditions(existing []metav1.Condition, found []Condition, evaluated []string, observedGeneration int64, now metav1.Time) []metav1.Condition {
	type group struct {
		reason   string
		messages []string
		seen     map[string]bool
	}
	order := []string{}
	groups := map[string]*group{}
	for _, c := range found {
		g, ok := groups[c.Type]
		if !ok {
			g = &group{reason: c.Reason, seen: map[string]bool{}}
			groups[c.Type] = g
			order = append(order, c.Type)
		}
		if g.seen[c.Message] {
			continue
		}
		g.seen[c.Message] = true
		g.messages = append(g.messages, c.Message)
	}
	isEvaluated := map[string]bool{}
	for _, t := range evaluated {
		isEvaluated[t] = true
	}

	out := make([]metav1.Condition, 0, len(existing)+len(order))
	done := map[string]bool{}
	for _, ec := range existing {
		g, ok := groups[ec.Type]
		switch {
		case ok:
			c := metav1.Condition{
				Type:               ec.Type,
				Status:             metav1.ConditionTrue,
				Reason:             reasonOrDefault(ec.Type, g.reason),
				Message:            joinMessages(g.messages),
				ObservedGeneration: observedGeneration,
				LastTransitionTime: now,
			}
			if ec.Status == metav1.ConditionTrue {
				c.LastTransitionTime = ec.LastTransitionTime
			}
			out = append(out, c)
			done[ec.Type] = true
		case isEvaluated[ec.Type]:
			// resolved
		default:
			out = append(out, *ec.DeepCopy())
		}
	}
	for _, t := range order {
		if done[t] {
			continue
		}
		g := groups[t]
		out = append(out, metav1.Condition{
			Type:               t,
			Status:             metav1.ConditionTrue,
			Reason:             reasonOrDefault(t, g.reason),
			Message:            joinMessages(g.messages),
			ObservedGeneration: observedGeneration,
			LastTransitionTime: now,
		})
	}
	return out
}

func joinMessages(msgs []string) string {
	shown := msgs
	extra := 0
	if len(shown) > maxMergedMessages {
		extra = len(shown) - maxMergedMessages
		shown = shown[:maxMergedMessages]
	}
	s := strings.Join(shown, "; ")
	if extra > 0 {
		s += fmt.Sprintf("; and %d more", extra)
	}
	if len(s) > maxConditionMessage {
		s = truncateUTF8(s, maxConditionMessage-3) + "..."
	}
	return s
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// reasonOrDefault returns reason, or the stable reason of the condition type.
func reasonOrDefault(conditionType, reason string) string {
	if reason != "" {
		return reason
	}
	switch conditionType {
	case v1alpha1.ApplicationConditionComparisonError:
		return v1alpha1.ReasonComparisonFailed
	case v1alpha1.ApplicationConditionInvalidSpecError:
		return v1alpha1.ReasonInvalidSpec
	case v1alpha1.ApplicationConditionSharedResourceWarning:
		return v1alpha1.ReasonSharedResource
	case v1alpha1.ApplicationConditionRepeatedResourceWarning:
		return v1alpha1.ReasonRepeatedResource
	case v1alpha1.ApplicationConditionExcludedResourceWarning:
		return v1alpha1.ReasonExcludedResource
	case v1alpha1.ApplicationConditionOrphanedResourceWarning:
		return v1alpha1.ReasonOrphanedResources
	case ConditionResourcesTruncated:
		return ReasonResourcesTruncated
	default:
		return v1alpha1.ReasonUnknownError
	}
}
