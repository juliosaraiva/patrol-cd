package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Log messages of the stale initial admin Secret path (asserted verbatim).
const (
	msgStaleDeleted  = `"level":"WARN","msg":"initial admin password Secret predates patrol-secret (re-created without admin.password, e.g. by a re-install); deleted it, generating a new initial admin password"`
	msgStaleReplaced = `"msg":"stale initial admin password Secret already deleted or replaced by another run"`
	msgGenerated     = `"msg":"generated the initial admin password"`
	msgHashStored    = `"msg":"stored the initial admin password hash"`
)

// waitPastSecond waits until the wall clock is in a later second than ts. The envtest API
// server runs on this machine and stamps creationTimestamp with its clock at second
// granularity, so an object created afterwards is strictly newer than ts.
func waitPastSecond(t *testing.T, ts metav1.Time) {
	t.Helper()
	if d := time.Until(ts.Truncate(time.Second).Add(time.Second)); d > 0 {
		time.Sleep(d + 50*time.Millisecond)
	}
}

// requireNewer is a setup precondition: newer was created in a later second than older.
func requireNewer(t *testing.T, newer, older *corev1.Secret) {
	t.Helper()
	require.True(t, newer.CreationTimestamp.After(older.CreationTimestamp.Time),
		"setup: %s (%s) must be newer than %s (%s)", newer.Name, newer.CreationTimestamp, older.Name, older.CreationTimestamp)
}

// setAdminPassword stores a new admin hash in patrol-secret as `patrol account
// update-password` does.
func setAdminPassword(t *testing.T, kube kubernetes.Interface, ns, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	require.NoError(t, err)
	kv, err := auth.SecretWithPassword(nil, auth.AdminUsername, hash, time.Now())
	require.NoError(t, err)
	sec := getSecret(t, kube, ns, settings.DefaultSecretName)
	for k, v := range kv {
		sec.Data[k] = []byte(v)
	}
	_, err = kube.CoreV1().Secrets(ns).Update(context.Background(), sec, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// TestBootstrapStaleInitialSecret (finding DV-6): after `helm uninstall` deletes patrol-secret
// (rendered by the chart) and a re-install renders it again without admin.password, the kept
// patrol-initial-admin-secret predates the new patrol-secret. The bootstrap replaces it with a
// new password instead of re-hashing the stale one; neither the stale initial password nor the
// password changed before the uninstall logs in afterwards.
func TestBootstrapStaleInitialSecret(t *testing.T) {
	t.Parallel()
	kube, ns := bootstrapKube(t)
	ctx := context.Background()
	createSettingsCM(t, kube, ns, nil)
	createSecret(t, kube, ns, settings.DefaultSecretName, partOf, nil) // as the chart renders it
	logs, err := runBootstrapLogged(t, ns, nil)
	require.NoError(t, err, logs)
	first := getSecret(t, kube, ns, InitialAdminSecretName)
	firstPassword := string(first.Data[InitialAdminSecretKey])
	require.Regexp(t, initialPasswordRE, firstPassword)

	// The admin changes the password; then `helm uninstall` removes patrol-secret and keeps the
	// initial Secret (created at runtime); the re-install renders patrol-secret again.
	const changed = "Changed0Admin0Password01"
	setAdminPassword(t, kube, ns, changed)
	require.NoError(t, adminLogin(t, kube, ns, changed))
	require.NoError(t, kube.CoreV1().Secrets(ns).Delete(ctx, settings.DefaultSecretName, metav1.DeleteOptions{}))
	waitPastSecond(t, first.CreationTimestamp)
	reinstalled := createSecret(t, kube, ns, settings.DefaultSecretName, partOf, nil)
	requireNewer(t, reinstalled, first)

	logs2, err := runBootstrapLogged(t, ns, nil)
	require.NoError(t, err, logs2)
	second := getSecret(t, kube, ns, InitialAdminSecretName)
	assert.NotEqual(t, first.UID, second.UID, "the stale initial Secret is replaced, not updated")
	newPassword := string(second.Data[InitialAdminSecretKey])
	require.Regexp(t, initialPasswordRE, newPassword)
	require.False(t, firstPassword == newPassword, "a new initial password (values not printed)")
	hash := string(getSecret(t, kube, ns, settings.DefaultSecretName).Data[auth.KeyAdminPassword])
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(newPassword)))
	require.NoError(t, adminLogin(t, kube, ns, newPassword))
	require.ErrorIs(t, adminLogin(t, kube, ns, firstPassword), auth.ErrInvalidCredentials, "stale initial password")
	require.ErrorIs(t, adminLogin(t, kube, ns, changed), auth.ErrInvalidCredentials, "password of the uninstalled release")
	assert.True(t, newPassword == cliInitialPassword(t, ns), "patrol admin initial-password prints the new password")
	assert.Equal(t, 1, strings.Count(logs2, msgStaleDeleted), logs2)
	assert.Equal(t, 1, strings.Count(logs2, msgGenerated), logs2)

	// Converged: a re-run writes nothing.
	names := []string{InternalTokenSecretName, InitialAdminSecretName, settings.DefaultSecretName}
	before := resourceVersions(t, kube, ns, names...)
	logs3, err := runBootstrapLogged(t, ns, nil)
	require.NoError(t, err, logs3)
	assert.Equal(t, before, resourceVersions(t, kube, ns, names...))

	// A run that stops between the new initial Secret and its hash leaves an initial Secret
	// newer than patrol-secret: the next run re-hashes it (crash recovery is unchanged).
	sec := getSecret(t, kube, ns, settings.DefaultSecretName)
	delete(sec.Data, auth.KeyAdminPassword)
	delete(sec.Data, auth.KeyAdminPasswordMtime)
	_, err = kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
	require.NoError(t, err)
	logs4, err := runBootstrapLogged(t, ns, nil)
	require.NoError(t, err, logs4)
	assert.Equal(t, second.ResourceVersion, getSecret(t, kube, ns, InitialAdminSecretName).ResourceVersion, "initial Secret untouched")
	require.NoError(t, adminLogin(t, kube, ns, newPassword))
	assert.NotContains(t, logs4, msgStaleDeleted)

	hash4 := string(getSecret(t, kube, ns, settings.DefaultSecretName).Data[auth.KeyAdminPassword])
	requireNoSecretValues(t, logs+logs2+logs3+logs4, firstPassword, newPassword, changed, hash, hash4)
}

