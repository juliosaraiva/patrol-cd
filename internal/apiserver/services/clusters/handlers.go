package clusters

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// errDenied is the answer for missing and forbidden clusters alike (upstream
// common.PermissionDeniedAPIError), so the API does not reveal which servers exist.
func errDenied() error { return rest.PermissionDenied("permission denied") }

// authorize enforces action on the cluster (auditing a denial) and hides the details of
// the denial behind errDenied.
func authorize(ctx context.Context, action string, c *v1alpha1.Cluster) error {
	obj := rbacObject(&c.Spec)
	rest.AuditFrom(ctx).SetCheck(rbac.ResourceClusters, action, obj)
	if err := rest.Authorize(ctx, rbac.ResourceClusters, action, obj); err != nil {
		return errDenied()
	}
	return nil
}

// target annotates the audit record with the cluster acted on.
func target(ctx context.Context, c *v1alpha1.Cluster) {
	if c == nil || c.Name == "" {
		return
	}
	rest.AuditFrom(ctx).SetTarget(rest.ObjectRef{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster", Namespace: c.Namespace, Name: c.Name, UID: c.UID})
}

// authorizeAmbientAuth requires action on the server's unscoped RBAC object (a grant
// outside any project) for a cluster that authenticates with execProviderConfig or
// awsAuthConfig: the controller (and patrol-server with ConnectorOptions.AllowExec) would
// run the command, or send tokens minted with Patrol's own cloud identity, to the server
// URL chosen by the caller. Project-scoped grants cannot store such configurations.
func authorizeAmbientAuth(ctx context.Context, action string, spec *v1alpha1.ClusterSpec) error {
	if !usesAmbientAuth(spec) {
		return nil
	}
	if err := rest.Authorize(ctx, rbac.ResourceClusters, action, spec.Server); err != nil {
		var re *rest.Error
		if errors.As(err, &re) {
			return rest.PermissionDenied("awsAuthConfig and execProviderConfig need clusters, %s on the server outside any project: %s", action, re.Detail)
		}
		return err
	}
	return nil
}

// list serves GET /api/v1/clusters.
func (s *Service) list(ctx context.Context, req *apiclient.ListClustersRequest) (*apiclient.RegisteredClusterList, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var sel labels.Selector
	if req.Selector != "" {
		var err error
		if sel, err = labels.Parse(req.Selector); err != nil {
			return nil, rest.InvalidArgument("invalid selector: %v", err)
		}
	}
	entries, rv, err := s.listEffective(ctx)
	if err != nil {
		return nil, err
	}
	// Upstream filters: by id, then name, then server; each keeps the first match.
	if req.IDValue != "" {
		switch req.IDType {
		case apiclient.ClusterIDTypeName:
			entries = firstMatch(entries, func(e entry) bool { return e.c.Spec.Name == req.IDValue })
		case apiclient.ClusterIDTypeNameEscaped:
			name, err := urlUnescape(req.IDValue)
			if err != nil {
				return nil, err
			}
			entries = firstMatch(entries, func(e entry) bool { return e.c.Spec.Name == name })
		case "", apiclient.ClusterIDTypeServer:
			server := clusterlib.NormalizeServer(req.IDValue)
			entries = firstMatch(entries, func(e entry) bool { return e.server() == server })
		default:
			return nil, rest.InvalidArgument("invalid id.type %q: must be server, name or name_escaped", req.IDType)
		}
	}
	if req.Name != "" {
		entries = firstMatch(entries, func(e entry) bool { return e.c.Spec.Name == req.Name })
	}
	if req.Server != "" {
		server := clusterlib.NormalizeServer(req.Server)
		entries = firstMatch(entries, func(e entry) bool { return e.server() == server })
	}
	visible := make([]entry, 0, len(entries))
	for _, e := range entries {
		if sel != nil && !sel.Matches(labels.Set(e.c.Labels)) {
			continue
		}
		if !rest.Can(ctx, rbac.ResourceClusters, rbac.ActionGet, rbacObject(&e.c.Spec)) {
			continue
		}
		visible = append(visible, e)
	}
	page, next, err := paginate(visible, entry.server, req.Limit, req.Continue)
	if err != nil {
		return nil, err
	}
	secrets := map[string]map[string][]byte{}
	if len(page) > 0 {
		if secrets, err = s.clusterSecrets(ctx); err != nil {
			return nil, err
		}
	}
	out := &apiclient.RegisteredClusterList{Metadata: metav1.ListMeta{ResourceVersion: rv, Continue: next}, Items: make([]apiclient.RegisteredCluster, 0, len(page))}
	for _, e := range page {
		var creds map[string][]byte
		if ref := e.c.Spec.Config.CredentialsRef; ref != nil && !e.synthetic {
			creds = secrets[ref.Name]
		}
		out.Items = append(out.Items, Sanitize(e.c, creds, e.synthetic))
	}
	return out, nil
}

func firstMatch(entries []entry, match func(entry) bool) []entry {
	for _, e := range entries {
		if match(e) {
			return []entry{e}
		}
	}
	return []entry{}
}

func urlUnescape(v string) (string, error) {
	out, err := url.QueryUnescape(v)
	if err != nil {
		return "", rest.InvalidArgument("failed to unescape cluster name: %v", err)
	}
	return out, nil
}

// get serves GET /api/v1/clusters/{server}.
func (s *Service) get(ctx context.Context, req *apiclient.ClusterIDRequest) (*apiclient.RegisteredCluster, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	entries, _, err := s.resolve(ctx, req.Server, req.IDType)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errDenied()
	}
	e := entries[0]
	if err := authorize(ctx, rbac.ActionGet, e.c); err != nil {
		return nil, err
	}
	return s.toAPI(ctx, e)
}

// create serves POST /api/v1/clusters (upstream ClusterService.Create).
func (s *Service) create(ctx context.Context, req *apiclient.CreateClusterRequest) (*apiclient.RegisteredCluster, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	in := &req.Cluster
	server, err := normalizeServer(in.Server, s.d.Config.AllowLinkLocal)
	if err != nil {
		return nil, err
	}
	obj := apiclient.ClusterRBACObject(in.Project, server)
	rest.AuditFrom(ctx).SetCheck(rbac.ResourceClusters, rbac.ActionCreate, obj)
	if err := rest.Authorize(ctx, rbac.ResourceClusters, rbac.ActionCreate, obj); err != nil {
		var re *rest.Error
		if errors.As(err, &re) {
			return nil, rest.PermissionDenied("permission denied while creating cluster: %s", re.Detail)
		}
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	if err := clusterlib.ValidateRegistration(server, st.InClusterEnabled()); err != nil {
		return nil, rest.InvalidArgument("%v", err)
	}
	d, err := validateInput(in, server, s.d.Config.AllowLinkLocal)
	if err != nil {
		return nil, err
	}
	if err := authorizeAmbientAuth(ctx, rbac.ActionCreate, &d.spec); err != nil {
		return nil, err
	}
	if err := s.checkProject(ctx, d.spec.Project); err != nil {
		return nil, err
	}
	// The connection is checked first (upstream order): only a caller holding working
	// credentials for the server learns whether it is registered.
	version, probed, err := s.probe(ctx, d.spec, d.creds)
	if err != nil {
		return nil, err
	}
	entries, _, err := s.resolve(ctx, server, apiclient.ClusterIDTypeServer)
	if err != nil {
		return nil, err
	}
	// The implicit in-cluster cluster counts as existing: an object registering its server
	// replaces it for every project (name, namespaces, project scope), so it is held to the
	// update rules below, never to the create permission of a project (upstream creates its
	// Secret for any create holder).
	var existing *entry
	if len(entries) > 0 {
		existing = &entries[0]
		// A cluster the caller cannot see is reported as existing, never shown.
		if !rest.Can(ctx, rbac.ResourceClusters, rbac.ActionGet, rbacObject(&existing.c.Spec)) {
			return nil, rest.AlreadyExists("cluster %q already exists", server)
		}
	}
	if existing != nil {
		var cur map[string][]byte
		if !existing.synthetic {
			if cur, err = s.credentials(ctx, existing.c); err != nil {
				return nil, err
			}
		}
		diff := clusterDiff(existing.c, cur, d)
		switch {
		case len(diff) == 0:
			target(ctx, existing.c)
			if probed {
				s.recordProbe(ctx, existing.c, version)
			}
			return s.toAPI(ctx, *existing)
		case req.Upsert || existing.synthetic:
			// An upsert is a full update: update on the existing cluster and on its new
			// project's object (upstream delegates to Update). Registering the implicit
			// in-cluster cluster needs the same, upsert or not: update on its unscoped
			// object.
			if err := authorize(ctx, rbac.ActionUpdate, existing.c); err != nil {
				return nil, err
			}
			if err := rest.Authorize(ctx, rbac.ResourceClusters, rbac.ActionUpdate, rbacObject(&d.spec)); err != nil {
				return nil, errDenied()
			}
			if err := authorizeAmbientAuth(ctx, rbac.ActionUpdate, &d.spec); err != nil {
				return nil, err
			}
			return s.applyUpdate(ctx, *existing, d, nil, "", version, probed)
		default:
			return nil, rest.InvalidArgument("existing cluster spec is different; use upsert flag to force update; difference in keys %q", strings.Join(diff, ","))
		}
	}
	created, err := s.createObjects(ctx, d)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, rest.AlreadyExists("cluster %q already exists", server)
		}
		return nil, err
	}
	target(ctx, created)
	rest.AuditFrom(ctx).SetMessage("cluster %s created", server)
	if probed {
		s.recordProbe(ctx, created, version)
	}
	return s.toAPI(ctx, entry{c: s.fresh(ctx, created)})
}

