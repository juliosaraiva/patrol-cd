package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

const testNS = "patrol"

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fixture is a started Lister and Resolver over fake clientsets.
type fixture struct {
	dyn      *dynamicfake.FakeDynamicClient
	kube     *kubefake.Clientset
	lister   *Lister
	resolver *Resolver
	clock    *fakeClock
	logs     *syncBuffer
	reg      *prometheus.Registry
}

type fixtureSettings struct {
	opts  Options
	clock *fakeClock
	// strictEgress keeps the default token endpoint policy (no loopback). Fixtures allow
	// loopback otherwise because every fake issuer is an httptest server on 127.0.0.1.
	strictEgress bool
}

type fixtureOption func(*fixtureSettings)

func toUnstructured(t testing.TB, obj runtime.Object, kind string) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
	return u
}

func listKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		RepositoryGVR:           "RepositoryList",
		RepositoryCredentialGVR: "RepositoryCredentialList",
		SCMProviderGVR:          "SCMProviderList",
	}
}

// newFixture starts a Lister and Resolver over the given objects (Repositories,
// RepositoryCredentials, SCMProviders and Secrets).
func newFixture(t *testing.T, objs []runtime.Object, opts ...fixtureOption) *fixture {
	t.Helper()
	var dynObjs, kubeObjs []runtime.Object
	for _, o := range objs {
		switch v := o.(type) {
		case *v1alpha1.Repository:
			v.Namespace = testNS
			dynObjs = append(dynObjs, toUnstructured(t, v, "Repository"))
		case *v1alpha1.RepositoryCredential:
			v.Namespace = testNS
			dynObjs = append(dynObjs, toUnstructured(t, v, "RepositoryCredential"))
		case *v1alpha1.SCMProvider:
			v.Namespace = testNS
			dynObjs = append(dynObjs, toUnstructured(t, v, "SCMProvider"))
		case *corev1.Secret:
			if v.Namespace == "" {
				v.Namespace = testNS
			}
			kubeObjs = append(kubeObjs, v)
		default:
			t.Fatalf("unsupported fixture object %T", o)
		}
	}
	settings := &fixtureSettings{}
	for _, fn := range opts {
		fn(settings)
	}
	if settings.clock == nil {
		settings.clock = newFakeClock(time.Now())
	}
	f := &fixture{
		dyn:   dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds(), dynObjs...),
		kube:  kubefake.NewClientset(kubeObjs...),
		clock: settings.clock,
		logs:  &syncBuffer{},
		reg:   prometheus.NewRegistry(),
	}
	logger := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l, err := NewLister(f.dyn, f.kube, ListerOptions{Namespace: testNS, Logger: logger})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, l.Start(ctx))
	o := settings.opts
	o.Logger, o.Registerer, o.Now = logger, f.reg, f.clock.Now
	if !settings.strictEgress {
		o.AllowLoopbackTokenEndpoints = true
	}
	if o.LookupEnv == nil {
		o.LookupEnv = func(string) (string, bool) { return "", false }
	}
	r, err := NewResolver(l, o)
	require.NoError(t, err)
	f.lister, f.resolver = l, r
	return f
}

func withClock(c *fakeClock) fixtureOption {
	return func(s *fixtureSettings) { s.clock = c }
}

func withOptions(fn func(*Options)) fixtureOption {
	return func(s *fixtureSettings) { fn(&s.opts) }
}

func withTransport(rt http.RoundTripper) fixtureOption {
	return withOptions(func(o *Options) { o.Transport = rt })
}

func withAzureWI(p AzureTokenProvider) fixtureOption {
	return withOptions(func(o *Options) { o.AzureWorkloadIdentity = p })
}

// withStrictEgress keeps the default endpoint policy (loopback refused).
func withStrictEgress() fixtureOption {
	return func(s *fixtureSettings) { s.strictEgress = true }
}

// withLoopbackIssuers allows 127.0.0.1 as a Google token host, Microsoft Entra authority and
// Azure Container Registry host (the fake issuers are httptest servers).
func withLoopbackIssuers() fixtureOption {
	return withOptions(func(o *Options) {
		o.GoogleTokenHosts = []string{"127.0.0.1"}
		o.AzureAuthorityHosts = []string{"127.0.0.1"}
		o.AzureContainerRegistryHosts = []string{"127.0.0.1"}
	})
}

func withRepoServer(c apiclient.Client) fixtureOption {
	return withOptions(func(o *Options) { o.RepoServer = c })
}

