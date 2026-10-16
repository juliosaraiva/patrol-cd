package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// treeStreamMinInterval is the minimum delay between two tree reads of one stream
// triggered by Application events.
const treeStreamMinInterval = 500 * time.Millisecond

// isStateMissing reports whether a tree API error means the controller has no state for
// the Application yet (unknown Application or not compared since the replica started):
// upstream's cache miss.
func isStateMissing(err error) bool {
	var te *treeapi.Error
	return errors.As(err, &te) && (te.Code == treeapi.CodeNotFound || te.Code == treeapi.CodeUnavailable)
}

// isTransientTreeError reports errors a stream survives (the controller restarts or the
// shard moves).
func isTransientTreeError(err error) bool {
	return isStateMissing(err) || errors.Is(err, treeapi.ErrUnavailable) || errors.Is(err, treeapi.ErrNotOwned) ||
		errors.Is(err, treeapi.ErrDeadlineExceeded)
}

// treeError maps a tree API error to an API error. Messages of invalid-argument and
// permission-denied answers are the controller's validation messages (upstream wording) and
// are returned; other failures are reported generically and logged.
func (s *Service) treeError(ctx context.Context, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var te *treeapi.Error
	if errors.As(err, &te) {
		switch te.Code {
		case treeapi.CodeInvalidArgument:
			return rest.InvalidArgument("%s", te.Message)
		case treeapi.CodePermissionDenied:
			return rest.PermissionDenied("%s", te.Message)
		case treeapi.CodeTooLarge:
			return &rest.Error{Status: http.StatusRequestEntityTooLarge, Code: pubapi.CodeResourceExhausted, Detail: te.Message, Cause: err}
		case treeapi.CodeDeadlineExceeded:
			return &rest.Error{Status: http.StatusGatewayTimeout, Code: pubapi.CodeUnavailable, Detail: what + ": the application controller did not answer in time", Cause: err}
		case treeapi.CodeNotFound, treeapi.CodeUnavailable:
			return rest.Unavailable("%s: the application state is not available yet", what).WithCause(err)
		}
	}
	s.log().WarnContext(ctx, "applications: controller tree API call failed", "what", what, "error", err)
	if errors.Is(err, treeapi.ErrUnavailable) || errors.Is(err, treeapi.ErrNotOwned) || errors.Is(err, treeapi.ErrUnauthenticated) {
		return rest.Unavailable("%s: the application controller is unavailable", what).WithCause(err)
	}
	return rest.Internal(fmt.Errorf("%s: %w", what, err))
}

// appStateError returns the comparison/spec error conditions of app (upstream
// getCachedAppState), or nil.
func appStateError(app *v1alpha1.Application) error {
	var msgs []string
	for _, c := range app.Status.Conditions {
		if c.Status != metav1.ConditionTrue {
			continue
		}
		if c.Type == v1alpha1.ApplicationConditionComparisonError || c.Type == v1alpha1.ApplicationConditionInvalidSpecError {
			msgs = append(msgs, c.Type+": "+c.Message)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return rest.FailedPrecondition("%s", strings.Join(msgs, ";"))
}

// withAppState runs read against the controller's state of app. When the controller has
// no state yet (upstream cache miss), the Application's error conditions are reported if
// any; otherwise a normal refresh is requested and awaited (bounded by
// Config.RefreshTimeout), and read runs once more.
func (s *Service) withAppState(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, what string, read func() error) error {
	err := read()
	if err == nil {
		return nil
	}
	if !isStateMissing(err) {
		return s.treeError(ctx, what, err)
	}
	if serr := appStateError(app); serr != nil {
		return serr
	}
	if _, rerr := s.refresh(ctx, app, proj, v1alpha1.RefreshTypeNormal); rerr != nil {
		return rerr
	}
	if err := read(); err != nil {
		return s.treeError(ctx, what, err)
	}
	return nil
}

// appTree returns the resource tree of app from the controller.
func (s *Service) appTree(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject) (*compare.ApplicationTree, error) {
	tc, err := s.treeClient()
	if err != nil {
		return nil, err
	}
	var tree *compare.ApplicationTree
	err = s.withAppState(ctx, app, proj, "error getting app resource tree", func() error {
		t, err := tc.Tree(ctx, app)
		tree = t
		return err
	})
	if err != nil {
		return nil, err
	}
	if tree == nil {
		tree = &compare.ApplicationTree{}
	}
	return tree, nil
}

// findTreeNode returns the node (managed or orphaned) with the identity and a UID (a live
// object), or nil (upstream getAppLiveResource).
func findTreeNode(tree *compare.ApplicationTree, group, kind, namespace, name string) *compare.ResourceNode {
	n := tree.FindNode(group, kind, namespace, name)
	if n == nil || n.UID == "" {
		return nil
	}
	return n
}

// notInTree is the upstream InvalidArgument for a resource outside the Application's tree.
func notInTree(kind, group, name, app string) error {
	return rest.InvalidArgument("%s %s %s not found as part of application %s", kind, group, name, app)
}

// resourceTree serves GET /applications/{name}/resource-tree (applications, get): the tree
// the controller computed, unfiltered (as upstream, anyone who may get the Application sees
// its whole tree; nodes carry no object data).
func (s *Service) resourceTree(ctx context.Context, q *pubapi.ApplicationTreeQuery) (*pubapi.ApplicationTree, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: q.Name, namespace: q.AppNamespace, project: q.Project})
	if err != nil {
		return nil, err
	}
	tree, err := s.appTree(ctx, app, proj)
	if err != nil {
		return nil, err
	}
	out := toPublicTree(tree)
	return &out, nil
}

