package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// checkShippedDefaults verifies that the patrol-cm defaults of values.yaml are the ones
// internal/settings documents as shipped (DefaultResourceExclusionsYAML,
// DefaultIgnoreResourceUpdates), so the chart and the static manifests cannot drift from the
// code.
func checkShippedDefaults(values []byte) error {
	var doc struct {
		Configs struct {
			CM map[string]any `json:"cm"`
		} `json:"configs"`
	}
	if err := yaml.Unmarshal(values, &doc); err != nil {
		return err
	}
	cm := doc.Configs.CM
	var errs []error
	if got, _ := cm[settings.KeyResourceExclusions].(string); got != settings.DefaultResourceExclusionsYAML {
		errs = append(errs, fmt.Errorf("configs.cm.%q differs from settings.DefaultResourceExclusionsYAML", settings.KeyResourceExclusions))
	}
	want := settings.DefaultIgnoreResourceUpdates()
	for k, v := range want {
		if got, _ := cm[k].(string); got != v {
			errs = append(errs, fmt.Errorf("configs.cm.%q differs from settings.DefaultIgnoreResourceUpdates()", k))
		}
	}
	var extra []string
	for k := range cm {
		if _, ok := want[k]; !ok && strings.HasPrefix(k, "resource.customizations.ignoreResourceUpdates.") {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	for _, k := range extra {
		errs = append(errs, fmt.Errorf("configs.cm.%q is not a shipped default of internal/settings", k))
	}
	return errors.Join(errs...)
}
