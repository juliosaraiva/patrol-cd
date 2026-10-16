package core_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

func TestMemorySecretStore(t *testing.T) {
	t.Parallel()
	s := core.NewMemorySecretStore(map[string]string{"a": "1"})
	var seen map[string]string
	s.OnUpdate = func(m map[string]string) { seen = m }
	require.NoError(t, s.Update(context.Background(), func(d map[string]string) (map[string]string, error) {
		d["b"] = "2"
		return d, nil
	}))
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, s.Data())
	assert.Equal(t, s.Data(), seen)
	boom := errors.New("boom")
	err := s.Update(context.Background(), func(map[string]string) (map[string]string, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, s.Data(), "a failed mutation changes nothing")
}

func TestKubeSecretStoreRetriesConflicts(t *testing.T) {
	t.Parallel()
	cs := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "patrol-secret", Namespace: "patrol"}, Data: map[string][]byte{"a": []byte("1")}})
	var conflicts atomic.Int32
	cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts.Add(1) <= 2 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "patrol-secret", errors.New("stale"))
		}
		return false, nil, nil
	})
	store := &core.KubeSecretStore{Client: cs, Namespace: "patrol"}
	calls := 0
	require.NoError(t, store.Update(context.Background(), func(d map[string]string) (map[string]string, error) {
		calls++
		d["b"] = "2"
		return d, nil
	}))
	assert.Equal(t, 3, calls, "the mutation is re-applied on a fresh read after each conflict")
	sec, err := cs.CoreV1().Secrets("patrol").Get(context.Background(), "patrol-secret", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "2", string(sec.Data["b"]))

	missing := &core.KubeSecretStore{Client: fake.NewClientset(), Namespace: "patrol"}
	err = missing.Update(context.Background(), func(d map[string]string) (map[string]string, error) { return d, nil })
	require.ErrorIs(t, err, core.ErrSecretMissing)
}

// TestEnvtestSecretStoreConcurrency runs concurrent token creations against a real API
// server: every update lands thanks to resourceVersion-based optimistic concurrency.
func TestEnvtestSecretStoreConcurrency(t *testing.T) {
	t.Parallel()
	cs, err := kubernetes.NewForConfig(envConfig(t))
	require.NoError(t, err)
	ctx := context.Background()
	ns := fmt.Sprintf("apiserver-core-%d", time.Now().UnixNano()%1_000_000)
	_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-secret", Namespace: ns},
		Data:       map[string][]byte{auth.KeyServerSecretKey: []byte(serverKey)},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	store := &core.KubeSecretStore{Client: cs, Namespace: ns}
	acct := auth.Account{Name: "ci", Enabled: true, Capabilities: []auth.Capability{auth.CapabilityAPIKey}}
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			errs[i] = store.Update(ctx, func(d map[string]string) (map[string]string, error) {
				return auth.AddToken(d, acct, auth.TokenMeta{ID: "tok-" + strconv.Itoa(i), IssuedAt: time.Now().Unix()})
			})
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, "patrol-secret", metav1.GetOptions{})
	require.NoError(t, err)
	accts, err := auth.AccountsFromMaps(map[string]string{"accounts.ci": "apiKey"}, auth.SecretData(sec.Data))
	require.NoError(t, err)
	got, err := accts.Get("ci")
	require.NoError(t, err)
	assert.Len(t, got.Tokens, n, "no concurrent update was lost")
	assert.Equal(t, serverKey, string(sec.Data[auth.KeyServerSecretKey]), "unrelated keys are preserved")
}
