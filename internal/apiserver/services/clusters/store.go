package clusters

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

var errNotConfigured = errors.New("not configured")

// entry is one effective cluster: a Cluster object, or the implicit in-cluster cluster.
type entry struct {
	c         *v1alpha1.Cluster
	synthetic bool
}

func (e entry) server() string { return e.c.Spec.Server }

func (s *Service) ready() error {
	if s.d.Client == nil || s.d.Config.Namespace == "" {
		return rest.Unavailable("the cluster API is not configured").WithCause(errNotConfigured)
	}
	return nil
}

func (s *Service) settings() (*settings.Settings, error) {
	if s.d.Settings == nil {
		return nil, rest.Unavailable("settings are not available").WithCause(errNotConfigured)
	}
	st := s.d.Settings.Current()
	if st == nil {
		return nil, rest.Unavailable("settings are not loaded yet")
	}
	return st, nil
}

func (s *Service) connector() (Connector, error) {
	if s.d.Connector == nil {
		return nil, rest.Unavailable("cluster connection checks are not configured").WithCause(errNotConfigured)
	}
	return s.d.Connector, nil
}

// listEffective returns the effective clusters sorted by server, and the list's
// resourceVersion: per server the Cluster object with the smallest name (duplicates are a
// misconfiguration; internal/clusters and internal/project decide the same way), plus the
// implicit in-cluster cluster when enabled and no object registers its server. The
// in-cluster server is left out entirely while cluster.inClusterEnabled is "false".
func (s *Service) listEffective(ctx context.Context) ([]entry, string, error) {
	st, err := s.settings()
	if err != nil {
		return nil, "", err
	}
	var list v1alpha1.ClusterList
	if err := s.d.Client.List(ctx, &list, client.InNamespace(s.d.Config.Namespace)); err != nil {
		return nil, "", err
	}
	inCluster := st.InClusterEnabled()
	byServer := map[string]*v1alpha1.Cluster{}
	for i := range list.Items {
		c := &list.Items[i]
		c.Spec.Server = clusterlib.NormalizeServer(c.Spec.Server)
		if clusterlib.IsInClusterServer(c.Spec.Server) && !inCluster {
			continue
		}
		if cur, ok := byServer[c.Spec.Server]; ok && cur.Name < c.Name {
			continue
		}
		byServer[c.Spec.Server] = c
	}
	out := make([]entry, 0, len(byServer)+1)
	for _, c := range byServer {
		out = append(out, entry{c: c})
	}
	if _, ok := byServer[v1alpha1.KubernetesInternalAPIServerAddr]; inCluster && !ok {
		out = append(out, entry{c: s.syntheticInCluster(), synthetic: true})
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.server(), b.server()) })
	return out, list.ResourceVersion, nil
}

// syntheticInCluster returns the implicit in-cluster cluster, with the status probed by
// the registry when one is wired.
func (s *Service) syntheticInCluster() *v1alpha1.Cluster {
	c := project.InClusterCluster()
	if s.d.InCluster != nil {
		if rc, err := s.d.InCluster.ByServer(v1alpha1.KubernetesInternalAPIServerAddr); err == nil && rc != nil && clusterlib.IsSynthetic(rc) {
			c.Status = *rc.Status.DeepCopy()
		}
	}
	return c
}

// resolve returns the clusters an identifier designates: the cluster of a server URL, or
// every cluster with a name (sorted by server) for id.type=name|name_escaped.
func (s *Service) resolve(ctx context.Context, value, idType string) ([]entry, bool, error) {
	byName := false
	switch idType {
	case "", apiclient.ClusterIDTypeServer:
	case apiclient.ClusterIDTypeName:
		byName = true
	case apiclient.ClusterIDTypeNameEscaped:
		v, err := url.QueryUnescape(value)
		if err != nil {
			return nil, true, rest.InvalidArgument("failed to unescape cluster name: %v", err)
		}
		value, byName = v, true
	default:
		return nil, false, rest.InvalidArgument("invalid id.type %q: must be server, name or name_escaped", idType)
	}
	entries, _, err := s.listEffective(ctx)
	if err != nil {
		return nil, byName, err
	}
	var out []entry
	if byName {
		for _, e := range entries {
			if e.c.Spec.Name == value {
				out = append(out, e)
			}
		}
		return out, true, nil
	}
	server := clusterlib.NormalizeServer(value)
	for _, e := range entries {
		if e.server() == server {
			out = append(out, e)
		}
	}
	return out, false, nil
}

