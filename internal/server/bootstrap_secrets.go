package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// First-run bootstrap (`patrolcd bootstrap`, docs/architecture/components/bootstrap.md): the
// Secrets a fresh installation needs and its default SSH known hosts, created when absent
// and never overwritten.
const (
	// InitialAdminSecretName names the Secret holding the generated initial admin password
	// (upstream argocd-initial-admin-secret). `patrol admin initial-password` reads it.
	InitialAdminSecretName = "patrol-initial-admin-secret"
	// InitialAdminSecretKey is the data key of the initial admin password.
	InitialAdminSecretKey = "password"
	// InitialAdminPasswordLength is the length of a generated initial admin password
	// (characters of [A-Za-z0-9], about 143 bits).
	InitialAdminPasswordLength = 24
	// InternalTokenBytes is the number of random bytes of a generated internal token; the
	// Secret holds them hex encoded (64 characters, safe in an Authorization header).
	InternalTokenBytes = 32
	// DefaultBootstrapTimeout bounds one bootstrap run, retries included.
	DefaultBootstrapTimeout = 60 * time.Second
	// BootstrapFieldManager is the field manager of the bootstrap's own writes.
	BootstrapFieldManager = "patrolcd-bootstrap"
)

const (
	// initialAdminPasswordAlphabet is the alphabet of generated initial admin passwords.
	initialAdminPasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// minInternalTokenLength: an existing shorter token is kept but reported.
	minInternalTokenLength = 32
	// Retry delays of transient failures (doubling, jittered).
	bootstrapRetryInitial = 200 * time.Millisecond
	bootstrapRetryMax     = 5 * time.Second
)

// errBootstrapConfig reports an invalid BootstrapConfig.
var errBootstrapConfig = errors.New("bootstrap: a Kubernetes client and a namespace are required")

// BootstrapConfig configures RunBootstrap.
type BootstrapConfig struct {
	// Namespace is the control-plane namespace holding every managed object.
	Namespace string
	// Kube reaches the control-plane cluster. RunBootstrap needs, in Namespace only (never
	// list or watch): secrets create, get (patrol-secret, patrol-internal-token,
	// patrol-initial-admin-secret), update (patrol-secret) and delete
	// (patrol-initial-admin-secret, to replace a stale one); configmaps get (patrol-cm) and
	// get/update (patrol-ssh-known-hosts-cm, unless SkipSSHKnownHosts). A client with
	// client-side throttling can end a run with the rate limiter's error instead of the last
	// retriable failure (see newBootstrapClient).
	Kube kubernetes.Interface
	// SkipAdmin leaves the initial admin password alone (admin.password and
	// patrol-initial-admin-secret); server.secretkey is still ensured.
	SkipAdmin bool
	// SkipInternalToken leaves Secret patrol-internal-token alone.
	SkipInternalToken bool
	// SkipSSHKnownHosts leaves ConfigMap patrol-ssh-known-hosts-cm alone (known hosts managed
	// by the install's values or another tool).
	SkipSSHKnownHosts bool
	// Logger receives progress (object names and actions only; never values). Nil:
	// slog.Default().
	Logger *slog.Logger
	// Random is the entropy source of generated material (nil: crypto/rand.Reader). Tests
	// only.
	Random io.Reader
	// Now returns the current time (admin.passwordMtime; nil: time.Now).
	Now func() time.Time
}