// fresh re-reads c so that responses carry the status just recorded; c is returned when
// the read fails.
func (s *Service) fresh(ctx context.Context, c *v1alpha1.Cluster) *v1alpha1.Cluster {
	var cur v1alpha1.Cluster
	if err := s.d.Client.Get(ctx, client.ObjectKeyFromObject(c), &cur); err != nil {
		return c
	}
	return &cur
}

// probe checks connectivity with the target configuration. Clusters whose configuration
// would run an exec credential plugin or use Patrol's own cloud identity (execProviderConfig,
// awsAuthConfig) are not probed by the API server (probed false).
func (s *Service) probe(ctx context.Context, spec v1alpha1.ClusterSpec, creds map[string][]byte) (string, bool, error) {
	conn, err := s.connector()
	if err != nil {
		return "", false, err
	}
	version, err := conn.Probe(ctx, &v1alpha1.Cluster{Spec: spec}, creds)
	if errors.Is(err, ErrExecNotAllowed) {
		s.d.Logger.Info("cluster connection not checked by the API server: it uses execProviderConfig or awsAuthConfig", "server", spec.Server)
		return "", false, nil
	}
	if err != nil {
		return "", false, rest.InvalidArgument("error getting server version: %v", err)
	}
	return version, true, nil
}

// update serves PUT /api/v1/clusters/{server} (upstream ClusterService.Update).
func (s *Service) update(ctx context.Context, req *apiclient.UpdateClusterRequest) (*apiclient.RegisteredCluster, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	entries, _, err := s.resolve(ctx, req.Server, req.IDType)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errDenied()
	}
	cur := entries[0]
	if err := authorize(ctx, rbac.ActionUpdate, cur.c); err != nil {
		return nil, err
	}
	in := &req.Cluster
	if in.Server != "" {
		server, err := normalizeServer(in.Server, s.d.Config.AllowLinkLocal)
		if err != nil {
			return nil, err
		}
		if server != cur.server() {
			return nil, rest.InvalidArgument("server is immutable: cannot change %q to %q", cur.server(), server)
		}
	}
	fields := parseUpdatedFields(req.UpdatedFields)
	if len(req.UpdatedFields) == 0 {
		fields = nil
	}
	if fields == nil || fields["project"] {
		// The caller must also be allowed to update the cluster in its new project.
		newObj := apiclient.ClusterRBACObject(in.Project, cur.server())
		if err := rest.Authorize(ctx, rbac.ResourceClusters, rbac.ActionUpdate, newObj); err != nil {
			return nil, errDenied()
		}
	}
	d, err := validateInput(in, cur.server(), s.d.Config.AllowLinkLocal)
	if err != nil {
		return nil, err
	}
	if fields == nil || fields["project"] {
		if err := s.checkProject(ctx, d.spec.Project); err != nil {
			return nil, err
		}
	}
	if fields == nil || fields["config"] {
		if err := authorizeAmbientAuth(ctx, rbac.ActionUpdate, &d.spec); err != nil {
			return nil, err
		}
	}
	return s.applyUpdate(ctx, cur, d, fields, in.ResourceVersion, "", false)
}

