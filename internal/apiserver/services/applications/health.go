package applications

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
)

const (
	// resourceNotCreatedMessage is the health message of a managed resource whose tree node
	// has no health and no live object (upstream wording).
	resourceNotCreatedMessage = "Resource has not been created"
	// resourceHealthPause is how long a watch stream stops reading resource health from the
	// controller after the controller did not answer.
	resourceHealthPause = 30 * time.Second
)

// treeHealthByKey indexes the health of the managed nodes of tree by resource key (upstream
// inferResourcesStatusHealth): a node's own health, or Missing "Resource has not been
// created" for a node without health that has no live object (no UID, resourceVersion or
// creation time). Orphaned nodes do not count. The values are copies.
func treeHealthByKey(tree *compare.ApplicationTree) map[compare.ResourceKey]*v1alpha1.HealthStatus {
	out := map[compare.ResourceKey]*v1alpha1.HealthStatus{}
	if tree == nil {
		return out
	}
	for i := range tree.Nodes {
		n := &tree.Nodes[i]
		switch {
		case n.Health != nil:
			out[n.Key()] = n.Health.DeepCopy()
		case n.UID == "" && n.ResourceVersion == "" && n.CreatedAt == nil:
			out[n.Key()] = &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusMissing, Message: resourceNotCreatedMessage}
		}
	}
	return out
}

// needsTreeHealth reports whether the resource health of app lives in the controller's
// resource tree (controller.resource.health.persist=false) and app has resources to fill.
func needsTreeHealth(app *v1alpha1.Application) bool {
	return app != nil && app.Status.ResourceHealthSource == v1alpha1.ResourceHealthLocationAppTree && len(app.Status.Resources) > 0
}

// withTreeHealth returns app with status.resources[].health filled from the controller's
// resource tree when the controller does not persist resource health (upstream
// inferResourcesStatusHealth): every resource gets the health of its tree node, nil when the
// tree has none. The tree read is bounded by ResourceDeps.ResourceHealthTimeout.
//
// It never modifies app (watch events share one Application between subscribers): the
// filled Application is a copy. It reads the tree directly, never through withAppState, so a
// controller without state for app never triggers a refresh. When the tree cannot be read it
// returns app unchanged with the error; callers serve app anyway.
func (s *Service) withTreeHealth(ctx context.Context, app *v1alpha1.Application) (*v1alpha1.Application, error) {
	if !needsTreeHealth(app) || s.d.Resources.Tree == nil {
		return app, nil
	}
	tctx, cancel := context.WithTimeout(ctx, s.d.Resources.ResourceHealthTimeout)
	defer cancel()
	tree, err := s.d.Resources.Tree.Tree(tctx, app)
	if err != nil {
		s.log().DebugContext(ctx, "applications: resource health not filled from the resource tree", "application", app.QualifiedName(), "error", err)
		return app, fmt.Errorf("read the resource tree of %s: %w", app.QualifiedName(), err)
	}
	byKey := treeHealthByKey(tree)
	out := app.DeepCopy()
	for i := range out.Status.Resources {
		r := &out.Status.Resources[i]
		r.Health = byKey[compare.ResourceKey{Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}]
	}
	return out, nil
}

// pausesTreeHealth reports whether a withTreeHealth error means the controller did not
// answer (transport failure, timeout, routing error): a watch stream then stops reading
// resource health for resourceHealthPause. An answer of the controller (a *treeapi.Error,
// such as an Application not compared yet) concerns one Application and does not pause.
func pausesTreeHealth(err error) bool {
	var te *treeapi.Error
	return err != nil && !errors.As(err, &te)
}