// RunBootstrap converges the first-run Secrets of the control-plane namespace:
//
//  1. Secret patrol-internal-token (key token): 32 random bytes, hex encoded, created with
//     the Create API; an existing Secret is kept as is (never rotated).
//  2. Secret patrol-secret key server.secretkey (settings.EnsureServerSecretKey).
//  3. Unless SkipSSHKnownHosts: ConfigMap patrol-ssh-known-hosts-cm key ssh_known_hosts set
//     to DefaultSSHKnownHosts when the (labelled) ConfigMap exists without that key (update
//     guarded by the resourceVersion read); a present key is never changed.
//  4. Unless SkipAdmin or patrol-cm disables the admin account: when patrol-secret has no
//     admin.password, a random InitialAdminPasswordLength-character password stored in
//     Secret patrol-initial-admin-secret (Create: exactly one concurrent run wins) and its
//     bcrypt hash plus admin.passwordMtime in patrol-secret (update guarded by the
//     resourceVersion read). An initial Secret left by an interrupted run is reused, so
//     the hash always matches the stored password; one created in an earlier second than
//     patrol-secret predates it (patrol-secret was re-created, e.g. by a re-install) and is
//     replaced (delete guarded by its UID, then Create). An existing admin.password is never
//     changed.
//
// Every step reads before it writes, so a converged namespace is left unchanged (no write
// at all). Concurrent runs are safe: creations race on Create, updates on the
// resourceVersion, and a run that loses re-reads and adopts the winner's material.
// Conflicts and transient API failures (timeouts, throttling, server errors, connection
// failures, and Forbidden/Unauthorized while RBAC propagates) are retried with backoff
// until ctx ends; anything else fails at once. Secret values are never logged or returned
// in errors.
func RunBootstrap(ctx context.Context, cfg BootstrapConfig) error {
	if cfg.Kube == nil || cfg.Namespace == "" {
		return errBootstrapConfig
	}
	b := &bootstrapper{cfg: cfg, log: cfg.Logger, random: cfg.Random, now: cfg.Now}
	if b.log == nil {
		b.log = slog.Default()
	}
	if b.random == nil {
		b.random = rand.Reader
	}
	if b.now == nil {
		b.now = time.Now
	}
	delay := bootstrapRetryInitial
	var last error // last retriable failure
	for attempt := 1; ; attempt++ {
		err := b.runOnce(ctx)
		if err == nil {
			return nil
		}
		if last != nil && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			// The deadline (or a cancellation) cut an attempt short, e.g. a request in
			// flight: report the failure that kept us retrying. A client-side rate limiter
			// that refuses a request because its wait would pass the deadline returns a
			// plain error while ctx is still live; that error is not recognized here and is
			// returned as is (the command's client has no rate limiter, newBootstrapClient).
			return fmt.Errorf("bootstrap did not converge before the deadline: %w (last attempt: %v)", last, err)
		}
		if ctx.Err() != nil || !retriableBootstrapError(err) {
			return err
		}
		last = err
		wait := jitter(delay)
		if apierrors.IsConflict(err) {
			// Expected when replicas start together: another run wrote first.
			b.log.Info("object changed concurrently; re-reading", "attempt", attempt, "retryIn", wait.Round(time.Millisecond))
		} else {
			b.log.Warn("bootstrap step failed; retrying", "attempt", attempt, "retryIn", wait.Round(time.Millisecond), "error", err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("bootstrap did not converge before the deadline: %w", err)
		case <-t.C:
		}
		delay = min(2*delay, bootstrapRetryMax)
	}
}

// jitter returns d scaled by a random factor in [0.5, 1.5).
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(mathrand.Int64N(int64(d))) //nolint:gosec // retry jitter, not security sensitive
}

// retriableBootstrapError reports failures worth another attempt: conflicts, transient API
// server conditions, Forbidden/Unauthorized (RBAC objects applied together with the
// workload may not be effective yet) and connection failures.
func retriableBootstrapError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch {
	case apierrors.IsConflict(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err),
		apierrors.IsTooManyRequests(err), apierrors.IsInternalError(err), apierrors.IsServiceUnavailable(err),
		apierrors.IsUnexpectedServerError(err), apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return true
	}
	// Dial and transport failures (not every net.Error: *url.Error implements it for TLS
	// verification failures too, which do not heal).
	var opErr *net.OpError
	var netErr net.Error
	if errors.As(err, &opErr) || (errors.As(err, &netErr) && netErr.Timeout()) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return utilnet.IsConnectionRefused(err) || utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err)
}

type bootstrapper struct {
	cfg    BootstrapConfig
	log    *slog.Logger
	random io.Reader
	now    func() time.Time
}

// runOnce runs every step once; each is idempotent.
func (b *bootstrapper) runOnce(ctx context.Context) error {
	if b.cfg.SkipInternalToken {
		b.log.Info("skipping the internal token (--skip-internal-token)", "secret", InternalTokenSecretName)
	} else if err := b.ensureInternalToken(ctx); err != nil {
		return err
	}
	generated, err := settings.EnsureServerSecretKey(ctx, b.cfg.Kube, b.cfg.Namespace,
		settings.WithBootstrapLogger(b.log), settings.WithRandom(b.random))
	if err != nil {
		return fmt.Errorf("ensure %s in Secret %s: %w", settings.SecretKeyServerSecretKey, settings.DefaultSecretName, err)
	}
	if !generated {
		b.log.Info("session signing key present; unchanged", "secret", settings.DefaultSecretName, "key", settings.SecretKeyServerSecretKey)
	}
	if b.cfg.SkipSSHKnownHosts {
		b.log.Info("skipping the default known hosts (--skip-ssh-known-hosts)", "configmap", settings.DefaultSSHKnownHostsConfigMapName)
	} else if err := b.ensureSSHKnownHosts(ctx); err != nil {
		return err
	}
	if b.cfg.SkipAdmin {
		b.log.Info("skipping the initial admin password (--skip-admin)")
		return nil
	}
	return b.ensureAdmin(ctx)
}

