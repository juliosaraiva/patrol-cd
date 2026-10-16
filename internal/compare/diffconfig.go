package compare

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// AppIgnoreRules converts an Application's spec.ignoreDifferences into diff rules.
func AppIgnoreRules(ignores v1alpha1.IgnoreDifferences) []diff.IgnoreRule {
	out := make([]diff.IgnoreRule, 0, len(ignores))
	for _, r := range ignores {
		out = append(out, diff.IgnoreRule{
			Group:                 r.Group,
			Kind:                  r.Kind,
			Name:                  r.Name,
			Namespace:             r.Namespace,
			JSONPointers:          r.JSONPointers,
			JQPathExpressions:     r.JQPathExpressions,
			ManagedFieldsManagers: r.ManagedFieldsManagers,
		})
	}
	return out
}

// NewNormalizer builds the diff normalizer of an Application: its ignoreDifferences, then the
// system overrides and the ignoreResourceStatusField rule, then knownTypeFields (upstream
// order). Unsupported known types are logged; invalid jq expressions are an error.
func NewNormalizer(st *settings.Settings, app *v1alpha1.Application, jqTimeout time.Duration, log *slog.Logger) (diff.Normalizer, error) {
	rules := append(AppIgnoreRules(app.Spec.IgnoreDifferences), st.SystemIgnoreRules()...)
	n, err := diff.NewNormalizer(diff.NormalizerConfig{
		Rules:           rules,
		KnownTypeFields: st.KnownTypeFields(),
		Ignore:          diff.IgnoreNormalizerOptions{JQTimeout: jqTimeout, Log: log},
	})
	if err != nil {
		if n == nil {
			return nil, fmt.Errorf("failed to create diff normalizer: %w", err)
		}
		log.Warn("unsupported knownTypeFields are ignored", "error", err)
	}
	return n, nil
}

func hasManagerRules(app *v1alpha1.Application, st *settings.Settings) bool {
	for _, r := range app.Spec.IgnoreDifferences {
		if len(r.ManagedFieldsManagers) > 0 {
			return true
		}
	}
	for _, r := range st.SystemIgnoreRules() {
		if len(r.ManagedFieldsManagers) > 0 {
			return true
		}
	}
	return false
}

// diffSetup is the diff configuration of one comparison (upstream DiffConfigBuilder).
type diffSetup struct {
	opts       diff.Options
	normalizer diff.Normalizer
	err        error
	conditions []Condition
}

// diffSetup builds the diff options: normalizer, aggregated roles, server-side diff
// (controller default overridden by the compare-options annotation), mutation webhook
// removal, structured merge for ServerSideApply=true, tracking normalization, and the type
// parser and dry runner when needed. Provider failures become UnknownError conditions, as
// upstream.
func (m *Manager) diffSetup(ctx context.Context, st *settings.Settings, app *v1alpha1.Application, cluster *v1alpha1.Cluster, t *tracking.ResourceTracker, method tracking.Method, targets []*unstructured.Unstructured, log *slog.Logger) diffSetup {
	var s diffSetup
	n, err := NewNormalizer(st, app, m.d.JQTimeout, log)
	if err != nil {
		s.err = err
		return s
	}
	s.normalizer = n
	o := diff.DefaultOptions()
	o.Normalizer = n
	o.IgnoreAggregatedRoles = st.CompareOptions().IgnoreAggregatedRoles
	ann := diff.ParseCompareAnnotation(app.Annotations, v1alpha1.AnnotationKeyCompareOptions)
	o.ServerSide = ann.ServerSideDiffEnabled(m.d.ServerSideDiffEnabled)
	o.IgnoreMutationWebhook = !ann.IncludeMutationWebhook
	o.StructuredMerge = app.Spec.SyncPolicy != nil && app.Spec.SyncPolicy.SyncOptions.HasOption(v1alpha1.SyncOptionKeyServerSideApply+"="+v1alpha1.SyncOptionValueTrue)
	o.SyncOptionsAnnotation = v1alpha1.AnnotationKeySyncOptions
	o.Manager = diff.DefaultManager
	labelKey, annotationKey := t.DiffTrackingKeys(method)
	o.Tracking = diff.TrackingOptions{LabelKey: labelKey, AnnotationKey: annotationKey}
	o.Log = log

	needsParser := o.ServerSide || o.StructuredMerge || hasManagerRules(app, st)
	if !needsParser {
		for _, t := range targets {
			if t != nil && diff.HasAnnotationOption(t.GetAnnotations(), v1alpha1.AnnotationKeySyncOptions, v1alpha1.SyncOptionKeyServerSideApply+"="+v1alpha1.SyncOptionValueTrue) {
				needsParser = true
				break
			}
		}
	}
	if needsParser && m.d.ServerSideDiff != nil {
		p, err := m.d.ServerSideDiff.GVKParser(ctx, cluster)
		if err != nil {
			s.conditions = append(s.conditions, newCondition(v1alpha1.ApplicationConditionUnknownError, v1alpha1.ReasonUnknownError, err.Error()))
		} else {
			o.GVKParser = p
		}
	}
	if o.ServerSide {
		if m.d.ServerSideDiff == nil {
			s.conditions = append(s.conditions, newCondition(v1alpha1.ApplicationConditionUnknownError, v1alpha1.ReasonUnknownError,
				"server-side diff is not available: no dry run provider is configured"))
		} else if dr, err := m.d.ServerSideDiff.DryRunner(ctx, cluster); err != nil {
			log.Error("CompareAppState error getting server side diff dry run applier", "error", err)
			s.conditions = append(s.conditions, newCondition(v1alpha1.ApplicationConditionUnknownError, v1alpha1.ReasonUnknownError, err.Error()))
		} else {
			o.DryRunner = dr
		}
	}
	s.opts = o
	return s
}
