package generators

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ListGenerator produces one parameter set per list element, followed by the items of
// elementsYaml.
type ListGenerator struct{}

// RequeueAfter implements Generator: the list generator never polls.
func (g *ListGenerator) RequeueAfter(*v1alpha1.ApplicationSetGenerator) time.Duration {
	return NoRequeueAfter
}

// Template implements Generator.
func (g *ListGenerator) Template(gen *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &gen.List.Template
}

// GenerateParams implements Generator. With goTemplate an element is passed as is. With
// fasttemplate every value must be a string, except "values", which must be an object of
// strings flattened to values.<key>. elementsYaml items are appended unmodified.
func (g *ListGenerator) GenerateParams(_ context.Context, gen *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	if gen == nil || gen.List == nil {
		return nil, ErrEmptyAppSetGenerator
	}
	res := make([]map[string]any, 0, len(gen.List.Elements))
	for _, item := range gen.List.Elements {
		var element map[string]any
		if err := json.Unmarshal(item.Raw, &element); err != nil {
			return nil, fmt.Errorf("error unmarshling list element %w", err)
		}
		if appSet.Spec.GoTemplate {
			if element == nil {
				element = map[string]any{}
			}
			res = append(res, element)
			continue
		}
		params := map[string]any{}
		for key, value := range element {
			if key == "values" {
				values, ok := value.(map[string]any)
				if !ok {
					return nil, errors.New("error parsing values map")
				}
				for k, v := range values {
					s, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("error parsing value as string: values.%s is %T", k, v)
					}
					params["values."+k] = s
				}
				continue
			}
			s, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("error parsing value as string: %s is %T", key, value)
			}
			params[key] = s
		}
		res = append(res, params)
	}
	if gen.List.ElementsYaml != "" {
		var yamlElements []map[string]any
		if err := yaml.Unmarshal([]byte(gen.List.ElementsYaml), &yamlElements); err != nil {
			return nil, fmt.Errorf("error unmarshling decoded ElementsYaml %w", err)
		}
		res = append(res, yamlElements...)
	}
	return res, nil
}