// fakeRepoServer implements apiclient.Client.TestRepository (other methods panic).
type fakeRepoServer struct {
	apiclient.Client
	mu    sync.Mutex
	calls []apiclient.Repository
	fn    func(apiclient.Repository) (*apiclient.TestRepositoryResponse, error)
}

func (f *fakeRepoServer) TestRepository(_ context.Context, req *apiclient.TestRepositoryRequest) (*apiclient.TestRepositoryResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Repo)
	f.mu.Unlock()
	if f.fn == nil {
		return &apiclient.TestRepositoryResponse{VerifiedRepository: true}, nil
	}
	return f.fn(req.Repo)
}

func (f *fakeRepoServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRepoServer) lastCall(t *testing.T) apiclient.Repository {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.calls)
	return f.calls[len(f.calls)-1]
}

// Object builders.

func repo(name, url string, mut ...func(*v1alpha1.Repository)) *v1alpha1.Repository {
	r := &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, UID: repoUID(name)},
		Spec:       v1alpha1.RepositorySpec{URL: url, Type: v1alpha1.RepositoryTypeGit, Purpose: v1alpha1.RepositoryPurposeRead},
	}
	for _, m := range mut {
		m(r)
	}
	return r
}

func cred(name, prefix string, mut ...func(*v1alpha1.RepositoryCredential)) *v1alpha1.RepositoryCredential {
	c := &v1alpha1.RepositoryCredential{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec:       v1alpha1.RepositoryCredentialSpec{URLPrefix: prefix, Type: v1alpha1.RepositoryTypeGit, Purpose: v1alpha1.RepositoryPurposeRead},
	}
	for _, m := range mut {
		m(c)
	}
	return c
}

func secret(name, secretType string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{v1alpha1.LabelKeySecretType: secretType}},
		Data:       map[string][]byte{},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

// repoUID is the UID fixtures give the Repository named name.
func repoUID(name string) types.UID { return types.UID("uid-repository-" + name) }

// ownedBy binds a Secret to the fixture Repositories named owners through ownerReferences
// (a Secret referenced by a project-scoped Repository must be bound to its project).
func ownedBy(s *corev1.Secret, owners ...string) *corev1.Secret {
	for _, o := range owners {
		s.OwnerReferences = append(s.OwnerReferences, metav1.OwnerReference{
			APIVersion: v1alpha1.GroupVersion.String(), Kind: "Repository", Name: o, UID: repoUID(o),
		})
	}
	return s
}

func withProject(p string) func(*v1alpha1.Repository) {
	return func(r *v1alpha1.Repository) { r.Spec.Project = p }
}

func withCreds(secretName string) func(*v1alpha1.Repository) {
	return func(r *v1alpha1.Repository) {
		r.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: secretName}
	}
}

func withPurpose(p v1alpha1.RepositoryPurpose) func(*v1alpha1.Repository) {
	return func(r *v1alpha1.Repository) { r.Spec.Purpose = p }
}

func withType(typ v1alpha1.RepositoryType) func(*v1alpha1.Repository) {
	return func(r *v1alpha1.Repository) { r.Spec.Type = typ }
}

func credWithCreds(secretName string) func(*v1alpha1.RepositoryCredential) {
	return func(c *v1alpha1.RepositoryCredential) {
		c.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: secretName}
	}
}

func credWithPurpose(p v1alpha1.RepositoryPurpose) func(*v1alpha1.RepositoryCredential) {
	return func(c *v1alpha1.RepositoryCredential) { c.Spec.Purpose = p }
}

func credWithType(typ v1alpha1.RepositoryType) func(*v1alpha1.RepositoryCredential) {
	return func(c *v1alpha1.RepositoryCredential) { c.Spec.Type = typ }
}

func githubApp(appID int64, installationID int64, baseURL string) *v1alpha1.RepositoryGitHubApp {
	app := &v1alpha1.RepositoryGitHubApp{AppID: appID, EnterpriseBaseURL: baseURL}
	if installationID != 0 {
		app.InstallationID = ptr.To(installationID)
	}
	return app
}

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
	testKeyPEM  string
)

// rsaKey returns a shared RSA key (PKCS#1 PEM) for GitHub App tests.
func rsaKey(t testing.TB) (*rsa.PrivateKey, string) {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
		testKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	})
	return testKey, testKeyPEM
}

// trustTransport returns a transport trusting the certificates of the given TLS test servers.
func trustTransport(servers ...*httptest.Server) *http.Transport {
	pool := x509.NewCertPool()
	for _, s := range servers {
		pool.AddCert(s.Certificate())
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return tr
}

// counterValue reads a counter of the fixture registry.
func (f *fixture) counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := f.reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metric
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func metaName(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNS}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