// merged is the result of applying a request onto an existing cluster.
type merged struct {
	spec        v1alpha1.ClusterSpec
	labels      map[string]string
	annotations map[string]string
	// creds are the credentials to store; writeCreds is false when they stay unchanged.
	creds      map[string][]byte
	writeCreds bool
}

// mergeUpdate applies d onto cur: every field when fields is nil (full replacement, which
// also replaces the credentials), else only the listed paths. The refresh annotation of a
// pending cache invalidation is kept.
func mergeUpdate(cur *v1alpha1.Cluster, curCreds map[string][]byte, d *desired, fields map[string]bool) merged {
	m := merged{spec: *cur.Spec.DeepCopy(), labels: maps.Clone(cur.Labels), annotations: maps.Clone(cur.Annotations), creds: curCreds}
	all := fields == nil
	if all || fields["name"] {
		m.spec.Name = d.spec.Name
	}
	if all || fields["namespaces"] {
		m.spec.Namespaces = slices.Clone(d.spec.Namespaces)
	}
	if all || fields["config"] {
		ref := m.spec.Config.CredentialsRef
		m.spec.Config = *d.spec.Config.DeepCopy()
		m.spec.Config.CredentialsRef = ref
		m.creds, m.writeCreds = d.creds, true
	}
	if all || fields["shard"] {
		m.spec.Shard = nil
		if d.spec.Shard != nil {
			m.spec.Shard = ptr.To(*d.spec.Shard)
		}
	}
	if all || fields["clusterResources"] {
		m.spec.ClusterResources = d.spec.ClusterResources
	}
	if all || fields["labels"] {
		m.labels = maps.Clone(d.labels)
	}
	if all || fields["annotations"] {
		ann := maps.Clone(d.annotations)
		if v, ok := cur.Annotations[v1alpha1.AnnotationKeyRefresh]; ok {
			if ann == nil {
				ann = map[string]string{}
			}
			if _, set := ann[v1alpha1.AnnotationKeyRefresh]; !set {
				ann[v1alpha1.AnnotationKeyRefresh] = v
			}
		}
		m.annotations = ann
	}
	if all || fields["project"] {
		m.spec.Project = d.spec.Project
	}
	return m
}

