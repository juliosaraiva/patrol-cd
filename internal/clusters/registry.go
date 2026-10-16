package clusters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ClusterGVR is the resource of the Cluster kind.
var ClusterGVR = v1alpha1.GroupVersion.WithResource("clusters")

// Index names of the cluster informer.
const (
	indexByServer      = "byServer"
	indexByName        = "byName"
	indexByProject     = "byProject"
	indexByCredentials = "byCredentials"
)

// DefaultRegistryResync is the informer resync period (upstream cluster informer: 3 minutes).
const DefaultRegistryResync = 3 * time.Minute

// RegistryOptions configures a Registry.
type RegistryOptions struct {
	// Namespace is the control-plane namespace holding Clusters and their Secrets. Required.
	Namespace string
	// InClusterEnabled is the initial cluster.inClusterEnabled value (nil means enabled).
	// Update it with SetInClusterEnabled.
	InClusterEnabled *bool
	// ResyncPeriod of the informers; zero means DefaultRegistryResync.
	ResyncPeriod time.Duration
	// SecretLabelSelector selects the credential Secrets that are cached. Empty means
	// "cd.patrol.example/secret-type=cluster"; Secrets referenced by Clusters must match it.
	SecretLabelSelector string
	// LocalServerVersion probes the control-plane cluster once during Start, for the
	// connection state of the synthetic in-cluster cluster. Nil leaves it unset.
	LocalServerVersion func(ctx context.Context) (string, error)
	// Logger receives warnings (malformed objects, duplicate servers). Nil discards them.
	Logger *slog.Logger
	// Now is the registry clock. Nil means time.Now.
	Now func() time.Time
}

// EventType is the kind of change of an effective cluster.
type EventType string

// Event types.
const (
	EventAdded    EventType = "Added"
	EventModified EventType = "Modified"
	EventDeleted  EventType = "Deleted"
)

// Changes says what differs in a Modified event.
type Changes struct {
	// Identity: a different object now serves the server (for example a Cluster object
	// replacing the synthetic in-cluster cluster).
	Identity bool
	// Spec: spec differs.
	Spec bool
	// Metadata: labels or annotations differ.
	Metadata bool
	// Credentials: the referenced Secret was created, changed or deleted, or credentialsRef
	// points elsewhere.
	Credentials bool
}

// Event reports a change of the effective cluster serving Server. Old is nil for Added and New
// is nil for Deleted. Status-only updates of Cluster objects produce no event. The in-cluster
// server follows upstream: a Cluster object for it replaces the synthetic cluster (Modified),
// deleting the object restores the synthetic one (Modified), and disabling in-cluster deletes
// it. Old and New are copies owned by the subscriber.
type Event struct {
	Type    EventType
	Server  string
	Old     *v1alpha1.Cluster
	New     *v1alpha1.Cluster
	Changes Changes
}

// Registry is an informer-backed, read-only view of the Cluster objects in the control-plane
// namespace, their credential Secrets and the synthetic in-cluster cluster. Lookups are served
// from memory and return deep copies. When several Cluster objects declare the same server, the
// one with the smallest name serves it (as internal/project ClusterIndex decides) and a warning
// is logged.
type Registry struct {
	ns     string
	logger *slog.Logger
	now    func() time.Time

	clusterInformer cache.SharedIndexInformer
	secretInformer  cache.SharedIndexInformer
	clusters        cache.Indexer
	secrets         cache.Indexer
	localProbe      func(ctx context.Context) (string, error)

	ready   atomic.Bool
	started atomic.Bool

	mu        sync.Mutex
	inCluster bool
	local     *v1alpha1.ConnectionState // probe result of the synthetic cluster
	localVer  string
	snapshot  map[string]entry
	subs      map[uint64]*subscriber
	nextSub   uint64
	dupWarned map[string]int

	// publishHook, when set (tests), receives every event synchronously under r.mu.
	publishHook func(Event)
}

type entry struct {
	cluster *v1alpha1.Cluster // shared with the indexer; never modified
	// secret fingerprints the credentials Secret (name and data); "" when there is none.
	secret string
}

