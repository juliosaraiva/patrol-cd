package hydrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/commitserver"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// render generates the manifests of app's dry source at revision (the dry targetRevision
// when empty) through the repo-server, bypassing its manifest and revision caches
// (APP-118). Tracking metadata is removed from every object so the hydrated output does not
// depend on the Application or the installation. It returns the path details for the
// commit-server and the resolved dry revision.
func (h *Hydrator) render(ctx context.Context, st *settings.Settings, app *v1alpha1.Application, revision string, proj *v1alpha1.AppProject) (commitserver.PathDetails, string, error) {
	if proj == nil {
		return commitserver.PathDetails{}, "", fmt.Errorf("no project resolved for app %q", app.QualifiedName())
	}
	src := app.Spec.SourceHydrator.GetDrySource()
	if revision == "" {
		revision = src.TargetRevision
	}
	repo, err := h.d.Repositories.GetRepository(ctx, src.RepoURL, proj.Name)
	if err != nil {
		return commitserver.PathDetails{}, "", fmt.Errorf("failed to get repo objects for app %q: failed to get repository %q: %w", app.QualifiedName(), git.SanitizeURL(src.RepoURL), err)
	}
	if repo == nil {
		repo = &apiclient.Repository{URL: src.RepoURL}
	}
	var helmRepos []apiclient.Repository
	var helmCreds []apiclient.RepositoryCredentialTemplate
	if h.d.HelmRepositories != nil {
		helmRepos, helmCreds, err = h.d.HelmRepositories.ListHelmRepositories(ctx, proj)
		if err != nil {
			return commitserver.PathDetails{}, "", fmt.Errorf("failed to get repo objects for app %q: failed to list Helm repositories for project %q: %w", app.QualifiedName(), proj.Name, err)
		}
	}
	kubeVersion := ""
	if h.d.ClusterInfo != nil {
		kubeVersion, err = h.d.ClusterInfo.KubeVersion(ctx, app)
		if err != nil {
			return commitserver.PathDetails{}, "", fmt.Errorf("failed to get repo objects for app %q: failed to get cluster version: %w", app.QualifiedName(), err)
		}
	}
	si, _ := project.EffectiveSourceIntegrity(proj)
	method := st.TrackingMethod()
	resp, err := h.d.RepoServer.GenerateManifest(ctx, &apiclient.ManifestRequest{
		Repo:               *repo,
		Revision:           revision,
		NoCache:            true,
		NoRevisionCache:    true,
		AppName:            tracking.InstanceName(app.Name, app.Namespace, h.d.ControlPlaneNamespace),
		AppNamespace:       app.Namespace,
		Namespace:          app.Spec.Destination.Namespace,
		Source:             src,
		Repos:              helmRepos,
		HelmRepoCreds:      helmCreds,
		KustomizeOptions:   kustomizeOptions(st),
		HelmOptions:        apiclient.HelmOptions{ValuesFileSchemes: st.HelmValuesFileSchemes()},
		KubeVersion:        kubeVersion,
		InstanceLabelKey:   st.InstanceLabelKey(),
		TrackingMethod:     string(method),
		InstallationID:     st.InstallationID(),
		EnabledSourceTypes: enabledSourceTypes(st),
		ProjectName:        proj.Name,
		ProjectSourceRepos: proj.Spec.SourceRepos,
		SourceIntegrity:    si,
	})
	if err != nil {
		return commitserver.PathDetails{}, "", fmt.Errorf("failed to get repo objects for app %q: %w", app.QualifiedName(), err)
	}
	if err := checkSourceIntegrity(app, si, !h.d.DisableGPG, src, resp); err != nil {
		return commitserver.PathDetails{}, "", err
	}
	tracker := tracking.New(tracking.Config{InstanceLabelKey: st.InstanceLabelKey(), ControlPlaneNamespace: h.d.ControlPlaneNamespace})
	manifests := make([]json.RawMessage, 0, len(resp.Manifests))
	for i, m := range resp.Manifests {
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON([]byte(m)); err != nil {
			return commitserver.PathDetails{}, "", fmt.Errorf("failed to get repo objects for app %q: manifest %d is not a Kubernetes object: %w", app.QualifiedName(), i, err)
		}
		if err := tracker.RemoveAppInstance(obj, method); err != nil {
			return commitserver.PathDetails{}, "", fmt.Errorf("failed to remove the app instance value: %w", err)
		}
		data, err := obj.MarshalJSON()
		if err != nil {
			return commitserver.PathDetails{}, "", fmt.Errorf("failed to marshal object: %w", err)
		}
		manifests = append(manifests, data)
	}
	return commitserver.PathDetails{
		Path:      app.Spec.SourceHydrator.SyncSource.Path,
		Manifests: manifests,
		Commands:  resp.Commands,
	}, resp.Revision, nil
}

func kustomizeOptions(st *settings.Settings) apiclient.KustomizeOptions {
	k := st.Kustomize()
	out := apiclient.KustomizeOptions{BuildOptions: k.BuildOptions}
	for _, v := range k.Versions {
		if out.Versions == nil {
			out.Versions = map[string]string{}
			out.VersionBuildOptions = map[string]string{}
		}
		out.Versions[v.Name] = v.Path
		if v.BuildOptions != "" {
			out.VersionBuildOptions[v.Name] = v.BuildOptions
		}
	}
	return out
}

func enabledSourceTypes(st *settings.Settings) map[string]bool {
	in := st.EnabledSourceTypes()
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for t, ok := range in {
		out[string(t)] = ok
	}
	return out
}

// checkSourceIntegrity enforces source integrity on the rendered dry revision (APP-134,
// upstream getManifests). It fails closed: a result reporting problems is an error whether
// or not the project has criteria for the source or GPG is enabled here, and a source with
// criteria must come with a result.
func checkSourceIntegrity(app *v1alpha1.Application, si *v1alpha1.SourceIntegrity, gpgEnabled bool, src v1alpha1.ApplicationSource, resp *apiclient.ManifestResponse) error {
	if r := resp.SourceIntegrityResult; r != nil && !r.IsValid() {
		return fmt.Errorf("source integrity verification failed for app %q dry revision %q: %s", app.QualifiedName(), resp.Revision, integrityProblems(r))
	}
	if resp.SourceIntegrityResult == nil && project.HasSourceIntegrityCriteria(si, gpgEnabled, src) {
		return fmt.Errorf("source integrity verification required but not performed for app %q dry revision %q", app.QualifiedName(), resp.Revision)
	}
	return nil
}

func integrityProblems(r *apiclient.SourceIntegrityResult) string {
	var out []string
	for _, c := range r.Checks {
		for _, p := range c.Problems {
			out = append(out, c.Name+": "+p)
		}
	}
	return strings.Join(out, "; ")
}