// isClusterSecret reports whether a Secret is labeled as cluster credentials; only those
// are read or written by this API (and seen by the cluster registry).
func isClusterSecret(sec *corev1.Secret) bool {
	return sec != nil && sec.Labels[v1alpha1.LabelKeySecretType] == v1alpha1.LabelValueSecretTypeCluster
}

func isManaged(sec *corev1.Secret) bool {
	return sec != nil && sec.Annotations[v1alpha1.AnnotationKeyManagedBy] == v1alpha1.AnnotationValueManagedByPatrol
}

// secret returns the credentials Secret of c (nil when it has none or it does not exist).
func (s *Service) secret(ctx context.Context, c *v1alpha1.Cluster) (*corev1.Secret, error) {
	ref := c.Spec.Config.CredentialsRef
	if ref == nil || ref.Name == "" {
		return nil, nil
	}
	var sec corev1.Secret
	if err := s.d.Client.Get(ctx, client.ObjectKey{Namespace: s.d.Config.Namespace, Name: ref.Name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &sec, nil
}

// credentials returns the credentials of c; Secrets without the cluster label are ignored
// (the registry ignores them too).
func (s *Service) credentials(ctx context.Context, c *v1alpha1.Cluster) (map[string][]byte, error) {
	sec, err := s.secret(ctx, c)
	if err != nil || !isClusterSecret(sec) {
		return nil, err
	}
	return maps.Clone(sec.Data), nil
}

// clusterSecrets returns the data of every cluster credentials Secret by name.
func (s *Service) clusterSecrets(ctx context.Context) (map[string]map[string][]byte, error) {
	var list corev1.SecretList
	sel := labels.SelectorFromSet(labels.Set{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster})
	if err := s.d.Client.List(ctx, &list, client.InNamespace(s.d.Config.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	out := make(map[string]map[string][]byte, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = list.Items[i].Data
	}
	return out, nil
}

func ownerRef(c *v1alpha1.Cluster) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster", Name: c.Name, UID: c.UID}
}

// freeSecretName returns base when no Secret has that name, else base with a random
// suffix.
func (s *Service) freeSecretName(ctx context.Context, base string) (string, error) {
	name := base
	for range 5 {
		var sec corev1.Secret
		err := s.d.Client.Get(ctx, client.ObjectKey{Namespace: s.d.Config.Namespace, Name: name}, &sec)
		if apierrors.IsNotFound(err) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
		name = base + "-" + utilrand.String(5)
	}
	return "", rest.Conflict("could not find a free name for the credentials Secret of cluster %q", base)
}

func (s *Service) newSecret(name string, owner *v1alpha1.Cluster, data map[string][]byte) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: s.d.Config.Namespace,
			Labels:      map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster},
			Annotations: map[string]string{v1alpha1.AnnotationKeyManagedBy: v1alpha1.AnnotationValueManagedByPatrol},
		},
		Type: corev1.SecretTypeOpaque,
		Data: maps.Clone(data),
	}
	if owner != nil && owner.UID != "" {
		sec.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
	}
	return sec
}