// NewRegistry creates a Registry reading Clusters through dyn and Secrets through kube. Call
// Start before any lookup.
func NewRegistry(dyn dynamic.Interface, kube kubernetes.Interface, opts RegistryOptions) (*Registry, error) {
	if dyn == nil || kube == nil {
		return nil, errors.New("cluster registry: nil client")
	}
	if opts.Namespace == "" {
		return nil, errors.New("cluster registry: namespace is required")
	}
	resync := opts.ResyncPeriod
	if resync <= 0 {
		resync = DefaultRegistryResync
	}
	selector := opts.SecretLabelSelector
	if selector == "" {
		selector = v1alpha1.LabelKeySecretType + "=" + v1alpha1.LabelValueSecretTypeCluster
	}
	ci := dynamicinformer.NewFilteredDynamicInformer(dyn, ClusterGVR, opts.Namespace, resync, clusterIndexers(), nil).Informer()
	si := coreinformers.NewFilteredSecretInformer(kube, opts.Namespace, resync, cache.Indexers{}, func(lo *metav1.ListOptions) {
		lo.LabelSelector = selector
	})
	r := newRegistry(ci.GetIndexer(), si.GetIndexer(), opts)
	r.clusterInformer, r.secretInformer = ci, si
	if err := ci.SetTransform(r.transformCluster); err != nil {
		return nil, fmt.Errorf("cluster registry: set cluster transform: %w", err)
	}
	if err := si.SetTransform(stripManagedFields); err != nil {
		return nil, fmt.Errorf("cluster registry: set secret transform: %w", err)
	}
	if _, err := ci.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.onClusterChange(obj) },
		UpdateFunc: func(oldObj, newObj any) { r.onClusterChange(oldObj, newObj) },
		DeleteFunc: func(obj any) { r.onClusterChange(obj) },
	}); err != nil {
		return nil, fmt.Errorf("cluster registry: add cluster handler: %w", err)
	}
	if _, err := si.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.onSecretChange(obj) },
		UpdateFunc: func(_, newObj any) { r.onSecretChange(newObj) },
		DeleteFunc: func(obj any) { r.onSecretChange(obj) },
	}); err != nil {
		return nil, fmt.Errorf("cluster registry: add secret handler: %w", err)
	}
	return r, nil
}

// newRegistry builds the registry core over existing indexers (tests drive it directly).
func newRegistry(clusters, secrets cache.Indexer, opts RegistryOptions) *Registry {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	inCluster := true
	if opts.InClusterEnabled != nil {
		inCluster = *opts.InClusterEnabled
	}
	return &Registry{
		ns:         opts.Namespace,
		logger:     logger.With("component", "cluster-registry"),
		now:        now,
		clusters:   clusters,
		secrets:    secrets,
		localProbe: opts.LocalServerVersion,
		inCluster:  inCluster,
		snapshot:   map[string]entry{},
		subs:       map[uint64]*subscriber{},
		dupWarned:  map[string]int{},
	}
}

func clusterIndexers() cache.Indexers {
	str := func(f func(*v1alpha1.Cluster) string) cache.IndexFunc {
		return func(obj any) ([]string, error) {
			c, ok := obj.(*v1alpha1.Cluster)
			if !ok {
				return nil, nil
			}
			if v := f(c); v != "" {
				return []string{v}, nil
			}
			return nil, nil
		}
	}
	return cache.Indexers{
		indexByServer: str(func(c *v1alpha1.Cluster) string { return NormalizeServer(c.Spec.Server) }),
		indexByName:   str(func(c *v1alpha1.Cluster) string { return c.Spec.Name }),
		indexByProject: str(func(c *v1alpha1.Cluster) string {
			return c.Spec.Project
		}),
		indexByCredentials: str(func(c *v1alpha1.Cluster) string {
			if c.Spec.Config.CredentialsRef == nil {
				return ""
			}
			return c.Spec.Config.CredentialsRef.Name
		}),
	}
}

