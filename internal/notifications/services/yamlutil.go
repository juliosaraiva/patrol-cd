package services

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// parseYAMLList parses a YAML (or JSON) list rendered by a template.
func parseYAMLList(service, field, raw string) ([]any, error) {
	var out []any
	if err := yaml.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%w: %s: %s is not a YAML/JSON list: %v", ErrInvalidNotification, service, field, err)
	}
	return out, nil
}
