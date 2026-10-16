package applications

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/links"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// linkCluster returns the destination cluster exposed to link templates (upstream
// getObjectsForDeepLinks): nil when the destination does not resolve, an error when the
// project does not permit it.
func (s *Service) linkCluster(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, st *settings.Settings) (*v1alpha1.Cluster, error) {
	kl, _ := s.lookups(st)
	cluster, err := project.ResolveDestination(ctx, app.Spec.Destination, kl)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		s.log().WarnContext(ctx, "applications: cannot resolve the destination for deep links", "application", app.QualifiedName(), "error", err)
		return nil, nil
	}
	ok, err := project.IsDestinationPermitted(ctx, proj, project.DestinationOf(cluster, app.Spec.Destination.Namespace), kl)
	if err != nil {
		return nil, rest.Unavailable("error checking the destination cluster").WithCause(err)
	}
	if !ok {
		return nil, rest.InvalidArgument("error getting destination cluster")
	}
	return cluster, nil
}

// listLinks serves GET /applications/{name}/links (application.links).
func (s *Service) listLinks(ctx context.Context, req *pubapi.ApplicationLinksRequest) (*pubapi.LinksResponse, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: firstNonEmpty(req.AppNamespace, req.Namespace), project: req.Project})
	if err != nil {
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	cluster, err := s.linkCluster(ctx, app, proj, st)
	if err != nil {
		return nil, err
	}
	lctx, err := links.NewContext(links.Input{Application: app, Cluster: cluster})
	if err != nil {
		return nil, rest.Internal(err)
	}
	if _, ok := lctx[links.KeyManagedByURL]; !ok && st.URL() != "" {
		lctx[links.KeyManagedByURL] = st.URL()
	}
	out, problems := links.Evaluate(lctx, app.Name, st.DeepLinks(settings.KeyApplicationLinks))
	for _, p := range problems {
		s.log().WarnContext(ctx, "applications: application deep link skipped", "problem", p)
	}
	return out, nil
}

// listResourceLinks serves GET /applications/{name}/resource/links (resource.links) for a
// managed resource (read through LiveResources, Secret data masked) or the Application
// itself.
func (s *Service) listResourceLinks(ctx context.Context, req *pubapi.ApplicationResourceRequest) (*pubapi.LinksResponse, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	if req.Kind == "" || req.ResourceName == "" {
		return nil, rest.InvalidArgument("kind and resourceName are required")
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	name := req.ResourceName
	if req.Group == v1alpha1.GroupName && req.Kind == "Application" && req.ResourceName == app.Name &&
		(req.Namespace == "" || req.Namespace == app.Namespace) {
		cp := app.DeepCopy()
		cp.APIVersion, cp.Kind = v1alpha1.GroupVersion.String(), "Application"
		u, err := toUnstructured(cp)
		if err != nil {
			return nil, rest.Internal(err)
		}
		obj = u.Object
	} else {
		if s.d.LiveResources == nil {
			return nil, unavailable("live resource access")
		}
		live, err := s.d.LiveResources.GetManagedResource(ctx, app, proj, ResourceRef{Group: req.Group, Version: req.Version, Kind: req.Kind, Namespace: req.Namespace, Name: req.ResourceName})
		if err != nil {
			if errors.Is(err, ErrResourceNotInTree) {
				return nil, rest.InvalidArgument("%s %s %s not found as part of application %s", req.Kind, req.Group, req.ResourceName, app.Name)
			}
			return nil, err
		}
		if isCoreSecret(live) {
			if live, _, err = diff.HideSecretData(live, nil, sensitiveAnnotations(st)); err != nil {
				return nil, rest.Internal(err)
			}
		}
		obj = live.Object
	}
	cluster, err := s.linkCluster(ctx, app, proj, st)
	if err != nil {
		return nil, err
	}
	lctx, err := links.NewContext(links.Input{Resource: obj, Application: app, Cluster: cluster, Project: proj})
	if err != nil {
		return nil, rest.Internal(err)
	}
	out, problems := links.Evaluate(lctx, name, st.DeepLinks(settings.KeyResourceLinks))
	for _, p := range problems {
		s.log().WarnContext(ctx, "applications: resource deep link skipped", "problem", p)
	}
	return out, nil
}

func toUnstructured(obj *v1alpha1.Application) (*unstructured.Unstructured, error) {
	m, err := runtimeToMap(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}