// transformCluster converts informer objects to *v1alpha1.Cluster once, at ingestion. Objects
// that do not convert are kept as is and skipped by lookups.
func (r *Registry) transformCluster(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	c := &v1alpha1.Cluster{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, c); err != nil {
		r.logger.Warn("failed to convert cluster object, skipping it", "name", u.GetName(), "error", err)
		return obj, nil
	}
	c.ManagedFields = nil
	c.Spec.Server = NormalizeServer(c.Spec.Server)
	c.Spec.Namespaces = NormalizeNamespaces(c.Spec.Namespaces)
	return c, nil
}

func stripManagedFields(obj any) (any, error) {
	if s, ok := obj.(*corev1.Secret); ok {
		s.ManagedFields = nil
	}
	return obj, nil
}

// Start runs the informers until ctx ends, waits for the initial listing, probes the
// control-plane cluster for the synthetic in-cluster entry and computes the initial state
// (subscribers receive Added events). It returns an error if ctx ends before the caches sync.
func (r *Registry) Start(ctx context.Context) error {
	if r.clusterInformer == nil {
		return errors.New("cluster registry: not created by NewRegistry")
	}
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("cluster registry: already started")
	}
	go r.clusterInformer.RunWithContext(ctx)
	go r.secretInformer.RunWithContext(ctx)
	if !cache.WaitForCacheSync(ctx.Done(), r.clusterInformer.HasSynced, r.secretInformer.HasSynced) {
		return fmt.Errorf("cluster registry: %w: %w", ErrNotSynced, context.Cause(ctx))
	}
	r.probeLocal(ctx)
	r.markReady()
	go func() {
		<-ctx.Done()
		r.closeSubscribers()
	}()
	return nil
}

// HasSynced reports whether Start completed.
func (r *Registry) HasSynced() bool { return r.ready.Load() }

func (r *Registry) probeLocal(ctx context.Context) {
	if r.localProbe == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ver, err := r.localProbe(pctx)
	now := metav1.NewTime(r.now())
	state := &v1alpha1.ConnectionState{Status: v1alpha1.ConnectionStatusSuccessful, AttemptedAt: &now}
	if err != nil {
		state.Status = v1alpha1.ConnectionStatusFailed
		state.Message = err.Error()
		ver = ""
	}
	r.mu.Lock()
	r.local, r.localVer = state, ver
	r.mu.Unlock()
}

// markReady computes the initial state and enables incremental events.
func (r *Registry) markReady() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready.Store(true)
	r.reconcileLocked(r.allServersLocked()...)
}

// SetInClusterEnabled updates cluster.inClusterEnabled. Disabling removes the in-cluster
// cluster (synthetic or object) from lookups and emits a Deleted event; enabling restores it.
func (r *Registry) SetInClusterEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inCluster == enabled {
		return
	}
	r.inCluster = enabled
	if r.ready.Load() {
		r.reconcileLocked(InClusterServer)
	}
}

// InClusterEnabled returns the current cluster.inClusterEnabled value.
func (r *Registry) InClusterEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inCluster
}

func serverOf(obj any) (string, bool) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	c, ok := obj.(*v1alpha1.Cluster)
	if !ok {
		return "", false
	}
	return NormalizeServer(c.Spec.Server), true
}

