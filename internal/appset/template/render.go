package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ApplicationFromTemplate builds the Application an ApplicationSet template describes (before
// rendering). The template's namespace is copied but is overwritten by the caller with the
// ApplicationSet namespace after rendering.
func ApplicationFromTemplate(t v1alpha1.ApplicationSetTemplate) *v1alpha1.Application {
	t = *t.DeepCopy()
	app := &v1alpha1.Application{}
	app.Name = t.Name
	app.Namespace = t.Namespace
	app.Labels = t.Labels
	app.Annotations = t.Annotations
	app.Finalizers = t.Finalizers
	app.Spec = t.Spec
	return app
}

// RenderApplication renders every string field and map key of tmpl with params. With empty
// params the template is returned unchanged (a deep copy) and no finalizer is added. Otherwise,
// when the rendered Application declares no finalizers and syncPolicy does not preserve
// resources on deletion, the resources finalizer is added.
func RenderApplication(tmpl *v1alpha1.Application, syncPolicy *v1alpha1.ApplicationSetSyncPolicy, params map[string]any, o Options) (*v1alpha1.Application, error) {
	if tmpl == nil {
		return nil, errors.New("application template is empty")
	}
	if len(params) == 0 {
		return tmpl.DeepCopy(), nil
	}
	src := tmpl.DeepCopy()
	if err := normalizeRawExtensions(&src.Spec); err != nil {
		return nil, err
	}
	doc, err := toGeneric(src)
	if err != nil {
		return nil, fmt.Errorf("failed to encode application template: %w", err)
	}
	w := walker{params: params, opts: o, ev: newEvaluation()}
	rendered, err := w.value(doc)
	if err != nil {
		return nil, err
	}
	out := &v1alpha1.Application{}
	if err := fromGeneric(rendered, out); err != nil {
		return nil, fmt.Errorf("failed to decode rendered application: %w", err)
	}
	if (syncPolicy == nil || !syncPolicy.PreserveResourcesOnDeletion) && len(out.Finalizers) == 0 {
		out.Finalizers = []string{v1alpha1.FinalizerResources}
	}
	return out, nil
}

// ApplyTemplatePatch renders spec.templatePatch with params and applies it as a strategic merge
// patch onto app. The patch cannot change spec.project.
func ApplyTemplatePatch(app *v1alpha1.Application, templatePatch string, params map[string]any, o Options) (*v1alpha1.Application, error) {
	replaced, err := Replace(templatePatch, params, o)
	if err != nil {
		return nil, fmt.Errorf("error replacing values in templatePatch: %w", err)
	}
	appJSON, err := json.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("error while marshalling Application: %w", err)
	}
	patchJSON, err := ConvertYAMLToJSON(replaced)
	if err != nil {
		return nil, fmt.Errorf("error while converting template to json %q: %w", patchJSON, err)
	}
	if err := json.Unmarshal([]byte(patchJSON), &v1alpha1.Application{}); err != nil {
		return nil, fmt.Errorf("invalid templatePatch %q: %w", patchJSON, err)
	}
	patched, err := strategicpatch.StrategicMergePatch(appJSON, []byte(patchJSON), v1alpha1.Application{})
	if err != nil {
		return nil, fmt.Errorf("error while applying templatePatch template to json %q: %w", patchJSON, err)
	}
	out := &v1alpha1.Application{}
	if err := json.Unmarshal(patched, out); err != nil {
		return nil, fmt.Errorf("error while unmarshalling patched application: %w", err)
	}
	// The project cannot be changed by a patch: it is the security boundary of the generated
	// Application and the value the ApplicationSet author templated.
	out.Spec.Project = app.Spec.Project
	return out, nil
}

// ConvertYAMLToJSON converts str to JSON unless it already starts with '{'.
func ConvertYAMLToJSON(str string) (string, error) {
	trimmed := strings.TrimSpace(str)
	if trimmed != "" && trimmed[0] == '{' {
		return str, nil
	}
	j, err := yaml.YAMLToJSON([]byte(str))
	if err != nil {
		return str, err
	}
	return string(j), nil
}

// walker renders the strings and object keys of a generic JSON document as one evaluation.
type walker struct {
	params map[string]any
	opts   Options
	ev     *evaluation
}

func (w walker) value(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return w.ev.replace(t, w.params, w.opts)
	case map[string]any:
		return w.object(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			r, err := w.value(t[i])
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

func (w walker) object(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		val := m[k]
		if val == nil {
			continue
		}
		r, err := w.value(val)
		if err != nil {
			return nil, err
		}
		key, err := w.ev.replace(k, w.params, w.opts)
		if err != nil {
			return nil, err
		}
		out[key] = r
	}
	return out, nil
}

// normalizeRawExtensions converts YAML raw values (helm.valuesObject) to JSON so the template
// can be encoded; objects decoded from the API server already hold JSON.
func normalizeRawExtensions(spec *v1alpha1.ApplicationSpec) error {
	fix := func(re *runtime.RawExtension) error {
		if re == nil || len(re.Raw) == 0 || json.Valid(re.Raw) {
			return nil
		}
		j, err := yaml.YAMLToJSON(re.Raw)
		if err != nil {
			return fmt.Errorf("error while converting template to json %q: %w", string(re.Raw), err)
		}
		re.Raw = j
		return nil
	}
	sources := []*v1alpha1.ApplicationSource{spec.Source}
	for i := range spec.Sources {
		sources = append(sources, &spec.Sources[i])
	}
	for _, s := range sources {
		if s != nil && s.Helm != nil {
			if err := fix(s.Helm.ValuesObject); err != nil {
				return err
			}
		}
	}
	if spec.SourceHydrator != nil && spec.SourceHydrator.DrySource.Helm != nil {
		return fix(spec.SourceHydrator.DrySource.Helm.ValuesObject)
	}
	return nil
}

// toGeneric encodes v as a generic JSON document, keeping numbers exact.
func toGeneric(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func fromGeneric(doc any, out any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
