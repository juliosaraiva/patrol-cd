package applications

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// forbidden is the deliberately uninformative denial of Application reads: the same
// answer for "does not exist" and "may not see it" when the caller named no project
// (upstream PermissionDeniedAPIError).
func forbidden() error { return rest.PermissionDenied("permission denied") }

// appNotFound is the 404 returned when the caller named a project.
func appNotFound(name string) error {
	return rest.NotFound("applications.%s %q not found", v1alpha1.GroupName, name)
}

// rbacObject returns the RBAC object of app ("<project>/<name>" in the control-plane
// namespace, "<project>/<namespace>/<name>" elsewhere).
func (s *Service) rbacObject(app *v1alpha1.Application) string {
	return rbac.AppObject(s.controlNS(), app.Spec.GetProject(), s.nsOrDefault(app.Namespace), app.Name)
}

// authorizeApp enforces action on app (audited on denial) and returns the RBAC error.
func (s *Service) authorizeApp(ctx context.Context, action string, app *v1alpha1.Application) error {
	return rest.Authorize(ctx, rbac.ResourceApplications, action, s.rbacObject(app))
}

// canApp reports whether the caller may perform action on app (not audited).
func (s *Service) canApp(ctx context.Context, action string, app *v1alpha1.Application) bool {
	return rest.Can(ctx, rbac.ResourceApplications, action, s.rbacObject(app))
}

// appRequest addresses one Application.
type appRequest struct {
	name      string
	namespace string // as given ("" = control-plane namespace)
	project   string // expected project ("" = none given)
	// resourceVersion reads a specific version (GET ?resourceVersion=).
	resourceVersion string
}

// getApp reads an Application and authorizes action on it with the upstream
// existence-hiding rules (getAppEnforceRBAC):
//
//   - with a project: a denial on "<project>/<name>" is 403 (after reading the object
//     anyway, so timing does not reveal existence); a missing Application is 404; a denial
//     on the Application's real project, or another project than requested, is 404;
//   - without a project: missing, forbidden and namespace-not-enabled are all 403.
//
// It then resolves the effective project; failures are the vague InvalidArgument
// "app is not allowed in project ..., or the project does not exist".
func (s *Service) getApp(ctx context.Context, action string, req appRequest) (*v1alpha1.Application, *v1alpha1.AppProject, error) {
	app, err := s.getAppOnly(ctx, action, req)
	if err != nil {
		return nil, nil, err
	}
	proj, err := s.appProject(ctx, app)
	if err != nil {
		return nil, nil, err
	}
	return app, proj, nil
}

// getAppOnly is getApp without the project resolution.
func (s *Service) getAppOnly(ctx context.Context, action string, req appRequest) (*v1alpha1.Application, error) {
	c, err := s.kube()
	if err != nil {
		return nil, err
	}
	ns := s.nsOrDefault(req.namespace)
	if req.name == "" {
		return nil, rest.InvalidArgument("invalid request: application name is missing")
	}
	fetch := func() (*v1alpha1.Application, error) {
		if !s.namespaceEnabled(ns) {
			return nil, errNamespaceDisabled
		}
		app := &v1alpha1.Application{}
		var opts []client.GetOption
		if req.resourceVersion != "" {
			opts = append(opts, &client.GetOptions{Raw: &metav1.GetOptions{ResourceVersion: req.resourceVersion}})
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: req.name}, app, opts...); err != nil {
			return nil, err
		}
		return app, nil
	}
	if req.project != "" {
		if err := rest.Authorize(ctx, rbac.ResourceApplications, action, rbac.AppObject(s.controlNS(), req.project, ns, req.name)); err != nil {
			_, _ = fetch() // equalize the timing of "no access" and "exists elsewhere"
			return nil, forbidden()
		}
	}
	app, err := fetch()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if apierrors.IsNotFound(err) {
			if req.project != "" {
				return nil, appNotFound(req.name)
			}
			return nil, forbidden()
		}
		if !errors.Is(err, errNamespaceDisabled) {
			s.log().WarnContext(ctx, "applications: failed to read application", "namespace", ns, "name", req.name, "error", err)
		}
		return nil, forbidden()
	}
	if err := s.authorizeApp(ctx, action, app); err != nil {
		if req.project != "" {
			return nil, appNotFound(req.name)
		}
		return nil, forbidden()
	}
	if req.project != "" && app.Spec.GetProject() != req.project {
		return nil, appNotFound(req.name)
	}
	return app, nil
}

// errNamespaceDisabled marks reads of Applications in namespaces outside
// application.namespaces (reported as 403 like a missing Application).
var errNamespaceDisabled = errors.New("namespace not enabled")

// appProject resolves the effective project of app (scoped repositories and clusters,
// global projects, sourceNamespaces). Every failure is reported with the vague upstream
// message so project names cannot be enumerated.
func (s *Service) appProject(ctx context.Context, app *v1alpha1.Application) (*v1alpha1.AppProject, error) {
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	_, l := s.lookups(st)
	proj, gerr, err := project.ResolveAppProject(ctx, app, s.controlNS(), l)
	if gerr != nil {
		s.log().WarnContext(ctx, "applications: global projects could not be applied", "project", app.Spec.GetProject(), "error", gerr)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var np *project.NotPermittedError
		if !project.IsNotFound(err) && !errors.As(err, &np) {
			s.log().WarnContext(ctx, "applications: error getting app project", "project", app.Spec.GetProject(), "error", err)
		}
		return nil, rest.InvalidArgument("%s", project.AppNotAllowedMessage(app.Spec.Project))
	}
	return proj, nil
}

// effectiveValue combines the namespace of the path/query with one given in a body
// (Argo CD request bodies carry appNamespace); conflicting values are rejected.
func effectiveValue(field, query, body string) (string, error) {
	switch {
	case body == "" || body == query:
		return query, nil
	case query == "":
		return body, nil
	}
	return "", rest.InvalidArgument("%s %q in the body does not match %q", field, body, query)
}

// checkBodyName rejects a body name that differs from the path.
func checkBodyName(path, body string) error {
	if body != "" && body != path {
		return rest.InvalidArgument("name %q in the body does not match the path name %q", body, path)
	}
	return nil
}

// auditApp records app as the target of the request (Kubernetes Event on the Application).
func auditApp(ctx context.Context, app *v1alpha1.Application, action, object, reason, format string, args ...any) {
	a := rest.AuditFrom(ctx)
	if a == nil || app == nil {
		return
	}
	a.SetCheck(rbac.ResourceApplications, action, object)
	a.SetTarget(rest.ObjectRef{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: "Application",
		Namespace: app.Namespace, Name: app.Name, UID: app.UID,
	})
	if reason != "" {
		a.SetEventReason(reason)
	}
	if format != "" {
		actor := actorName(ctx)
		a.SetMessage("%s %s", actor, fmt.Sprintf(format, args...))
	}
}

// actorName is the user name recorded in events and Operation initiators: the account,
// the SSO email or subject, or "" for anonymous callers.
func actorName(ctx context.Context) string {
	id := rest.IdentityFrom(ctx)
	if id == nil || id.Principal == nil {
		if id != nil && (id.Anonymous || id.AuthDisabled) {
			return "anonymous"
		}
		return "Unknown user"
	}
	return id.Principal.Username()
}

// initiatorName is the verified user recorded in Operation spec.initiatedBy ("" without
// a verified principal).
func initiatorName(ctx context.Context) string {
	id := rest.IdentityFrom(ctx)
	if id == nil || id.Principal == nil {
		return ""
	}
	return id.Principal.Username()
}
