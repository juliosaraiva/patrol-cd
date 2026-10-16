package projects

import (
	"context"
	"slices"
	"strings"

	"github.com/gobwas/glob"
	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// grant is a permission (action update) a project change requires.
type grant struct {
	resource string
	object   string
}

// wildcardObject is the RBAC object checked for destinations whose cluster set cannot be
// bounded now: name globs, negated names, and names no registered cluster has (yet). Only
// a grant matching every object (such as "*") matches it.
const wildcardObject = "*"

// nameResolver returns the servers of the registered clusters with a name.
type nameResolver func(ctx context.Context, name string) ([]string, error)

// isLiteralName reports whether a destination name designates exactly the clusters with
// that name (no glob meta character, not negated).
func isLiteralName(name string) bool {
	return !strings.HasPrefix(name, "!") && glob.QuoteMeta(name) == name
}

// destinationObjects returns the RBAC objects a destination entry designates: its server
// (the pattern as written, upstream), and for a name the servers registered under it, or
// wildcardObject for a name glob or an unregistered name. An entry with neither designates
// "" (upstream DestinationClusters).
func destinationObjects(ctx context.Context, d v1alpha1.ApplicationDestination, resolve nameResolver) ([]string, error) {
	var out []string
	if d.Server != "" {
		out = append(out, d.Server)
	}
	if d.Name != "" {
		if !isLiteralName(d.Name) {
			return append(out, wildcardObject), nil
		}
		servers, err := resolve(ctx, d.Name)
		if err != nil {
			return nil, err
		}
		if len(servers) == 0 {
			return append(out, wildcardObject), nil
		}
		out = append(out, servers...)
	}
	if d.Server == "" && d.Name == "" {
		out = append(out, "")
	}
	return out, nil
}

// changedEntries returns the entries present in exactly one of a and b, compared whole
// (each kept once).
func changedEntries[T comparable](a, b []T) []T {
	var out []T
	for _, v := range a {
		if !slices.Contains(b, v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	for _, v := range b {
		if !slices.Contains(a, v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// requiredGrants returns the permissions a change of a project spec from old to next
// requires (TEN-046; upstream compares destination servers only):
//
//   - every destination added, removed or changed (server, name and namespace compared
//     together, so widening a namespace counts) needs clusters, update on every object it
//     designates (destinationObjects);
//   - every source repository added or removed needs repositories, update on it;
//   - a change of any cluster or namespace resource allow/deny list needs clusters, update
//     on every object of every destination of next;
//   - every destinationServiceAccounts entry added, removed or changed (the account
//     impersonated on a server) needs clusters, update on its server.
//
// Grants are ordered by category, each category sorted, without duplicates.
func requiredGrants(ctx context.Context, old, next *v1alpha1.AppProjectSpec, resolve nameResolver) ([]grant, error) {
	var out []grant
	add := func(resource string, objects []string) {
		slices.Sort(objects)
		for _, o := range slices.Compact(objects) {
			g := grant{resource: resource, object: o}
			if !slices.Contains(out, g) {
				out = append(out, g)
			}
		}
	}
	var objects []string
	for _, d := range changedEntries(old.Destinations, next.Destinations) {
		objs, err := destinationObjects(ctx, d, resolve)
		if err != nil {
			return nil, err
		}
		objects = append(objects, objs...)
	}
	add(rbac.ResourceClusters, objects)
	add(rbac.ResourceRepositories, symmetricDifference(next.SourceRepos, old.SourceRepos))
	sem := equality.Semantic
	if !sem.DeepEqual(old.ClusterResourceWhitelist, next.ClusterResourceWhitelist) ||
		!sem.DeepEqual(old.ClusterResourceBlacklist, next.ClusterResourceBlacklist) ||
		!sem.DeepEqual(old.NamespaceResourceWhitelist, next.NamespaceResourceWhitelist) ||
		!sem.DeepEqual(old.NamespaceResourceBlacklist, next.NamespaceResourceBlacklist) {
		objects = nil
		for _, d := range next.Destinations {
			objs, err := destinationObjects(ctx, d, resolve)
			if err != nil {
				return nil, err
			}
			objects = append(objects, objs...)
		}
		add(rbac.ResourceClusters, objects)
	}
	objects = nil
	for _, sa := range changedEntries(old.DestinationServiceAccounts, next.DestinationServiceAccounts) {
		objects = append(objects, sa.Server)
	}
	add(rbac.ResourceClusters, objects)
	return out, nil
}

// withGlobals returns p with its global projects merged (internal/project
// MergeVirtualProject) and the names of those global projects. Problems with the
// globalProjects setting are logged, as elsewhere.
func (s *Service) withGlobals(ctx context.Context, p *v1alpha1.AppProject, settings []project.GlobalProjectSetting) (*v1alpha1.AppProject, []string) {
	globals, err := project.GlobalProjects(ctx, p, settings, s)
	if err != nil {
		s.d.Logger.Warn("global projects of a project could not all be resolved", "project", p.Name, "error", err)
	}
	virtual := p.DeepCopy()
	names := make([]string, 0, len(globals))
	for _, g := range globals {
		virtual = project.MergeVirtualProject(virtual, g)
		names = append(names, g.Name)
	}
	return virtual, names
}

// checkEscalation refuses a change of a project from old to next (an update, or a creation
// with old the bare project) that widens what the project may do beyond the caller's own
// permissions. requiredGrants is applied to the project's own specs (upstream) and to the
// specs with their global projects merged, since labels select global projects whose
// destinations, sources, resource lists and service accounts the project inherits; gaining
// or losing a global project (which also escapes its deny lists and sync windows) further
// needs projects, update on that global project.
func (s *Service) checkEscalation(ctx context.Context, old, next *v1alpha1.AppProject) error {
	st, err := s.settings()
	if err != nil {
		return err
	}
	var lookup project.ClusterLookup
	resolve := func(ctx context.Context, name string) ([]string, error) {
		if lookup == nil {
			l, err := s.clusterLookup(ctx)
			if err != nil {
				return nil, err
			}
			lookup = l
		}
		return lookup.GetClusterServersByName(ctx, name)
	}
	grants, err := requiredGrants(ctx, &old.Spec, &next.Spec, resolve)
	if err != nil {
		return err
	}
	settings := globalSettings(st)
	oldVirtual, oldGlobals := s.withGlobals(ctx, old, settings)
	nextVirtual, nextGlobals := s.withGlobals(ctx, next, settings)
	for _, name := range symmetricDifference(oldGlobals, nextGlobals) {
		if g := (grant{resource: rbac.ResourceProjects, object: name}); !slices.Contains(grants, g) {
			grants = append(grants, g)
		}
	}
	inherited, err := requiredGrants(ctx, &oldVirtual.Spec, &nextVirtual.Spec, resolve)
	if err != nil {
		return err
	}
	for _, g := range inherited {
		if !slices.Contains(grants, g) {
			grants = append(grants, g)
		}
	}
	for _, g := range grants {
		if err := rest.Authorize(ctx, g.resource, rbac.ActionUpdate, g.object); err != nil {
			return err
		}
	}
	return nil
}
