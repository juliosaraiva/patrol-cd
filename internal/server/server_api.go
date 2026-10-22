package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applicationsets"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/badge"
	clustersvc "github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/extensions"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/projects"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/repositories"
	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/auth/revocation"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/webhook"
)

// TreeAPIClientOptions configure patrol-server's client of the controller tree API.
type TreeAPIClientOptions struct {
	// URL, when set, sends every call to one controller (single-replica and local
	// development); otherwise calls are routed to the replica holding the Application's
	// shard through the headless Service.
	URL string
	// StatefulSet and Service name the controller workload and its headless Service
	// (defaults patrol-controller); Port is the tree API port (default 8090).
	StatefulSet, Service string
	Port                 int
	// TLS selects the transport (Plaintext by default; the internal token authenticates).
	TLS TLSClientOptions
}

// newTreeClient returns the tree API client with shard routing.
func newTreeClient(o TreeAPIClientOptions, token, namespace string, registry sharding.DestinationClusterLookup, holders treeapi.HolderResolver) (*treeapi.Client, error) {
	tlsCfg, err := clientTLSConfig(o.TLS)
	if err != nil {
		return nil, fmt.Errorf("controller tree API client TLS: %w", err)
	}
	var router treeapi.Router
	if o.URL != "" {
		router = treeapi.StaticRouter{URL: o.URL}
	} else {
		scheme := "https"
		if o.TLS.Plaintext {
			scheme = "http"
		}
		router = &treeapi.ShardRouter{
			Shards: sharding.StatusShardResolver{Clusters: registry}, Holders: holders,
			StatefulSet: o.StatefulSet, Service: o.Service, Namespace: namespace, Port: o.Port, Scheme: scheme,
		}
	}
	return treeapi.NewClient(router, token, treeapi.WithHTTPClient(&http.Client{
		Transport: internalTransport("controller-tree-api", tlsCfg), Timeout: treeapi.DefaultClientTimeout,
	}))
}

// repositoryResolverAdapter adapts *repository.Resolver to the value-returning resolver of
// the ApplicationSet generators.
func repositoryResolverAdapter(r *repository.Resolver) generators.RepositoryResolver {
	return generators.RepositoryResolverFunc(func(ctx context.Context, repoURL, proj string) (apiclient.Repository, error) {
		repo, err := r.GetRepository(ctx, repoURL, proj)
		if err != nil {
			return apiclient.Repository{}, err
		}
		if repo == nil {
			return apiclient.Repository{URL: repoURL, Project: proj}, nil
		}
		return *repo, nil
	})
}

// generatorsConfig is the generator configuration of the API server (generate route,
// create dry-run and webhook Matrix evaluation).
func (s *serverAssembly) generatorsConfig(repo apiclient.Client, resolver *repository.Resolver) generators.Config {
	a := s.cfg.AppSet
	return generators.Config{
		Client:    s.reader,
		Namespace: s.cfg.Namespace,
		Repos: &generators.RepoServerService{
			Client: repo, Repositories: repositoryResolverAdapter(resolver),
			SubmodulesEnabled: true, NewGitFileGlobbing: a.EnableNewGitFileGlobbing,
		},
		Dynamic: s.dyn,
		SCM: generators.SCMConfig{
			Enabled: a.EnableSCMProviders, AllowedProviders: a.AllowedSCMProviders, RootCAPath: a.SCMRootCAPath,
			TokenRefStrictMode: true, Logger: s.log,
		},
		Logger: s.log,
	}
}

