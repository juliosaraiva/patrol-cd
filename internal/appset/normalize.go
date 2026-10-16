package appset

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

// NormalizeApplicationSpec returns a normalized copy of spec, so the controller does not fight
// with defaulting done elsewhere: an empty project becomes "default", an empty sync policy is
// dropped, and empty Helm/Kustomize/Directory source options are dropped (a directory keeping
// only include/exclude keeps just those), as upstream NormalizeApplicationSpec.
func NormalizeApplicationSpec(spec *v1alpha1.ApplicationSpec) *v1alpha1.ApplicationSpec {
	out := spec.DeepCopy()
	if out.Project == "" {
		out.Project = "default"
	}
	if out.SyncPolicy != nil && isEmptyJSON(out.SyncPolicy) {
		out.SyncPolicy = nil
	}
	if len(out.Sources) > 0 {
		for i := range out.Sources {
			normalizeSource(&out.Sources[i])
		}
	} else if out.Source != nil {
		normalizeSource(out.Source)
	}
	return out
}

func normalizeSource(s *v1alpha1.ApplicationSource) {
	if s.Kustomize != nil && isEmptyJSON(s.Kustomize) {
		s.Kustomize = nil
	}
	if s.Helm != nil && isEmptyJSON(s.Helm) {
		s.Helm = nil
	}
	if d := s.Directory; d != nil && !d.Recurse && isEmptyJSON(d.Jsonnet) {
		if d.Exclude == "" && d.Include == "" {
			s.Directory = nil
		} else {
			s.Directory = &v1alpha1.ApplicationSourceDirectory{Exclude: d.Exclude, Include: d.Include}
		}
	}
}

// isEmptyJSON reports whether v encodes to an empty JSON object (every field omitted).
func isEmptyJSON(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(b), []byte("{}")) || bytes.Equal(bytes.TrimSpace(b), []byte("null"))
}

// ignoreDiffer applies spec.ignoreApplicationDifferences to a live/desired Application pair.
// A nil ignoreDiffer applies nothing.
type ignoreDiffer struct {
	n *diff.IgnoreNormalizer
}

// newIgnoreDiffer compiles the rules of an ApplicationSet (group cd.patrol.example, kind
// Application; a rule without name applies to every Application). Invalid jq expressions are
// errors ("failed to build ignore diff config: ...").
func newIgnoreDiffer(rules v1alpha1.ApplicationSetIgnoreDifferences, log *slog.Logger) (*ignoreDiffer, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]diff.IgnoreRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, diff.IgnoreRule{
			Group:             v1alpha1.GroupName,
			Kind:              "Application",
			Name:              r.Name,
			JSONPointers:      append([]string(nil), r.JSONPointers...),
			JQPathExpressions: append([]string(nil), r.JQPathExpressions...),
		})
	}
	n, err := diff.NewIgnoreNormalizer(out, diff.IgnoreNormalizerOptions{Log: log})
	if err != nil {
		return nil, fmt.Errorf("failed to build ignore diff config: %w", err)
	}
	return &ignoreDiffer{n: n}, nil
}

// apply removes the ignored fields from both live and desired in place, then re-normalizes
// both specs (removing an ignored field may leave an empty parent behind). The desired name,
// namespace and type metadata cannot be changed by a rule.
func (d *ignoreDiffer) apply(ctx context.Context, live, desired *v1alpha1.Application) error {
	if d == nil {
		return nil
	}
	name, namespace := desired.Name, desired.Namespace
	liveType, desiredType := live.TypeMeta, desired.TypeMeta
	for _, app := range []*v1alpha1.Application{live, desired} {
		u, err := toUnstructuredApp(app)
		if err != nil {
			return fmt.Errorf("failed to apply ignore differences: %w", err)
		}
		if err := d.n.Normalize(ctx, u); err != nil {
			return fmt.Errorf("failed to apply ignore differences: %w", err)
		}
		normalized := &v1alpha1.Application{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, normalized); err != nil {
			return fmt.Errorf("failed to apply ignore differences: %w", err)
		}
		*app = *normalized
	}
	live.TypeMeta, desired.TypeMeta = liveType, desiredType
	desired.Name, desired.Namespace = name, namespace
	live.Spec = *NormalizeApplicationSpec(&live.Spec)
	desired.Spec = *NormalizeApplicationSpec(&desired.Spec)
	return nil
}

// toUnstructuredApp converts app with its group/version/kind set, so rules scoped to the
// Application kind match both the live object (decoded from the API) and the generated one
// (built without type metadata).
func toUnstructuredApp(app *v1alpha1.Application) (*unstructured.Unstructured, error) {
	cp := app.DeepCopy()
	cp.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Application"))
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cp)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// specsEquivalent reports whether the desired spec matches the live one after normalization
// and the ignore rules, i.e. whether the write path would leave the spec untouched.
func specsEquivalent(ctx context.Context, d *ignoreDiffer, live, desired *v1alpha1.Application) (bool, error) {
	l := live.DeepCopy()
	w := desired.DeepCopy()
	l.Spec = *NormalizeApplicationSpec(&l.Spec)
	w.Spec = *NormalizeApplicationSpec(&w.Spec)
	if err := d.apply(ctx, l, w); err != nil {
		return false, err
	}
	return jsonEqual(l.Spec, w.Spec)
}

// jsonEqual compares two values by their JSON encoding: nil and empty collections, map
// ordering and time zones compare equal, as the upstream semantic equality does for the
// Application fields.
func jsonEqual(a, b any) (bool, error) {
	ja, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ja, jb), nil
}