// applyUpdate writes d onto the existing cluster cur (see mergeUpdate), after checking
// connectivity with the result (unless version was already probed with it). The implicit
// in-cluster cluster is materialized as a Cluster object (upstream creates its Secret).
//
// The write re-applies d onto the latest stored object on every attempt, so fields outside
// a partial update keep the values written concurrently (during the connection check, say).
// The stored object must still be the one that was authorized (same UID and server); when
// it has moved to another project meanwhile, update is required there too.
func (s *Service) applyUpdate(ctx context.Context, cur entry, d *desired, fields map[string]bool, clientRV, version string, probed bool) (*apiclient.RegisteredCluster, error) {
	var curCreds map[string][]byte
	if !cur.synthetic {
		if clientRV != "" && clientRV != cur.c.ResourceVersion {
			return nil, rest.Conflict("the cluster %q has been modified; reload it and retry", cur.server())
		}
		var err error
		if curCreds, err = s.credentials(ctx, cur.c); err != nil {
			return nil, err
		}
	}
	m := mergeUpdate(cur.c, curCreds, d, fields)
	if version == "" && !probed {
		var err error
		if version, probed, err = s.probe(ctx, m.spec, m.creds); err != nil {
			return nil, err
		}
	}
	if cur.synthetic {
		created, err := s.createObjects(ctx, &desired{spec: m.spec, labels: m.labels, annotations: m.annotations, creds: m.creds})
		if err != nil {
			return nil, err
		}
		target(ctx, created)
		if probed {
			s.recordProbe(ctx, created, version)
		}
		return s.toAPI(ctx, entry{c: s.fresh(ctx, created)})
	}
	modified := func() error {
		return rest.Conflict("the cluster %q has been modified; reload it and retry", cur.server())
	}
	var (
		updated v1alpha1.Cluster
		written merged
	)
	first := true
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.d.Client.Get(ctx, client.ObjectKeyFromObject(cur.c), &updated); err != nil {
			if apierrors.IsNotFound(err) {
				return modified()
			}
			return err
		}
		if updated.UID != cur.c.UID || clusterlib.NormalizeServer(updated.Spec.Server) != cur.server() {
			return modified()
		}
		if first && clientRV != "" && updated.ResourceVersion != clientRV {
			return modified()
		}
		first = false
		if rbacObject(&updated.Spec) != rbacObject(&cur.c.Spec) {
			if err := authorize(ctx, rbac.ActionUpdate, &updated); err != nil {
				return err
			}
		}
		// Re-apply onto the latest object: only the requested paths change; credentialsRef,
		// status and every other field stay as stored.
		written = mergeUpdate(&updated, curCreds, d, fields)
		ref := updated.Spec.Config.CredentialsRef
		updated.Spec = *written.spec.DeepCopy()
		updated.Spec.Config.CredentialsRef = ref
		updated.Labels = maps.Clone(written.labels)
		updated.Annotations = maps.Clone(written.annotations)
		err := s.d.Client.Update(ctx, &updated)
		if apierrors.IsConflict(err) && clientRV != "" {
			return modified()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if written.writeCreds {
		if err := s.writeCredentials(ctx, &updated, written.creds); err != nil {
			return nil, err
		}
	}
	target(ctx, &updated)
	rest.AuditFrom(ctx).SetMessage("cluster %s updated", updated.Spec.Server)
	// The check is recorded only when it was made with the connection settings written (a
	// concurrent writer may have changed them; the controller checks those).
	if probed && sameConnection(&m.spec, &written.spec) {
		s.recordProbe(ctx, &updated, version)
	}
	return s.toAPI(ctx, entry{c: s.fresh(ctx, &updated)})
}

// sameConnection reports whether two specs have the same server and connection settings
// (credentialsRef aside; the credentials of both come from the same request or read).
func sameConnection(a, b *v1alpha1.ClusterSpec) bool {
	ac, bc := a.Config, b.Config
	ac.CredentialsRef, bc.CredentialsRef = nil, nil
	return clusterlib.NormalizeServer(a.Server) == clusterlib.NormalizeServer(b.Server) && equality.Semantic.DeepEqual(ac, bc)
}

// clusterDiff lists the differing keys between an existing cluster and a create request
// (upstream Cluster.Equals: server, name, namespaces, shard, clusterResources, labels,
// annotations, config; Patrol also compares the project). Credential differences are
// reported as "Config" only, never by key.
func clusterDiff(cur *v1alpha1.Cluster, curCreds map[string][]byte, d *desired) []string {
	var out []string
	add := func(differs bool, keys ...string) {
		if differs {
			out = append(out, keys...)
		}
	}
	add(cur.Spec.Name != d.spec.Name, "Name")
	add(strings.Join(cur.Spec.Namespaces, ",") != strings.Join(d.spec.Namespaces, ","), "Namespaces")
	shard := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	add(shard(cur.Spec.Shard) != shard(d.spec.Shard), "Shard")
	add(cur.Spec.ClusterResources != d.spec.ClusterResources, "ClusterResources")
	add(cur.Spec.Project != d.spec.Project, "Project")
	add(!maps.Equal(userAnnotations(cur.Annotations), userAnnotations(d.annotations)), "Annotations")
	add(!maps.Equal(nonEmpty(cur.Labels), nonEmpty(d.labels)), "Labels")
	cc, dc := cur.Spec.Config, d.spec.Config
	cc.CredentialsRef, dc.CredentialsRef = nil, nil
	if !equality.Semantic.DeepEqual(cc, dc) || !credentialsEqual(curCreds, d.creds) {
		out = append(out, "Config")
	}
	return out
}

// userAnnotations drops the annotations Patrol maintains itself.
func userAnnotations(a map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		switch k {
		case v1alpha1.AnnotationKeyRefresh, v1alpha1.AnnotationKeyManagedBy, lastAppliedAnnotation:
			continue
		}
		out[k] = v
	}
	return out
}

func nonEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func credentialsEqual(a, b map[string][]byte) bool {
	return maps.EqualFunc(a, b, func(x, y []byte) bool { return string(x) == string(y) })
}

// delete serves DELETE /api/v1/clusters/{server}. By name, every cluster with the name is
// deleted; each must be deletable by the caller (checked before anything is deleted, with
// the cluster's own project: upstream checks every one against the project of the first).
func (s *Service) delete(ctx context.Context, req *apiclient.ClusterIDRequest) (*apiclient.Empty, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	entries, _, err := s.resolve(ctx, req.Server, req.IDType)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errDenied()
	}
	for _, e := range entries {
		if err := authorize(ctx, rbac.ActionDelete, e.c); err != nil {
			return nil, err
		}
	}
	var deleted []string
	for _, e := range entries {
		if e.synthetic {
			// The implicit in-cluster cluster has no object (upstream: NotFound).
			return nil, rest.NotFound("cluster %q not found", e.server())
		}
		sec, err := s.secret(ctx, e.c)
		if err != nil {
			return nil, err
		}
		if err := s.d.Client.Delete(ctx, e.c, client.Preconditions{UID: ptr.To(e.c.UID)}); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		// Credentials Secrets written by this API are deleted with the cluster (the owner
		// reference also lets the garbage collector do it); others are left alone.
		if isManaged(sec) && isClusterSecret(sec) {
			if err := s.d.Client.Delete(ctx, sec, client.Preconditions{UID: ptr.To(sec.UID)}); err != nil && !apierrors.IsNotFound(err) {
				s.d.Logger.Warn("failed to delete the credentials Secret of a deleted cluster", "secret", sec.Name, "error", err)
			}
		}
		target(ctx, e.c)
		deleted = append(deleted, e.server())
	}
	rest.AuditFrom(ctx).SetMessage("cluster(s) %s deleted", strings.Join(deleted, ", "))
	return &apiclient.Empty{}, nil
}