// buildAPI assembles authentication, RBAC, the API services, the webhook handler and the
// apiserver.
func (s *serverAssembly) buildAPI(ctx context.Context, keys auth.KeySet) error {
	cfg := s.cfg
	log := s.log

	// RBAC follows patrol-rbac-cm (apiserver applies it) and the roles of every AppProject.
	s.enforcer = rbac.NewEnforcer(rbac.WithLogger(log))
	projInf, err := s.cache.GetInformer(ctx, &v1alpha1.AppProject{})
	if err != nil {
		return fmt.Errorf("project informer: %w", err)
	}
	if _, err := projInf.AddEventHandler(projectPolicyHandler{enforcer: s.enforcer, namespace: cfg.Namespace, log: log}); err != nil {
		return fmt.Errorf("follow project roles: %w", err)
	}

	// Authentication.
	revocations, err := revocation.NewSecretStore(s.kube, cfg.Namespace, revocation.WithLogger(log))
	if err != nil {
		return err
	}
	accounts := apiserver.AccountLookup(s.settings)
	dexURL, dexTransport, err := dexConnection(cfg)
	if err != nil {
		return err
	}
	sso, err := core.NewSSO(core.SSOConfig{DexServerURL: dexURL, DexTransport: dexTransport, Logger: log})
	if err != nil {
		return err
	}
	tokens, err := auth.NewTokenService(auth.TokenServiceConfig{
		Keys: keys, Accounts: accounts, Revocations: revocations,
		Projects: apiserver.ProjectTokenRegistryWithFallback(s.projectGetter(), s.liveProjectGetter()),
		External: sso.ExternalVerifier(s.settings),
	})
	if err != nil {
		return fmt.Errorf("token service: %w", err)
	}
	loginCfg, lerr := auth.LoginConfigFromEnv(os.LookupEnv)
	if lerr != nil {
		log.Warn("invalid login throttling variables; using defaults for them", "error", lerr)
	}
	login, err := auth.NewAuthenticator(accounts, loginCfg, auth.WithLoginLogger(log))
	if err != nil {
		return err
	}

	// Internal clients and lookups.
	repoClient, err := newRepoServerClient(cfg.RepoServer, cfg.InternalToken)
	if err != nil {
		return err
	}
	resolver, err := repository.NewResolver(s.lister, repository.Options{Logger: log, Registerer: s.registry, RepoServer: repoClient})
	if err != nil {
		return err
	}
	holders := sharding.NewLeaseHolders(sharding.NewLeaseClient(s.kube, cfg.Namespace), 0, nil)
	tree, err := newTreeClient(cfg.TreeAPI, cfg.InternalToken, cfg.Namespace, s.registryC, holders)
	if err != nil {
		return err
	}
	tuning := clusters.DefaultTuning()
	if cfg.ClusterTuning != nil {
		tuning = *cfg.ClusterTuning
	}
	clusterOpts := clusters.Options{Tuning: &tuning, FakeInCluster: clusters.FakeInClusterFromEnv()}

	// Application watch events.
	broadcaster := applications.NewBroadcaster(0)
	appInf, err := s.cache.GetInformer(ctx, &v1alpha1.Application{})
	if err != nil {
		return fmt.Errorf("application informer: %w", err)
	}
	if _, err := appInf.AddEventHandler(broadcaster); err != nil {
		return fmt.Errorf("watch applications: %w", err)
	}

	genCfg := s.generatorsConfig(repoClient, resolver)
	appCfg := applications.Config{
		Namespace:              cfg.Namespace,
		ApplicationNamespaces:  cfg.ApplicationNamespaces,
		DisableSyncWithReplace: !cfg.SyncWithReplaceAllowed,
		DisableGPG:             gpgDisabled(),
	}
	services := []apiserver.Service{
		applications.New(applications.Deps{
			Config: appCfg, Client: s.direct, Reader: s.reader, Settings: s.settings, Events: broadcaster,
			Repositories: resolver, RepoServer: repoClient,
			Resources: resourceDeps(tree, s.registryC, clusterOpts),
			Logger:    log,
		}),
		applicationsets.New(applicationsets.Deps{
			Config: applicationsets.Config{Namespace: cfg.Namespace, Namespaces: cfg.ApplicationSetNamespaces},
			Reader: s.reader, APIReader: s.direct, Writer: s.direct, Watcher: s.direct,
			Generator: applicationsets.NewGenerator(genCfg), Recorder: s.recorder, Logger: log,
		}),
		projects.New(projects.Deps{
			Config: projects.Config{Namespace: cfg.Namespace}, Client: s.direct, Settings: s.settings, Tokens: tokens,
			Events: s.kube.CoreV1().Events(cfg.Namespace), Clusters: s.registryC, Policies: s.enforcer, Logger: log,
		}),
		clustersvc.New(clustersvc.Deps{
			Config: clustersvc.Config{Namespace: cfg.Namespace}, Client: s.direct, Settings: s.settings,
			Connector: clustersvc.NewConnector(clustersvc.ConnectorOptions{Options: clusterOpts, AllowExec: cfg.AllowClusterExec}),
			Status:    clusters.NewStatusWriter(s.dyn), InCluster: s.registryC, Logger: log,
		}),
		repositories.New(repositories.Deps{
			Config: repositories.Config{Namespace: cfg.Namespace, HydratorEnabled: cfg.HydratorEnabled, ConnectionStateTTL: cfg.ConnectionStateTTL},
			Client: s.direct, Resolver: resolver, StatusWriter: repository.NewStatusWriter(s.dyn),
			RepoServer: repoClient, Settings: s.settings, Logger: log,
		}),
		notifications.New(notifications.Deps{Settings: s.settings, Logger: log}),
		badge.New(badge.Deps{
			Config:   badge.Config{Namespace: cfg.Namespace, ApplicationNamespaces: cfg.ApplicationNamespaces},
			Settings: s.settings, Reader: s.reader, Logger: log,
		}),
	}
	ext := extensions.New(extensions.Deps{
		Config: extensions.Config{
			Enabled: cfg.EnableProxyExtension, Namespace: cfg.Namespace, ApplicationNamespaces: cfg.ApplicationNamespaces,
		},
		Settings: s.settings, Reader: s.reader, Registry: s.registry, Logger: log,
	})

	wh, err := webhook.NewHandler(webhook.Config{
		Namespace: cfg.Namespace, ApplicationNamespaces: cfg.ApplicationNamespaces,
		ApplicationSetNamespaces: cfg.ApplicationSetNamespaces,
		Client:                   s.direct, Reader: s.reader, APIReader: s.direct,
		Settings:           webhook.CurrentSettings(s.settings),
		Workers:            cfg.WebhookParallelism,
		RefreshWorkers:     cfg.WebhookRefreshWorkers,
		SourceKey:          func(r *http.Request) string { return rest.ClientIP(r, cfg.TrustedProxies) },
		GeneratorEvaluator: appset.NewEvaluator(genCfg),
		ShutdownTimeout:    cfg.ShutdownTimeout,
		Registerer:         s.registry,
		Logger:             log,
	})
	if err != nil {
		return fmt.Errorf("webhook handler: %w", err)
	}
	s.webhook = wh
	services = append(services, &routesService{
		extensions: ext, webhook: wh, extensionsDir: cfg.ExtensionsDir, downloadDir: cfg.DownloadDir, log: log,
	})

	version := VersionInfo()
	version.Tools = core.ExecToolVersions("helm", "kustomize")
	apiCfg, err := s.apiConfig()
	if err != nil {
		return err
	}
	api, err := apiserver.New(apiCfg, apiserver.Deps{
		Settings: s.settings, Tokens: tokens, Login: login, Enforcer: s.enforcer,
		Core: core.Deps{
			Secrets: &core.KubeSecretStore{Client: s.kube, Namespace: cfg.Namespace},
			SSO:     sso, RepoServer: repoClient, Version: version,
		},
		Services:      services,
		EventRecorder: s.auditRecorder(),
		Registry:      s.registry,
		Gatherer:      s.registry,
		Ready:         s.ready,
		Logger:        log,
	})
	if err != nil {
		return err
	}
	s.api = api
	return nil
}

