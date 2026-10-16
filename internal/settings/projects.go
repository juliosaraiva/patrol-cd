package settings

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

// GlobalProject is one globalProjects entry (TEN-037): AppProjects matching Selector inherit
// the lists of ProjectName.
type GlobalProject struct {
	ProjectName   string
	LabelSelector metav1.LabelSelector
	// Selector is LabelSelector compiled.
	Selector labels.Selector
}

func (g GlobalProject) clone() GlobalProject {
	out := g
	out.LabelSelector = *g.LabelSelector.DeepCopy()
	if g.Selector != nil {
		out.Selector = g.Selector.DeepCopySelector()
	}
	return out
}

type globalProjectDoc struct {
	ProjectName   string               `json:"projectName,omitempty"`
	LabelSelector metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// parseGlobalProjects decodes globalProjects. As upstream, processing stops at the first
// entry whose selector is invalid (later entries are dropped); that entry is reported.
func (p *parser) parseGlobalProjects(v string) ([]GlobalProject, error) {
	docs := make([]globalProjectDoc, 0)
	if v != "" {
		if err := yaml.Unmarshal([]byte(v), &docs); err != nil {
			return nil, fmt.Errorf("error unmarshalling global project settings: %w", err)
		}
	}
	out := make([]GlobalProject, 0, len(docs))
	for i, d := range docs {
		sel, err := metav1.LabelSelectorAsSelector(&d.LabelSelector)
		if err != nil {
			p.errorf(SourceConfigMap, KeyGlobalProjects, "entry %d (%q): invalid labelSelector: %v; this and later entries are ignored", i, d.ProjectName, err)
			break
		}
		if d.ProjectName == "" {
			p.warn(SourceConfigMap, KeyGlobalProjects, "entry %d has no projectName", i)
		}
		out = append(out, GlobalProject{ProjectName: d.ProjectName, LabelSelector: d.LabelSelector, Selector: sel})
	}
	return out, nil
}
