package stack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	krand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	notifcontroller "github.com/juliosaraiva/patrol-cd/internal/notifications/controller"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	"github.com/juliosaraiva/patrol-cd/internal/server"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
	fakescm "github.com/juliosaraiva/patrol-cd/pkg/scm/fake"
)

// Component names (Stack.Restart, Stop, Start).
const (
	RepoServer              = "repo-server"
	Controller              = "controller"
	Server                  = "server"
	AppSetController        = "appset-controller"
	NotificationsController = "notifications-controller"
)

// Options configure a Stack.
type Options struct {
	// DedicatedDestinations adds destination clusters only this Stack uses ("dest-2", ...),
	// registered with ServiceAccount bearer tokens. Their kube-apiserver can be stopped and
	// started (Cluster.Stop/Start).
	DedicatedDestinations int
	// UnregisteredDedicated starts the dedicated destinations without registering them
	// (tests register them through the API or the CLI with Cluster.Credentials).
	UnregisteredDedicated bool
	// ControllerParams are patrol-cmd-params-cm values of the application controller (for
	// example "controller.repo.error.grace.period.seconds": "0").
	ControllerParams map[string]string
	// Settings are additional patrol-cm entries (installationID and accounts are set by the
	// Stack).
	Settings map[string]string
	// Accounts are local accounts (name → password) with the login and apiKey capabilities.
	Accounts map[string]string
	// RBACPolicy is patrol-rbac-cm policy.csv; RBACDefault is policy.default.
	RBACPolicy, RBACDefault string
	// AllowedErrors are regular expressions of ERROR log records the test expects.
	AllowedErrors []string
	// NoAppSetController and NoNotificationsController leave those components out.
	NoAppSetController, NoNotificationsController bool
	// RepoServerEnv are PATROL_* tuning variables of the repo-server (PATROL_EXEC_TIMEOUT, ...).
	RepoServerEnv map[string]string
	// Plugins configures a Config Management Plugin socket directory for the repo-server;
	// Stack.StartPlugin serves plugins (internal/cmp) in-process on it.
	Plugins bool
	// AppSetParams are ApplicationSet controller settings (policy, SCM allow-lists, ...).
	AppSetParams AppSetParams
	// ControllerReplicas runs that many application controller replicas (sharding by
	// cluster with Lease ownership; 0 or 1: one replica without sharding).
	ControllerReplicas int
	// ApplicationNamespaces enables Applications in other namespaces (application.namespaces)
	// for the controller, patrol-server and the notifications controller.
	ApplicationNamespaces []string
	// Server, Notifications and Controller adjust a component's configuration after the
	// harness built it (feature flags the Options do not cover).
	Server        func(*server.Config)
	Notifications func(*notifcontroller.Config)
	Controller    func(*server.ControllerConfig)
}

// Stack is one Patrol CD installation running in-process.
type Stack struct {
	T testing.TB
	// ID is the random suffix of every name of this Stack.
	ID string
	// Namespace is the control-plane namespace.
	Namespace string
	// InstallationID is patrol-cm installationID (keeps Stacks sharing a destination apart).
	InstallationID string
	// AdminPassword is the local admin password.
	AdminPassword string
	// InternalToken authenticates internal calls (repo-server, tree API).
	InternalToken string
	// WebhookSecret is patrol-secret webhook.github.secret (GitHubPush signs with it).
	WebhookSecret string

	opts    Options
	control *Cluster
	dests   []*Cluster
	git     *GitServer
	logs    *logSink
	ctx     context.Context

	serverAddr, serverMetrics      string
	repoAddr, repoProbes, repoRoot string
	treeAddr, controllerMetrics    string
	appsetMetrics, notifMetrics    string
	// pluginDir is the CMP socket directory (Options.Plugins).
	pluginDir string
	// extraControllers are the metrics and tree API addresses of controller replicas 1..n-1.
	extraControllers []controllerAddrs

	mu         sync.Mutex
	comps      map[string]*component
	gcs        map[*Cluster]*fakeGC
	admin      *apiclient.Client
	controller *settings.Params
	// controllerFaults injects failures into the controller's control-plane requests.
	controllerFaults faultInjector
	// scm is the fake SCM provider registered as type "fake" (AppSetParams.FakeSCM).
	scm *fakescm.Provider
}