// gpgDisabled mirrors PATROL_GPG_ENABLED=false (upstream ARGOCD_GPG_ENABLED).
func gpgDisabled() bool { return os.Getenv("PATROL_GPG_ENABLED") == "false" }

// DefaultDexTLSDir is where the install manifests mount Secret patrol-dex-server-tls (ca.crt,
// tls.crt), read with --dex-server-strict-tls (upstream /app/config/dex/tls).
const DefaultDexTLSDir = "/app/config/dex/tls"

// errDexCertificateMismatch is upstream's error when Dex presents another certificate than
// the pinned one.
var errDexCertificateMismatch = errors.New("dex server certificate does not match")

// dexConnection returns the Dex address and transport (OPS-102): plaintext; unverified TLS
// (upstream default for the bundled Dex); or strict TLS, verified against the Dex CA and
// pinned to Dex's own certificate, both read at start from DexCAFile and DexCertFile.
func dexConnection(cfg Config) (string, http.RoundTripper, error) {
	if cfg.DexServer == "" {
		return "", nil, nil
	}
	addr := cfg.DexServer
	switch {
	case hasScheme(addr):
	case cfg.DexPlaintext:
		addr = "http://" + addr
	default:
		addr = "https://" + addr
	}
	if cfg.DexPlaintext {
		return addr, nil, nil
	}
	if !cfg.DexStrictTLS {
		rt, err := core.DexTransport(false, nil)
		if err != nil {
			return "", nil, fmt.Errorf("transport to Dex: %w", err)
		}
		return addr, rt, nil
	}
	rt, err := dexStrictTransport(orDefault(cfg.DexCAFile, filepath.Join(DefaultDexTLSDir, "ca.crt")),
		orDefault(cfg.DexCertFile, filepath.Join(DefaultDexTLSDir, "tls.crt")))
	if err != nil {
		return "", nil, fmt.Errorf("strict TLS to Dex (--dex-server-strict-tls): %w", err)
	}
	return addr, rt, nil
}

// dexStrictTransport verifies Dex's certificate chain against the CA bundle in caFile and
// requires the leaf to be byte-identical to the certificate in certFile (upstream pins the
// mounted tls.crt). Rotating Dex's certificate needs a restart, as upstream.
func dexStrictTransport(caFile, certFile string) (http.RoundTripper, error) {
	pool, err := certPool(caFile)
	if err != nil {
		return nil, fmt.Errorf("the Dex CA certificate is needed: %w", err)
	}
	pinned, err := readCertificateDER(certFile)
	if err != nil {
		return nil, fmt.Errorf("the pinned Dex certificate is needed: %w", err)
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unexpected default transport")
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		// Runs after the chain and host name were verified.
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || !bytes.Equal(raw[0], pinned) {
				return errDexCertificateMismatch
			}
			return nil
		},
	}
	return tr, nil
}