// rotateAuth serves POST /api/v1/clusters/{server}/rotate-auth (upstream RotateAuth).
func (s *Service) rotateAuth(ctx context.Context, req *apiclient.ClusterIDRequest) (*apiclient.Empty, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	entries, _, err := s.resolve(ctx, req.Server, req.IDType)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errDenied()
	}
	for _, e := range entries {
		if err := authorize(ctx, rbac.ActionUpdate, e.c); err != nil {
			return nil, err
		}
	}
	conn, err := s.connector()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := s.rotateOne(ctx, conn, e); err != nil {
			return nil, err
		}
	}
	return &apiclient.Empty{}, nil
}

func (s *Service) rotateOne(ctx context.Context, conn Connector, e entry) error {
	notBearer := rest.InvalidArgument("Cluster '%s' does not use bearer token authentication", e.server())
	if e.synthetic {
		return notBearer
	}
	data, err := s.credentials(ctx, e.c)
	if err != nil {
		return err
	}
	creds := clusterlib.CredentialsFromSecretData(data)
	cfg := e.c.Spec.Config
	if creds.BearerToken == "" || cfg.AWSAuthConfig != nil || cfg.ExecProviderConfig != nil || clusterlib.IsInClusterServer(e.server()) {
		return notBearer
	}
	notLegacy := func(err error) error {
		return rest.InvalidArgument("cluster '%s' does not use a legacy service-account token: %v", e.server(), err)
	}
	claims, err := clusterlib.ParseServiceAccountToken(creds.BearerToken)
	if err != nil {
		return notLegacy(err)
	}
	if claims.Namespace == "" || claims.SecretName == "" || claims.ServiceAccountName == "" {
		return notLegacy(clusterlib.ErrNotLegacyToken)
	}
	kube, err := conn.Kubernetes(ctx, e.c, data)
	if err != nil {
		return rest.Unavailable("failed to connect to cluster %q: %v", e.server(), err)
	}
	persisted := false
	persist := func(ctx context.Context, newToken string) error {
		next := creds
		next.CertData, next.KeyData = nil, nil // token authentication: no client certificate
		next.BearerToken = newToken
		nd := next.SecretData()
		version, err := conn.Probe(ctx, e.c, nd)
		if err != nil {
			return fmt.Errorf("failed to get server version: %w", err)
		}
		if err := s.writeCredentials(ctx, e.c, nd); err != nil {
			return err
		}
		persisted = true
		s.recordProbe(ctx, e.c, version)
		return nil
	}
	_, err = clusterlib.RotateManagerToken(ctx, kube, creds.BearerToken, persist, s.d.Config.Bootstrap)
	target(ctx, e.c)
	switch {
	case err == nil:
		rest.AuditFrom(ctx).SetMessage("cluster %s credentials rotated", e.server())
		return nil
	case errors.Is(err, clusterlib.ErrNotLegacyToken), errors.Is(err, clusterlib.ErrNoBearerToken):
		return notLegacy(err)
	case persisted:
		return rest.Unavailable("rotated the token of cluster %q but failed to remove the old token: %v", e.server(), err)
	default:
		var re *rest.Error
		if errors.As(err, &re) {
			return re
		}
		return rest.Unavailable("failed to rotate the token of cluster %q: %v", e.server(), err)
	}
}

