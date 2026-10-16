package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/gobwas/glob"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// SettingGlobalProjects is the patrol-cm key holding the global project settings.
const SettingGlobalProjects = "globalProjects"

// GlobalProjectSetting is one entry of the globalProjects setting: every project matched by
// LabelSelector inherits from the project ProjectName.
type GlobalProjectSetting struct {
	ProjectName   string               `json:"projectName,omitempty"`
	LabelSelector metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// ParseGlobalProjectSettings parses the YAML list stored under globalProjects. An empty value
// yields no settings.
func ParseGlobalProjectSettings(value string) ([]GlobalProjectSetting, error) {
	settings := make([]GlobalProjectSetting, 0)
	if value == "" {
		return settings, nil
	}
	if err := yaml.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("error unmarshalling global project settings: %w", err)
	}
	return settings, nil
}

// ProjectGetter reads AppProjects from the control-plane namespace. A missing project must
// be reported with an error for which apierrors.IsNotFound or errors.Is(err,
// ErrProjectNotFound) is true.
type ProjectGetter interface {
	GetProject(ctx context.Context, name string) (*v1alpha1.AppProject, error)
}

// GlobalProjects returns the global projects proj inherits from, in configuration order
// (upstream GetGlobalProjects): an entry applies when its label selector matches proj's
// labels and it names another project. Like upstream, an invalid selector or a global
// project that cannot be read stops the evaluation: the projects collected so far are
// returned together with an error describing why the remaining entries were skipped
// (callers log it and go on).
func GlobalProjects(ctx context.Context, proj *v1alpha1.AppProject, settings []GlobalProjectSetting, getter ProjectGetter) ([]*v1alpha1.AppProject, error) {
	var out []*v1alpha1.AppProject
	if proj == nil {
		return nil, nil
	}
	for _, gp := range settings {
		if gp.ProjectName == proj.Name {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&gp.LabelSelector)
		if err != nil {
			return out, fmt.Errorf("global project %q: invalid label selector, ignoring this and later entries: %w", gp.ProjectName, err)
		}
		if !selector.Matches(labels.Set(proj.Labels)) {
			continue
		}
		if getter == nil {
			return out, fmt.Errorf("global project %q: no project getter, ignoring this and later entries", gp.ProjectName)
		}
		global, err := getter.GetProject(ctx, gp.ProjectName)
		if err != nil {
			return out, fmt.Errorf("global project %q: ignoring this and later entries: %w", gp.ProjectName, err)
		}
		if global != nil {
			out = append(out, global)
		}
	}
	return out, nil
}

// MergeVirtualProject returns a copy of proj with the inheritable lists of global appended
// (upstream mergeVirtualProject): clusterResourceWhitelist/Blacklist,
// namespaceResourceWhitelist/Blacklist, syncWindows, sourceRepos, destinations and
// destinationServiceAccounts. Roles, sourceNamespaces, orphanedResources, signature keys,
// source integrity, permitOnlyProjectScopedClusters and description are not inherited.
// Appending to an absent namespaceResourceWhitelist turns "everything" into "only the
// inherited kinds", as upstream.
func MergeVirtualProject(proj, global *v1alpha1.AppProject) *v1alpha1.AppProject {
	out := proj.DeepCopy()
	if out == nil || global == nil {
		return out
	}
	g := global.Spec
	out.Spec.ClusterResourceWhitelist = appendCopy(out.Spec.ClusterResourceWhitelist, g.ClusterResourceWhitelist)
	out.Spec.ClusterResourceBlacklist = appendCopy(out.Spec.ClusterResourceBlacklist, g.ClusterResourceBlacklist)
	out.Spec.NamespaceResourceWhitelist = appendCopy(out.Spec.NamespaceResourceWhitelist, g.NamespaceResourceWhitelist)
	out.Spec.NamespaceResourceBlacklist = appendCopy(out.Spec.NamespaceResourceBlacklist, g.NamespaceResourceBlacklist)
	for _, w := range g.SyncWindows {
		out.Spec.SyncWindows = append(out.Spec.SyncWindows, *w.DeepCopy())
	}
	out.Spec.SourceRepos = appendCopy(out.Spec.SourceRepos, g.SourceRepos)
	out.Spec.Destinations = appendCopy(out.Spec.Destinations, g.Destinations)
	out.Spec.DestinationServiceAccounts = appendCopy(out.Spec.DestinationServiceAccounts, g.DestinationServiceAccounts)
	return out
}