// readCertificateDER returns the DER bytes of the first certificate of a PEM file.
func readCertificateDER(path string) ([]byte, error) {
	data, err := readPEMFile(path)
	if err != nil {
		return nil, fmt.Errorf("read certificate: %w", err)
	}
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return nil, fmt.Errorf("%s contains no certificate", path)
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return nil, fmt.Errorf("parse certificate %s: %w", path, err)
		}
		return b.Bytes, nil
	}
}

func hasScheme(addr string) bool {
	for _, p := range []string{"http://", "https://"} {
		if len(addr) >= len(p) && addr[:len(p)] == p {
			return true
		}
	}
	return false
}

// projectPolicyHandler pushes the roles of every AppProject of the control-plane namespace
// to the RBAC enforcer (changes made with kubectl or by other replicas).
type projectPolicyHandler struct {
	enforcer  *rbac.Enforcer
	namespace string
	log       interface {
		Warn(msg string, args ...any)
	}
}

func (h projectPolicyHandler) set(obj any) {
	p, ok := obj.(*v1alpha1.AppProject)
	if !ok || p.Namespace != h.namespace {
		return
	}
	if err := h.enforcer.SetProjectPolicies(p.Name, project.RBACRoles(p)); err != nil {
		h.log.Warn("project role policies have problems; invalid lines are ignored", "project", p.Name, "error", err)
	}
}

// OnAdd implements cache.ResourceEventHandler.
func (h projectPolicyHandler) OnAdd(obj any, _ bool) { h.set(obj) }

// OnUpdate implements cache.ResourceEventHandler.
func (h projectPolicyHandler) OnUpdate(_, obj any) { h.set(obj) }

// OnDelete implements cache.ResourceEventHandler.
func (h projectPolicyHandler) OnDelete(obj any) {
	if d, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	if p, ok := obj.(*v1alpha1.AppProject); ok && p.Namespace == h.namespace {
		h.enforcer.DeleteProjectPolicies(p.Name)
	}
}

// bootstrapAttempts bounds the conflict retries of the startup normalization (upstream: 3).
const bootstrapAttempts = 3

// bootstrapObjects runs the startup tasks of the API server once the caches synced: create
// the default project when it is missing (TEN-002) and normalize project token metadata
// (TEN-034). Failures are logged; the server keeps serving (upstream exits on a token
// normalization failure).
func (s *serverAssembly) bootstrapObjects(ctx context.Context) {
	if err := ensureDefaultProject(ctx, s.direct, s.cfg.Namespace); err != nil {
		s.log.Error("cannot create the default project", "error", err)
	}
	if err := normalizeProjectTokens(ctx, s.direct, s.cfg.Namespace); err != nil {
		s.log.Error("cannot normalize project tokens", "error", err)
	}
}

// ensureDefaultProject creates AppProject "default" with the permissive upstream spec when it
// does not exist; an existing one is never changed.
func ensureDefaultProject(ctx context.Context, c client.Client, namespace string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var existing v1alpha1.AppProject
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: v1alpha1.DefaultAppProjectName}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	p := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAppProjectName, Namespace: namespace},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:              []string{"*"},
			Destinations:             []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
			ClusterResourceWhitelist: []v1alpha1.ClusterResourceRestrictionItem{{Group: "*", Kind: "*"}},
		},
	}
	if err := c.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// normalizeProjectTokens migrates and reconciles the token metadata of every AppProject of
// the control-plane namespace (project.NormalizeJWTTokens), writing spec and status.
func normalizeProjectTokens(ctx context.Context, c client.Client, namespace string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var list v1alpha1.AppProjectList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	backoff := retry.DefaultRetry
	backoff.Steps = bootstrapAttempts
	for i := range list.Items {
		key := client.ObjectKeyFromObject(&list.Items[i])
		err := retry.RetryOnConflict(backoff, func() error {
			var p v1alpha1.AppProject
			if err := c.Get(ctx, key, &p); err != nil {
				return err
			}
			if !project.NormalizeJWTTokens(&p) {
				return nil
			}
			status := p.Status.DeepCopy()
			if err := c.Update(ctx, &p); err != nil {
				return err
			}
			p.Status = *status
			return c.Status().Update(ctx, &p)
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("normalize project %s: %w", key.Name, err)
		}
	}
	return nil
}