// createObjects creates the Cluster object for d and its credentials Secret (owned by the
// Cluster). The object is named as upstream names cluster Secrets
// ("cluster-<host>-<hash>"), with a random suffix when that name is taken.
func (s *Service) createObjects(ctx context.Context, d *desired) (*v1alpha1.Cluster, error) {
	base, err := clusterlib.ObjectNameForServer(d.spec.Server)
	if err != nil {
		return nil, rest.InvalidArgument("%v", err)
	}
	obj := &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: base, Namespace: s.d.Config.Namespace, Labels: maps.Clone(d.labels), Annotations: maps.Clone(d.annotations)},
		Spec:       *d.spec.DeepCopy(),
	}
	obj.Spec.Config.CredentialsRef = nil
	var secretName string
	if len(d.creds) > 0 {
		if secretName, err = s.freeSecretName(ctx, base); err != nil {
			return nil, err
		}
		obj.Spec.Config.CredentialsRef = &v1alpha1.LocalSecretReference{Name: secretName}
	}
	for attempt := 0; ; attempt++ {
		err = s.d.Client.Create(ctx, obj)
		if err == nil || !apierrors.IsAlreadyExists(err) || attempt >= 3 {
			break
		}
		obj.Name = base + "-" + utilrand.String(5)
	}
	if err != nil {
		return nil, err
	}
	if secretName == "" {
		return obj, nil
	}
	if err := s.d.Client.Create(ctx, s.newSecret(secretName, obj, d.creds)); err != nil {
		// Roll back: a Cluster pointing to a missing Secret would be unusable.
		if derr := s.d.Client.Delete(context.WithoutCancel(ctx), obj, client.Preconditions{UID: ptr.To(obj.UID)}); derr != nil && !apierrors.IsNotFound(derr) {
			s.d.Logger.Warn("failed to roll back the Cluster after its credentials Secret could not be created", "cluster", obj.Name, "error", derr)
		}
		return nil, err
	}
	return obj, nil
}

// writeCredentials stores data as the credentials of c: the referenced cluster Secret is
// replaced (only recognized keys, so unknown keys are dropped, as upstream) and marked as
// managed by Patrol and owned by c; a cluster without credentialsRef gets a new Secret.
// A referenced Secret that is not labeled as cluster credentials is never modified.
func (s *Service) writeCredentials(ctx context.Context, c *v1alpha1.Cluster, data map[string][]byte) error {
	ns := s.d.Config.Namespace
	if ref := c.Spec.Config.CredentialsRef; ref != nil && ref.Name != "" {
		return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var sec corev1.Secret
			err := s.d.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &sec)
			if apierrors.IsNotFound(err) {
				if len(data) == 0 {
					return nil
				}
				return s.d.Client.Create(ctx, s.newSecret(ref.Name, c, data))
			}
			if err != nil {
				return err
			}
			if !isClusterSecret(&sec) {
				return rest.FailedPrecondition("the credentials Secret %q of cluster %q is not labeled %s=%s; refusing to modify it",
					ref.Name, c.Spec.Server, v1alpha1.LabelKeySecretType, v1alpha1.LabelValueSecretTypeCluster)
			}
			sec.Data = maps.Clone(data)
			if sec.Annotations == nil {
				sec.Annotations = map[string]string{}
			}
			sec.Annotations[v1alpha1.AnnotationKeyManagedBy] = v1alpha1.AnnotationValueManagedByPatrol
			if c.UID != "" && !slices.ContainsFunc(sec.OwnerReferences, func(o metav1.OwnerReference) bool { return o.UID == c.UID }) {
				sec.OwnerReferences = append(sec.OwnerReferences, ownerRef(c))
			}
			return s.d.Client.Update(ctx, &sec)
		})
	}
	if len(data) == 0 {
		return nil
	}
	name, err := s.freeSecretName(ctx, c.Name)
	if err != nil {
		return err
	}
	if err := s.d.Client.Create(ctx, s.newSecret(name, c, data)); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur v1alpha1.Cluster
		if err := s.d.Client.Get(ctx, client.ObjectKeyFromObject(c), &cur); err != nil {
			return err
		}
		cur.Spec.Config.CredentialsRef = &v1alpha1.LocalSecretReference{Name: name}
		if err := s.d.Client.Update(ctx, &cur); err != nil {
			return err
		}
		c.Spec.Config.CredentialsRef = cur.Spec.Config.CredentialsRef
		c.ResourceVersion = cur.ResourceVersion
		return nil
	})
}