// invalidateCache serves POST /api/v1/clusters/{server}/invalidate-cache: it sets the
// refresh annotation (upstream refreshRequestedAt) to now.
func (s *Service) invalidateCache(ctx context.Context, req *apiclient.ClusterIDRequest) (*apiclient.RegisteredCluster, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	entries, _, err := s.resolve(ctx, req.Server, req.IDType)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errDenied()
	}
	e := entries[0]
	if err := authorize(ctx, rbac.ActionUpdate, e.c); err != nil {
		return nil, err
	}
	now := s.d.Now().UTC().Format(time.RFC3339)
	if e.synthetic {
		created, err := s.createObjects(ctx, &desired{spec: e.c.Spec, annotations: map[string]string{v1alpha1.AnnotationKeyRefresh: now}})
		if err != nil {
			return nil, err
		}
		target(ctx, created)
		return s.toAPI(ctx, entry{c: created})
	}
	patched := e.c.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[v1alpha1.AnnotationKeyRefresh] = now
	if err := s.d.Client.Patch(ctx, patched, client.MergeFrom(e.c)); err != nil {
		return nil, err
	}
	target(ctx, patched)
	rest.AuditFrom(ctx).SetMessage("cluster %s cache invalidation requested", e.server())
	return s.toAPI(ctx, entry{c: patched})
}
