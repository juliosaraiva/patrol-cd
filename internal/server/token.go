package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Internal token (ADR-0006): the shared bearer token of the repo-server, commit-server,
// cmp-server and controller tree API, held by Secret patrol-internal-token.
const (
	// InternalTokenSecretName names the Secret holding the token.
	InternalTokenSecretName = "patrol-internal-token"
	// InternalTokenSecretKey is the Secret key holding the token.
	InternalTokenSecretKey = "token"
	// DefaultInternalTokenFile is where the install manifests mount the Secret key.
	DefaultInternalTokenFile = "/app/config/internal-token/" + InternalTokenSecretKey
	// EnvInternalTokenFile overrides the default token file.
	EnvInternalTokenFile = "PATROL_INTERNAL_TOKEN_FILE"
	// maxTokenBytes bounds the token file.
	maxTokenBytes = 64 << 10
	// tokenReadTimeout bounds reading the Secret.
	tokenReadTimeout = 15 * time.Second
)

// errNoToken reports a missing internal token.
var errNoToken = errors.New("no internal token: mount Secret " + InternalTokenSecretName + " (key " + InternalTokenSecretKey + ") and pass --internal-token-file")

// defaultTokenFile is the token file used when the flag is not given.
func defaultTokenFile() string {
	if p := os.Getenv(EnvInternalTokenFile); p != "" {
		return p
	}
	return DefaultInternalTokenFile
}

// readTokenFile reads a bounded, non-empty token file.
func readTokenFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("read internal token file: %w", err)
	}
	if len(data) > maxTokenBytes {
		return "", fmt.Errorf("internal token file %s exceeds %d bytes", path, maxTokenBytes)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("internal token file %s is empty", path)
	}
	return tok, nil
}

// loadInternalToken returns the internal token: the file when it exists; otherwise, when
// the file is the default one (not given explicitly) and kube is set, the Secret of the
// control-plane namespace (development against a cluster without mounted Secrets). The
// token is never logged.
func loadInternalToken(ctx context.Context, path string, explicit bool, kube kubernetes.Interface, namespace string) (string, error) {
	if path == "" {
		path = defaultTokenFile()
	}
	tok, err := readTokenFile(path)
	if err == nil {
		return tok, nil
	}
	if !errors.Is(err, fs.ErrNotExist) || explicit || kube == nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w (%s does not exist)", errNoToken, path)
		}
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, tokenReadTimeout)
	defer cancel()
	sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, InternalTokenSecretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w (neither %s nor Secret %s/%s exists)", errNoToken, path, namespace, InternalTokenSecretName)
		}
		return "", fmt.Errorf("read Secret %s/%s: %w", namespace, InternalTokenSecretName, err)
	}
	tok = strings.TrimSpace(string(sec.Data[InternalTokenSecretKey]))
	if tok == "" {
		return "", fmt.Errorf("%w (Secret %s/%s has no %q key)", errNoToken, namespace, InternalTokenSecretName, InternalTokenSecretKey)
	}
	return tok, nil
}