// watchResourceTree serves GET /stream/applications/{name}/resource-tree: ADDED with the
// current tree, then MODIFIED whenever the controller's tree changes (ETag polling every
// TreePollInterval and on Application events). Every event is authorized against the
// current Application (applications, get); a caller who loses access gets a 403 error
// event, and a deleted Application a DELETED event, both ending the stream.
func (s *Service) watchResourceTree(ctx context.Context, q *pubapi.ApplicationTreeQuery, sink *rest.Sink[pubapi.ApplicationTreeEvent]) error {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: q.Name, namespace: q.AppNamespace, project: q.Project})
	if err != nil {
		return err
	}
	tc, err := s.treeClient()
	if err != nil {
		return err
	}
	var events <-chan Event
	if s.d.Events != nil {
		ch, cancel := s.d.Events.Subscribe()
		defer cancel()
		events = ch
	}
	var (
		tree *compare.ApplicationTree
		etag string
	)
	err = s.withAppState(ctx, app, proj, "error getting app resource tree", func() error {
		t, tag, _, err := tc.TreeIfChanged(ctx, app, "")
		tree, etag = t, tag
		return err
	})
	if err != nil {
		return err
	}
	if tree == nil {
		tree = &compare.ApplicationTree{}
	}
	if err := sink.Send(pubapi.ApplicationTreeEvent{Type: pubapi.EventAdded, Object: toPublicTree(tree)}); err != nil {
		return err
	}
	deleted := func() error {
		_ = sink.Send(pubapi.ApplicationTreeEvent{Type: pubapi.EventDeleted, Object: pubapi.ApplicationTree{}})
		return nil
	}
	lastFetch := time.Now()
	fetch := func() error {
		lastFetch = time.Now()
		t, tag, changed, err := tc.TreeIfChanged(ctx, app, etag)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if isTransientTreeError(err) {
				s.log().DebugContext(ctx, "applications: resource tree stream: controller not available", "application", app.QualifiedName(), "error", err)
				return nil
			}
			return s.treeError(ctx, "error getting app resource tree", err)
		}
		if !changed || t == nil {
			return nil
		}
		etag = tag
		return sink.Send(pubapi.ApplicationTreeEvent{Type: pubapi.EventModified, Object: toPublicTree(t)})
	}
	ticker := time.NewTicker(s.d.Resources.TreePollInterval)
	defer ticker.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				events = nil // fell behind: polling continues
				continue
			}
			a := ev.Application
			if a == nil || a.Namespace != app.Namespace || a.Name != app.Name {
				continue
			}
			if ev.Type == pubapi.EventDeleted || a.UID != app.UID {
				return deleted()
			}
			app = a.DeepCopy()
			if !s.canApp(ctx, rbac.ActionGet, app) {
				return forbidden()
			}
			if wait := treeStreamMinInterval - time.Since(lastFetch); wait > 0 {
				if debounce == nil {
					debounce = time.After(wait)
				}
				continue
			}
			if err := fetch(); err != nil {
				return err
			}
		case <-debounce:
			debounce = nil
			if err := fetch(); err != nil {
				return err
			}
		case <-ticker.C:
			cur := &v1alpha1.Application{}
			if err := s.d.Reader.Get(ctx, client.ObjectKeyFromObject(app), cur); err != nil {
				if apierrors.IsNotFound(err) {
					return deleted()
				}
				if ctx.Err() != nil {
					return nil
				}
				s.log().DebugContext(ctx, "applications: resource tree stream: cannot read the application", "application", app.QualifiedName(), "error", err)
				continue
			}
			if cur.UID != app.UID {
				return deleted()
			}
			app = cur
			if !s.canApp(ctx, rbac.ActionGet, app) {
				return forbidden()
			}
			if err := fetch(); err != nil {
				return err
			}
		}
	}
}