// appendCopy appends b to a; nil+nil stays nil (an absent whitelist stays absent).
func appendCopy[T any](a, b []T) []T {
	if len(b) == 0 {
		return a
	}
	return append(slices.Clip(a), b...)
}

// VirtualProject returns proj with every applicable global project merged (upstream
// GetAppVirtualProject). The error, if any, comes from GlobalProjects; the returned project
// then includes the global projects collected before the failure.
func VirtualProject(ctx context.Context, proj *v1alpha1.AppProject, settings []GlobalProjectSetting, getter ProjectGetter) (*v1alpha1.AppProject, error) {
	if proj == nil {
		return nil, nil
	}
	globals, err := GlobalProjects(ctx, proj, settings, getter)
	virtual := proj.DeepCopy()
	for _, g := range globals {
		virtual = MergeVirtualProject(virtual, g)
	}
	return virtual, err
}

// ScopedRepositoryLister returns the URLs of the (read) repositories scoped to a project
// (Repository.spec.project).
type ScopedRepositoryLister interface {
	GetProjectRepositoryURLs(ctx context.Context, project string) ([]string, error)
}

// Lookups bundles the read access EffectiveProject and ResolveAppProject need. Any field may
// be nil: a nil Repositories or Clusters adds no scoped resources, a nil Projects disables
// global projects.
type Lookups struct {
	Projects       ProjectGetter
	Repositories   ScopedRepositoryLister
	Clusters       ClusterLookup
	GlobalProjects []GlobalProjectSetting
}

// EffectiveProject returns the project an Application is evaluated against (upstream
// GetAppProjectByName): a copy of proj with the URLs of its scoped repositories appended to
// sourceRepos (TEN-038), a destination per scoped cluster ({server, "*"} or one per listed
// namespace, TEN-039), and the global projects merged (TEN-037). Global project failures are
// returned in globalErr (non-fatal, log it); lookup failures of scoped resources are fatal.
//
// Scoped resources are identities, not patterns (deliberate difference from upstream, which
// appends the raw values): each widens the project by exactly itself. A scoped repository is
// appended only when its normalized URL is a non-empty glob literal that is not negated (see
// scopedRepoEntry); a scoped cluster's server and namespaces are appended with every glob
// meta character and a leading '!' escaped (see literalPattern). The project's own deny
// entries still apply to scoped resources.
func EffectiveProject(ctx context.Context, proj *v1alpha1.AppProject, l Lookups) (effective *v1alpha1.AppProject, globalErr error, err error) {
	if proj == nil {
		return nil, nil, fmt.Errorf("no project: %w", ErrProjectNotFound)
	}
	out := proj.DeepCopy()
	if l.Repositories != nil {
		urls, err := l.Repositories.GetProjectRepositoryURLs(ctx, proj.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("error getting project repositories: %w", err)
		}
		for _, u := range urls {
			if entry, ok := scopedRepoEntry(u); ok {
				out.Spec.SourceRepos = append(out.Spec.SourceRepos, entry)
				continue
			}
			warnIgnoredScopedRepo(proj.Name, u)
		}
	}
	if l.Clusters != nil {
		clusters, err := l.Clusters.GetProjectClusters(ctx, proj.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("error getting project clusters: %w", err)
		}
		for _, c := range clusters {
			if c == nil {
				continue
			}
			server := normalizeServer(c.Spec.Server)
			if server == "" {
				continue
			}
			server = literalPattern(server)
			if len(c.Spec.Namespaces) == 0 {
				out.Spec.Destinations = append(out.Spec.Destinations, v1alpha1.ApplicationDestination{Server: server, Namespace: "*"})
				continue
			}
			for _, ns := range c.Spec.Namespaces {
				out.Spec.Destinations = append(out.Spec.Destinations, v1alpha1.ApplicationDestination{Server: server, Namespace: literalPattern(ns)})
			}
		}
	}
	virtual, gerr := VirtualProject(ctx, out, l.GlobalProjects, l.Projects)
	return virtual, gerr, nil
}

