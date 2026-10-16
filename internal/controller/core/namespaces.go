package core

import (
	"slices"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

// NamespaceMatcher decides which namespaces the controller reconciles Applications in: the
// control-plane namespace always, plus namespaces matching application.namespaces (APP-067).
// Entries are shell globs or regular expressions wrapped in "/" (upstream
// glob.MatchStringInList with REGEXP), the same syntax as AppProject.spec.sourceNamespaces,
// whose matcher in internal/project is reused so both checks agree.
type NamespaceMatcher struct {
	controlPlane string
	patterns     *v1alpha1.AppProject
}

// NewNamespaceMatcher returns a matcher for the control-plane namespace and patterns.
func NewNamespaceMatcher(controlPlane string, patterns []string) *NamespaceMatcher {
	var clean []string
	for _, p := range patterns {
		if p != "" {
			clean = append(clean, p)
		}
	}
	return &NamespaceMatcher{
		controlPlane: controlPlane,
		patterns:     &v1alpha1.AppProject{Spec: v1alpha1.AppProjectSpec{SourceNamespaces: clean}},
	}
}

// Allowed reports whether Applications (and their Operations) in namespace are reconciled.
func (m *NamespaceMatcher) Allowed(namespace string) bool {
	if namespace == m.controlPlane {
		return true
	}
	if namespace == "" {
		return false
	}
	return project.IsAppNamespacePermitted(m.patterns, namespace, m.controlPlane)
}

// ClusterWide reports whether namespaces other than the control-plane namespace are enabled,
// so Applications must be watched in every namespace.
func (m *NamespaceMatcher) ClusterWide() bool { return len(m.patterns.Spec.SourceNamespaces) > 0 }

// Patterns returns the configured patterns.
func (m *NamespaceMatcher) Patterns() []string { return slices.Clone(m.patterns.Spec.SourceNamespaces) }

// ControlPlane returns the control-plane namespace.
func (m *NamespaceMatcher) ControlPlane() string { return m.controlPlane }
