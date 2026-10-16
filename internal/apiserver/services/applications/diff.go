package applications

import (
	"context"
	"fmt"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// serverSideDiff serves POST /applications/{name}/server-side-diff (applications, get):
// the controller replica owning the Application runs server-side apply dry runs with the
// Application's diff settings and its own cluster credentials (upstream does not
// impersonate here, SYNC-124). Every live state must match its declared identity (checked
// here with upstream's messages, and again by the controller, which also requires it to be
// a managed resource of the Application), and a target manifest paired with a live state
// must name the same object (see validateServerSideDiffPairs). Secret results are masked
// (by the controller, and again here).
func (s *Service) serverSideDiff(ctx context.Context, req *pubapi.ApplicationServerSideDiffRequest) (*pubapi.ServerSideDiffResponse, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: req.Name, namespace: req.AppNamespace, project: req.Project})
	if err != nil {
		return nil, err
	}
	if n := max(len(req.LiveResources), len(req.TargetManifests)); n > MaxServerSideDiffResources {
		return nil, rest.ResourceExhausted("server-side diff of %d resources exceeds the limit of %d per request", n, MaxServerSideDiffResources)
	}
	if len(req.LiveResources) != len(req.TargetManifests) {
		return nil, rest.InvalidArgument("liveResources (%d) and targetManifests (%d) must be index-aligned and of equal length", len(req.LiveResources), len(req.TargetManifests))
	}
	if err := validateServerSideDiffPairs(req.LiveResources, req.TargetManifests); err != nil {
		return nil, err
	}
	tc, err := s.treeClient()
	if err != nil {
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	live := make([]treeapi.ResourceDiff, len(req.LiveResources))
	for i, d := range req.LiveResources {
		live[i] = internalDiff(d)
	}
	var res *treeapi.ServerSideDiffResponse
	err = s.withAppState(ctx, app, proj, "error performing server-side diff", func() error {
		var err error
		res, err = tc.ServerSideDiff(ctx, app, live, req.TargetManifests)
		return err
	})
	if err != nil {
		return nil, err
	}
	hide := sensitiveAnnotations(st)
	out := &pubapi.ServerSideDiffResponse{Items: make([]pubapi.ResourceDiff, 0, len(res.Items)), Modified: res.Modified}
	for _, it := range res.Items {
		d := publicDiff(it)
		if err := maskResourceDiff(&d, hide); err != nil {
			return nil, rest.Internal(err)
		}
		out.Items = append(out.Items, d)
	}
	return out, nil
}

// validateServerSideDiffPairs checks the index-aligned pairs before anything reaches the
// controller. First every present live state must carry its declared identity (upstream's
// checks and messages, server/application/application.go ServerSideDiff). Then every
// target manifest paired with a present live state must name that same object: the
// controller dry-run applies the target with its own credentials when both sides exist, so
// a target naming any other object (another tenant's, or one in kube-system) would return
// that object's predicted live state to a caller who only needs applications, get on some
// Application. Upstream validates the live side only; Patrol refuses such pairs (the CLIs
// pair live and target by resource key, so legitimate requests always match). A target
// without a live state is not dry-run and needs no check.
func validateServerSideDiffPairs(live []pubapi.ResourceDiff, targets []string) error {
	present := make([]bool, len(live))
	for i := range live {
		lr := &live[i]
		obj, err := parseState(lr.LiveState)
		if err != nil {
			return rest.InvalidArgument("error unmarshaling live state for %s/%s: %v", lr.Kind, lr.Name, err)
		}
		if obj == nil {
			continue
		}
		gvk := obj.GroupVersionKind()
		switch {
		case obj.GetName() != lr.Name:
			return rest.InvalidArgument("name mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Name, obj.GetName(), lr.Group, lr.Kind, lr.Name)
		case obj.GetNamespace() != lr.Namespace:
			return rest.InvalidArgument("namespace mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Namespace, obj.GetNamespace(), lr.Group, lr.Kind, lr.Name)
		case gvk.Group != lr.Group:
			return rest.InvalidArgument("group mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Group, gvk.Group, lr.Group, lr.Kind, lr.Name)
		case gvk.Kind != lr.Kind:
			return rest.InvalidArgument("kind mismatch: expected %s, got %s for live resource %s/%s/%s", lr.Kind, gvk.Kind, lr.Group, lr.Kind, lr.Name)
		}
		present[i] = true
	}
	for i, m := range targets {
		obj, err := parseState(m)
		if err != nil {
			return rest.InvalidArgument("error unmarshaling target manifest %d: %v", i, err)
		}
		if obj == nil || !present[i] {
			continue
		}
		lr := &live[i]
		gvk := obj.GroupVersionKind()
		if gvk.Group != lr.Group || gvk.Kind != lr.Kind || obj.GetNamespace() != lr.Namespace || obj.GetName() != lr.Name {
			return rest.InvalidArgument("target manifest %d (%s) does not match live resource %s",
				i, diffIdentity(gvk.Group, gvk.Kind, obj.GetNamespace(), obj.GetName()), diffIdentity(lr.Group, lr.Kind, lr.Namespace, lr.Name))
		}
	}
	return nil
}

// diffIdentity formats a resource identity as group/kind namespace/name.
func diffIdentity(group, kind, namespace, name string) string {
	return fmt.Sprintf("%s/%s %s/%s", group, kind, namespace, name)
}
