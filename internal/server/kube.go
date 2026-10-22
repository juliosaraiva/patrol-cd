package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	krest "k8s.io/client-go/rest"
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

// Defaults of the wait for the Patrol CRDs (waitForCRDs). The timeout stays below the time the
// chart's liveness probes need to kill a container that serves no health endpoint yet (the
// listeners start after the wait; the ApplicationSet and notifications controllers have no startup
// probe: first probe at 10 s, three failures 30 s apart), so a CRD that never appears ends in this
// error and not in a kill.
const (
	crdWaitTimeout  = time.Minute
	crdWaitInterval = 2 * time.Second
	// crdProbeTimeout bounds one discovery request.
	crdProbeTimeout = 10 * time.Second
)

// waitForCRDs blocks until the API server serves every kind of the Patrol API group, polling
// discovery every interval for at most timeout. A Helm release installs the CRDs together with the
// Deployments, and a large CRD (ApplicationSet) can take longer to be served than the pods take to
// start; without this wait the components exit with "no matches for kind", the kubelet restarts
// them, and a loaded cluster shows restarts that mean nothing is wrong. Errors from discovery
// (including an API server that cannot be reached yet) are retried until the timeout and
// returned with the last of them.
func waitForCRDs(ctx context.Context, cfg *krest.Config, log *slog.Logger, timeout, interval time.Duration) error {
	scheme, err := newScheme()
	if err != nil {
		return err
	}
	want := patrolResourceKinds(scheme)
	probe := krest.CopyConfig(cfg)
	probe.Timeout = crdProbeTimeout
	dc, err := discovery.NewDiscoveryClientForConfig(probe)
	if err != nil {
		return fmt.Errorf("discovery client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	waited := false
	for {
		missing, lastErr := missingPatrolKinds(ctx, dc, want)
		if lastErr == nil && len(missing) == 0 {
			if waited {
				log.Info("the Patrol CRDs are served", "waited", time.Since(start).Round(time.Millisecond).String())
			}
			return nil
		}
		if !waited {
			log.Info("waiting for the Patrol CRDs to be served", "group", v1alpha1.GroupVersion.String(), "missing", strings.Join(missing, ","), "error", fmt.Sprint(lastErr))
			waited = true
		}
		select {
		case <-ctx.Done():
			if cause := ctx.Err(); !errors.Is(cause, context.DeadlineExceeded) {
				return cause
			}
			msg := fmt.Sprintf("the Patrol CRDs (%s) are not served after %s: missing %s", v1alpha1.GroupVersion, timeout, strings.Join(missing, ", "))
			if lastErr != nil {
				return fmt.Errorf("%s: %w", msg, lastErr)
			}
			return errors.New(msg)
		case <-time.After(interval):
		}
	}
}

// patrolResourceKinds returns the kinds of the Patrol API group that are resources (the ones that
// come with a list kind).
func patrolResourceKinds(s *runtime.Scheme) []string {
	known := s.KnownTypes(v1alpha1.GroupVersion)
	var kinds []string
	for k := range known {
		if _, ok := known[k+"List"]; ok {
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)
	return kinds
}

// missingPatrolKinds lists the wanted kinds that discovery does not report. A group that is not
// served at all is a normal answer (all kinds missing, no error); any other failure is returned.
// It asks discovery's REST client with ctx, because ServerResourcesForGroupVersion takes none and
// a request in flight would outlive the wait's deadline and a stop signal.
func missingPatrolKinds(ctx context.Context, dc discovery.DiscoveryInterface, want []string) ([]string, error) {
	list := &metav1.APIResourceList{}
	err := dc.RESTClient().Get().AbsPath("/apis/" + v1alpha1.GroupVersion.String()).Do(ctx).Into(list)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return want, nil
		}
		return want, err
	}
	served := map[string]bool{}
	for _, r := range list.APIResources {
		served[r.Kind] = true
	}
	var missing []string
	for _, k := range want {
		if !served[k] {
			missing = append(missing, k)
		}
	}
	return missing, nil
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
