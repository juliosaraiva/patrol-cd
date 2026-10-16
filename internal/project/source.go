package project

import (
	"fmt"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// IsSourcePermitted reports whether repoURL may be used by Applications of proj
// (spec.sourceRepos, upstream AppProject.IsSourcePermitted).
//
// Pattern and URL are normalized with NormalizeGitURL and matched as gobwas globs with '/'
// as separator; "*" matches everything. A source matching a "!"-prefixed entry is rejected
// regardless of the other entries. Otherwise the source is permitted when any entry matched,
// and, as upstream evaluates it, a negated entry that does NOT match the source counts as a
// match: ["!https://a/x"] permits every repository except https://a/x. An empty list permits
// nothing.
func IsSourcePermitted(proj *v1alpha1.AppProject, repoURL string) bool {
	if proj == nil {
		return false
	}
	return sourceReposPermit(proj.Spec.SourceRepos, repoURL)
}

func sourceReposPermit(sourceRepos []string, repoURL string) bool {
	src := NormalizeGitURL(repoURL)
	anyMatched := false
	for _, entry := range sourceRepos {
		var normalized string
		if isDenyPattern(entry) {
			normalized = "!" + NormalizeGitURL(strings.TrimPrefix(entry, "!"))
		} else {
			normalized = NormalizeGitURL(entry)
		}
		if matchPattern(normalized, src, true, '/') {
			anyMatched = true
		} else if isDenyPattern(normalized) {
			return false
		}
	}
	return anyMatched
}

// IsAppNamespacePermitted reports whether an Application in appNamespace may use proj
// (upstream AppProject.IsAppNamespacePermitted). Applications in the control-plane namespace,
// or without a namespace yet, may use any project; others need a spec.sourceNamespaces entry
// matching their namespace: a glob, or a regular expression wrapped in "/" (RE2 syntax).
func IsAppNamespacePermitted(proj *v1alpha1.AppProject, appNamespace, controlPlaneNamespace string) bool {
	if appNamespace == "" || appNamespace == controlPlaneNamespace {
		return true
	}
	if proj == nil {
		return false
	}
	return matchStringInList(proj.Spec.SourceNamespaces, appNamespace)
}

// CheckAppNamespace returns a *NotPermittedError when app may not use proj because of its
// namespace, nil otherwise.
func CheckAppNamespace(proj *v1alpha1.AppProject, app *v1alpha1.Application, controlPlaneNamespace string) error {
	if app == nil {
		return fmt.Errorf("no application")
	}
	if IsAppNamespacePermitted(proj, app.Namespace, controlPlaneNamespace) {
		return nil
	}
	name := ""
	if proj != nil {
		name = proj.Name
	}
	return &NotPermittedError{Application: app.Name, Namespace: app.Namespace, Project: name}
}
