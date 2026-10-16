package repositories_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/repositories"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func metaName(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }

// selfSignedPEM returns a PEM certificate for cn and the PEM of its private key.
func selfSignedPEM(t *testing.T, cn string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kb, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

// sshPublicKey returns the base64 key and the SHA256 fingerprint of a new ed25519 key.
func sshPublicKey(t *testing.T) (string, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(k.Marshal()), ssh.FingerprintSHA256(k)
}

// TestCertificates covers REPO-057..061/API-029.
func TestCertificates(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	cert, key := selfSignedPEM(t, "git.example.com")
	sshKey, fp := sshPublicKey(t)
	sshKey2, _ := sshPublicKey(t)

	created, err := admin.CreateCertificates(ctx, &apiclient.RepositoryCertificateCreateRequest{Body: apiclient.RepositoryCertificateList{Items: []apiclient.RepositoryCertificate{
		{ServerName: "git.example.com", CertType: "https", CertData: "leading text\n" + cert},
		{ServerName: "github.com", CertType: "ssh", CertSubType: "ssh-ed25519", CertData: sshKey},
		{ServerName: "[git.example.com]:2222", CertType: "ssh", CertSubType: "ssh-ed25519", CertData: sshKey2},
	}}})
	require.NoError(t, err)
	require.Len(t, created.Items, 3)
	assert.Equal(t, "ecdsa", created.Items[0].CertSubType)
	assert.Equal(t, "CN=git.example.com", created.Items[0].CertInfo)
	assert.Equal(t, cert, created.Items[0].CertData)
	assert.Equal(t, fp, created.Items[1].CertInfo)

	// The ConfigMaps hold only certificate blocks, and are labelled for the settings layer.
	var tlsCM, sshCM corev1.ConfigMap
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: settings.DefaultTLSCertsConfigMapName}, &tlsCM))
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: settings.DefaultSSHKnownHostsConfigMapName}, &sshCM))
	assert.Equal(t, cert, tlsCM.Data["git.example.com"])
	assert.Equal(t, v1alpha1.LabelValuePartOf, tlsCM.Labels[v1alpha1.LabelKeyPartOf])
	assert.Equal(t, "github.com ssh-ed25519 "+sshKey+"\n[git.example.com]:2222 ssh-ed25519 "+sshKey2+"\n", sshCM.Data["ssh_known_hosts"])

	list := func(q apiclient.RepositoryCertificateQuery) []apiclient.RepositoryCertificate {
		t.Helper()
		l, err := e.client("reader").ListCertificates(ctx, &q)
		require.NoError(t, err)
		for _, c := range l.Items {
			assert.Empty(t, c.CertData, "certData is never listed")
		}
		return l.Items
	}
	assert.Len(t, list(apiclient.RepositoryCertificateQuery{}), 3)
	assert.Len(t, list(apiclient.RepositoryCertificateQuery{CertType: "ssh"}), 2)
	assert.Len(t, list(apiclient.RepositoryCertificateQuery{CertType: "tls"}), 1)
	assert.Len(t, list(apiclient.RepositoryCertificateQuery{HostNamePattern: "[git.example.com]:2222"}), 1, "brackets are literal")
	assert.Len(t, list(apiclient.RepositoryCertificateQuery{HostNamePattern: "*.com"}), 2)
	assert.Len(t, list(apiclient.RepositoryCertificateQuery{CertType: "ssh", CertSubType: "ssh-rsa"}), 0)
	_, err = admin.ListCertificates(ctx, &apiclient.RepositoryCertificateQuery{CertType: "pgp"})
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, "unknown certificate type: pgp", ae.Detail)

	add := func(c *apiclient.Client, upsert bool, items ...apiclient.RepositoryCertificate) (*apiclient.RepositoryCertificateList, error) {
		return c.CreateCertificates(ctx, &apiclient.RepositoryCertificateCreateRequest{Upsert: upsert, Body: apiclient.RepositoryCertificateList{Items: items}})
	}
	// Identical: nothing created. Different without upsert: refused. Upsert replaces.
	same, err := add(admin, false, apiclient.RepositoryCertificate{ServerName: "github.com", CertType: "ssh", CertSubType: "ssh-ed25519", CertData: sshKey})
	require.NoError(t, err)
	assert.Empty(t, same.Items)
	_, err = add(admin, false, apiclient.RepositoryCertificate{ServerName: "github.com", CertType: "ssh", CertSubType: "ssh-ed25519", CertData: sshKey2})
	ae = requireStatus(t, err, http.StatusConflict, apiclient.CodeAlreadyExists)
	assert.Equal(t, "key for 'github.com' (subtype: 'ssh-ed25519') already exists, and upsert was not specified", ae.Detail)
	cert2, _ := selfSignedPEM(t, "git.example.com")
	_, err = add(admin, false, apiclient.RepositoryCertificate{ServerName: "git.example.com", CertType: "https", CertData: cert2})
	ae = requireStatus(t, err, http.StatusConflict, apiclient.CodeAlreadyExists)
	assert.Equal(t, "TLS certificate for server 'git.example.com' already exists, and upsert was not specified", ae.Detail)
	up, err := add(admin, true, apiclient.RepositoryCertificate{ServerName: "git.example.com", CertType: "https", CertData: cert2})
	require.NoError(t, err)
	require.Len(t, up.Items, 1)

	// Invalid input; nothing is saved when one item of a batch fails.
	for _, tc := range []struct {
		item   apiclient.RepositoryCertificate
		detail string
	}{
		{apiclient.RepositoryCertificate{ServerName: "bad host!", CertType: "https", CertData: cert}, "invalid hostname in request: bad host!"},
		{apiclient.RepositoryCertificate{ServerName: "x.example.com", CertType: "https", CertData: cert + key}, "private key material"},
		{apiclient.RepositoryCertificate{ServerName: "x.example.com", CertType: "https", CertData: "garbage"}, "no valid PEM data received"},
		{apiclient.RepositoryCertificate{ServerName: "x.example.com", CertType: "https", CertData: "-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydA==\n-----END CERTIFICATE-----\n"}, "could not parse X509 data from input"},
		{apiclient.RepositoryCertificate{ServerName: "x.example.com", CertType: "ssh", CertSubType: "ssh-ed25519", CertData: "AAAA bad"}, "base64 public key"},
		{apiclient.RepositoryCertificate{ServerName: "x.example.com", CertType: "ssh", CertSubType: "ssh-rsa", CertData: sshKey}, "is not of type ssh-rsa"},
		{apiclient.RepositoryCertificate{ServerName: "x.example.com", CertType: "gpg", CertData: "x"}, "unknown certificate type: gpg"},
	} {
		_, err := add(admin, false, apiclient.RepositoryCertificate{ServerName: "new.example.com", CertType: "ssh", CertSubType: "ssh-ed25519", CertData: sshKey}, tc.item)
		ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
		assert.Contains(t, ae.Detail, tc.detail)
	}
	assert.Len(t, list(apiclient.RepositoryCertificateQuery{HostNamePattern: "new.example.com"}), 0, "failed batches save nothing")

	// RBAC: readers list, only certificates create/delete may change.
	_, err = add(e.client("reader"), false, apiclient.RepositoryCertificate{ServerName: "r.example.com", CertType: "ssh", CertSubType: "ssh-ed25519", CertData: sshKey})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = e.client("nobody").ListCertificates(ctx, &apiclient.RepositoryCertificateQuery{})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = e.client("reader").DeleteCertificates(ctx, &apiclient.RepositoryCertificateQuery{})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	// Comments in the known hosts data are kept by rewrites.
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: settings.DefaultSSHKnownHostsConfigMapName}, &sshCM))
	sshCM.Data["ssh_known_hosts"] = "# managed by ops\n" + sshCM.Data["ssh_known_hosts"]
	require.NoError(t, e.kc.Update(ctx, &sshCM))

	// Delete one SSH host, then a TLS host; an empty selector removes everything.
	removed, err := admin.DeleteCertificates(ctx, &apiclient.RepositoryCertificateQuery{CertType: "ssh", HostNamePattern: "github.com"})
	require.NoError(t, err)
	require.Len(t, removed.Items, 1)
	assert.Equal(t, sshKey, removed.Items[0].CertData)
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: settings.DefaultSSHKnownHostsConfigMapName}, &sshCM))
	assert.True(t, strings.HasPrefix(sshCM.Data["ssh_known_hosts"], "# managed by ops\n"))
	removed, err = admin.DeleteCertificates(ctx, &apiclient.RepositoryCertificateQuery{CertType: "https", HostNamePattern: "git.example.com"})
	require.NoError(t, err)
	require.Len(t, removed.Items, 1)
	assert.Equal(t, cert2, removed.Items[0].CertData)
	removed, err = admin.DeleteCertificates(ctx, &apiclient.RepositoryCertificateQuery{})
	require.NoError(t, err)
	assert.Len(t, removed.Items, 1)
	assert.Empty(t, list(apiclient.RepositoryCertificateQuery{}))

	// An unlabelled ConfigMap is ignored by the settings layer: refused rather than edited.
	tlsCM = corev1.ConfigMap{}
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: settings.DefaultTLSCertsConfigMapName}, &tlsCM))
	delete(tlsCM.Labels, v1alpha1.LabelKeyPartOf)
	require.NoError(t, e.kc.Update(ctx, &tlsCM))
	_, err = add(admin, false, apiclient.RepositoryCertificate{ServerName: "git.example.com", CertType: "https", CertData: cert})
	requireStatus(t, err, http.StatusPreconditionFailed, apiclient.CodeFailedPrecondition)
}

