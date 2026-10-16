package app

import (
	"fmt"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// resourceSelector is one --resource filter: GROUP:KIND:NAME or GROUP:KIND:NAMESPACE/NAME,
// optionally prefixed with "!" (exclusion). Group, kind, name and namespace may be "*";
// an empty namespace matches any namespace.
type resourceSelector struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Exclude   bool
}

// String renders the selector as given on the command line.
func (s resourceSelector) String() string {
	name := s.Name
	if s.Namespace != "" {
		name = s.Namespace + "/" + s.Name
	}
	prefix := ""
	if s.Exclude {
		prefix = "!"
	}
	return prefix + s.Group + ":" + s.Kind + ":" + name
}

// matches reports whether the selector names the resource (upstream
// SyncOperationResource.Compare).
func (s resourceSelector) matches(group, kind, namespace, name string) bool {
	return (s.Group == "*" || s.Group == group) &&
		(s.Kind == "*" || s.Kind == kind) &&
		(s.Name == "*" || s.Name == name) &&
		(s.Namespace == "*" || s.Namespace == "" || s.Namespace == namespace)
}

// parseResourceSelectors parses --resource values (upstream parseSelectedResources).
func parseResourceSelectors(values []string) ([]resourceSelector, error) {
	out := make([]resourceSelector, 0, len(values))
	for _, value := range values {
		resource := value
		exclude := false
		if after, ok := strings.CutPrefix(resource, "!"); ok {
			resource = after
			exclude = true
		}
		fields := strings.Split(resource, ":")
		if len(fields) != 3 {
			return nil, fmt.Errorf("resource should have GROUP:KIND:NAME, but instead got: %s", resource)
		}
		name, namespace := fields[2], ""
		if strings.Contains(name, "/") {
			parts := strings.Split(name, "/")
			if len(parts) != 2 {
				return nil, fmt.Errorf("resource with namespace should have GROUP:KIND:NAMESPACE/NAME, but instead got: %s", resource)
			}
			namespace, name = parts[0], parts[1]
		}
		out = append(out, resourceSelector{Group: fields[0], Kind: fields[1], Namespace: namespace, Name: name, Exclude: exclude})
	}
	return out, nil
}

// includeResource applies include then exclude selectors (upstream argo.IncludeResource):
// without include selectors everything is included; an exclusion always wins.
func includeResource(selectors []resourceSelector, group, kind, namespace, name string) bool {
	included, hasInclude := false, false
	for _, s := range selectors {
		if s.Exclude {
			continue
		}
		hasInclude = true
		if s.matches(group, kind, namespace, name) {
			included = true
			break
		}
	}
	if !hasInclude {
		included = true
	}
	if !included {
		return false
	}
	for _, s := range selectors {
		if s.Exclude && s.matches(group, kind, namespace, name) {
			return false
		}
	}
	return true
}

// selectAppResources expands selectors into the concrete resources of the Application they
// select (upstream filterAppResources). Patrol's sync API takes concrete resources, so
// wildcards and exclusions are resolved here against status.resources.
func selectAppResources(app *v1alpha1.Application, selectors []resourceSelector) []v1alpha1.SyncOperationResource {
	if app == nil || len(selectors) == 0 {
		return nil
	}
	var out []v1alpha1.SyncOperationResource
	for _, r := range app.Status.Resources {
		if includeResource(selectors, r.Group, r.Kind, r.Namespace, r.Name) {
			out = append(out, v1alpha1.SyncOperationResource{Group: r.Group, Kind: r.Kind, Name: r.Name, Namespace: r.Namespace})
		}
	}
	return out
}
