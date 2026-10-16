package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// newScheme returns the scheme of the control-plane clients (core types and Patrol CRDs).
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	return s, nil
}

// literalNamespaceRE matches Kubernetes namespace names (no glob or regexp syntax).
var literalNamespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// cacheNamespaces returns the namespaces an informer cache watches for objects that may
// live in the control-plane namespace or in namespaces matching patterns (globs or
// "/regexp/"): the literal namespaces when every pattern is a plain name (namespaced RBAC
// suffices), otherwise all namespaces (callers filter with the pattern matcher).
func cacheNamespaces(control string, patterns []string) map[string]crcache.Config {
	out := map[string]crcache.Config{control: {}}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !literalNamespaceRE.MatchString(p) {
			return map[string]crcache.Config{crcache.AllNamespaces: {}}
		}
		out[p] = crcache.Config{}
	}
	return out
}

// splitReader serves cached reads of the informer-backed types and reads everything else
// (Secrets, ConfigMaps, Events, ...) from the API server, so the server never starts
// cluster-wide informers of arbitrary types.
type splitReader struct {
	cached   client.Reader
	live     client.Reader
	isCached func(obj runtime.Object) bool
}

func (s *splitReader) pick(obj runtime.Object) client.Reader {
	if s.isCached(obj) {
		return s.cached
	}
	return s.live
}

// Get implements client.Reader.
func (s *splitReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return s.pick(obj).Get(ctx, key, obj, opts...)
}

// List implements client.Reader.
func (s *splitReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return s.pick(list).List(ctx, list, opts...)
}

// isPatrolCachedType reports the CRD types the server caches.
func isPatrolCachedType(obj runtime.Object) bool {
	switch obj.(type) {
	case *v1alpha1.Application, *v1alpha1.ApplicationList,
		*v1alpha1.ApplicationSet, *v1alpha1.ApplicationSetList,
		*v1alpha1.AppProject, *v1alpha1.AppProjectList,
		*v1alpha1.Cluster, *v1alpha1.ClusterList,
		*v1alpha1.Repository, *v1alpha1.RepositoryList,
		*v1alpha1.SCMProvider, *v1alpha1.SCMProviderList:
		return true
	}
	return false
}

// eventRecorder records Kubernetes Events for component from the control-plane identity.
type eventRecorder struct {
	broadcaster record.EventBroadcaster
	record.EventRecorder
}

func newEventRecorder(kube kubernetes.Interface, scheme *runtime.Scheme, component string) *eventRecorder {
	b := record.NewBroadcaster()
	b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	return &eventRecorder{broadcaster: b, EventRecorder: b.NewRecorder(scheme, corev1.EventSource{Component: component})}
}

// Shutdown flushes and stops the broadcaster.
func (r *eventRecorder) Shutdown() { r.broadcaster.Shutdown() }