// controllerAddrs are the listeners of one application controller replica.
type controllerAddrs struct{ metrics, tree string }

// ControllerReplica returns the component name of application controller replica i (0 is
// Controller).
func ControllerReplica(i int) string {
	if i == 0 {
		return Controller
	}
	return fmt.Sprintf("%s-%d", Controller, i)
}

// component is one control-plane process running as goroutines.
type component struct {
	name   string
	run    func(ctx context.Context) error
	ready  string // probe URL answering 200 when ready
	cancel context.CancelFunc
	done   chan error
}

// Start starts a Stack and stops it at the end of the test. It skips the test unless
// integration tests are enabled (Enabled).
func Start(t *testing.T, opts Options) *Stack {
	t.Helper()
	if !Enabled() {
		t.Skipf("integration tests are disabled: set %s=1 or %s", EnvIntegration, EnvAssets)
	}
	env, err := shared()
	if err != nil {
		t.Fatalf("start the shared clusters: %v", err)
	}
	id := krand.String(6)
	s := &Stack{
		T: t, ID: id, Namespace: "patrol-" + id, InstallationID: "it-" + id,
		AdminPassword: "admin-" + krand.String(12), InternalToken: randomHex(t, 24), WebhookSecret: randomHex(t, 16),
		opts: opts, control: env.control, logs: &logSink{},
		comps: map[string]*component{}, gcs: map[*Cluster]*fakeGC{},
	}
	if opts.AppSetParams.SCMProviders {
		s.scm = fakescm.New(fakescm.Options{Name: "integration"})
	}
	if err := s.logs.allow(defaultAllowedErrors...); err != nil {
		t.Fatal(err)
	}
	if err := s.logs.allow(opts.AllowedErrors...); err != nil {
		t.Fatal(err)
	}
	// Registered first, so it runs last: after every component stopped.
	t.Cleanup(func() { s.logs.finish(t) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.ctx = ctx

	s.dests = []*Cluster{env.control, env.dest}
	assets, err := assetsDir()
	if err != nil {
		t.Fatal(err)
	}
	for i := range opts.DedicatedDestinations {
		name := fmt.Sprintf("dest-%d", i+2)
		c, err := startEnvtest(assets, envtestOptions{name: name, auth: AuthBearerToken, certDir: t.TempDir()})
		if err != nil {
			t.Fatalf("start destination %s: %v", name, err)
		}
		c.dedicated = true
		t.Cleanup(func() {
			if err := c.stopEnv(); err != nil {
				t.Errorf("stop destination %s: %v", name, err)
			}
		})
		s.dests = append(s.dests, c)
	}

	s.git = newGitServer(t, "git-"+krand.String(16))
	for _, a := range []*string{&s.serverAddr, &s.serverMetrics, &s.repoAddr, &s.repoProbes, &s.treeAddr, &s.controllerMetrics, &s.appsetMetrics, &s.notifMetrics} {
		*a = freeAddr(t)
	}
	s.repoRoot = filepath.Join(t.TempDir(), "repo-server")
	t.Cleanup(func() { _ = makeRemovable(s.repoRoot) })
	for i := 1; i < opts.ControllerReplicas; i++ {
		s.extraControllers = append(s.extraControllers, controllerAddrs{metrics: freeAddr(t), tree: freeAddr(t)})
	}
	if opts.Plugins {
		// Unix socket paths are limited to 104 bytes on macOS: t.TempDir() is too deep.
		dir, err := os.MkdirTemp("", "pcmp")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		s.pluginDir = dir
	}

	s.bootstrap(t)
	s.defineComponents(t)
	order := []string{RepoServer}
	for i := 0; i < max(1, opts.ControllerReplicas); i++ {
		order = append(order, ControllerReplica(i))
	}
	order = append(order, Server, AppSetController, NotificationsController)
	for _, name := range order {
		if c := s.comps[name]; c != nil {
			s.startComponent(t, c)
		}
	}
	for _, name := range order {
		if c := s.comps[name]; c != nil {
			s.waitReady(t, c)
		}
	}
	return s
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// handedOut holds every address freeAddr returned in this process. A port is free again as
// soon as its probe listener closes, so the kernel can hand it out twice before the component
// that was given it binds it (two components of one stack got the same metrics port on a CI
// runner); no address is returned twice.
var handedOut = struct {
	sync.Mutex
	addrs map[string]bool
}{addrs: map[string]bool{}}

// freeAddr returns a loopback address that was free a moment ago and that no other caller in
// this process was given. Components that restart keep their address, so clients and the tree
// API URL stay valid.
func freeAddr(t testing.TB) string {
	t.Helper()
	handedOut.Lock()
	defer handedOut.Unlock()
	for range 100 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		if !handedOut.addrs[addr] {
			handedOut.addrs[addr] = true
			return addr
		}
	}
	t.Fatal("freeAddr: no unused loopback port after 100 attempts")
	return ""
}

// makeRemovable restores owner permissions below dir (the repo-server makes its root
// traversable but not listable).
func makeRemovable(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil && p == dir {
				return nil
			}
			_ = os.Chmod(p, 0o700)
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

func partOf() map[string]string {
	return map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}
}

// bootstrap creates the control-plane namespace, the settings objects, the repository
// credentials of the git server and the destination clusters.
func (s *Stack) bootstrap(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	c := s.control.Client
	s.control.CreateNamespace(t, s.Namespace, nil)

	cm := map[string]string{
		settings.KeyInstallationID: s.InstallationID,
		// Explicit refreshes and live-state events drive the tests; the periodic refresh
		// is a safety net only.
		"timeout.reconciliation":        "120s",
		"timeout.reconciliation.jitter": "0s",
	}
	now := time.Now().UTC().Format(time.RFC3339)
	secret := map[string][]byte{
		settings.SecretKeyServerSecretKey:    []byte(randomHex(t, 32)),
		settings.SecretKeyAdminPassword:      []byte(hash(t, s.AdminPassword)),
		settings.SecretKeyAdminPasswordMtime: []byte(now),
		settings.SecretKeyWebhookGitHub:      []byte(s.WebhookSecret),
	}
	names := make([]string, 0, len(s.opts.Accounts))
	for name := range s.opts.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cm["accounts."+name] = "apiKey, login"
		secret["accounts."+name+".password"] = []byte(hash(t, s.opts.Accounts[name]))
		secret["accounts."+name+".passwordMtime"] = []byte(now)
	}
	for k, v := range s.opts.Settings {
		cm[k] = v
	}
	objs := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: s.Namespace, Labels: partOf()}, Data: cm},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Namespace: s.Namespace, Labels: partOf()}, Data: map[string]string{
			"policy.csv": s.opts.RBACPolicy, "policy.default": s.opts.RBACDefault,
		}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultCmdParamsConfigMapName, Namespace: s.Namespace, Labels: partOf()}, Data: s.controllerParams()},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: s.Namespace, Labels: partOf()}, Data: secret},
		// Credentials of every repository of the git server (credential template).
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "git-server-creds", Namespace: s.Namespace,
				Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeRepoCreds}},
			Data: map[string][]byte{
				v1alpha1.RepositorySecretKeyUsername: []byte(s.git.Username),
				v1alpha1.RepositorySecretKeyPassword: []byte(s.git.Password),
			},
		},
		&v1alpha1.RepositoryCredential{
			ObjectMeta: metav1.ObjectMeta{Name: "git-server", Namespace: s.Namespace},
			Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: s.git.URL + "/", Type: v1alpha1.RepositoryTypeGit,
				RepositoryAccess: v1alpha1.RepositoryAccess{CredentialsRef: &v1alpha1.LocalSecretReference{Name: "git-server-creds"}}},
		},
	}
	for _, d := range s.dests {
		if d.Auth == AuthInCluster {
			continue // synthetic in-cluster destination
		}
		if d.dedicated && s.opts.UnregisteredDedicated {
			continue
		}
		secretName := "cluster-" + d.Name
		objs = append(objs,
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: s.Namespace,
					Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}},
				Data: d.credentials,
			},
			&v1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: s.Namespace},
				Spec: v1alpha1.ClusterSpec{Server: d.Server, Name: d.Name,
					Config: v1alpha1.ClusterConfig{CredentialsRef: &v1alpha1.LocalSecretReference{Name: secretName}}},
			})
	}
	for _, o := range objs {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
	p, issues := settings.ResolveParams(settings.ComponentController, s.controllerParams(), func(string) (string, bool) { return "", false })
	if settings.HasErrors(issues) {
		t.Fatalf("controller parameters: %v", issues)
	}
	s.controller = p
}