// TestBootstrapStaleInitialSecretConcurrent: 8 runs start together on a namespace with a
// stale initial Secret. Exactly one deletes it (UID precondition), exactly one creates the
// replacement, every run converges on that password and one hash is stored.
func TestBootstrapStaleInitialSecretConcurrent(t *testing.T) {
	t.Parallel()
	kube, ns := bootstrapKube(t)
	createSettingsCM(t, kube, ns, nil)
	const stale = "Stale0Initial0Password01"
	old := createSecret(t, kube, ns, InitialAdminSecretName, nil, map[string][]byte{InitialAdminSecretKey: []byte(stale)})
	waitPastSecond(t, old.CreationTimestamp)
	requireNewer(t, createSecret(t, kube, ns, settings.DefaultSecretName, partOf, nil), old)

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
	for i, err := range errs {
		require.NoError(t, err, "run %d logs:\n%s", i, logs[i])
	}
	t.Logf("stale initial Secret: deleted %d, already replaced %d, generated %d, adopted %d",
		strings.Count(all, msgStaleDeleted), strings.Count(all, msgStaleReplaced), strings.Count(all, msgGenerated),
		strings.Count(all, "initial admin password created concurrently by another run")+
			strings.Count(all, "initial admin password Secret exists; storing the hash"))
	assert.Equal(t, 1, strings.Count(all, msgStaleDeleted), all)
	assert.Equal(t, 1, strings.Count(all, msgGenerated), all)
	assert.Equal(t, 1, strings.Count(all, msgHashStored), all)

	final := getSecret(t, kube, ns, InitialAdminSecretName)
	assert.NotEqual(t, old.UID, final.UID)
	password := string(final.Data[InitialAdminSecretKey])
	require.Regexp(t, initialPasswordRE, password)
	require.False(t, stale == password, "a new initial password (values not printed)")
	hash := string(getSecret(t, kube, ns, settings.DefaultSecretName).Data[auth.KeyAdminPassword])
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)))
	require.NoError(t, adminLogin(t, kube, ns, password))
	require.ErrorIs(t, adminLogin(t, kube, ns, stale), auth.ErrInvalidCredentials)
	requireNoSecretValues(t, all, stale, password, hash)
}