func (r *Registry) onClusterChange(objs ...any) {
	var servers []string
	for _, o := range objs {
		if s, ok := serverOf(o); ok {
			servers = append(servers, s)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready.Load() {
		return
	}
	r.reconcileLocked(servers...)
}

func (r *Registry) onSecretChange(obj any) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return
	}
	items, err := r.clusters.ByIndex(indexByCredentials, s.Name)
	if err != nil {
		r.logger.Warn("query clusters by credentials secret", "secret", s.Name, "error", err)
		return
	}
	var servers []string
	for _, it := range items {
		if c, ok := it.(*v1alpha1.Cluster); ok {
			servers = append(servers, NormalizeServer(c.Spec.Server))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready.Load() {
		return
	}
	r.reconcileLocked(servers...)
}

// allServersLocked returns every server with an object plus the in-cluster server.
func (r *Registry) allServersLocked() []string {
	set := map[string]struct{}{InClusterServer: {}}
	for _, s := range r.clusters.ListIndexFuncValues(indexByServer) {
		set[s] = struct{}{}
	}
	for s := range r.snapshot {
		set[s] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// reconcileLocked compares the effective entry of each server with the last notified one and
// emits events. r.mu must be held.
func (r *Registry) reconcileLocked(servers ...string) {
	slices.Sort(servers)
	servers = slices.Compact(servers)
	for _, s := range servers {
		prev, had := r.snapshot[s]
		cur, has := r.effectiveEntryLocked(s)
		switch {
		case !had && !has:
		case !had:
			r.snapshot[s] = cur
			r.publishLocked(Event{Type: EventAdded, Server: s, New: cur.cluster.DeepCopy()})
		case !has:
			delete(r.snapshot, s)
			r.publishLocked(Event{Type: EventDeleted, Server: s, Old: prev.cluster.DeepCopy()})
		default:
			r.snapshot[s] = cur
			if ch := diffEntries(prev, cur); ch != (Changes{}) {
				r.publishLocked(Event{Type: EventModified, Server: s, Old: prev.cluster.DeepCopy(), New: cur.cluster.DeepCopy(), Changes: ch})
			}
		}
	}
}

func diffEntries(a, b entry) Changes {
	return Changes{
		Identity:    a.cluster.UID != b.cluster.UID || a.cluster.Name != b.cluster.Name,
		Spec:        !equality.Semantic.DeepEqual(a.cluster.Spec, b.cluster.Spec),
		Metadata:    !maps.Equal(a.cluster.Labels, b.cluster.Labels) || !maps.Equal(a.cluster.Annotations, b.cluster.Annotations),
		Credentials: a.secret != b.secret,
	}
}

// effectiveEntryLocked returns the cluster serving server and the resource version of its
// credentials Secret.
func (r *Registry) effectiveEntryLocked(server string) (entry, bool) {
	c := r.effectiveLocked(server)
	if c == nil {
		return entry{}, false
	}
	e := entry{cluster: c}
	if ref := c.Spec.Config.CredentialsRef; ref != nil {
		if obj, ok, _ := r.secrets.GetByKey(r.ns + "/" + ref.Name); ok {
			if s, ok := obj.(*corev1.Secret); ok {
				e.secret = secretFingerprint(s)
			}
		}
	}
	return e, true
}

// secretFingerprint hashes a Secret's name and data, so only credential changes (not metadata
// updates) are reported. The hash never leaves the process.
func secretFingerprint(s *corev1.Secret) string {
	h := sha256.New()
	_, _ = io.WriteString(h, s.Name)
	for _, k := range slices.Sorted(maps.Keys(s.Data)) {
		_, _ = fmt.Fprintf(h, "\x00%s\x00%d\x00", k, len(s.Data[k]))
		_, _ = h.Write(s.Data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// effectiveLocked returns the cluster serving server (shared, read-only), or nil.
func (r *Registry) effectiveLocked(server string) *v1alpha1.Cluster {
	if server == InClusterServer && !r.inCluster {
		return nil
	}
	objs := r.objectsByIndex(indexByServer, server)
	if len(objs) > 0 {
		if len(objs) > 1 && r.dupWarned[server] != len(objs) {
			r.dupWarned[server] = len(objs)
			names := make([]string, 0, len(objs))
			for _, o := range objs {
				names = append(names, o.Name)
			}
			r.logger.Warn("several Cluster objects declare the same server; using the first by name", "server", server, "clusters", names)
		}
		return objs[0]
	}
	if server == InClusterServer {
		return r.syntheticLocked()
	}
	return nil
}

// objectsByIndex returns the clusters of an index value sorted by name.
func (r *Registry) objectsByIndex(index, value string) []*v1alpha1.Cluster {
	items, err := r.clusters.ByIndex(index, value)
	if err != nil {
		r.logger.Warn("query cluster index", "index", index, "error", err)
		return nil
	}
	out := make([]*v1alpha1.Cluster, 0, len(items))
	for _, it := range items {
		c, ok := it.(*v1alpha1.Cluster)
		if !ok {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// syntheticLocked returns the synthetic in-cluster cluster.
func (r *Registry) syntheticLocked() *v1alpha1.Cluster {
	c := &v1alpha1.Cluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{Namespace: r.ns},
		Spec:       v1alpha1.ClusterSpec{Name: InClusterName, Server: InClusterServer},
	}
	if r.local != nil {
		st := r.local.DeepCopy()
		now := metav1.NewTime(r.now())
		st.AttemptedAt = &now
		c.Status.ConnectionState = st
		c.Status.ServerVersion = r.localVer
	}
	return c
}

// IsSynthetic reports whether c is the synthetic in-cluster cluster (it has no object, so its
// status cannot be written).
func IsSynthetic(c *v1alpha1.Cluster) bool {
	return c != nil && c.UID == "" && c.Name == "" && IsInClusterServer(c.Spec.Server)
}

func (r *Registry) checkReady() error {
	if !r.ready.Load() {
		return ErrNotSynced
	}
	return nil
}

// ByServer returns the cluster serving server (trailing slashes ignored). Errors match
// ErrClusterNotFound; for the disabled in-cluster server the message is upstream's
// `cluster "https://kubernetes.default.svc" is disabled`.
func (r *Registry) ByServer(server string) (*v1alpha1.Cluster, error) {
	if err := r.checkReady(); err != nil {
		return nil, err
	}
	server = NormalizeServer(server)
	r.mu.Lock()
	defer r.mu.Unlock()
	if server == InClusterServer && !r.inCluster {
		return nil, &NotFoundError{Server: server, Disabled: true}
	}
	c := r.effectiveLocked(server)
	if c == nil {
		return nil, &NotFoundError{Server: server}
	}
	return c.DeepCopy(), nil
}

// ServersByName returns the sorted servers of the clusters named name. As upstream, the name
// "in-cluster" resolves to the in-cluster server when it is enabled and no cluster carries
// that name, and the in-cluster server is dropped while disabled.
func (r *Registry) ServersByName(name string) ([]string, error) {
	if err := r.checkReady(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serversByNameLocked(name), nil
}

func (r *Registry) serversByNameLocked(name string) []string {
	set := map[string]struct{}{}
	for _, c := range r.objectsByIndex(indexByName, name) {
		s := NormalizeServer(c.Spec.Server)
		// Only the object that serves its server answers to its name.
		if eff := r.effectiveLocked(s); eff != nil && eff.Name == c.Name {
			set[s] = struct{}{}
		}
	}
	if eff := r.effectiveLocked(InClusterServer); eff != nil && eff.Spec.Name == name && IsSynthetic(eff) {
		set[InClusterServer] = struct{}{}
	}
	if len(set) == 0 && name == InClusterName && r.inCluster {
		return []string{InClusterServer}
	}
	return slices.Sorted(maps.Keys(set))
}

// ByName returns the only cluster named name, or a *NameError (no match or several matches).
func (r *Registry) ByName(name string) (*v1alpha1.Cluster, error) {
	servers, err := r.ServersByName(name)
	if err != nil {
		return nil, err
	}
	if len(servers) != 1 {
		return nil, &NameError{Name: name, Servers: servers}
	}
	return r.ByServer(servers[0])
}

// All returns every effective cluster sorted by server, including the synthetic in-cluster
// cluster when enabled and not overridden.
func (r *Registry) All() ([]*v1alpha1.Cluster, error) {
	if err := r.checkReady(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*v1alpha1.Cluster
	for _, s := range r.allServersLocked() {
		if c := r.effectiveLocked(s); c != nil {
			out = append(out, c.DeepCopy())
		}
	}
	return out, nil
}

// ProjectClusters returns the effective clusters scoped to project (spec.project), sorted by
// server.
func (r *Registry) ProjectClusters(project string) ([]*v1alpha1.Cluster, error) {
	if err := r.checkReady(); err != nil {
		return nil, err
	}
	if project == "" {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*v1alpha1.Cluster
	for _, c := range r.objectsByIndex(indexByProject, project) {
		if eff := r.effectiveLocked(NormalizeServer(c.Spec.Server)); eff != nil && eff.Name == c.Name {
			out = append(out, c.DeepCopy())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Server < out[j].Spec.Server })
	return out, nil
}

// GetCluster is ByServer with the internal/project ClusterLookup signature.
func (r *Registry) GetCluster(_ context.Context, server string) (*v1alpha1.Cluster, error) {
	return r.ByServer(server)
}

// GetClusterServersByName is ServersByName with the internal/project ClusterLookup signature.
func (r *Registry) GetClusterServersByName(_ context.Context, name string) ([]string, error) {
	return r.ServersByName(name)
}

// GetProjectClusters is ProjectClusters with the internal/project ClusterLookup signature.
func (r *Registry) GetProjectClusters(_ context.Context, project string) ([]*v1alpha1.Cluster, error) {
	return r.ProjectClusters(project)
}

// ErrDestinationMissing is upstream's message for a destination with neither server nor name.
const ErrDestinationMissing = "Destination server missing from app spec"

// ResolveDestination returns the server URL of an Application destination (upstream
// GetDestinationServer): a server is returned normalized without a lookup; a name must match
// exactly one cluster.
func (r *Registry) ResolveDestination(dest v1alpha1.ApplicationDestination) (string, error) {
	switch {
	case dest.Name != "" && dest.Server != "":
		return "", fmt.Errorf("application destination can't have both name and server defined: %s %s", dest.Name, dest.Server)
	case dest.Server != "":
		return NormalizeServer(dest.Server), nil
	case dest.Name != "":
		servers, err := r.ServersByName(dest.Name)
		if err != nil {
			return "", fmt.Errorf("error getting cluster by name %q: %w", dest.Name, err)
		}
		if len(servers) != 1 {
			return "", &NameError{Name: dest.Name, Servers: servers}
		}
		return servers[0], nil
	default:
		return "", errors.New(ErrDestinationMissing) //nolint:staticcheck // upstream's capitalized message

	}
}

// DestinationCluster returns the cluster of an Application destination (upstream
// GetDestinationCluster), wrapping lookup errors with upstream's context.
func (r *Registry) DestinationCluster(dest v1alpha1.ApplicationDestination) (*v1alpha1.Cluster, error) {
	server, err := r.ResolveDestination(dest)
	if err != nil {
		return nil, err
	}
	c, err := r.ByServer(server)
	if err != nil {
		if dest.Server != "" {
			return nil, fmt.Errorf("error getting cluster by server %q: %w", dest.Server, err)
		}
		return nil, fmt.Errorf("error getting cluster by URL: %w", err)
	}
	return c, nil
}

// CountApplications returns how many apps target server, skipping apps for which permitted
// returns false (upstream: apps whose project does not permit their namespace) and apps whose
// destination does not resolve.
func (r *Registry) CountApplications(apps []v1alpha1.Application, server string, permitted func(*v1alpha1.Application) bool) int64 {
	server = NormalizeServer(server)
	var n int64
	for i := range apps {
		if permitted != nil && !permitted(&apps[i]) {
			continue
		}
		s, err := r.ResolveDestination(apps[i].Spec.Destination)
		if err == nil && s == server {
			n++
		}
	}
	return n
}

// secretRequiredReason explains why a referenced Secret may be invisible to the registry.
const secretRequiredReason = "not found (Secrets referenced by Clusters must exist in the control-plane namespace with label " +
	v1alpha1.LabelKeySecretType + "=" + v1alpha1.LabelValueSecretTypeCluster + ")"

// SecretData returns a copy of the data of the cluster's credentials Secret, or nil when the
// cluster has no credentialsRef. A missing Secret is a *SecretError (ErrSecretNotFound).
func (r *Registry) SecretData(c *v1alpha1.Cluster) (map[string][]byte, error) {
	if err := r.checkReady(); err != nil {
		return nil, err
	}
	if c == nil || c.Spec.Config.CredentialsRef == nil {
		return nil, nil
	}
	name := c.Spec.Config.CredentialsRef.Name
	obj, ok, err := r.secrets.GetByKey(r.ns + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("get credentials secret: %w", err)
	}
	s, isSecret := obj.(*corev1.Secret)
	if !ok || !isSecret {
		return nil, &SecretError{Cluster: c.Name, Namespace: r.ns, Secret: name, Reason: secretRequiredReason}
	}
	out := make(map[string][]byte, len(s.Data))
	for k, v := range s.Data {
		out[k] = slices.Clone(v)
	}
	return out, nil
}

// RestConfig builds the tuned configuration of c with its credentials (RestConfig).
func (r *Registry) RestConfig(ctx context.Context, c *v1alpha1.Cluster, opts Options) (*rest.Config, error) {
	data, err := r.SecretData(c)
	if err != nil {
		return nil, err
	}
	return RestConfig(ctx, c, data, opts)
}

// RawRestConfig builds the untuned configuration of c with its credentials (RawRestConfig).
func (r *Registry) RawRestConfig(ctx context.Context, c *v1alpha1.Cluster, opts Options) (*rest.Config, error) {
	data, err := r.SecretData(c)
	if err != nil {
		return nil, err
	}
	return RawRestConfig(ctx, c, data, opts)
}

// Subscribe registers fn for change events and returns a function that unregisters it. fn
// first receives an Added event for every current cluster (once the registry is ready), then
// changes in order. Events are delivered on a goroutine per subscriber, so a slow subscriber
// does not delay others; fn must not block indefinitely.
func (r *Registry) Subscribe(fn func(Event)) (cancel func()) {
	s := newSubscriber(fn)
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = s
	if r.ready.Load() {
		for _, srv := range slices.Sorted(maps.Keys(r.snapshot)) {
			s.enqueue(Event{Type: EventAdded, Server: srv, New: r.snapshot[srv].cluster.DeepCopy()})
		}
	}
	r.mu.Unlock()
	go s.run()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, id)
			r.mu.Unlock()
			s.close()
		})
	}
}

func (r *Registry) publishLocked(ev Event) {
	if r.publishHook != nil {
		r.publishHook(Event{Type: ev.Type, Server: ev.Server, Old: ev.Old.DeepCopy(), New: ev.New.DeepCopy(), Changes: ev.Changes})
	}
	for _, id := range slices.Sorted(maps.Keys(r.subs)) {
		s := r.subs[id]
		e := ev
		if len(r.subs) > 1 {
			e.Old, e.New = ev.Old.DeepCopy(), ev.New.DeepCopy()
		}
		s.enqueue(e)
	}
}

func (r *Registry) closeSubscribers() {
	r.mu.Lock()
	subs := r.subs
	r.subs = map[uint64]*subscriber{}
	r.mu.Unlock()
	for _, s := range subs {
		s.close()
	}
}

type subscriber struct {
	fn     func(Event)
	mu     sync.Mutex
	queue  []Event
	signal chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newSubscriber(fn func(Event)) *subscriber {
	return &subscriber{fn: fn, signal: make(chan struct{}, 1), done: make(chan struct{})}
}

func (s *subscriber) enqueue(ev Event) {
	s.mu.Lock()
	s.queue = append(s.queue, ev)
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *subscriber) close() { s.once.Do(func() { close(s.done) }) }

func (s *subscriber) run() {
	for {
		select {
		case <-s.done:
			return
		case <-s.signal:
		}
		for {
			s.mu.Lock()
			batch := s.queue
			s.queue = nil
			s.mu.Unlock()
			if len(batch) == 0 {
				break
			}
			for _, ev := range batch {
				select {
				case <-s.done:
					return
				default:
				}
				s.fn(ev)
			}
		}
	}
}