// scopedRepoEntry returns the sourceRepos entry that permits exactly the scoped repository
// url, or false when url cannot be expressed as such an entry. sourceRepos entries are
// normalized before matching, so the entry is url itself and the check is made on its
// normalized form: it must be non-empty (an unparsable URL normalizes to "", which would
// match every unparsable source), contain no glob meta character, and neither url nor its
// normalized form may start with '!' (a negated entry permits every other repository).
func scopedRepoEntry(url string) (string, bool) {
	normalized := NormalizeGitURL(url)
	if normalized == "" || isDenyPattern(url) || isDenyPattern(normalized) || glob.QuoteMeta(normalized) != normalized {
		return "", false
	}
	return url, true
}

// ignoredScopedRepos remembers the ignored scoped repositories already logged, so a
// misconfigured Repository is reported once rather than on every evaluation.
var ignoredScopedRepos = newCache[[2]string, struct{}](compiledCacheSize)

func warnIgnoredScopedRepo(project, url string) {
	key := [2]string{project, url}
	if _, seen := ignoredScopedRepos.Get(key); seen {
		return
	}
	ignoredScopedRepos.Add(key, struct{}{})
	slog.Default().Warn("project: ignoring project-scoped repository whose URL is not a literal repository URL", "project", project, "url", redactURL(url))
}

// literalPattern returns a destinations pattern matching exactly s: glob meta characters are
// escaped, and so is a leading '!' so that the entry is never a negation.
func literalPattern(s string) string {
	q := glob.QuoteMeta(s)
	if isDenyPattern(q) {
		return `\` + q
	}
	return q
}

// IsNotFound reports whether err means "project does not exist".
func IsNotFound(err error) bool {
	return errors.Is(err, ErrProjectNotFound) || apierrors.IsNotFound(err)
}

// ResolveAppProject returns the effective project of app (upstream GetAppProject plus the
// controller's getAppProj): it reads spec.project (default "default"), builds the effective
// project and checks that app's namespace may use it. Errors: a not-found error (IsNotFound)
// when the project does not exist, a *NotPermittedError for a namespace not listed in
// sourceNamespaces, other lookup errors wrapped. globalErr is the non-fatal global project
// problem described in EffectiveProject.
func ResolveAppProject(ctx context.Context, app *v1alpha1.Application, controlPlaneNamespace string, l Lookups) (proj *v1alpha1.AppProject, globalErr error, err error) {
	if app == nil {
		return nil, nil, fmt.Errorf("no application")
	}
	name := app.Spec.GetProject()
	if l.Projects == nil {
		return nil, nil, fmt.Errorf("could not retrieve AppProject '%s': no project getter configured", name)
	}
	raw, err := l.Projects.GetProject(ctx, name)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("could not retrieve AppProject '%s': %w", name, err)
	}
	if raw == nil {
		return nil, nil, fmt.Errorf("project %q: %w", name, ErrProjectNotFound)
	}
	effective, globalErr, err := EffectiveProject(ctx, raw, l)
	if err != nil {
		return nil, globalErr, err
	}
	if err := CheckAppNamespace(effective, app, controlPlaneNamespace); err != nil {
		return nil, globalErr, err
	}
	return effective, globalErr, nil
}
