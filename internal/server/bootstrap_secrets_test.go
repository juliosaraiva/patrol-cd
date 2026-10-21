package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/cli/admin"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

var initialPasswordRE = regexp.MustCompile(`^[A-Za-z0-9]{24}$`)

func TestRandomStringRejectionSampling(t *testing.T) {
	t.Parallel()
	// 248..255 are rejected for a 62-character alphabet (248 = 4*62); the rest map modulo 62.
	src := []byte{248, 249, 250, 251, 252, 253, 254, 255, 0, 61, 62, 247, 123, 186}
	got, err := randomString(bytes.NewReader(append(src, make([]byte, 64)...)), 6, initialAdminPasswordAlphabet)
	require.NoError(t, err)
	// Kept bytes 0, 61, 62, 247, 123, 186 map to A, 9, A, 9, 9, A.
	assert.Equal(t, "A9A99A", got)
}

func TestRandomStringMapping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   byte
		want byte
	}{{0, 'A'}, {25, 'Z'}, {26, 'a'}, {51, 'z'}, {52, '0'}, {61, '9'}, {62, 'A'}, {123, '9'}, {124, 'A'}, {247, '9'}} {
		got, err := randomString(bytes.NewReader([]byte{tc.in, 0, 0}), 1, initialAdminPasswordAlphabet)
		require.NoError(t, err)
		assert.Equal(t, string(tc.want), got, "byte %d", tc.in)
	}
	for b := 248; b <= 255; b++ {
		// A rejected byte is skipped: the next one decides.
		got, err := randomString(bytes.NewReader([]byte{byte(b), 1, 0}), 1, initialAdminPasswordAlphabet)
		require.NoError(t, err)
		assert.Equal(t, "B", got, "byte %d must be rejected", b)
	}
}

func TestGenerateInitialAdminPassword(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	chars := map[rune]int{}
	for range 300 {
		p, err := generateInitialAdminPassword(rand.Reader)
		require.NoError(t, err)
		require.Regexp(t, initialPasswordRE, p)
		require.False(t, seen[p], "duplicate password")
		seen[p] = true
		for _, c := range p {
			chars[c]++
		}
	}
	// 7200 draws over 62 characters: each appears (missing one has probability ~1e-49).
	assert.Len(t, chars, len(initialAdminPasswordAlphabet))

	_, err := generateInitialAdminPassword(iotestErrReader{})
	require.Error(t, err)
	_, err = generateInitialAdminPassword(bytes.NewReader([]byte{1, 2, 3}))
	require.Error(t, err, "short entropy source")
}