// TestBootstrapStaleTimestamps runs the bootstrap on a fake clientset with explicit
// creationTimestamps: only an initial Secret created in an earlier second than patrol-secret
// is stale. Zero timestamps (fake clientsets, objects without metadata) and equal ones
// (created in the same second) keep the initial Secret, as before the staleness check.
func TestBootstrapStaleTimestamps(t *testing.T) {
	t.Parallel()
	base := metav1.NewTime(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	later := metav1.NewTime(base.Add(time.Second))
	const stored = "Stored0Initial0Password1"
	for _, tc := range []struct {
		name                  string
		settingsTS, initialTS metav1.Time
		wantStale             bool
	}{
		{name: "both zero", wantStale: false},
		{name: "patrol-secret zero", initialTS: base, wantStale: false},
		{name: "initial zero", settingsTS: later, wantStale: false},
		{name: "same second", settingsTS: base, initialTS: base, wantStale: false},
		{name: "initial newer (crash recovery)", settingsTS: base, initialTS: later, wantStale: false},
		{name: "patrol-secret newer (re-install)", settingsTS: later, initialTS: base, wantStale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const initialUID = types.UID("initial-uid-1")
			kube := fake.NewClientset(
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: "patrol",
					CreationTimestamp: tc.settingsTS, Labels: partOf}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: InitialAdminSecretName, Namespace: "patrol",
					CreationTimestamp: tc.initialTS, UID: initialUID},
					Data: map[string][]byte{InitialAdminSecretKey: []byte(stored)}},
			)
			buf := &syncBuffer{}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			require.NoError(t, RunBootstrap(ctx, BootstrapConfig{
				Namespace: "patrol", Kube: kube, SkipSSHKnownHosts: true,
				Logger: slog.New(slog.NewJSONHandler(buf, nil)),
			}))
			logs := buf.String()
			var deletes []k8stesting.DeleteActionImpl
			for _, a := range kube.Actions() {
				if d, ok := a.(k8stesting.DeleteActionImpl); ok && d.GetResource().Resource == "secrets" {
					deletes = append(deletes, d)
				}
			}
			password := string(getSecret(t, kube, "patrol", InitialAdminSecretName).Data[InitialAdminSecretKey])
			hash := getSecret(t, kube, "patrol", settings.DefaultSecretName).Data[auth.KeyAdminPassword]
			require.NoError(t, bcrypt.CompareHashAndPassword(hash, []byte(password)))
			requireNoSecretValues(t, logs, stored, password, string(hash))
			if !tc.wantStale {
				assert.True(t, stored == password, "initial Secret reused (values not printed)")
				assert.Empty(t, deletes)
				assert.NotContains(t, logs, msgStaleDeleted)
				return
			}
			require.Regexp(t, initialPasswordRE, password)
			assert.False(t, stored == password, "a new initial password (values not printed)")
			require.Len(t, deletes, 1)
			assert.Equal(t, InitialAdminSecretName, deletes[0].GetName())
			pre := deletes[0].GetDeleteOptions().Preconditions
			require.NotNil(t, pre, "delete carries a precondition")
			require.NotNil(t, pre.UID)
			assert.Equal(t, initialUID, *pre.UID, "delete guarded by the UID read")
			assert.Contains(t, logs, msgStaleDeleted)
		})
	}
}