// armoredKey returns a new key's armored public key, its ID and the armored private key.
func armoredKey(t *testing.T, name string) (pub, id, priv string) {
	t.Helper()
	ent, err := openpgp.NewEntity(name, "", name+"@example.com", nil)
	require.NoError(t, err)
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, ent.Serialize(w))
	require.NoError(t, w.Close())
	var p bytes.Buffer
	pw, err := armor.Encode(&p, openpgp.PrivateKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, ent.SerializePrivate(pw, nil))
	require.NoError(t, pw.Close())
	return b.String() + "\n", ent.PrimaryKey.KeyIdString(), p.String()
}

// TestGPGKeys covers REPO-064/065/API-031.
func TestGPGKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	pub1, id1, priv1 := armoredKey(t, "alice")
	pub2, id2, _ := armoredKey(t, "bob")

	_, err := admin.CreateGPGKeys(ctx, "  ")
	ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, "submitted key data is empty", ae.Detail)
	_, err = admin.CreateGPGKeys(ctx, priv1)
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	_, err = admin.CreateGPGKeys(ctx, "not a key")
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)

	res, err := admin.CreateGPGKeys(ctx, pub1)
	require.NoError(t, err)
	require.Len(t, res.Created.Items, 1)
	assert.Equal(t, id1, res.Created.Items[0].KeyID)
	assert.Equal(t, "ultimate", res.Created.Items[0].Trust)
	assert.Contains(t, res.Created.Items[0].Owner, "alice")
	// One new key and one already configured, in one submission.
	res, err = admin.CreateGPGKeys(ctx, pub1+pub2)
	require.NoError(t, err)
	require.Len(t, res.Created.Items, 1)
	assert.Equal(t, id2, res.Created.Items[0].KeyID)
	assert.Equal(t, []string{id1}, res.Skipped)

	l, err := e.client("reader").ListGPGKeys(ctx)
	require.NoError(t, err)
	require.Len(t, l.Items, 2)
	for _, k := range l.Items {
		assert.Empty(t, k.KeyData, "key data is not listed")
		assert.Len(t, k.Fingerprint, 40)
	}
	k, err := admin.GetGPGKey(ctx, strings.ToLower(id1))
	require.NoError(t, err)
	assert.Contains(t, k.KeyData, "BEGIN PGP PUBLIC KEY BLOCK")
	k2, err := admin.GetGPGKey(ctx, k.Fingerprint)
	require.NoError(t, err, "by fingerprint")
	assert.Equal(t, id1, k2.KeyID)
	_, err = admin.GetGPGKey(ctx, "0123456789ABCDEF")
	ae = requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	assert.Equal(t, "no such key: 0123456789ABCDEF", ae.Detail)
	_, err = admin.GetGPGKey(ctx, "xyz")
	ae = requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	assert.Equal(t, "'xyz' is not a valid GnuPG key ID", ae.Detail)

	// The ConfigMap is keyed by key ID, as the repo-server keyring sync expects.
	var cm corev1.ConfigMap
	require.NoError(t, e.kc.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: settings.DefaultGPGKeysConfigMapName}, &cm))
	assert.Len(t, cm.Data, 2)
	parsed, err := settings.ParseGPGPublicKey(cm.Data[id1])
	require.NoError(t, err)
	assert.Equal(t, id1, parsed.KeyID)

	// RBAC.
	_, err = e.client("reader").CreateGPGKeys(ctx, pub1)
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	err = e.client("reader").DeleteGPGKey(ctx, id1)
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = e.client("nobody").GetGPGKey(ctx, id1)
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	// Delete by fingerprint (normalized, unlike upstream), then it is gone.
	require.NoError(t, admin.DeleteGPGKey(ctx, k.Fingerprint))
	err = admin.DeleteGPGKey(ctx, id1)
	ae = requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	assert.Equal(t, "no such key configured: "+id1, ae.Detail)
	l, err = admin.ListGPGKeys(ctx)
	require.NoError(t, err)
	assert.Len(t, l.Items, 1)
}

