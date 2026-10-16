package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// NewProjectGetter returns a notifications.ProjectGetter reading AppProjects of the control-plane
// namespace through reader (the controller's cache). AppProjects are always looked up in the
// control-plane namespace, also for Applications of other namespaces (upstream #28137).
func NewProjectGetter(reader client.Reader, namespace string) notifications.ProjectGetter {
	return projectGetter{reader: reader, namespace: namespace}
}

type projectGetter struct {
	reader    client.Reader
	namespace string
}

func (p projectGetter) GetAppProject(ctx context.Context, name string) (map[string]any, error) {
	u := newAppProject()
	if err := p.reader.Get(ctx, types.NamespacedName{Namespace: p.namespace, Name: name}, u); err != nil {
		return nil, fmt.Errorf("cannot get application project %q: %w", name, err)
	}
	return u.Object, nil
}

// NewRepoService adapts the repo-server client to the engine's repo.* helpers. resolver may be
// nil (requests carry the repository URL only).
func NewRepoService(c apiclient.Client, resolver RepositoryResolver) notifications.RepoService {
	if c == nil {
		return nil
	}
	return &repoService{client: c, resolver: resolver}
}

type repoService struct {
	client   apiclient.Client
	resolver RepositoryResolver
}

func (s *repoService) repository(ctx context.Context, repoURL, project string) (apiclient.Repository, error) {
	if strings.TrimSpace(repoURL) == "" {
		return apiclient.Repository{}, errors.New("application has no source repository URL")
	}
	if s.resolver == nil {
		return apiclient.Repository{URL: repoURL, Project: project}, nil
	}
	repo, err := s.resolver.ResolveRepository(ctx, repoURL, project)
	if err != nil {
		return apiclient.Repository{}, fmt.Errorf("resolving repository: %w", err)
	}
	if repo == nil {
		return apiclient.Repository{URL: repoURL, Project: project}, nil
	}
	r := *repo
	if r.URL == "" {
		r.URL = repoURL
	}
	return r, nil
}

// GetCommitMetadata implements notifications.RepoService.
func (s *repoService) GetCommitMetadata(ctx context.Context, repoURL, revision, project string) (*notifications.CommitMetadata, error) {
	repo, err := s.repository(ctx, repoURL, project)
	if err != nil {
		return nil, err
	}
	md, err := s.client.GetRevisionMetadata(ctx, &apiclient.RevisionMetadataRequest{Repo: repo, Revision: revision})
	if err != nil {
		return nil, fmt.Errorf("getting revision metadata: %w", err)
	}
	if md == nil {
		return nil, errors.New("repo-server returned no revision metadata")
	}
	return &notifications.CommitMetadata{
		Message: md.Message,
		Author:  md.Author,
		Date:    md.Date,
		Tags:    append([]string(nil), md.Tags...),
	}, nil
}

// GetAppDetails implements notifications.RepoService for the application's first source.
func (s *repoService) GetAppDetails(ctx context.Context, app map[string]any) (*notifications.AppDetail, error) {
	source, err := firstSource(app)
	if err != nil {
		return nil, err
	}
	project, _ := nestedString(app, "spec", "project")
	if project == "" {
		project = "default"
	}
	repo, err := s.repository(ctx, source.RepoURL, project)
	if err != nil {
		return nil, err
	}
	name, _ := nestedString(app, "metadata", "name")
	d, err := s.client.GetAppDetails(ctx, &apiclient.AppDetailsQuery{Repo: repo, Source: source, AppName: name})
	if err != nil {
		return nil, fmt.Errorf("getting application details: %w", err)
	}
	if d == nil {
		return nil, errors.New("repo-server returned no application details")
	}
	out := &notifications.AppDetail{Type: d.Type}
	if d.Helm != nil {
		h := &notifications.CustomHelmAppSpec{HelmAppSpec: notifications.HelmAppSpec{
			Name:       d.Helm.Name,
			ValueFiles: append([]string(nil), d.Helm.ValueFiles...),
			Values:     d.Helm.Values,
		}}
		for _, p := range d.Helm.Parameters {
			h.HelmAppSpec.Parameters = append(h.HelmAppSpec.Parameters, notifications.HelmParameter{Name: p.Name, Value: p.Value, ForceString: p.ForceString})
		}
		for _, p := range d.Helm.FileParameters {
			h.HelmAppSpec.FileParameters = append(h.HelmAppSpec.FileParameters, notifications.HelmFileParameter{Name: p.Name, Path: p.Path})
		}
		// Upstream dereferences source.helm without a nil check; a chart without helm overrides
		// simply has none here.
		if source.Helm != nil {
			for _, p := range source.Helm.Parameters {
				h.HelmParameterOverrides = append(h.HelmParameterOverrides, notifications.HelmParameter{Name: p.Name, Value: p.Value, ForceString: p.ForceString})
			}
		}
		out.Helm = h
	}
	if d.Kustomize != nil {
		out.Kustomize = &notifications.KustomizeAppSpec{Images: append([]string(nil), d.Kustomize.Images...)}
	}
	if d.Directory != nil {
		out.Directory = &notifications.DirectoryAppSpec{}
	}
	return out, nil
}

// firstSource returns the source repo.GetAppDetails describes, like upstream
// GetSourcePtrByIndex(0): the hydrator's sync source, else the first of spec.sources, else
// spec.source.
func firstSource(app map[string]any) (v1alpha1.ApplicationSource, error) {
	var spec struct {
		Source         *v1alpha1.ApplicationSource `json:"source,omitempty"`
		Sources        v1alpha1.ApplicationSources `json:"sources,omitempty"`
		SourceHydrator *v1alpha1.SourceHydrator    `json:"sourceHydrator,omitempty"`
	}
	raw, ok := app["spec"].(map[string]any)
	if !ok {
		return v1alpha1.ApplicationSource{}, errors.New("application has no spec")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return v1alpha1.ApplicationSource{}, fmt.Errorf("encoding application spec: %w", err)
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return v1alpha1.ApplicationSource{}, fmt.Errorf("decoding application sources: %w", err)
	}
	switch {
	case spec.SourceHydrator != nil:
		return spec.SourceHydrator.GetSyncSource(), nil
	case len(spec.Sources) > 0:
		return spec.Sources[0], nil
	case spec.Source != nil:
		return *spec.Source, nil
	}
	return v1alpha1.ApplicationSource{}, errors.New("application has no source")
}

func nestedString(obj map[string]any, path ...string) (string, bool) {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur = m[p]
	}
	s, ok := cur.(string)
	return s, ok
}