// checkProject requires a project-scoped cluster's project to exist (scoping a cluster to a
// project widens that project's destinations, so a typo must not silently scope it to a
// project created later).
func (s *Service) checkProject(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	var p v1alpha1.AppProject
	if err := s.d.Client.Get(ctx, client.ObjectKey{Namespace: s.d.Config.Namespace, Name: name}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return rest.InvalidArgument("project %q does not exist", name)
		}
		return err
	}
	return nil
}

// recordProbe stores a successful connection check; failures are logged (the cluster is
// already saved and the controller refreshes the status).
func (s *Service) recordProbe(ctx context.Context, c *v1alpha1.Cluster, version string) {
	if s.d.Status == nil || c == nil || c.Name == "" {
		return
	}
	if _, err := s.d.Status.RecordProbe(ctx, c, version, nil); err != nil {
		s.d.Logger.Warn("failed to record the cluster connection state", "cluster", c.Name, "error", err)
	}
}

// Sanitize converts a cluster for API responses: execProviderConfig, managed fields and the
// last-applied annotation are removed and credentials are reduced to presence flags.
func Sanitize(c *v1alpha1.Cluster, creds map[string][]byte, inCluster bool) apiclient.RegisteredCluster {
	cp := c.DeepCopy()
	cp.ManagedFields = nil
	delete(cp.Annotations, lastAppliedAnnotation)
	hasExec := cp.Spec.Config.ExecProviderConfig != nil
	cp.Spec.Config.ExecProviderConfig = nil
	cr := clusterlib.CredentialsFromSecretData(creds)
	return apiclient.RegisteredCluster{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Cluster"},
		Metadata: cp.ObjectMeta,
		Spec:     cp.Spec,
		Status:   cp.Status,
		Credentials: apiclient.ClusterCredentialsInfo{
			HasBearerToken:        cr.BearerToken != "",
			HasBasicAuth:          cr.Username != "" || cr.Password != "",
			HasCAData:             len(cr.CAData) > 0,
			HasClientCertificate:  cr.HasCertAuth(),
			HasExecProviderConfig: hasExec,
		},
		InCluster: inCluster,
	}
}

// toAPI converts an entry, reading its credential flags.
func (s *Service) toAPI(ctx context.Context, e entry) (*apiclient.RegisteredCluster, error) {
	var creds map[string][]byte
	if !e.synthetic {
		var err error
		if creds, err = s.credentials(ctx, e.c); err != nil {
			return nil, err
		}
	}
	out := Sanitize(e.c, creds, e.synthetic)
	return &out, nil
}

// encodeContinue and decodeContinue implement the opaque list continuation token (the
// last returned key).
func encodeContinue(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

func decodeContinue(tok string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) == 0 {
		return "", rest.InvalidArgument("invalid continue token")
	}
	return string(b), nil
}

// paginate returns the items after the continue key, at most limit of them (0: all), and
// the continue token of the next page.
func paginate[T any](items []T, key func(T) string, limit int64, cont string) ([]T, string, error) {
	if limit < 0 {
		return nil, "", rest.InvalidArgument("limit must not be negative")
	}
	start := 0
	if cont != "" {
		after, err := decodeContinue(cont)
		if err != nil {
			return nil, "", err
		}
		start = len(items)
		for i, it := range items {
			if key(it) > after {
				start = i
				break
			}
		}
	}
	items = items[start:]
	if limit == 0 || int64(len(items)) <= limit {
		return items, "", nil
	}
	page := items[:limit]
	return page, encodeContinue(key(page[len(page)-1])), nil
}
