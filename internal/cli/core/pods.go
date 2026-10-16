package core

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/juliosaraiva/patrol-cd/internal/cli/transport"
	"github.com/juliosaraiva/patrol-cd/internal/server"
)

// DialPodFunc opens a connection to a pod port (transport.PodDialer.DialPod).
type DialPodFunc func(ctx context.Context, namespace, pod string, port int) (net.Conn, error)

// controllerPodTTL is how long a verified controller pod is trusted without a new check.
const controllerPodTTL = 30 * time.Second

// podRouter reaches the repo-server and controller pods of the Patrol namespace through
// Kubernetes port-forward with the caller's credentials. The repo-server pod is selected
// by label and cached; a failed dial selects again (pod restarts). Controller pods come
// from shard routing (the shard Lease holder, else <StatefulSet>-<shard>); before the
// internal token is sent to one, the pod must exist in the namespace, be Running and
// Ready, and either carry the controller name label or be owned by the controller
// StatefulSet.
type podRouter struct {
	kube           kubernetes.Interface
	namespace      string
	repoServerName string
	controllerName string
	controllerSts  string
	dial           DialPodFunc
	now            func() time.Time

	mu       sync.Mutex
	repoPod  string
	verified map[string]time.Time
}

func newPodRouter(kube kubernetes.Interface, namespace, repoServerName, controllerName, controllerSts string, dial DialPodFunc) *podRouter {
	return &podRouter{
		kube: kube, namespace: namespace, repoServerName: repoServerName, controllerName: controllerName,
		controllerSts: controllerSts, dial: dial, now: time.Now, verified: map[string]time.Time{},
	}
}

// Dial implements server.CoreConfig.Dial.
func (r *podRouter) Dial(ctx context.Context, t server.CoreDialTarget) (net.Conn, error) {
	switch t.Component {
	case server.CoreComponentRepoServer:
		return r.dialRepoServer(ctx, t.Port)
	case server.CoreComponentController:
		return r.dialController(ctx, t.Pod, t.Port)
	}
	return nil, fmt.Errorf("core mode: unknown component %q", t.Component)
}

func (r *podRouter) dialRepoServer(ctx context.Context, port int) (net.Conn, error) {
	var lastErr error
	for attempt := range 2 {
		pod, err := r.repoServerPod(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		c, err := r.dial(ctx, r.namespace, pod, port)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// repoServerPod returns the cached repo-server pod, or selects a ready one.
func (r *podRouter) repoServerPod(ctx context.Context, reselect bool) (string, error) {
	r.mu.Lock()
	cached := r.repoPod
	r.mu.Unlock()
	if cached != "" && !reselect {
		return cached, nil
	}
	selector := transport.LabelAppName + "=" + r.repoServerName
	pods, err := r.kube.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("list pods with selector %s in namespace %s: %w", selector, r.namespace, err)
	}
	ready := readyPods(pods.Items)
	if len(ready) == 0 {
		return "", fmt.Errorf("cannot find ready pod with selector: [%s] in namespace %s - use --repo-server-name (or PATROL_REPO_SERVER_NAME) and -n/--namespace (or the kube context namespace) to select the repo-server pods", selector, r.namespace)
	}
	pod := ready[0]
	if reselect && len(ready) > 1 && pod == cached {
		pod = ready[1]
	}
	r.mu.Lock()
	r.repoPod = pod
	r.mu.Unlock()
	return pod, nil
}

// readyPods returns the names of Running, Ready pods that are not being deleted, sorted.
func readyPods(items []corev1.Pod) []string {
	var out []string
	for i := range items {
		if podServing(&items[i]) {
			out = append(out, items[i].Name)
		}
	}
	slices.Sort(out)
	return out
}

func podServing(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *podRouter) dialController(ctx context.Context, pod string, port int) (net.Conn, error) {
	if err := r.verifyController(ctx, pod); err != nil {
		return nil, err
	}
	c, err := r.dial(ctx, r.namespace, pod, port)
	if err != nil {
		r.mu.Lock()
		delete(r.verified, pod)
		r.mu.Unlock()
		return nil, err
	}
	return c, nil
}

// verifyController checks that pod is a serving controller pod (cached briefly).
func (r *podRouter) verifyController(ctx context.Context, name string) error {
	r.mu.Lock()
	until, ok := r.verified[name]
	r.mu.Unlock()
	if ok && r.now().Before(until) {
		return nil
	}
	p, err := r.kube.CoreV1().Pods(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("controller pod %s/%s: %w", r.namespace, name, err)
	}
	if !r.isControllerPod(p) {
		return fmt.Errorf("core mode: pod %s/%s selected by shard routing is not a controller pod (label %s=%s or owner StatefulSet %s required); the internal token is not sent to it", r.namespace, name, transport.LabelAppName, r.controllerName, r.controllerSts)
	}
	if !podServing(p) {
		return fmt.Errorf("core mode: controller pod %s/%s is not ready", r.namespace, name)
	}
	r.mu.Lock()
	r.verified[name] = r.now().Add(controllerPodTTL)
	r.mu.Unlock()
	return nil
}

func (r *podRouter) isControllerPod(p *corev1.Pod) bool {
	if r.controllerName != "" && p.Labels[transport.LabelAppName] == r.controllerName {
		return true
	}
	for _, o := range p.OwnerReferences {
		if o.Kind == "StatefulSet" && o.APIVersion == "apps/v1" && o.Name == r.controllerSts && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}