// managedResources serves GET /applications/{name}/managed-resources (applications, get):
// the controller's managed resources (hooks excluded) matching the filters, with core
// Secret data and sensitive annotations masked.
func (s *Service) managedResources(ctx context.Context, q *pubapi.ApplicationManagedResourcesQuery) (*pubapi.ManagedResourcesResponse, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: q.Name, namespace: q.AppNamespace, project: q.Project})
	if err != nil {
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
	var items []treeapi.ResourceDiff
	err = s.withAppState(ctx, app, proj, "error getting cached app managed resources", func() error {
		var err error
		items, err = tc.ManagedResources(ctx, app, treeapi.ManagedResourcesQuery{
			Group: q.Group, Kind: q.Kind, ResourceNamespace: q.Namespace, ResourceName: q.ResourceName,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	hide := sensitiveAnnotations(st)
	out := &pubapi.ManagedResourcesResponse{Items: make([]pubapi.ResourceDiff, 0, len(items))}
	for _, it := range items {
		if it.Hook || !matchesFilter(it, q.Group, q.Kind, q.Namespace, q.ResourceName) {
			continue
		}
		d := publicDiff(it)
		if err := maskResourceDiff(&d, hide); err != nil {
			return nil, rest.Internal(err)
		}
		out.Items = append(out.Items, d)
	}
	return out, nil
}

// matchesFilter re-applies the managed resource filters (exact match on non-empty values).
func matchesFilter(d treeapi.ResourceDiff, group, kind, namespace, name string) bool {
	return (group == "" || d.Group == group) && (kind == "" || d.Kind == kind) &&
		(namespace == "" || d.Namespace == namespace) && (name == "" || d.Name == name)
}

func publicDiff(d treeapi.ResourceDiff) pubapi.ResourceDiff {
	return pubapi.ResourceDiff{
		Group: d.Group, Kind: d.Kind, Namespace: d.Namespace, Name: d.Name,
		TargetState: d.TargetState, LiveState: d.LiveState, Diff: d.Diff, Hook: d.Hook,
		NormalizedLiveState: d.NormalizedLiveState, PredictedLiveState: d.PredictedLiveState,
		ResourceVersion: d.ResourceVersion, Modified: d.Modified,
	}
}

func internalDiff(d pubapi.ResourceDiff) treeapi.ResourceDiff {
	return treeapi.ResourceDiff{
		Group: d.Group, Kind: d.Kind, Namespace: d.Namespace, Name: d.Name,
		TargetState: d.TargetState, LiveState: d.LiveState, Diff: d.Diff, Hook: d.Hook,
		NormalizedLiveState: d.NormalizedLiveState, PredictedLiveState: d.PredictedLiveState,
		ResourceVersion: d.ResourceVersion, Modified: d.Modified,
	}
}

// parseState decodes an object state of a ResourceDiff; "" and "null" are absent sides.
func parseState(s string) (*unstructured.Unstructured, error) {
	t := strings.TrimSpace(s)
	if t == "" || t == "null" {
		return nil, nil
	}
	obj := map[string]any{}
	if err := json.Unmarshal([]byte(t), &obj); err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

func isSecretObject(u *unstructured.Unstructured) bool { return u != nil && isCoreSecret(u) }

// maskPairStates masks a (target, live) pair of states when either is a core Secret (or
// the item declares itself one), keeping the masks of equal values equal across the
// pair.
func maskPairStates(declaredSecret bool, target, live *string, hide map[string]bool) error {
	t, err := parseState(*target)
	if err != nil {
		return fmt.Errorf("parse state: %w", err)
	}
	l, err := parseState(*live)
	if err != nil {
		return fmt.Errorf("parse state: %w", err)
	}
	if !declaredSecret && !isSecretObject(t) && !isSecretObject(l) {
		return nil
	}
	if t == nil && l == nil {
		return nil
	}
	mt, ml, err := diff.HideSecretData(t, l, hide)
	if err != nil {
		return fmt.Errorf("error hiding secret data: %w", err)
	}
	if mt != nil {
		b, err := json.Marshal(mt.Object)
		if err != nil {
			return err
		}
		*target = string(b)
	}
	if ml != nil {
		b, err := json.Marshal(ml.Object)
		if err != nil {
			return err
		}
		*live = string(b)
	}
	return nil
}

// maskResourceDiff masks the Secret data of a managed resource or server-side diff item
// (defense in depth: the controller masks them already). Masking is decided from the
// parsed objects, not only from the item's declared kind; an item whose states cannot be
// parsed is an error (fail closed).
func maskResourceDiff(d *pubapi.ResourceDiff, hide map[string]bool) error {
	declared := d.Group == "" && d.Kind == "Secret"
	if err := maskPairStates(declared, &d.TargetState, &d.LiveState, hide); err != nil {
		return err
	}
	return maskPairStates(declared, &d.PredictedLiveState, &d.NormalizedLiveState, hide)
}

// toPublicTree converts the controller's tree to the API type.
func toPublicTree(t *compare.ApplicationTree) pubapi.ApplicationTree {
	if t == nil {
		return pubapi.ApplicationTree{}
	}
	out := pubapi.ApplicationTree{
		Nodes:         publicNodes(t.Nodes),
		OrphanedNodes: publicNodes(t.OrphanedNodes),
	}
	if len(t.Hosts) > 0 {
		out.Hosts = make([]pubapi.HostInfo, 0, len(t.Hosts))
		for _, h := range t.Hosts {
			hi := pubapi.HostInfo{Name: h.Name, SystemInfo: h.SystemInfo, Labels: cloneStrings(h.Labels)}
			for _, r := range h.ResourcesInfo {
				hi.ResourcesInfo = append(hi.ResourcesInfo, pubapi.HostResourceInfo{
					ResourceName: r.ResourceName, RequestedByApp: r.RequestedByApp,
					RequestedByNeighbors: r.RequestedByNeighbors, Capacity: r.Capacity,
				})
			}
			out.Hosts = append(out.Hosts, hi)
		}
	}
	return out
}

func cloneStrings(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func publicRef(r compare.ResourceRef) pubapi.ResourceRef {
	return pubapi.ResourceRef{Group: r.Group, Version: r.Version, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, UID: r.UID}
}

func publicNodes(nodes []compare.ResourceNode) []pubapi.ResourceNode {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]pubapi.ResourceNode, 0, len(nodes))
	for _, n := range nodes {
		pn := pubapi.ResourceNode{
			ResourceRef:     publicRef(n.ResourceRef),
			ResourceVersion: n.ResourceVersion,
			Images:          append([]string(nil), n.Images...),
		}
		for _, p := range n.ParentRefs {
			pn.ParentRefs = append(pn.ParentRefs, publicRef(p))
		}
		for _, i := range n.Info {
			pn.Info = append(pn.Info, pubapi.InfoItem{Name: i.Name, Value: i.Value})
		}
		if ni := n.NetworkingInfo; ni != nil {
			pni := &pubapi.ResourceNetworkingInfo{
				TargetLabels: cloneStrings(ni.TargetLabels), Labels: cloneStrings(ni.Labels),
				ExternalURLs: append([]string(nil), ni.ExternalURLs...),
			}
			for _, r := range ni.TargetRefs {
				pni.TargetRefs = append(pni.TargetRefs, publicRef(r))
			}
			for _, ing := range ni.Ingress {
				pni.Ingress = append(pni.Ingress, pubapi.LoadBalancerIngress{Hostname: ing.Hostname, IP: ing.IP})
			}
			pn.NetworkingInfo = pni
		}
		if n.Health != nil {
			h := *n.Health
			pn.Health = &h
		}
		if n.CreatedAt != nil {
			c := *n.CreatedAt
			pn.CreatedAt = &c
		}
		out = append(out, pn)
	}
	return out
}