// controllerParams are the patrol-cmd-params-cm entries (Options.ControllerParams plus
// application.namespaces).
func (s *Stack) controllerParams() map[string]string {
	out := map[string]string{}
	for k, v := range s.opts.ControllerParams {
		out[k] = v
	}
	if len(s.opts.ApplicationNamespaces) > 0 {
		out["application.namespaces"] = strings.Join(s.opts.ApplicationNamespaces, ",")
	}
	return out
}

func hash(t testing.TB, password string) string {
	t.Helper()
	h, err := auth.HashPasswordWithCost(password, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// level is the component log level (debug with PATROL_INTEGRATION_DEBUG=1).
func (s *Stack) level() slog.Level {
	if os.Getenv("PATROL_INTEGRATION_DEBUG") == "1" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// defineComponents builds the run functions of every component from the Stack's addresses.
func (s *Stack) defineComponents(t *testing.T) {
	t.Helper()
	level := s.level()
	repoClient := server.RepoServerClientOptions{Address: s.repoAddr, TLS: server.TLSClientOptions{}}
	kustomize := filepath.Join(RepoRoot(), "bin", "kustomize")
	if _, err := os.Stat(kustomize); err != nil {
		kustomize = "kustomize"
	}
	repoEnv := s.opts.RepoServerEnv
	s.comps[RepoServer] = &component{name: RepoServer, ready: "http://" + s.repoProbes + "/healthz?full=true", run: func(ctx context.Context) error {
		cfg := server.RepoServerConfig{Server: reposerver.Config{
			ListenAddress: s.repoAddr, MetricsAddress: s.repoProbes, Token: s.InternalToken, RootDir: s.repoRoot,
			SSHKnownHostsPath: reposerver.TrustMaterialDisabled, TLSCertsDir: reposerver.TrustMaterialDisabled,
			GPGKeysDir: reposerver.TrustMaterialDisabled, HelmPath: "helm", KustomizePath: kustomize, GitPath: "git",
			PluginSocketDir: s.pluginDir,
			Logger:          s.logs.logger(RepoServer, level),
		}, TLS: server.TLSServerOptions{MinVersion: "1.2", MaxVersion: "1.3"}}
		applyRepoServerEnv(&cfg.Server, repoEnv)
		return server.RunRepoServer(ctx, cfg)
	}}
	replicas := max(1, s.opts.ControllerReplicas)
	for i := range replicas {
		name := ControllerReplica(i)
		identity := fmt.Sprintf("patrol-controller-%d", i)
		addrs := controllerAddrs{metrics: s.controllerMetrics, tree: s.treeAddr}
		if i > 0 {
			addrs = s.extraControllers[i-1]
		}
		env := map[string]string{}
		if replicas > 1 {
			// Static sharding by cluster: one Lease per shard, the shortest heartbeat
			// (10 s, lease duration 30 s) so failover is observable within a test.
			env[sharding.EnvControllerReplicas] = strconv.Itoa(replicas)
			env[sharding.EnvHeartbeatTime] = "10"
		}
		s.comps[name] = &component{name: name, ready: "http://" + addrs.metrics + "/readyz", run: func(ctx context.Context) error {
			log := s.logs.logger(name, level)
			cfg := server.ControllerConfig{
				Namespace: s.Namespace, RestConfig: s.controllerFaults.wrap(s.control.Config), Params: s.controller, InternalToken: s.InternalToken,
				RepoServer: repoClient, MetricsAddress: addrs.metrics, TreeAPIAddress: addrs.tree,
				Sharding: sharding.ConfigFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, identity, log),
				Identity: identity, ShutdownTimeout: 10 * time.Second, Logger: log,
			}
			if s.opts.Controller != nil {
				s.opts.Controller(&cfg)
			}
			return server.RunController(ctx, cfg)
		}}
	}
	s.comps[Server] = &component{name: Server, ready: "http://" + s.serverMetrics + "/readyz", run: func(ctx context.Context) error {
		cfg := server.Config{
			Namespace: s.Namespace, RestConfig: s.control.Config,
			ListenAddress: s.serverAddr, MetricsAddress: s.serverMetrics, Insecure: true,
			InternalToken: s.InternalToken, RepoServer: repoClient,
			TreeAPI:       server.TreeAPIClientOptions{URL: "http://" + s.treeAddr, TLS: server.TLSClientOptions{Plaintext: true}},
			ExtensionsDir: s.ExtensionsDir(), DownloadDir: filepath.Join(s.repoRoot, "..", "download"),
			ApplicationNamespaces: s.opts.ApplicationNamespaces, ApplicationSetNamespaces: s.opts.AppSetParams.Namespaces,
			SyncWithReplaceAllowed: true, ShutdownTimeout: 10 * time.Second, Logger: s.logs.logger(Server, level),
		}
		if s.opts.Server != nil {
			s.opts.Server(&cfg)
		}
		return server.Run(ctx, cfg)
	}}
	if !s.opts.NoAppSetController {
		s.comps[AppSetController] = &component{name: AppSetController, ready: "http://" + s.appsetMetrics + "/readyz", run: func(ctx context.Context) error {
			rc, err := s.appSetRunConfig()
			if err != nil {
				return err
			}
			return server.RunAppSetController(ctx, server.AppSetConfig{
				Run: rc, InternalToken: s.InternalToken, RepoServer: repoClient,
				MetricsAddress: s.appsetMetrics, HealthzAddress: "-", ShutdownTimeout: 10 * time.Second,
				Logger: s.logs.logger(AppSetController, level),
			})
		}}
	}
	if !s.opts.NoNotificationsController {
		s.comps[NotificationsController] = &component{name: NotificationsController, ready: "http://" + s.notifMetrics + "/readyz", run: func(ctx context.Context) error {
			nc := notifcontroller.Config{RestConfig: s.control.Config, Namespace: s.Namespace, ApplicationNamespaces: s.opts.ApplicationNamespaces}
			if s.opts.Notifications != nil {
				s.opts.Notifications(&nc)
			}
			return server.RunNotificationsController(ctx, server.NotificationsConfig{
				Controller:    nc,
				InternalToken: s.InternalToken, RepoServer: repoClient,
				MetricsAddress: s.notifMetrics, ShutdownTimeout: 10 * time.Second,
				Logger: s.logs.logger(NotificationsController, level),
			})
		}}
	}
	t.Cleanup(func() { s.stopComponents(t) })
}

// stopComponents stops every running component in reverse dependency order: clients first,
// the repo-server last. Stopped components are skipped, so it can run more than once.
func (s *Stack) stopComponents(t testing.TB) {
	t.Helper()
	names := []string{NotificationsController, AppSetController, Server}
	for i := max(1, s.opts.ControllerReplicas) - 1; i >= 0; i-- {
		names = append(names, ControllerReplica(i))
	}
	for _, name := range append(names, RepoServer) {
		if c := s.comps[name]; c != nil {
			s.stopComponent(t, c)
		}
	}
}

// applyRepoServerEnv maps the repo-server tuning variables of Options.RepoServerEnv.
func applyRepoServerEnv(c *reposerver.Config, env map[string]string) {
	for k, v := range env {
		switch k {
		case "PATROL_EXEC_TIMEOUT":
			if d, err := time.ParseDuration(v); err == nil {
				c.ExecTimeout = d
			}
		case "PATROL_PAUSE_GEN_AFTER_FAILED_ATTEMPTS":
			var n int
			if _, err := fmt.Sscan(v, &n); err == nil {
				c.PauseGenerationAfterFailedAttempts = n
			}
		case "PATROL_GIT_ATTEMPTS_COUNT":
			var n int
			if _, err := fmt.Sscan(v, &n); err == nil {
				c.GitRetryAttempts = n
			}
		}
	}
}

func (s *Stack) startComponent(t testing.TB, c *component) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan error, 1)
	go func() { done <- c.run(ctx) }()
	c.cancel, c.done = cancel, done
}