// TestSCMProviders covers the SCMProvider CRUD routes (Patrol addition).
func TestSCMProviders(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	admin := e.client("admin")
	gh := &apiclient.SCMProviderWriteRequest{
		Metadata: metav1.ObjectMeta{Name: "github-org", Labels: map[string]string{"team": "platform"}},
		Spec: v1alpha1.SCMProviderSpec{
			Type: v1alpha1.SCMProviderTypeGitHub, APIURL: "https://api.github.com",
			Auth:    &v1alpha1.SCMProviderAuth{TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "gh-token", Key: "token"}},
			Webhook: &v1alpha1.SCMProviderWebhook{SecretRef: v1alpha1.SecretKeyReference{Name: "gh-webhook", Key: "secret"}},
		},
	}
	created, err := admin.CreateSCMProvider(ctx, gh)
	require.NoError(t, err)
	assert.Equal(t, "SCMProvider", created.Kind)
	assert.Equal(t, "gh-token", created.Spec.Auth.TokenSecretRef.Name)
	_, err = admin.CreateSCMProvider(ctx, gh)
	requireStatus(t, err, http.StatusConflict, apiclient.CodeAlreadyExists)
	_, err = admin.CreateSCMProvider(ctx, &apiclient.SCMProviderWriteRequest{Metadata: metaName("gitlab-x"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitLab}})
	require.NoError(t, err)

	// Invalid specs.
	for _, tc := range []struct {
		spec   v1alpha1.SCMProviderSpec
		detail string
	}{
		{v1alpha1.SCMProviderSpec{Type: "svn"}, `spec.type "svn" is invalid`},
		{v1alpha1.SCMProviderSpec{Type: "github", APIURL: "javascript:alert(1)"}, "must be an http or https URL"},
		{v1alpha1.SCMProviderSpec{Type: "github", APIURL: "https://user:pw@api.github.com"}, "must not contain user information"},
		{v1alpha1.SCMProviderSpec{Type: "github", Auth: &v1alpha1.SCMProviderAuth{
			TokenSecretRef: &v1alpha1.SecretKeyReference{Name: "a", Key: "t"}, BasicAuthSecretRef: &v1alpha1.LocalSecretReference{Name: "b"},
		}}, "at most one of"},
		{v1alpha1.SCMProviderSpec{Type: "gitlab", StatusReporting: &v1alpha1.SCMProviderStatusReporting{Mode: v1alpha1.StatusReportingModeChecks}}, "requires type github with auth.githubApp"},
		{v1alpha1.SCMProviderSpec{Type: "github", CASecretRef: &v1alpha1.SecretKeyReference{Name: "Bad_Name", Key: "ca"}}, "spec.caSecretRef.name"},
	} {
		_, err := admin.CreateSCMProvider(ctx, &apiclient.SCMProviderWriteRequest{Metadata: metaName("bad"), Spec: tc.spec})
		ae := requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
		assert.Contains(t, ae.Detail, tc.detail)
	}
	// Secret values are never accepted: a credentials field is an unknown field.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/api/v1/scmproviders",
		strings.NewReader(`{"metadata":{"name":"x"},"spec":{"type":"github"},"credentials":{"token":"ghp_secret"}}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+admin.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// RBAC per name: scm may get github-* only; list filtered.
	scm := e.client("scm")
	l, err := scm.ListSCMProviders(ctx)
	require.NoError(t, err)
	require.Len(t, l.Items, 1)
	assert.Equal(t, "github-org", l.Items[0].Name)
	_, err = scm.GetSCMProvider(ctx, "gitlab-x")
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = scm.GetSCMProvider(ctx, "github-missing")
	requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	_, err = scm.CreateSCMProvider(ctx, &apiclient.SCMProviderWriteRequest{Metadata: metaName("github-new"), Spec: v1alpha1.SCMProviderSpec{Type: "github"}})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	// role:readonly: every provider once internal/rbac grants it scmproviders get
	// (pending, see rbacKnowsSCMProviders); until then the list is filtered to nothing.
	readerList, err := e.client("reader").ListSCMProviders(ctx)
	require.NoError(t, err)
	if rbacKnowsSCMProviders() {
		assert.Len(t, readerList.Items, 2)
	} else {
		assert.Empty(t, readerList.Items)
	}

	// Update: optimistic concurrency, name must match.
	cur, err := admin.GetSCMProvider(ctx, "github-org")
	require.NoError(t, err)
	upd := &apiclient.SCMProviderWriteRequest{Metadata: metav1.ObjectMeta{Name: "github-org", ResourceVersion: "1"}, Spec: cur.Spec}
	upd.Spec.RateLimit = &v1alpha1.SCMProviderRateLimit{RequestsPerSecond: 5}
	_, err = admin.UpdateSCMProvider(ctx, "github-org", upd)
	requireStatus(t, err, http.StatusConflict, apiclient.CodeConflict)
	upd.Metadata.ResourceVersion = cur.ResourceVersion
	_, err = admin.UpdateSCMProvider(ctx, "gitlab-x", upd)
	requireStatus(t, err, http.StatusBadRequest, apiclient.CodeInvalidArgument)
	updated, err := admin.UpdateSCMProvider(ctx, "github-org", upd)
	require.NoError(t, err)
	assert.Equal(t, int32(5), updated.Spec.RateLimit.RequestsPerSecond)
	assert.Empty(t, updated.Labels, "labels replaced")
	_, err = scm.UpdateSCMProvider(ctx, "github-org", upd)
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)

	require.NoError(t, admin.DeleteSCMProvider(ctx, "github-org"))
	err = admin.DeleteSCMProvider(ctx, "github-org")
	requireStatus(t, err, http.StatusNotFound, apiclient.CodeNotFound)
	err = scm.DeleteSCMProvider(ctx, "gitlab-x")
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
}

// rbacKnowsSCMProviders reports whether internal/rbac declares the scmproviders resource
// (with its strict-mode actions and built-in grants), requested from the rbac owner.
func rbacKnowsSCMProviders() bool {
	return slices.Contains(rbac.Resources(), repositories.ResourceSCMProviders)
}

// TestSCMProvidersBuiltinRoles runs the SCM provider routes with the built-in policy only
// (the local admin account holds role:admin). Until internal/rbac declares scmproviders
// with role:readonly get and role:admin create/update/delete, the built-in roles carry no
// grant for it and every call is denied (the documented gap); once it does, admin manages
// providers, references them from repositories, and readers list and get them.
func TestSCMProvidersBuiltinRoles(t *testing.T) {
	t.Parallel()
	builtinOnly := "g, admin, role:admin\ng, reader, role:readonly\n"
	e := newEnv(t, envOptions{policy: &builtinOnly})
	ctx := t.Context()
	admin, reader := e.client("admin"), e.client("reader")
	req := &apiclient.SCMProviderWriteRequest{Metadata: metaName("github-org"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub}}
	ref := repoBody("https://github.com/org/app", "", nil)
	ref.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "github-org"}

	if !rbacKnowsSCMProviders() {
		_, err := admin.CreateSCMProvider(ctx, req)
		requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
		require.NoError(t, e.kc.Create(ctx, &v1alpha1.SCMProvider{ObjectMeta: metav1.ObjectMeta{Name: "github-org", Namespace: e.ns}, Spec: req.Spec}))
		l, err := admin.ListSCMProviders(ctx)
		require.NoError(t, err)
		assert.Empty(t, l.Items)
		_, err = admin.GetSCMProvider(ctx, "github-org")
		requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
		_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: ref})
		requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
		l, err = reader.ListSCMProviders(ctx)
		require.NoError(t, err)
		assert.Empty(t, l.Items)
		return
	}

	created, err := admin.CreateSCMProvider(ctx, req)
	require.NoError(t, err)
	l, err := reader.ListSCMProviders(ctx)
	require.NoError(t, err)
	require.Len(t, l.Items, 1)
	_, err = reader.GetSCMProvider(ctx, "github-org")
	require.NoError(t, err)
	_, err = reader.CreateSCMProvider(ctx, &apiclient.SCMProviderWriteRequest{Metadata: metaName("github-other"), Spec: req.Spec})
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	upd := &apiclient.SCMProviderWriteRequest{Metadata: metav1.ObjectMeta{Name: "github-org", ResourceVersion: created.ResourceVersion}, Spec: req.Spec}
	upd.Spec.RateLimit = &v1alpha1.SCMProviderRateLimit{RequestsPerSecond: 2}
	_, err = reader.UpdateSCMProvider(ctx, "github-org", upd)
	requireStatus(t, err, http.StatusForbidden, apiclient.CodePermissionDenied)
	_, err = admin.UpdateSCMProvider(ctx, "github-org", upd)
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err := e.lister.SCMProvider("github-org"); return err == nil }, 10*time.Second, 20*time.Millisecond)
	_, err = admin.CreateRepository(ctx, &apiclient.RepositoryCreateRequest{Body: ref})
	require.NoError(t, err)
	requireStatus(t, reader.DeleteSCMProvider(ctx, "github-org"), http.StatusForbidden, apiclient.CodePermissionDenied)
	require.NoError(t, admin.DeleteSCMProvider(ctx, "github-org"))
}
