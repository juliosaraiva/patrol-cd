package generators

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const (
	controlNS = "patrol"
	appSetNS  = "team-a"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	// Build sets resource versions on the objects it is given, and parallel subtests share their
	// fixtures (the Secrets of TestSCMProviderBlocks): hand it copies.
	copies := make([]client.Object, len(objs))
	for i, o := range objs {
		copies[i] = o.DeepCopyObject().(client.Object)
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(copies...).Build()
}

func appSet(goTemplate bool, opts ...string) *v1alpha1.ApplicationSet {
	return &v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: appSetNS},
		Spec: v1alpha1.ApplicationSetSpec{
			GoTemplate:        goTemplate,
			GoTemplateOptions: opts,
			Template: v1alpha1.ApplicationSetTemplate{
				Spec: v1alpha1.ApplicationSpec{Project: "default"},
			},
		},
	}
}

func cluster(name, specName, server string, labels, annotations map[string]string) *v1alpha1.Cluster {
	return &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS, Labels: labels, Annotations: annotations},
		Spec:       v1alpha1.ClusterSpec{Name: specName, Server: server},
	}
}

func secret(ns, name string, labels map[string]string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func configMap(ns, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}
}

// appProject is a project in the control-plane namespace that permits ApplicationSets (and
// Applications) of appSetNS.
func appProject(name string) *v1alpha1.AppProject {
	return &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS},
		Spec:       v1alpha1.AppProjectSpec{SourceNamespaces: []string{appSetNS}},
	}
}

func jsonElements(t *testing.T, elems ...any) []apiextensionsv1.JSON {
	t.Helper()
	out := make([]apiextensionsv1.JSON, 0, len(elems))
	for _, e := range elems {
		b, err := json.Marshal(e)
		require.NoError(t, err)
		out = append(out, apiextensionsv1.JSON{Raw: b})
	}
	return out
}

func rawJSON(t *testing.T, v any) *apiextensionsv1.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return &apiextensionsv1.JSON{Raw: b}
}

func generate(t *testing.T, set Set, entry v1alpha1.ApplicationSetGenerator, as *v1alpha1.ApplicationSet) ([]map[string]any, error) {
	t.Helper()
	res, err := set.Transform(context.Background(), entry, as.Spec.Template, as, nil)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, r := range res {
		out = append(out, r.Params...)
	}
	return out, nil
}

// fakeRepos is an in-memory RepoService.
type fakeRepos struct {
	dirs     []string
	files    map[string]map[string][]byte // pattern → files
	requests []RepoRequest
	err      error
}

func (f *fakeRepos) GetFiles(_ context.Context, req RepoRequest, pattern string) (map[string][]byte, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.files[pattern], nil
}

func (f *fakeRepos) GetDirectories(_ context.Context, req RepoRequest) ([]string, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.dirs, nil
}

func toObjects(objs []any) []client.Object {
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(client.Object))
	}
	return out
}

// testCAPEM is a throwaway self-signed CA certificate.
var testCAPEM = func() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}()
