package core

import (
	"context"
	"errors"
	"fmt"
	"maps"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// SecretStore applies read-modify-write updates to patrol-secret.
type SecretStore interface {
	// Update reads the Secret, calls mutate with its data (a copy, values decoded to
	// strings) and writes the returned data with the resourceVersion that was read. On a
	// conflict it re-reads and calls mutate again, so mutate must be repeatable (it may
	// mint a fresh token on each attempt). An error from mutate aborts the update.
	Update(ctx context.Context, mutate func(data map[string]string) (map[string]string, error)) error
}

// KubeSecretStore is a SecretStore on a Kubernetes Secret with optimistic concurrency.
type KubeSecretStore struct {
	Client    kubernetes.Interface
	Namespace string
	// Name defaults to settings.DefaultSecretName (patrol-secret).
	Name string
}

// ErrSecretMissing is returned when patrol-secret does not exist.
var ErrSecretMissing = errors.New("settings secret not found")

// Update implements SecretStore. Conflicts are retried with client-go's default backoff
// (5 attempts).
func (s *KubeSecretStore) Update(ctx context.Context, mutate func(map[string]string) (map[string]string, error)) error {
	name := s.Name
	if name == "" {
		name = settings.DefaultSecretName
	}
	secrets := s.Client.CoreV1().Secrets(s.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("%w: %s/%s", ErrSecretMissing, s.Namespace, name)
			}
			return fmt.Errorf("get secret %s: %w", name, err)
		}
		data, err := mutate(auth.SecretData(sec.Data))
		if err != nil {
			return err
		}
		next := sec.DeepCopy()
		next.Data = make(map[string][]byte, len(data))
		for k, v := range data {
			next.Data[k] = []byte(v)
		}
		next.StringData = nil
		if _, err := secrets.Update(ctx, next, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				return err
			}
			return fmt.Errorf("update secret %s: %w", name, err)
		}
		return nil
	})
}

// MemorySecretStore is an in-memory SecretStore for tests and single-process use.
type MemorySecretStore struct {
	data map[string]string
	ch   chan struct{}
	// OnUpdate, when set, is called with a copy of the data after every write.
	OnUpdate func(map[string]string)
}

// NewMemorySecretStore returns a store holding data.
func NewMemorySecretStore(data map[string]string) *MemorySecretStore {
	m := &MemorySecretStore{data: maps.Clone(data), ch: make(chan struct{}, 1)}
	m.ch <- struct{}{}
	return m
}

// Update implements SecretStore.
func (m *MemorySecretStore) Update(ctx context.Context, mutate func(map[string]string) (map[string]string, error)) error {
	select {
	case <-m.ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { m.ch <- struct{}{} }()
	next, err := mutate(maps.Clone(m.data))
	if err != nil {
		return err
	}
	m.data = maps.Clone(next)
	if m.OnUpdate != nil {
		m.OnUpdate(maps.Clone(next))
	}
	return nil
}

// Data returns a copy of the stored data.
func (m *MemorySecretStore) Data() map[string]string {
	<-m.ch
	defer func() { m.ch <- struct{}{} }()
	return maps.Clone(m.data)
}

// secretError maps SecretStore failures to API errors.
func secretError(err error) error {
	switch {
	case errors.Is(err, ErrSecretMissing):
		return rest.FailedPrecondition("the settings Secret does not exist").WithCause(err)
	case apierrors.IsConflict(err):
		return rest.Conflict("the settings Secret was modified concurrently; retry").WithCause(err)
	}
	return err
}
