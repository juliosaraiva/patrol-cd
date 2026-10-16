package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	clustersvc "github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// coreClusterConnector is the clusters service Connector of core mode. The default
// Connector authenticates with the credentials stored in a Cluster's Secret (on a remote
// cluster, patrol's manager service account) or sent in the request; core mode uses
// neither. Connection checks (cluster add/set/upsert) and token rotation (rotate-auth)
// authenticate with the caller's own kubeconfig credentials for the cluster's server
// (CoreConfig.DestinationConfig), so the destination cluster sees, audits and authorizes
// the caller.
//
// Without such credentials nothing is sent to the cluster: Probe returns
// clustersvc.ErrExecNotAllowed, which the clusters service treats as "connection not
// checked by the API server" (the request proceeds, no connection state is recorded and
// the application controller checks the connection), and Kubernetes fails, so rotate-auth
// is refused before anything changes.
type coreClusterConnector struct {
	config  func(ctx context.Context, c *v1alpha1.Cluster) (*krest.Config, error)
	timeout time.Duration
}

var _ clustersvc.Connector = (*coreClusterConnector)(nil)

// restConfig returns the caller's configuration for the server of cl. Only the server
// selects it: the connection settings of the request or the stored object (TLS, proxy,
// exec plugin, AWS) are never applied to the caller's credentials.
func (c *coreClusterConnector) restConfig(ctx context.Context, cl *v1alpha1.Cluster) (*krest.Config, error) {
	if cl == nil || cl.Spec.Server == "" {
		return nil, errors.New("core mode: the cluster has no server")
	}
	cfg, err := c.config(ctx, &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: cl.Spec.Server}})
	if err != nil {
		return nil, err
	}
	cfg = krest.CopyConfig(cfg)
	cfg.Timeout = c.timeout
	return cfg, nil
}

// Probe checks the connection with the caller's credentials and returns the server
// version; the credentials argument (stored or from the request) is ignored.
func (c *coreClusterConnector) Probe(ctx context.Context, cl *v1alpha1.Cluster, _ map[string][]byte) (string, error) {
	cfg, err := c.restConfig(ctx, cl)
	if err != nil {
		return "", fmt.Errorf("%w (core mode checks cluster connections only with your own kubeconfig credentials: %v)", clustersvc.ErrExecNotAllowed, err)
	}
	pctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return clusters.Probe(pctx, cfg)
}

// Kubernetes returns a client for the cluster with the caller's credentials (rotate-auth);
// the credentials argument is ignored.
func (c *coreClusterConnector) Kubernetes(ctx context.Context, cl *v1alpha1.Cluster, _ map[string][]byte) (kubernetes.Interface, error) {
	cfg, err := c.restConfig(ctx, cl)
	if err != nil {
		return nil, fmt.Errorf("core mode changes a cluster only with your own kubeconfig credentials for it, never with the credentials stored for Patrol CD: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

// coreNamespaceScoped scopes List and Watch calls that name no namespace to the
// control-plane namespace. The ApplicationSet service lists across all namespaces and
// keeps the enabled ones; core mode enables no ApplicationSet namespace besides the
// control plane's, so the result is the same, and a caller whose Kubernetes RBAC is
// limited to that namespace can list and watch.
type coreNamespaceScoped struct {
	client.WithWatch
	namespace string
}

func (c coreNamespaceScoped) scoped(opts []client.ListOption) []client.ListOption {
	if (&client.ListOptions{}).ApplyOptions(opts).Namespace != "" {
		return opts
	}
	return append(slices.Clone(opts), client.InNamespace(c.namespace))
}

// List implements client.Reader.
func (c coreNamespaceScoped) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.WithWatch.List(ctx, list, c.scoped(opts)...)
}

// Watch implements client.WithWatch.
func (c coreNamespaceScoped) Watch(ctx context.Context, list client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	return c.WithWatch.Watch(ctx, list, c.scoped(opts)...)
}