// TestBootstrapStaleDeleteConflict: another run replaced the stale initial Secret between this
// run's read and its delete, so the UID-guarded delete fails with Conflict. The run tolerates
// it, its Create fails with AlreadyExists, and it adopts the replacement's password.
func TestBootstrapStaleDeleteConflict(t *testing.T) {
	t.Parallel()
	base := metav1.NewTime(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	const stale, replacement = "Stale0Initial0Password01", "Replacement0Password0001"
	kube := fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: "patrol",
			CreationTimestamp: metav1.NewTime(base.Add(time.Second)), Labels: partOf}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: InitialAdminSecretName, Namespace: "patrol",
			CreationTimestamp: base, UID: "stale-uid"},
			Data: map[string][]byte{InitialAdminSecretKey: []byte(stale)}},
	)
	var once sync.Once
	kube.PrependReactor("delete", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() != InitialAdminSecretName {
			return false, nil, nil
		}
		var err error
		once.Do(func() {
			// The other run deleted the stale Secret and created its replacement first.
			repl := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: InitialAdminSecretName, Namespace: "patrol",
				CreationTimestamp: metav1.NewTime(base.Add(2 * time.Second)), UID: "replacement-uid"},
				Data: map[string][]byte{InitialAdminSecretKey: []byte(replacement)}}
			if uerr := kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), repl, "patrol"); uerr != nil {
				err = uerr
				return
			}
			err = apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, InitialAdminSecretName,
				errors.New("Precondition failed: UID in precondition: stale-uid, UID in object meta: replacement-uid"))
		})
		return err != nil, nil, err
	})
	buf := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, RunBootstrap(ctx, BootstrapConfig{
		Namespace: "patrol", Kube: kube, SkipSSHKnownHosts: true, Logger: slog.New(slog.NewJSONHandler(buf, nil)),
	}))
	logs := buf.String()
	sec := getSecret(t, kube, "patrol", InitialAdminSecretName)
	assert.Equal(t, types.UID("replacement-uid"), sec.UID, "the replacement is kept")
	hash := getSecret(t, kube, "patrol", settings.DefaultSecretName).Data[auth.KeyAdminPassword]
	require.NoError(t, bcrypt.CompareHashAndPassword(hash, []byte(replacement)), "the replacement's password is hashed")
	assert.Contains(t, logs, msgStaleReplaced)
	assert.Contains(t, logs, "initial admin password created concurrently by another run; using it")
	assert.NotContains(t, logs, msgGenerated)
	requireNoSecretValues(t, logs, stale, replacement, string(hash))
}

// TestDeleteStaleInitialSecretPreconditions checks the API server's side of the UID-guarded
// delete: a stale read whose Secret is already gone gets NotFound, and one whose Secret was
// replaced gets Conflict (the replacement is not deleted). deleteStaleInitialSecret
// tolerates both.
func TestDeleteStaleInitialSecretPreconditions(t *testing.T) {
	t.Parallel()
	kube, ns := bootstrapKube(t)
	ctx := context.Background()
	secrets := kube.CoreV1().Secrets(ns)
	const stalePassword = "Stale0Initial0Password01"
	data := map[string][]byte{InitialAdminSecretKey: []byte(stalePassword)}
	stale := createSecret(t, kube, ns, InitialAdminSecretName, nil, data)
	require.NoError(t, secrets.Delete(ctx, InitialAdminSecretName, metav1.DeleteOptions{}))
	buf := &syncBuffer{}
	b := &bootstrapper{cfg: BootstrapConfig{Namespace: ns, Kube: kube}, log: slog.New(slog.NewJSONHandler(buf, nil))}

	require.NoError(t, b.deleteStaleInitialSecret(ctx, stale), "already deleted (NotFound)")

	repl := createSecret(t, kube, ns, InitialAdminSecretName, nil, data)
	_, err := secrets.Get(ctx, InitialAdminSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	// What the API server answers to the stale UID: Conflict.
	uid := stale.UID
	err = secrets.Delete(ctx, InitialAdminSecretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	require.True(t, apierrors.IsConflict(err), "want Conflict, got %v", err)
	require.NoError(t, b.deleteStaleInitialSecret(ctx, stale), "replaced (Conflict)")
	assert.Equal(t, repl.UID, getSecret(t, kube, ns, InitialAdminSecretName).UID, "the replacement is not deleted")
	assert.Equal(t, 2, strings.Count(buf.String(), msgStaleReplaced), buf.String())
	requireNoSecretValues(t, buf.String(), stalePassword)
}
