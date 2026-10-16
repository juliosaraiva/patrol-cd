package settings

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func getSecretKey(t *testing.T, client *fake.Clientset) []byte {
	t.Helper()
	sec, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), DefaultSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	return sec.Data[SecretKeyServerSecretKey]
}

func TestEnsureServerSecretKeyCreatesSecret(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	gen, err := EnsureServerSecretKey(context.Background(), client, testNamespace, WithBootstrapLogger(quietLogger()))
	require.NoError(t, err)
	assert.True(t, gen)
	sec, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), DefaultSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Len(t, sec.Data[SecretKeyServerSecretKey], ServerSecretKeyLength)
	assert.Equal(t, LabelValuePartOf, sec.Labels[LabelKeyPartOf])

	gen, err = EnsureServerSecretKey(context.Background(), client, testNamespace, WithBootstrapLogger(quietLogger()))
	require.NoError(t, err)
	assert.False(t, gen, "an existing key is kept")
}

func TestEnsureServerSecretKeyAddsMissingKey(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(newSecret(DefaultSecretName, map[string]string{"admin.password": "hash"}, true))
	key := bytes.Repeat([]byte{7}, ServerSecretKeyLength)
	gen, err := EnsureServerSecretKey(context.Background(), client, testNamespace, WithRandom(bytes.NewReader(key)), WithBootstrapLogger(quietLogger()))
	require.NoError(t, err)
	assert.True(t, gen)
	assert.Equal(t, key, getSecretKey(t, client))
	sec, err := client.CoreV1().Secrets(testNamespace).Get(context.Background(), DefaultSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("hash"), sec.Data["admin.password"], "other keys are preserved")
}

func TestEnsureServerSecretKeyConflictKeepsOtherReplicasKey(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(newSecret(DefaultSecretName, map[string]string{}, true))
	other := bytes.Repeat([]byte{9}, ServerSecretKeyLength)
	conflicted := false
	client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		// Another replica stores its key first.
		sec := newSecret(DefaultSecretName, map[string]string{SecretKeyServerSecretKey: string(other)}, true)
		if err := client.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, sec, testNamespace); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, DefaultSecretName, errors.New("the object has been modified"))
	})
	gen, err := EnsureServerSecretKey(context.Background(), client, testNamespace, WithBootstrapLogger(quietLogger()))
	require.NoError(t, err)
	assert.False(t, gen, "the other replica's key wins")
	assert.Equal(t, other, getSecretKey(t, client))
}

func TestEnsureServerSecretKeyCreateRace(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	other := bytes.Repeat([]byte{5}, ServerSecretKeyLength)
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sec := newSecret(DefaultSecretName, map[string]string{SecretKeyServerSecretKey: string(other)}, true)
		if err := client.Tracker().Add(sec); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, DefaultSecretName)
	})
	gen, err := EnsureServerSecretKey(context.Background(), client, testNamespace, WithBootstrapLogger(quietLogger()))
	require.NoError(t, err)
	assert.False(t, gen)
	assert.Equal(t, other, getSecretKey(t, client))
}

func TestEnsureServerSecretKeyErrors(t *testing.T) {
	t.Parallel()
	_, err := EnsureServerSecretKey(context.Background(), nil, testNamespace)
	assert.Error(t, err)

	client := fake.NewClientset(newSecret(DefaultSecretName, map[string]string{}, true))
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, DefaultSecretName, errors.New("modified"))
	})
	_, err = EnsureServerSecretKey(context.Background(), client, testNamespace, WithBootstrapLogger(quietLogger()))
	assert.ErrorContains(t, err, "still conflicting after 5 attempts")

	forbidden := fake.NewClientset()
	forbidden.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, DefaultSecretName, errors.New("denied"))
	})
	_, err = EnsureServerSecretKey(context.Background(), forbidden, testNamespace, WithSecretName(DefaultSecretName))
	assert.True(t, apierrors.IsForbidden(err), err)

	short := fake.NewClientset()
	_, err = EnsureServerSecretKey(context.Background(), short, testNamespace, WithRandom(bytes.NewReader([]byte{1})), WithBootstrapLogger(quietLogger()))
	assert.ErrorContains(t, err, "generate server.secretkey")
}
