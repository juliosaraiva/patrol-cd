package settings

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// ServerSecretKeyLength is the length of a generated server.secretkey (upstream:
// util.MakeSignature(32)).
const ServerSecretKeyLength = 32

const maxBootstrapAttempts = 5

// BootstrapOption customizes EnsureServerSecretKey.
type BootstrapOption func(*bootstrapOptions)

type bootstrapOptions struct {
	secretName string
	random     io.Reader
	log        *slog.Logger
}

// WithSecretName selects the Secret (default patrol-secret).
func WithSecretName(name string) BootstrapOption {
	return func(o *bootstrapOptions) { o.secretName = name }
}

// WithRandom replaces the random source (tests only).
func WithRandom(r io.Reader) BootstrapOption { return func(o *bootstrapOptions) { o.random = r } }

// WithBootstrapLogger sets the logger (default slog.Default()).
func WithBootstrapLogger(l *slog.Logger) BootstrapOption {
	return func(o *bootstrapOptions) { o.log = l }
}

// EnsureServerSecretKey makes sure the settings Secret holds a non-empty server.secretkey,
// generating 32 random bytes when it is missing (upstream InitializeSettings). It creates the
// Secret when it does not exist (labelled app.kubernetes.io/part-of=patrol-cd) and otherwise
// updates it with the resourceVersion it read, so concurrent replicas cannot overwrite each
// other's key: on a conflict the Secret is read again and the other replica's key is kept.
// An existing key is never replaced. generated reports whether this call stored a new key.
func EnsureServerSecretKey(ctx context.Context, client kubernetes.Interface, namespace string, opts ...BootstrapOption) (generated bool, err error) {
	o := bootstrapOptions{secretName: DefaultSecretName, random: rand.Reader, log: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	if client == nil || namespace == "" {
		return false, fmt.Errorf("ensure %s: client and namespace are required", SecretKeyServerSecretKey)
	}
	secrets := client.CoreV1().Secrets(namespace)
	for attempt := 1; attempt <= maxBootstrapAttempts; attempt++ {
		sec, err := secrets.Get(ctx, o.secretName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			key, kerr := newServerKey(o.random)
			if kerr != nil {
				return false, kerr
			}
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      o.secretName,
					Namespace: namespace,
					Labels: map[string]string{
						v1alpha1.LabelKeyPartOf:  v1alpha1.LabelValuePartOf,
						v1alpha1.LabelKeyAppName: o.secretName,
					},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{SecretKeyServerSecretKey: key},
			}, metav1.CreateOptions{})
			if err == nil {
				o.log.Info("initialized server signature", "secret", o.secretName)
				return true, nil
			}
			if apierrors.IsAlreadyExists(err) {
				o.log.Warn("conflict when initializing settings, assuming updated by another replica", "secret", o.secretName)
				continue
			}
			return false, fmt.Errorf("create Secret %s/%s: %w", namespace, o.secretName, err)
		case err != nil:
			return false, fmt.Errorf("get Secret %s/%s: %w", namespace, o.secretName, err)
		}
		if len(sec.Data[SecretKeyServerSecretKey]) > 0 {
			return false, nil
		}
		key, kerr := newServerKey(o.random)
		if kerr != nil {
			return false, kerr
		}
		updated := sec.DeepCopy()
		if updated.Data == nil {
			updated.Data = map[string][]byte{}
		}
		updated.Data[SecretKeyServerSecretKey] = key
		if _, err := secrets.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				o.log.Warn("conflict when initializing settings, assuming updated by another replica", "secret", o.secretName)
				continue
			}
			return false, fmt.Errorf("update Secret %s/%s: %w", namespace, o.secretName, err)
		}
		o.log.Info("initialized server signature", "secret", o.secretName)
		return true, nil
	}
	return false, fmt.Errorf("ensure %s in Secret %s/%s: still conflicting after %d attempts", SecretKeyServerSecretKey, namespace, o.secretName, maxBootstrapAttempts)
}

func newServerKey(r io.Reader) ([]byte, error) {
	key := make([]byte, ServerSecretKeyLength)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("generate %s: %w", SecretKeyServerSecretKey, err)
	}
	return key, nil
}
