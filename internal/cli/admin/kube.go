package admin

import (
	"errors"
	"fmt"
	"sync"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// userAgent identifies the admin commands to the Kubernetes API server.
const userAgent = "patrol-admin"

// Resources of the Patrol kinds and of the Kubernetes kinds the admin commands use.
var (
	configMapsGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secretsGVR    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

// patrolGVR returns the resource of a Patrol kind in group.
func patrolGVR(group, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: v1alpha1.Version, Resource: resource}
}

// kubeOptions are the kubectl client flags shared by the admin commands.
type kubeOptions struct {
	f            *Factory
	loadingRules *clientcmd.ClientConfigLoadingRules
	overrides    clientcmd.ConfigOverrides

	once    sync.Once
	clients *kubeClients
	err     error
}

func newKubeOptions(f *Factory) *kubeOptions {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.DefaultClientConfig = &clientcmd.DefaultClientConfig
	return &kubeOptions{f: f, loadingRules: rules}
}

// bind registers the kubectl client flags (upstream cli.AddKubectlFlagsToCmd).
func (k *kubeOptions) bind(fs *pflag.FlagSet) {
	fs.StringVar(&k.loadingRules.ExplicitPath, "kubeconfig", "", "Path to a kube config. Only required if out-of-cluster")
	clientcmd.BindOverrideFlags(&k.overrides, fs, clientcmd.RecommendedConfigOverrideFlags(""))
}

func (k *kubeOptions) clientConfig() clientcmd.ClientConfig {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(k.loadingRules, &k.overrides)
}

// namespaceExplicit reports whether -n/--namespace was given.
func (k *kubeOptions) namespaceExplicit() bool { return k.overrides.Context.Namespace != "" }

// restConfig returns the REST configuration and namespace of the control-plane cluster.
func (k *kubeOptions) restConfig() (*rest.Config, string, error) {
	if k.f != nil && k.f.RESTConfig != nil {
		cfg, ns, err := k.f.RESTConfig()
		if err != nil {
			return nil, "", err
		}
		if cfg == nil {
			return nil, "", errors.New("no Kubernetes configuration")
		}
		if k.namespaceExplicit() {
			ns = k.overrides.Context.Namespace
		}
		if ns == "" {
			ns = "default"
		}
		return rest.CopyConfig(cfg), ns, nil
	}
	cc := k.clientConfig()
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("could not create k8s client: %w", err)
	}
	ns, _, err := cc.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("could not determine the namespace: %w", err)
	}
	return cfg, ns, nil
}

// kubeClients are the Kubernetes clients of one command invocation.
type kubeClients struct {
	config    *rest.Config
	namespace string
	kube      kubernetes.Interface
	dynamic   dynamic.Interface
	ctrl      client.Client
	discovery discovery.DiscoveryInterface
}

// newScheme returns a scheme with the Kubernetes and Patrol kinds.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// get builds (once) the clients of the control-plane cluster. tune adjusts the REST
// configuration before the clients are created (QPS, burst).
func (k *kubeOptions) get(tune func(*rest.Config)) (*kubeClients, error) {
	k.once.Do(func() {
		cfg, ns, err := k.restConfig()
		if err != nil {
			k.err = err
			return
		}
		if cfg.UserAgent == "" {
			cfg.UserAgent = userAgent
		}
		// API warnings (deprecations, finalizer name advice) go to stderr once each, as
		// kubectl prints them.
		cfg.WarningHandler = rest.NewWarningWriter(k.f.errOut(), rest.WarningWriterOptions{Deduplicate: true})
		if tune != nil {
			tune(cfg)
		}
		k.clients, k.err = newKubeClients(cfg, ns)
	})
	return k.clients, k.err
}

func newKubeClients(cfg *rest.Config, ns string) (*kubeClients, error) {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("could not create k8s client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("could not create k8s dynamic client: %w", err)
	}
	scheme, err := newScheme()
	if err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("could not create k8s http client: %w", err)
	}
	mapper, err := lazyRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	ctrl, err := client.New(cfg, client.Options{Scheme: scheme, HTTPClient: httpClient, Mapper: mapper})
	if err != nil {
		return nil, fmt.Errorf("could not create k8s client: %w", err)
	}
	return &kubeClients{config: cfg, namespace: ns, kube: kube, dynamic: dyn, ctrl: ctrl, discovery: kube.Discovery()}, nil
}