func TestNewInternalToken(t *testing.T) {
	t.Parallel()
	src := make([]byte, InternalTokenBytes)
	for i := range src {
		src[i] = byte(i)
	}
	tok, err := newInternalToken(bytes.NewReader(src))
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(src), tok)
	assert.Len(t, tok, 64)

	tok, err = newInternalToken(rand.Reader)
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9a-f]{64}$`, tok)
	_, err = newInternalToken(iotestErrReader{})
	require.Error(t, err)
}

func TestRetriableBootstrapError(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Resource: "secrets"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"conflict", apierrors.NewConflict(gr, "patrol-secret", errors.New("modified")), true},
		{"wrapped conflict", fmt.Errorf("update: %w", apierrors.NewConflict(gr, "x", errors.New("modified"))), true},
		{"server timeout", apierrors.NewServerTimeout(gr, "get", 1), true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), true},
		{"internal", apierrors.NewInternalError(errors.New("boom")), true},
		{"unavailable", apierrors.NewServiceUnavailable("starting"), true},
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("rbac")), true},
		{"unauthorized", apierrors.NewUnauthorized("token"), true},
		{"connection refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, true},
		{"eof", fmt.Errorf("get: %w", io.ErrUnexpectedEOF), true},
		{"dial timeout", &url.Error{Op: "Get", URL: "https://k8s", Err: &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}}, true},
		{"tls verification", &url.Error{Op: "Get", URL: "https://k8s", Err: errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")}, false},
		{"not found", apierrors.NewNotFound(gr, "x"), false},
		{"invalid", apierrors.NewBadRequest("bad"), false},
		{"canceled", context.Canceled, false},
		{"deadline", fmt.Errorf("get: %w", context.DeadlineExceeded), false},
		{"plain", errors.New("secret exists without a value"), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, retriableBootstrapError(tc.err), tc.name)
	}
}

func TestBootstrapNamespace(t *testing.T) {
	t.Parallel()
	saFile := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(saFile, []byte("from-sa\n"), 0o600))
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		name, flag string
		env        map[string]string
		sa         string
		want       string
		wantErr    string
	}{
		{name: "flag wins", flag: "patrol", env: map[string]string{"POD_NAMESPACE": "pod"}, sa: saFile, want: "patrol"},
		{name: "pod namespace", env: map[string]string{"POD_NAMESPACE": " pod "}, sa: saFile, want: "pod"},
		{name: "service account file", env: map[string]string{"POD_NAMESPACE": ""}, sa: saFile, want: "from-sa"},
		{name: "none", sa: filepath.Join(t.TempDir(), "missing"), wantErr: "no control-plane namespace"},
		{name: "invalid", flag: "Not_A_Namespace", wantErr: "invalid namespace"},
	}
	for _, tc := range cases {
		got, err := bootstrapNamespace(tc.flag, env(tc.env), tc.sa)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

func TestRunBootstrapValidatesConfig(t *testing.T) {
	t.Parallel()
	require.ErrorIs(t, RunBootstrap(context.Background(), BootstrapConfig{Namespace: "x"}), errBootstrapConfig)
}

// TestBootstrapRetries injects failures with a fake clientset: a Forbidden read (RBAC not
// effective yet) and a conflicting patrol-secret update are retried; the run converges.
func TestBootstrapRetries(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset()
	var mu sync.Mutex
	forbidden, conflicts := 1, 1
	kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if forbidden > 0 {
			forbidden--
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, InternalTokenSecretName, errors.New("RBAC: not yet"))
		}
		return false, nil, nil
	})
	kube.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		sec := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if _, ok := sec.Data[auth.KeyAdminPassword]; ok && conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, sec.Name, errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	buf := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, RunBootstrap(ctx, BootstrapConfig{
		Namespace: "patrol", Kube: kube, Logger: slog.New(slog.NewTextHandler(buf, nil)),
	}))
	logs := buf.String()
	assert.Contains(t, logs, "bootstrap step failed; retrying")
	assert.Contains(t, logs, "object changed concurrently; re-reading")
	assert.Zero(t, forbidden)
	assert.Zero(t, conflicts)
	password := string(getSecret(t, kube, "patrol", InitialAdminSecretName).Data[InitialAdminSecretKey])
	hash := getSecret(t, kube, "patrol", settings.DefaultSecretName).Data[auth.KeyAdminPassword]
	require.NoError(t, bcrypt.CompareHashAndPassword(hash, []byte(password)))
	requireNoSecretValues(t, logs, password, string(hash))
}

// TestBootstrapDeadline: a failure that persists is retried until the context ends, and the
// error names the last failure.
func TestBootstrapDeadline(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset()
	kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver starting")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := RunBootstrap(ctx, BootstrapConfig{Namespace: "patrol", Kube: kube, Logger: slog.New(slog.DiscardHandler)})
	require.ErrorContains(t, err, "did not converge before the deadline")
	assert.True(t, apierrors.IsServiceUnavailable(err), err)
}

// --- envtest ---

// bootstrapKube returns a client of the envtest API server and a fresh namespace.
func bootstrapKube(t *testing.T) (kubernetes.Interface, string) {
	t.Helper()
	requireEnvtest(t)
	kube, err := kubernetes.NewForConfig(testCfg)
	require.NoError(t, err)
	return kube, newNamespace(t, kube)
}

// runBootstrapLogged runs RunBootstrap with its own client and returns the captured logs
// (debug level: everything the bootstrap may log) and the error.
func runBootstrapLogged(t *testing.T, ns string, mutate func(*BootstrapConfig)) (string, error) {
	t.Helper()
	kube, err := kubernetes.NewForConfig(testCfg)
	require.NoError(t, err)
	buf := &syncBuffer{}
	cfg := BootstrapConfig{
		Namespace: ns, Kube: kube,
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err = RunBootstrap(ctx, cfg)
	return buf.String(), err
}

func getSecret(t *testing.T, kube kubernetes.Interface, ns, name string) *corev1.Secret {
	t.Helper()
	s, err := kube.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err, "Secret %s", name)
	return s
}

func requireSecretAbsent(t *testing.T, kube kubernetes.Interface, ns, name string) {
	t.Helper()
	_, err := kube.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "Secret %s must not exist: %v", name, err)
}

func createSecret(t *testing.T, kube kubernetes.Interface, ns, name string, labels map[string]string, data map[string][]byte) *corev1.Secret {
	t.Helper()
	s, err := kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	return s
}

// createSettingsCM creates patrol-cm as the install manifests ship it (part-of label).
func createSettingsCM(t *testing.T, kube kubernetes.Interface, ns string, data map[string]string) {
	t.Helper()
	_, err := kube.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: ns,
			Labels: map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}},
		Data: data,
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

var partOf = map[string]string{v1alpha1.LabelKeyPartOf: v1alpha1.LabelValuePartOf}

// adminLogin authenticates admin/password the way patrol-server does: settings.Parse over
// the live patrol-cm and patrol-secret, then auth.Authenticator. Without a patrol-cm in the
// namespace the empty, labelled one the install manifests ship is used (the server disables
// every account while patrol-cm is absent).
func adminLogin(t *testing.T, kube kubernetes.Interface, ns, password string) error {
	t.Helper()
	ctx := context.Background()
	in := settings.Input{Namespace: ns}
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, settings.DefaultConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm, err = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: ns, Labels: partOf}}, nil
	}
	require.NoError(t, err)
	in.ConfigMaps = append(in.ConfigMaps, cm)
	in.Secrets = append(in.Secrets, getSecret(t, kube, ns, settings.DefaultSecretName))
	authn, err := auth.NewAuthenticator(settings.Parse(in).Accounts(), auth.LoginConfig{},
		auth.WithLoginLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)
	acct, err := authn.Authenticate(ctx, auth.AdminUsername, password)
	if err == nil {
		assert.Equal(t, auth.AdminUsername, acct.Name)
	}
	return err
}

// cliInitialPassword runs `patrol admin initial-password` against the namespace and returns
// the password it prints (the CLI and the bootstrap must agree on the Secret name and key).
func cliInitialPassword(t *testing.T, ns string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := admin.NewCommand(&admin.Factory{
		IOStreams:  admin.IOStreams{Out: &out, ErrOut: &errOut},
		RESTConfig: func() (*rest.Config, string, error) { return testCfg, ns, nil },
	})
	cmd.SetArgs([]string{"initial-password"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	require.NoError(t, cmd.ExecuteContext(context.Background()), errOut.String())
	first, _, _ := strings.Cut(out.String(), "\n")
	return first
}

// requireNoSecretValues fails when output contains any of the secret values.
func requireNoSecretValues(t *testing.T, output string, values ...string) {
	t.Helper()
	for _, v := range values {
		require.NotEmpty(t, v)
		require.NotContains(t, output, v, "output discloses a secret value")
	}
}

func resourceVersions(t *testing.T, kube kubernetes.Interface, ns string, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range names {
		out[n] = getSecret(t, kube, ns, n).ResourceVersion
	}
	return out
}

// TestBootstrapFreshInstall: every object is created with its shape; the password logs in
// through the server's settings and authenticator path and matches the CLI output; a second
// run writes nothing; no log line carries a secret value.
func TestBootstrapFreshInstall(t *testing.T) {
	t.Parallel()
	kube, ns := bootstrapKube(t)
	createSettingsCM(t, kube, ns, nil)
	start := time.Now().Add(-time.Second).Truncate(time.Second)

	logs, err := runBootstrapLogged(t, ns, nil)
	require.NoError(t, err, logs)

	tokSec := getSecret(t, kube, ns, InternalTokenSecretName)
	assert.Equal(t, corev1.SecretTypeOpaque, tokSec.Type)
	assert.Equal(t, v1alpha1.LabelValuePartOf, tokSec.Labels[v1alpha1.LabelKeyPartOf])
	token := string(tokSec.Data[InternalTokenSecretKey])
	assert.Regexp(t, `^[0-9a-f]{64}$`, token)
	assert.Len(t, tokSec.Data, 1)

	initSec := getSecret(t, kube, ns, InitialAdminSecretName)
	assert.Equal(t, corev1.SecretTypeOpaque, initSec.Type)
	assert.NotContains(t, initSec.Labels, v1alpha1.LabelKeyPartOf, "the plaintext must stay out of the settings Secret informer")
	password := string(initSec.Data[InitialAdminSecretKey])
	assert.Regexp(t, initialPasswordRE, password)
	assert.Len(t, initSec.Data, 1)

	sec := getSecret(t, kube, ns, settings.DefaultSecretName)
	assert.Equal(t, v1alpha1.LabelValuePartOf, sec.Labels[v1alpha1.LabelKeyPartOf])
	assert.Len(t, sec.Data[settings.SecretKeyServerSecretKey], settings.ServerSecretKeyLength)
	hash := sec.Data[auth.KeyAdminPassword]
	cost, err := bcrypt.Cost(hash)
	require.NoError(t, err)
	assert.Equal(t, auth.DefaultBcryptCost, cost)
	require.NoError(t, bcrypt.CompareHashAndPassword(hash, []byte(password)))
	mtime, err := time.Parse(time.RFC3339, string(sec.Data[auth.KeyAdminPasswordMtime]))
	require.NoError(t, err)
	assert.False(t, mtime.Before(start), "mtime %s before %s", mtime, start)
	assert.False(t, mtime.After(time.Now()), "mtime in the future")

	require.NoError(t, adminLogin(t, kube, ns, password))
	require.ErrorIs(t, adminLogin(t, kube, ns, password+"x"), auth.ErrInvalidCredentials)
	assert.Equal(t, password, cliInitialPassword(t, ns))

	signingKey := string(sec.Data[settings.SecretKeyServerSecretKey])
	requireNoSecretValues(t, logs, password, token, string(hash), hex.EncodeToString([]byte(signingKey)))
	assert.NotContains(t, logs, signingKey)
	assert.Contains(t, logs, `"msg":"generated the initial admin password"`)
	assert.Contains(t, logs, `"msg":"created the internal token"`)

	// Second run: no write at all.
	before := resourceVersions(t, kube, ns, InternalTokenSecretName, InitialAdminSecretName, settings.DefaultSecretName)
	logs2, err := runBootstrapLogged(t, ns, nil)
	require.NoError(t, err, logs2)
	assert.Equal(t, before, resourceVersions(t, kube, ns, InternalTokenSecretName, InitialAdminSecretName, settings.DefaultSecretName))
	assert.Contains(t, logs2, `"msg":"admin password present; unchanged"`)
	requireNoSecretValues(t, logs2, password, token, string(hash))
	require.NoError(t, adminLogin(t, kube, ns, password))
}

// TestBootstrapConcurrent runs 8 bootstraps at once: exactly one creates each Secret, every
// run succeeds, and the single stored hash verifies against the single initial password.
func TestBootstrapConcurrent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, kube kubernetes.Interface, ns string)
	}{
		{name: "fresh namespace", prepare: func(*testing.T, kubernetes.Interface, string) {}},
		{name: "manifests ship empty patrol-secret, patrol-cm and known hosts ConfigMap", prepare: func(t *testing.T, kube kubernetes.Interface, ns string) {
			createSecret(t, kube, ns, settings.DefaultSecretName, partOf, nil)
			createSettingsCM(t, kube, ns, nil)
			createKnownHostsCM(t, kube, ns, partOf, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kube, ns := bootstrapKube(t)
			tc.prepare(t, kube, ns)
			const n = 8
			var wg sync.WaitGroup
			errs := make([]error, n)
			logs := make([]string, n)
			startGate := make(chan struct{})
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-startGate
					logs[i], errs[i] = runBootstrapLogged(t, ns, nil)
				}()
			}
			close(startGate)
			wg.Wait()
			all := strings.Join(logs, "\n")
			t.Logf("runs that adopted another run's Secret: internal token %d, initial password %d; conflicts re-read: %d",
				strings.Count(all, "internal token created concurrently by another run"),
				strings.Count(all, "initial admin password created concurrently by another run")+
					strings.Count(all, "initial admin password Secret exists; storing the hash"),
				strings.Count(all, "object changed concurrently; re-reading"))
			for i, err := range errs {
				require.NoError(t, err, "run %d logs:\n%s", i, logs[i])
			}
			assert.Equal(t, 1, strings.Count(all, `"msg":"generated the initial admin password"`), all)
			assert.Equal(t, 1, strings.Count(all, `"msg":"created the internal token"`), all)
			assert.Equal(t, 1, strings.Count(all, `"msg":"stored the initial admin password hash"`), all)
			if cm, err := kube.CoreV1().ConfigMaps(ns).Get(context.Background(), settings.DefaultSSHKnownHostsConfigMapName, metav1.GetOptions{}); err == nil {
				assert.Equal(t, 1, strings.Count(all, `"msg":"seeded the default known hosts"`), all)
				assert.Equal(t, DefaultSSHKnownHosts, cm.Data[settings.SSHKnownHostsKey])
			}

			password := string(getSecret(t, kube, ns, InitialAdminSecretName).Data[InitialAdminSecretKey])
			require.Regexp(t, initialPasswordRE, password)
			sec := getSecret(t, kube, ns, settings.DefaultSecretName)
			require.NoError(t, bcrypt.CompareHashAndPassword(sec.Data[auth.KeyAdminPassword], []byte(password)))
			require.Len(t, sec.Data[settings.SecretKeyServerSecretKey], settings.ServerSecretKeyLength)
			require.NoError(t, adminLogin(t, kube, ns, password))
			token := string(getSecret(t, kube, ns, InternalTokenSecretName).Data[InternalTokenSecretKey])
			requireNoSecretValues(t, all, password, token, string(sec.Data[auth.KeyAdminPassword]))
		})
	}
}

// TestBootstrapCrashRecovery covers the states an interrupted run leaves behind. A run creates
// (or finds) patrol-secret before the initial Secret, so the setups create them in that order;
// an initial Secret older than patrol-secret is stale instead (TestBootstrapStaleInitialSecret).
func TestBootstrapCrashRecovery(t *testing.T) {
	t.Parallel()
	const stored = "Stored0Initial0Password1" // 24 characters, as a crashed run would have left
	t.Run("initial secret without hash", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		createSecret(t, kube, ns, settings.DefaultSecretName, partOf, nil)
		initSec := createSecret(t, kube, ns, InitialAdminSecretName, nil, map[string][]byte{InitialAdminSecretKey: []byte(stored)})
		logs, err := runBootstrapLogged(t, ns, nil)
		require.NoError(t, err, logs)
		assert.Equal(t, initSec.ResourceVersion, getSecret(t, kube, ns, InitialAdminSecretName).ResourceVersion, "initial secret untouched")
		sec := getSecret(t, kube, ns, settings.DefaultSecretName)
		require.NoError(t, bcrypt.CompareHashAndPassword(sec.Data[auth.KeyAdminPassword], []byte(stored)))
		require.NoError(t, adminLogin(t, kube, ns, stored))
		assert.Equal(t, stored, cliInitialPassword(t, ns))
		requireNoSecretValues(t, logs, stored)
	})
	t.Run("initial secret newer than patrol-secret with signing key only", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		key := bytes.Repeat([]byte{7}, settings.ServerSecretKeyLength)
		ps := createSecret(t, kube, ns, settings.DefaultSecretName, partOf, map[string][]byte{settings.SecretKeyServerSecretKey: key})
		waitPastSecond(t, ps.CreationTimestamp) // strictly newer, not only the same second
		requireNewer(t, createSecret(t, kube, ns, InitialAdminSecretName, nil, map[string][]byte{InitialAdminSecretKey: []byte(stored)}), ps)
		logs, err := runBootstrapLogged(t, ns, nil)
		require.NoError(t, err, logs)
		sec := getSecret(t, kube, ns, settings.DefaultSecretName)
		assert.Equal(t, key, sec.Data[settings.SecretKeyServerSecretKey], "signing key kept")
		require.NoError(t, bcrypt.CompareHashAndPassword(sec.Data[auth.KeyAdminPassword], []byte(stored)))
		require.NoError(t, adminLogin(t, kube, ns, stored))
		requireNoSecretValues(t, logs, stored, string(sec.Data[auth.KeyAdminPassword]))
	})
	t.Run("initial secret without patrol-secret is stale", func(t *testing.T) {
		// patrol-secret was deleted after the initial Secret was created (no run creates them
		// in the other order): the patrol-secret this run creates is newer, so the initial
		// Secret is replaced.
		t.Parallel()
		kube, ns := bootstrapKube(t)
		initSec := createSecret(t, kube, ns, InitialAdminSecretName, nil, map[string][]byte{InitialAdminSecretKey: []byte(stored)})
		waitPastSecond(t, initSec.CreationTimestamp)
		logs, err := runBootstrapLogged(t, ns, nil)
		require.NoError(t, err, logs)
		requireNewer(t, getSecret(t, kube, ns, settings.DefaultSecretName), initSec)
		replaced := getSecret(t, kube, ns, InitialAdminSecretName)
		assert.NotEqual(t, initSec.UID, replaced.UID)
		password := string(replaced.Data[InitialAdminSecretKey])
		require.Regexp(t, initialPasswordRE, password)
		require.NoError(t, adminLogin(t, kube, ns, password))
		require.ErrorIs(t, adminLogin(t, kube, ns, stored), auth.ErrInvalidCredentials)
		assert.Contains(t, logs, msgStaleDeleted)
		requireNoSecretValues(t, logs, stored, password)
	})
	t.Run("initial secret without a password value", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		createSecret(t, kube, ns, settings.DefaultSecretName, partOf, nil)
		createSecret(t, kube, ns, InitialAdminSecretName, nil, map[string][]byte{"other": []byte("x")})
		logs, err := runBootstrapLogged(t, ns, nil)
		require.ErrorContains(t, err, "delete it so that bootstrap generates a new initial admin password", logs)
		assert.NotContains(t, getSecret(t, kube, ns, settings.DefaultSecretName).Data, auth.KeyAdminPassword)
	})
	t.Run("internal token secret without a token", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		empty := createSecret(t, kube, ns, InternalTokenSecretName, partOf, map[string][]byte{InternalTokenSecretKey: []byte("  \n")})
		logs, err := runBootstrapLogged(t, ns, nil)
		require.ErrorContains(t, err, `exists without a "token" value`, logs)
		assert.Equal(t, empty.ResourceVersion, getSecret(t, kube, ns, InternalTokenSecretName).ResourceVersion, "never modified")
	})
}

// TestBootstrapAdminDisabled: no admin password when patrol-cm disables the admin account
// (also fail-closed for a malformed value); the other objects are still created.
func TestBootstrapAdminDisabled(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]map[string]string{
		"admin.enabled false":          {"admin.enabled": "false"},
		"accounts.admin.enabled false": {"accounts.admin.enabled": "false"},
		"malformed value fails closed": {"admin.enabled": "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kube, ns := bootstrapKube(t)
			createSettingsCM(t, kube, ns, data)
			logs, err := runBootstrapLogged(t, ns, nil)
			require.NoError(t, err, logs)
			requireSecretAbsent(t, kube, ns, InitialAdminSecretName)
			sec := getSecret(t, kube, ns, settings.DefaultSecretName)
			assert.NotContains(t, sec.Data, auth.KeyAdminPassword)
			assert.NotContains(t, sec.Data, auth.KeyAdminPasswordMtime)
			assert.Len(t, sec.Data[settings.SecretKeyServerSecretKey], settings.ServerSecretKeyLength)
			getSecret(t, kube, ns, InternalTokenSecretName)
			assert.Contains(t, logs, "admin account disabled in patrol-cm")
		})
	}
}

// TestBootstrapKeepsOperatorMaterial: an operator-provided admin password, signing key and
// internal token are never touched, and no initial admin Secret is created.
func TestBootstrapKeepsOperatorMaterial(t *testing.T) {
	t.Parallel()
	kube, ns := bootstrapKube(t)
	const operatorPassword = "operator-chosen-password"
	const operatorToken = "operator-provided-internal-token-0123456789abcdef"
	hash, err := auth.HashPassword(operatorPassword)
	require.NoError(t, err)
	createSecret(t, kube, ns, settings.DefaultSecretName, partOf, map[string][]byte{
		settings.SecretKeyServerSecretKey: bytes.Repeat([]byte{9}, settings.ServerSecretKeyLength),
		auth.KeyAdminPassword:             []byte(hash),
		auth.KeyAdminPasswordMtime:        []byte("2026-01-02T03:04:05Z"),
	})
	createSecret(t, kube, ns, InternalTokenSecretName, partOf, map[string][]byte{InternalTokenSecretKey: []byte(operatorToken)})
	before := resourceVersions(t, kube, ns, settings.DefaultSecretName, InternalTokenSecretName)

	logs, err := runBootstrapLogged(t, ns, nil)
	require.NoError(t, err, logs)
	assert.Equal(t, before, resourceVersions(t, kube, ns, settings.DefaultSecretName, InternalTokenSecretName))
	requireSecretAbsent(t, kube, ns, InitialAdminSecretName)
	assert.Equal(t, operatorToken, string(getSecret(t, kube, ns, InternalTokenSecretName).Data[InternalTokenSecretKey]))
	require.NoError(t, adminLogin(t, kube, ns, operatorPassword))
	requireNoSecretValues(t, logs, operatorToken, hash)

	// A non-bcrypt admin.password is kept too, with a warning.
	kube2, ns2 := bootstrapKube(t)
	createSecret(t, kube2, ns2, settings.DefaultSecretName, partOf, map[string][]byte{auth.KeyAdminPassword: []byte("not-a-hash")})
	logs, err = runBootstrapLogged(t, ns2, nil)
	require.NoError(t, err, logs)
	assert.Equal(t, "not-a-hash", string(getSecret(t, kube2, ns2, settings.DefaultSecretName).Data[auth.KeyAdminPassword]))
	assert.Contains(t, logs, "admin.password is not a bcrypt hash")
	requireSecretAbsent(t, kube2, ns2, InitialAdminSecretName)
}

// bootstrapPolicyRules is the RBAC contract of docs/architecture/components/bootstrap.md
// ("Kubernetes RBAC"); keep both in sync.
func bootstrapPolicyRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"},
			ResourceNames: []string{settings.DefaultSecretName, InternalTokenSecretName, InitialAdminSecretName}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"update"},
			ResourceNames: []string{settings.DefaultSecretName}},
		// Replacing a stale initial admin Secret (DV-6); index 3 (TestBootstrapRBACContract).
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"delete"},
			ResourceNames: []string{InitialAdminSecretName}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"},
			ResourceNames: []string{settings.DefaultConfigMapName}},
		// Default known hosts (unless --skip-ssh-known-hosts); keep last (TestBootstrapKnownHostsRBAC).
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "update"},
			ResourceNames: []string{settings.DefaultSSHKnownHostsConfigMapName}},
	}
}

// serviceAccountClient returns a client acting as service account sa of ns (impersonation
// by the envtest administrator), bound to a Role with rules.
func serviceAccountClient(t *testing.T, kube kubernetes.Interface, ns, sa string, rules []rbacv1.PolicyRule) kubernetes.Interface {
	t.Helper()
	ctx := context.Background()
	_, err := kube.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa}}, metav1.CreateOptions{})
	require.NoError(t, err)
	if rules != nil {
		_, err = kube.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: sa}, Rules: rules}, metav1.CreateOptions{})
		require.NoError(t, err)
		_, err = kube.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: sa},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: sa},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: ns}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	cfg := rest.CopyConfig(testCfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + ns + ":" + sa,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"},
	}
	c, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	if rules != nil {
		// Wait until the RBAC authorizer sees the binding (get on patrol-cm is in every
		// rule set used here), so the run under test does not race its propagation.
		require.Eventually(t, func() bool {
			_, err := c.CoreV1().ConfigMaps(ns).Get(ctx, settings.DefaultConfigMapName, metav1.GetOptions{})
			return err == nil || apierrors.IsNotFound(err)
		}, 30*time.Second, 100*time.Millisecond, "RBAC binding never became effective")
	}
	return c
}

// TestBootstrapRBACContract runs the bootstrap as a service account holding exactly the
// documented Role (create on Secrets; get on patrol-secret, patrol-internal-token and
// patrol-initial-admin-secret; update on patrol-secret; delete on patrol-initial-admin-secret;
// get on patrol-cm; get/update on patrol-ssh-known-hosts-cm; no list, watch or patch): a fresh
// install, a re-run and a re-install with a stale initial Secret converge. The role cannot
// delete the other two Secrets. Without the update rule, or without the delete rule when the
// initial Secret is stale, the run fails with Forbidden.
func TestBootstrapRBACContract(t *testing.T) {
	t.Parallel()
	runAs := func(t *testing.T, sa kubernetes.Interface, ns string, timeout time.Duration) (string, error) {
		t.Helper()
		buf := &syncBuffer{}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := RunBootstrap(ctx, BootstrapConfig{Namespace: ns, Kube: sa, Logger: slog.New(slog.NewJSONHandler(buf, nil))})
		return buf.String(), err
	}
	// reinstall deletes patrol-secret and renders it again in a later second than the kept
	// initial Secret (helm uninstall + install), as the administrator.
	reinstall := func(t *testing.T, kube kubernetes.Interface, ns string) *corev1.Secret {
		t.Helper()
		initSec := getSecret(t, kube, ns, InitialAdminSecretName)
		require.NoError(t, kube.CoreV1().Secrets(ns).Delete(context.Background(), settings.DefaultSecretName, metav1.DeleteOptions{}))
		waitPastSecond(t, initSec.CreationTimestamp)
		requireNewer(t, createSecret(t, kube, ns, settings.DefaultSecretName, partOf, nil), initSec)
		return initSec
	}
	t.Run("documented role suffices", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		createSettingsCM(t, kube, ns, nil)
		createKnownHostsCM(t, kube, ns, partOf, nil)
		sa := serviceAccountClient(t, kube, ns, "patrol-server", bootstrapPolicyRules())
		for run := 1; run <= 2; run++ {
			logs, err := runAs(t, sa, ns, 60*time.Second)
			require.NoError(t, err, "run %d:\n%s", run, logs)
		}
		password := string(getSecret(t, kube, ns, InitialAdminSecretName).Data[InitialAdminSecretKey])
		require.NoError(t, adminLogin(t, kube, ns, password))
		getSecret(t, kube, ns, InternalTokenSecretName)
		assert.Equal(t, DefaultSSHKnownHosts, getKnownHostsCM(t, kube, ns).Data[settings.SSHKnownHostsKey])

		// Re-install with the kept, stale initial Secret: replaced with the same role.
		stale := reinstall(t, kube, ns)
		logs, err := runAs(t, sa, ns, 60*time.Second)
		require.NoError(t, err, logs)
		assert.Contains(t, logs, "deleted it, generating a new initial admin password")
		replaced := getSecret(t, kube, ns, InitialAdminSecretName)
		assert.NotEqual(t, stale.UID, replaced.UID)
		newPassword := string(replaced.Data[InitialAdminSecretKey])
		require.NoError(t, adminLogin(t, kube, ns, newPassword))
		require.ErrorIs(t, adminLogin(t, kube, ns, password), auth.ErrInvalidCredentials)
		requireNoSecretValues(t, logs, password, newPassword, string(getSecret(t, kube, ns, settings.DefaultSecretName).Data[auth.KeyAdminPassword]))

		// The contract grants no list, and delete only on the initial admin Secret (dry run:
		// authorization is checked, nothing is deleted).
		ctx := context.Background()
		_, err = sa.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		require.True(t, apierrors.IsForbidden(err), err)
		dryRun := metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}
		for _, name := range []string{settings.DefaultSecretName, InternalTokenSecretName} {
			err = sa.CoreV1().Secrets(ns).Delete(ctx, name, dryRun)
			require.True(t, apierrors.IsForbidden(err), "delete %s: %v", name, err)
		}
		require.NoError(t, sa.CoreV1().Secrets(ns).Delete(ctx, InitialAdminSecretName, dryRun))
		getSecret(t, kube, ns, InitialAdminSecretName)
	})
	t.Run("without delete on the initial admin Secret", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		rules := bootstrapPolicyRules()
		require.Equal(t, []string{"delete"}, rules[3].Verbs)
		rules = append(rules[:3], rules[4:]...)
		sa := serviceAccountClient(t, kube, ns, "patrol-server", rules)
		// Fresh install and re-run: the delete rule is not used.
		for run := 1; run <= 2; run++ {
			logs, err := runAs(t, sa, ns, 60*time.Second)
			require.NoError(t, err, "run %d:\n%s", run, logs)
		}
		stale := reinstall(t, kube, ns)
		// The stale Secret cannot be replaced: Forbidden until the deadline (no hash is
		// written from the stale password).
		logs, err := runAs(t, sa, ns, 10*time.Second)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), err)
		assert.Equal(t, stale.UID, getSecret(t, kube, ns, InitialAdminSecretName).UID)
		assert.NotContains(t, getSecret(t, kube, ns, settings.DefaultSecretName).Data, auth.KeyAdminPassword)
		requireNoSecretValues(t, logs+err.Error(), string(stale.Data[InitialAdminSecretKey]))
	})
	t.Run("without update on patrol-secret", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		rules := bootstrapPolicyRules()
		rules = append(rules[:2], rules[3:]...)
		sa := serviceAccountClient(t, kube, ns, "patrol-server", rules)
		// Long enough for a first complete attempt (one bcrypt hash) even on a loaded
		// machine with the race detector; the Forbidden update is then retried until the end.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := RunBootstrap(ctx, BootstrapConfig{Namespace: ns, Kube: sa, Logger: slog.New(slog.DiscardHandler)})
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), err)
	})
}

func TestBootstrapSkipFlags(t *testing.T) {
	t.Parallel()
	t.Run("skip admin", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		logs, err := runBootstrapLogged(t, ns, func(c *BootstrapConfig) { c.SkipAdmin = true })
		require.NoError(t, err, logs)
		requireSecretAbsent(t, kube, ns, InitialAdminSecretName)
		sec := getSecret(t, kube, ns, settings.DefaultSecretName)
		assert.NotContains(t, sec.Data, auth.KeyAdminPassword)
		assert.NotEmpty(t, sec.Data[settings.SecretKeyServerSecretKey])
		getSecret(t, kube, ns, InternalTokenSecretName)
	})
	t.Run("skip internal token", func(t *testing.T) {
		t.Parallel()
		kube, ns := bootstrapKube(t)
		logs, err := runBootstrapLogged(t, ns, func(c *BootstrapConfig) { c.SkipInternalToken = true })
		require.NoError(t, err, logs)
		requireSecretAbsent(t, kube, ns, InternalTokenSecretName)
		getSecret(t, kube, ns, InitialAdminSecretName)
	})
}

// TestBootstrapCommand runs `patrolcd bootstrap` through the command tree (flags, logging
// setup, kubeconfig loading, exit status) and checks that neither stdout nor stderr carries
// a secret value.
func TestBootstrapCommand(t *testing.T) {
	t.Parallel()
	kube, ns := bootstrapKube(t)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, testKubeconfig, 0o600))

	// Main installs the process-wide default logger, which keeps writing to these buffers after it
	// returns (log lines of http servers of tests running in parallel included), so they are
	// synchronized and the lines are checked for structure, not for the component of every line.
	var stdout, stderr syncBuffer
	code := Main("patrolcd", []string{"bootstrap", "--kubeconfig", kubeconfig, "-n", ns, "--logformat", "json", "--loglevel", "debug"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	assert.Empty(t, stdout.String(), "bootstrap prints nothing on stdout")
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	require.NotEmpty(t, lines)
	var bootstrapLines int
	for _, l := range lines {
		require.True(t, strings.HasPrefix(l, "{"), "structured log line: %s", l)
		if strings.Contains(l, `"component":"bootstrap"`) {
			bootstrapLines++
		}
	}
	assert.Positive(t, bootstrapLines, "the bootstrap logs with its component")
	assert.Contains(t, stderr.String(), `"msg":"converged"`)

	password := string(getSecret(t, kube, ns, InitialAdminSecretName).Data[InitialAdminSecretKey])
	token := string(getSecret(t, kube, ns, InternalTokenSecretName).Data[InternalTokenSecretKey])
	hash := string(getSecret(t, kube, ns, settings.DefaultSecretName).Data[auth.KeyAdminPassword])
	requireNoSecretValues(t, stdout.String()+stderr.String(), password, token, hash)

	// Again (text format): converged, nothing written, nothing disclosed.
	before := resourceVersions(t, kube, ns, InternalTokenSecretName, InitialAdminSecretName, settings.DefaultSecretName)
	stdout.Reset()
	stderr.Reset()
	code = Main("patrolcd", []string{"bootstrap", "--kubeconfig", kubeconfig, "--namespace", ns, "--logformat", "text"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	assert.Empty(t, stdout.String())
	assert.Equal(t, before, resourceVersions(t, kube, ns, InternalTokenSecretName, InitialAdminSecretName, settings.DefaultSecretName))
	requireNoSecretValues(t, stdout.String()+stderr.String(), password, token, hash)

	// Failures exit 1: a namespace that does not exist (NotFound is not retried), invalid flags.
	stderr.Reset()
	begin := time.Now()
	code = Main("patrolcd", []string{"bootstrap", "--kubeconfig", kubeconfig, "-n", "no-such-namespace-" + ns, "--timeout", "30s"}, &stdout, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "not found")
	assert.Less(t, time.Since(begin), 20*time.Second, "permanent errors are not retried until the timeout")
	for _, args := range [][]string{
		{"bootstrap", "--kubeconfig", kubeconfig, "-n", ns, "--timeout", "0s"},
		{"bootstrap", "--kubeconfig", kubeconfig, "-n", ns, "--loglevel", "loud"},
		{"bootstrap", "--kubeconfig", kubeconfig, "-n", ns, "--logformat", "xml"},
		{"bootstrap", "--kubeconfig", kubeconfig, "-n", ns, "extra-argument"},
	} {
		stderr.Reset()
		assert.Equal(t, 1, Main("patrolcd", args, &stdout, &stderr), "%v: %s", args, stderr.String())
	}
	assert.Empty(t, stdout.String())
}

func TestBootstrapCommandRegistered(t *testing.T) {
	t.Parallel()
	var stdout, stderr syncBuffer
	require.Equal(t, 0, Main("patrolcd", []string{"--help"}, &stdout, &stderr), stderr.String())
	assert.Regexp(t, `(?m)^  bootstrap\s+Create the first-run Secrets`, stdout.String())
	var found bool
	for _, c := range NewRootCommand().Commands() {
		if c.Name() == "bootstrap" {
			found = true
			for _, f := range []string{"namespace", "skip-admin", "skip-internal-token", "timeout", "kubeconfig", "context", "loglevel", "logformat"} {
				assert.NotNil(t, c.Flags().Lookup(f), "--%s", f)
			}
			assert.Equal(t, DefaultBootstrapTimeout.String(), c.Flags().Lookup("timeout").DefValue)
		}
	}
	assert.True(t, found)
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