// stopComponent cancels a component and waits for a clean return.
func (s *Stack) stopComponent(t testing.TB, c *component) {
	t.Helper()
	s.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel, c.done = nil, nil
	s.mu.Unlock()
	if done == nil {
		return
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%s did not shut down cleanly: %v", c.name, err)
		}
	case <-time.After(Timeout(60 * time.Second)):
		t.Errorf("%s did not stop within %s of cancellation", c.name, Timeout(60*time.Second))
	}
}

var probeClient = &http.Client{Timeout: 5 * time.Second}

// waitReady polls the component's readiness probe; a component that exits fails the test.
func (s *Stack) waitReady(t testing.TB, c *component) {
	t.Helper()
	s.mu.Lock()
	done := c.done
	s.mu.Unlock()
	Eventually(t, 2*time.Minute, c.name+" ready", func() (bool, string) {
		select {
		case err := <-done:
			// The component is gone: clear it so that the stop at cleanup does not wait for an
			// exit it can no longer receive.
			s.mu.Lock()
			if c.done == done {
				c.cancel()
				c.cancel, c.done = nil, nil
			}
			s.mu.Unlock()
			t.Fatalf("%s exited during startup: %v", c.name, err)
		default:
		}
		resp, err := probeClient.Get(c.ready)
		if err != nil {
			return false, err.Error()
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	})
}

func (s *Stack) comp(t testing.TB, name string) *component {
	t.Helper()
	c := s.comps[name]
	if c == nil {
		t.Fatalf("component %s is not part of this Stack", name)
	}
	return c
}

// Stop stops a component (Controller, RepoServer, ...).
func (s *Stack) Stop(t testing.TB, name string) {
	t.Helper()
	s.stopComponent(t, s.comp(t, name))
	t.Logf("%s stopped", name)
}

// StartComponent starts a stopped component and waits until it is ready.
func (s *Stack) StartComponent(t testing.TB, name string) {
	t.Helper()
	c := s.comp(t, name)
	s.startComponent(t, c)
	s.waitReady(t, c)
	t.Logf("%s started", name)
}

// StartComponentNoWait starts a stopped component without waiting for readiness (its
// dependencies may be down on purpose).
func (s *Stack) StartComponentNoWait(t testing.TB, name string) {
	t.Helper()
	s.startComponent(t, s.comp(t, name))
	t.Logf("%s starting", name)
}

// Restart stops and starts a component (a new process: every in-memory state is lost).
func (s *Stack) Restart(t testing.TB, name string) {
	t.Helper()
	s.Stop(t, name)
	s.StartComponent(t, name)
}

// RestartController restarts the application controller.
func (s *Stack) RestartController(t testing.TB) { s.Restart(t, Controller) }

// StopRepoServer stops the repo-server; StartRepoServer starts it again.
func (s *Stack) StopRepoServer(t testing.TB) { s.Stop(t, RepoServer) }

// StartRepoServer starts the repo-server and waits until it is ready.
func (s *Stack) StartRepoServer(t testing.TB) { s.StartComponent(t, RepoServer) }

// DropRepoServerCache restarts the repo-server with an empty root directory: its manifest,
// revision and working-copy caches are gone.
func (s *Stack) DropRepoServerCache(t testing.TB) {
	t.Helper()
	s.Stop(t, RepoServer)
	if err := makeRemovable(s.repoRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.repoRoot); err != nil {
		t.Fatalf("wipe the repo-server root: %v", err)
	}
	s.StartComponent(t, RepoServer)
}

// ExtensionsDir is patrol-server's UI extensions directory (/extensions.js, /extensions/*).
func (s *Stack) ExtensionsDir() string { return filepath.Join(s.repoRoot, "..", "extensions") }

// Control is the control-plane cluster (also the destination "in-cluster").
func (s *Stack) Control() *Cluster { return s.control }

// Destination returns destination n: 0 is "in-cluster" (the control cluster), 1 the shared
// "dest-1", 2.. the dedicated destinations of Options.DedicatedDestinations.
func (s *Stack) Destination(n int) *Cluster {
	if n < 0 || n >= len(s.dests) {
		s.T.Fatalf("destination %d does not exist (have %d)", n, len(s.dests))
	}
	return s.dests[n]
}

// Git is the Stack's git server (its credentials are registered as a credential template).
func (s *Stack) Git() *GitServer { return s.git }

// NewGitServer starts another git server whose credentials Patrol does not know (tests
// register its repositories through the API).
func (s *Stack) NewGitServer(t testing.TB) *GitServer {
	t.Helper()
	g := newGitServer(t, "git-"+krand.String(16))
	// Cleanups run last-in first-out, so the server's (registered just now, after Start's)
	// would close it while the components still fetch from it and log the failure as an
	// ERROR. Stop the components first (only at the end of the stack's own test: a subtest's
	// server outlives nothing).
	if t == s.T {
		t.Cleanup(func() { s.stopComponents(t) })
	}
	return g
}

// AllowErrors adds patterns to the ERROR log allow-list (from now until the end).
func (s *Stack) AllowErrors(patterns ...string) {
	if err := s.logs.allow(patterns...); err != nil {
		s.T.Fatal(err)
	}
}

// LogMark returns the current position in the component log (for LogsSince).
func (s *Stack) LogMark() int { return s.logs.Len() }

// LogsSince returns the component log written after mark.
func (s *Stack) LogsSince(mark int) string { return s.logs.Since(mark) }

// NewNamespace creates a namespace in a destination for this Stack (released by the fake
// garbage collector) and returns its name: <prefix>-<stack id>.
func (s *Stack) NewNamespace(t testing.TB, d *Cluster, prefix string) string {
	t.Helper()
	name := prefix + "-" + s.ID
	d.CreateNamespace(t, name, nil)
	s.WatchNamespace(d, name)
	return name
}

// WatchNamespace makes the fake garbage collector release deletions in a namespace of d
// (for namespaces Patrol creates).
func (s *Stack) WatchNamespace(d *Cluster, namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gcs[d]
	if g == nil {
		g = &fakeGC{c: d}
		s.gcs[d] = g
		go g.run(s.ctx)
	}
	g.watch(namespace)
}

// Ptr returns a pointer to v (spec helpers).
func Ptr[T any](v T) *T { return ptr.To(v) }