// ensureInternalToken creates Secret patrol-internal-token when it does not exist and
// otherwise checks that it holds a token (it is never modified).
func (b *bootstrapper) ensureInternalToken(ctx context.Context) error {
	secrets := b.cfg.Kube.CoreV1().Secrets(b.cfg.Namespace)
	sec, err := secrets.Get(ctx, InternalTokenSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		token, terr := newInternalToken(b.random)
		if terr != nil {
			return terr
		}
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      InternalTokenSecretName,
				Namespace: b.cfg.Namespace,
				Labels: map[string]string{
					v1alpha1.LabelKeyPartOf:  v1alpha1.LabelValuePartOf,
					v1alpha1.LabelKeyAppName: InternalTokenSecretName,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{InternalTokenSecretKey: []byte(token)},
		}, metav1.CreateOptions{FieldManager: BootstrapFieldManager})
		if err == nil {
			b.log.Info("created the internal token", "secret", InternalTokenSecretName, "key", InternalTokenSecretKey)
			return nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Secret %s/%s: %w", b.cfg.Namespace, InternalTokenSecretName, err)
		}
		b.log.Info("internal token created concurrently by another run; keeping it", "secret", InternalTokenSecretName)
		sec, err = secrets.Get(ctx, InternalTokenSecretName, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("get Secret %s/%s: %w", b.cfg.Namespace, InternalTokenSecretName, err)
	}
	token := strings.TrimSpace(string(sec.Data[InternalTokenSecretKey]))
	if token == "" {
		return fmt.Errorf("secret %s/%s exists without a %q value: set one, or delete the Secret so that bootstrap generates it",
			b.cfg.Namespace, InternalTokenSecretName, InternalTokenSecretKey)
	}
	if len(token) < minInternalTokenLength {
		b.log.Warn("internal token is shorter than recommended; keeping it", "secret", InternalTokenSecretName, "minLength", minInternalTokenLength)
	}
	b.log.Info("internal token present; unchanged", "secret", InternalTokenSecretName)
	return nil
}

// ensureAdmin stores an initial admin password unless the admin account is disabled or
// already has a password.
func (b *bootstrapper) ensureAdmin(ctx context.Context) error {
	enabled, err := b.adminEnabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		b.log.Info("admin account disabled in patrol-cm; no initial admin password", "configmap", settings.DefaultConfigMapName)
		return nil
	}
	secrets := b.cfg.Kube.CoreV1().Secrets(b.cfg.Namespace)
	sec, err := secrets.Get(ctx, settings.DefaultSecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Secret %s/%s: %w", b.cfg.Namespace, settings.DefaultSecretName, err)
	}
	if hash := sec.Data[auth.KeyAdminPassword]; len(hash) > 0 {
		if _, cerr := bcrypt.Cost(hash); cerr != nil {
			b.log.Warn("admin.password is not a bcrypt hash: admin logins fail until it is fixed", "secret", settings.DefaultSecretName)
		}
		b.log.Info("admin password present; unchanged", "secret", settings.DefaultSecretName, "key", auth.KeyAdminPassword)
		return nil
	}
	password, err := b.initialAdminPassword(ctx, sec.CreationTimestamp)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash the initial admin password: %w", err)
	}
	kv, err := auth.SecretWithPassword(nil, auth.AdminUsername, hash, b.now())
	if err != nil {
		return err
	}
	updated := sec.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string][]byte{}
	}
	for k, v := range kv {
		updated.Data[k] = []byte(v)
	}
	// The update carries the resourceVersion read above: a concurrent writer makes it fail
	// with a conflict, and the retry re-reads patrol-secret (finding the winner's hash).
	if _, err := secrets.Update(ctx, updated, metav1.UpdateOptions{FieldManager: BootstrapFieldManager}); err != nil {
		return fmt.Errorf("update Secret %s/%s: %w", b.cfg.Namespace, settings.DefaultSecretName, err)
	}
	b.log.Info("stored the initial admin password hash", "secret", settings.DefaultSecretName,
		"keys", []string{auth.KeyAdminPassword, auth.KeyAdminPasswordMtime})
	return nil
}

