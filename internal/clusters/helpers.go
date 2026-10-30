package clusters

import (
	"fmt"
	"hash/fnv"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// IsSkipReconcile reports whether the cluster carries cd.patrol.example/skip-reconcile set to a
// strconv.ParseBool-true value; Applications targeting it are then not reconciled by any shard.
// An invalid value means not skipped, and a nil cluster (upstream: the in-cluster cluster) is
// never skipped.
func IsSkipReconcile(c *v1alpha1.Cluster) bool {
	if c == nil {
		return false
	}
	v, err := strconv.ParseBool(c.Annotations[v1alpha1.AnnotationKeySkipReconcile])
	return err == nil && v
}

// RefreshRequestedAt returns the time in the cd.patrol.example/refresh annotation (RFC 3339).
// ok is false when the annotation is absent or malformed (upstream logs and ignores it).
func RefreshRequestedAt(c *v1alpha1.Cluster) (t time.Time, ok bool, err error) {
	if c == nil {
		return time.Time{}, false, nil
	}
	v, found := c.Annotations[v1alpha1.AnnotationKeyRefresh]
	if !found {
		return time.Time{}, false, nil
	}
	t, err = time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse %s annotation of cluster %q: %w", v1alpha1.AnnotationKeyRefresh, c.Name, err)
	}
	return t, true, nil
}

// NeedsCacheInvalidation reports whether a refresh was requested after the live-state cache
// last synchronized (upstream: refreshRequestedAt after lastCacheSyncTime). A cache that never
// synchronized is not forced.
func NeedsCacheInvalidation(c *v1alpha1.Cluster, lastCacheSync *time.Time) bool {
	requested, ok, _ := RefreshRequestedAt(c)
	return ok && lastCacheSync != nil && lastCacheSync.Before(requested)
}

// CacheSettingsChange lists which cache-relevant settings differ between two versions of a
// cluster (upstream invalidates the live-state cache when any of them changes).
type CacheSettingsChange struct {
	// Config is true when spec.config differs. Credential Secret changes are reported by
	// the Registry as Event.Changes.Credentials.
	Config           bool
	Namespaces       bool
	ClusterResources bool
}

// Any reports whether anything changed.
func (c CacheSettingsChange) Any() bool { return c.Config || c.Namespaces || c.ClusterResources }

// DiffCacheSettings compares the cache-relevant settings of two clusters.
func DiffCacheSettings(oldC, newC *v1alpha1.Cluster) CacheSettingsChange {
	if oldC == nil || newC == nil {
		return CacheSettingsChange{Config: oldC != newC, Namespaces: oldC != newC, ClusterResources: oldC != newC}
	}
	return CacheSettingsChange{
		Config:           !reflect.DeepEqual(oldC.Spec.Config, newC.Spec.Config),
		Namespaces:       !reflect.DeepEqual(NormalizeNamespaces(oldC.Spec.Namespaces), NormalizeNamespaces(newC.Spec.Namespaces)),
		ClusterResources: oldC.Spec.ClusterResources != newC.Spec.ClusterResources,
	}
}

// NormalizeNamespaces trims entries and drops empty ones (upstream parses the comma-separated
// namespaces key the same way). It returns nil for an empty result.
func NormalizeNamespaces(ns []string) []string {
	var out []string
	for _, n := range ns {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ObjectNameForServer returns the name upstream gives API-created cluster Secrets,
// "cluster-<host>-<fnv32a(server)>": the host is lower-cased with the port removed, and an
// IPv6 host is unbracketed with ":" replaced by "-". `patrol cluster add` uses it for the
// Cluster object and its credentials Secret.
func ObjectNameForServer(server string) (string, error) {
	u, err := url.ParseRequestURI(server)
	if err != nil {
		return "", fmt.Errorf("invalid server URL: %w", err)
	}
	host := u.Host
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end >= 0 {
			addr, err := netip.ParseAddr(host[1:end])
			if err != nil {
				return "", fmt.Errorf("invalid server URL host: %w", err)
			}
			host = strings.ReplaceAll(addr.String(), ":", "-")
		}
	} else if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(server))
	return fmt.Sprintf("cluster-%s-%d", strings.ToLower(host), h.Sum32()), nil
}

// AutoLabelKubernetesVersion sets cd.patrol.example/kubernetes-version to version when the
// cluster opted in with cd.patrol.example/auto-label-cluster-info=true and the label differs.
// It reports whether the labels changed; the caller persists them.
func AutoLabelKubernetesVersion(c *v1alpha1.Cluster, version string) bool {
	if !WantsVersionLabel(c, version) {
		return false
	}
	c.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] = version
	return true
}

// WantsVersionLabel reports whether AutoLabelKubernetesVersion would change the labels of c.
func WantsVersionLabel(c *v1alpha1.Cluster, version string) bool {
	return c != nil && version != "" && c.Labels[v1alpha1.LabelKeyAutoLabelClusterInfo] == "true" &&
		c.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] != version
}

// ValidateRegistration rejects registering (creating) a cluster for the in-cluster server while
// cluster.inClusterEnabled is false, with upstream's message.
func ValidateRegistration(server string, inClusterEnabled bool) error {
	if IsInClusterServer(server) && !inClusterEnabled {
		return fmt.Errorf("cannot register cluster: %w", ErrInClusterDisabled)
	}
	return nil
}