// adminEnabled reads patrol-cm with the account rules of internal/auth: the admin account is
// enabled unless admin.enabled or accounts.admin.enabled is false (or not a boolean: fail
// closed). A missing patrol-cm leaves it enabled (upstream default).
func (b *bootstrapper) adminEnabled(ctx context.Context) (bool, error) {
	var data map[string]string
	cm, err := b.cfg.Kube.CoreV1().ConfigMaps(b.cfg.Namespace).Get(ctx, settings.DefaultConfigMapName, metav1.GetOptions{})
	switch {
	case err == nil:
		data = cm.Data
	case apierrors.IsNotFound(err):
		b.log.Info("patrol-cm not found; the admin account is enabled by default", "configmap", settings.DefaultConfigMapName)
	default:
		return false, fmt.Errorf("get ConfigMap %s/%s: %w", b.cfg.Namespace, settings.DefaultConfigMapName, err)
	}
	// Only patrol-cm keys reach the parser: account errors name keys, never Secret values.
	accounts, perr := auth.AccountsFromMaps(data, nil)
	if perr != nil {
		b.log.Warn("patrol-cm has malformed account settings", "configmap", settings.DefaultConfigMapName, "error", perr)
	}
	return accounts[auth.AdminUsername].Enabled, nil
}

// initialAdminPassword returns the password of Secret patrol-initial-admin-secret, creating
// the Secret with a generated password when it does not exist or is stale (see
// initialSecretStale; settingsCreated is the creationTimestamp of patrol-secret, which has no
// admin.password). Create arbitrates concurrent runs: the losers read the winner's password.
func (b *bootstrapper) initialAdminPassword(ctx context.Context, settingsCreated metav1.Time) (string, error) {
	secrets := b.cfg.Kube.CoreV1().Secrets(b.cfg.Namespace)
	sec, err := secrets.Get(ctx, InitialAdminSecretName, metav1.GetOptions{})
	switch {
	case err == nil && !initialSecretStale(sec, settingsCreated):
		// An earlier run stored the password and stopped before the hash, or a concurrent
		// run is between the two steps: hash the stored password.
		b.log.Info("initial admin password Secret exists; storing the hash of its password", "secret", InitialAdminSecretName)
		return b.storedInitialPassword(sec)
	case err == nil:
		if err := b.deleteStaleInitialSecret(ctx, sec); err != nil {
			return "", err
		}
	case !apierrors.IsNotFound(err):
		return "", fmt.Errorf("get Secret %s/%s: %w", b.cfg.Namespace, InitialAdminSecretName, err)
	}
	password, err := generateInitialAdminPassword(b.random)
	if err != nil {
		return "", err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      InitialAdminSecretName,
			Namespace: b.cfg.Namespace,
			// No part-of label: the settings manager caches part-of Secrets and settings
			// may reference their keys; the plaintext stays out of both.
			Labels: map[string]string{v1alpha1.LabelKeyAppName: InitialAdminSecretName},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{InitialAdminSecretKey: []byte(password)},
	}, metav1.CreateOptions{FieldManager: BootstrapFieldManager})
	if err == nil {
		b.log.Info("generated the initial admin password", "secret", InitialAdminSecretName, "key", InitialAdminSecretKey)
		return password, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create Secret %s/%s: %w", b.cfg.Namespace, InitialAdminSecretName, err)
	}
	b.log.Info("initial admin password created concurrently by another run; using it", "secret", InitialAdminSecretName)
	if sec, err = secrets.Get(ctx, InitialAdminSecretName, metav1.GetOptions{}); err != nil {
		return "", fmt.Errorf("get Secret %s/%s: %w", b.cfg.Namespace, InitialAdminSecretName, err)
	}
	return b.storedInitialPassword(sec)
}

// initialSecretStale reports whether the initial admin Secret was created in an earlier second
// than patrol-secret. ensureAdmin only asks when patrol-secret has no admin.password, and a run
// always creates (or finds) patrol-secret before it creates the initial Secret, so an initial
// Secret older than patrol-secret belongs to an earlier patrol-secret: that one was deleted
// (helm uninstall deletes the chart-rendered Secret, the initial Secret stays) and its admin
// password, possibly changed since, went with it. Re-hashing the old initial password would
// revert the admin password to it (finding DV-6). An initial Secret created after
// patrol-secret is the crash-recovery case (a run stopped before storing the hash) and is
// reused.
//
// Both timestamps are set by the API server at second granularity. Equal or zero timestamps
// (the same second, fake clientsets) count as not stale, which keeps the initial Secret as
// before this check. In an HA control plane the two timestamps can come from different API
// servers; clock skew can only invert the order of objects created within the skew of each
// other (docs/architecture/components/bootstrap.md, "Stale initial admin Secret").
func initialSecretStale(initial *corev1.Secret, settingsCreated metav1.Time) bool {
	created := initial.CreationTimestamp
	if created.IsZero() || settingsCreated.IsZero() {
		return false
	}
	return settingsCreated.After(created.Time)
}

// deleteStaleInitialSecret deletes the stale initial admin Secret read as sec. The UID
// precondition makes the delete fail rather than remove a Secret another run created in the
// meantime; NotFound (another run deleted it) and Conflict (another run already replaced it)
// are expected and the caller's Create then arbitrates as usual.
func (b *bootstrapper) deleteStaleInitialSecret(ctx context.Context, sec *corev1.Secret) error {
	uid := sec.UID
	err := b.cfg.Kube.CoreV1().Secrets(b.cfg.Namespace).Delete(ctx, InitialAdminSecretName,
		metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	switch {
	case err == nil:
		b.log.Warn("initial admin password Secret predates patrol-secret (re-created without admin.password, e.g. by a re-install); deleted it, generating a new initial admin password",
			"secret", InitialAdminSecretName, "settingsSecret", settings.DefaultSecretName)
		return nil
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		b.log.Info("stale initial admin password Secret already deleted or replaced by another run", "secret", InitialAdminSecretName)
		return nil
	default:
		return fmt.Errorf("delete stale Secret %s/%s: %w", b.cfg.Namespace, InitialAdminSecretName, err)
	}
}

// storedInitialPassword returns the password of an existing initial admin Secret, used
// verbatim (its hash must match what `patrol admin initial-password` prints).
func (b *bootstrapper) storedInitialPassword(sec *corev1.Secret) (string, error) {
	p := sec.Data[InitialAdminSecretKey]
	if len(p) == 0 {
		return "", fmt.Errorf("secret %s/%s exists without a %q value: delete it so that bootstrap generates a new initial admin password",
			b.cfg.Namespace, InitialAdminSecretName, InitialAdminSecretKey)
	}
	if len(p) > auth.MaxPasswordBytes {
		return "", fmt.Errorf("secret %s/%s: the %q value is longer than %d bytes",
			b.cfg.Namespace, InitialAdminSecretName, InitialAdminSecretKey, auth.MaxPasswordBytes)
	}
	return string(p), nil
}

// newInternalToken returns InternalTokenBytes random bytes, hex encoded.
func newInternalToken(r io.Reader) (string, error) {
	buf := make([]byte, InternalTokenBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("generate the internal token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// generateInitialAdminPassword returns InitialAdminPasswordLength characters drawn uniformly
// from initialAdminPasswordAlphabet: random bytes at or above the largest multiple of the
// alphabet size (248 for 62 characters) are rejected, so the modulo has no bias.
func generateInitialAdminPassword(r io.Reader) (string, error) {
	return randomString(r, InitialAdminPasswordLength, initialAdminPasswordAlphabet)
}

// randomString returns n characters of alphabet (at most 256 characters) chosen uniformly
// with rejection sampling over the bytes of r.
func randomString(r io.Reader, n int, alphabet string) (string, error) {
	if len(alphabet) == 0 || len(alphabet) > 256 {
		return "", fmt.Errorf("alphabet of %d characters: want 1..256", len(alphabet))
	}
	limit := 256 - 256%len(alphabet) // bytes >= limit are rejected
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2)
	defer clear(buf)
	for len(out) < n {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("generate the initial admin password: %w", err)
		}
		for _, c := range buf {
			if int(c) >= limit {
				continue
			}
			out = append(out, alphabet[int(c)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
