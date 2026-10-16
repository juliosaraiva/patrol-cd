package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

// Defaults (upstream values where upstream has them).
const (
	// DefaultMaxPayloadSize is webhook.maxPayloadSizeMB's default (50 MB).
	DefaultMaxPayloadSize = settings.DefaultWebhookMaxPayloadSizeMB * 1024 * 1024
	// DefaultWorkers is server.webhook.parallelism.limit's default.
	DefaultWorkers = 50
	// DefaultRefreshWorkers is server.webhook.refresh.workers' default.
	DefaultRefreshWorkers = 20
	// DefaultQueueSize is upstream's payload queue capacity.
	DefaultQueueSize = 50000
	// DefaultMaxQueuedBytes bounds the memory held by queued events (estimated from
	// their raw body sizes); a full budget answers 503 like a full queue.
	DefaultMaxQueuedBytes = 256 << 20
	// DefaultMaxInFlightBodyBytes bounds the request bodies being read and verified at
	// the same time, across all sources; an exhausted budget answers 503.
	DefaultMaxInFlightBodyBytes = 128 << 20
	// MaxEffectivePayloadSize is the largest body any provider accepts, whatever
	// webhook.maxPayloadSizeMB says: the pkg/scm verifiers reject bodies over
	// scm.MaxWebhookPayloadBytes (25 MiB, GitHub's own cap). Larger settings are clamped.
	MaxEffectivePayloadSize = scm.MaxWebhookPayloadBytes
	// DefaultDeliveryCacheSize and DefaultDeliveryTTL bound the redelivery cache.
	DefaultDeliveryCacheSize = 10000
	DefaultDeliveryTTL       = time.Hour
	// DefaultAPITimeout bounds each Kubernetes API call.
	DefaultAPITimeout = 10 * time.Second
	// DefaultShutdownTimeout bounds draining the queues when Start's context ends.
	DefaultShutdownTimeout = 30 * time.Second
	// DefaultPathPrefix is where the API server mounts the handler.
	DefaultPathPrefix = "/api/webhook"
	// maxSecretCandidates bounds the secrets tried per delivery (HMAC over up to 25 MiB
	// per candidate).
	maxSecretCandidates = 16
	// refreshAttempts and refreshRetryInterval: conflict retries of refresh patches, as
	// upstream (5 attempts, 100 ms apart).
	refreshAttempts      = 5
	refreshRetryInterval = 100 * time.Millisecond
)

// SettingsSource returns the current webhook settings: secrets of patrol-secret, the
// payload size limit and the refresh jitter. It is read for every request, so secret
// rotation and limit changes apply without a restart.
type SettingsSource func() settings.Webhook

// CurrentSettings adapts a settings snapshot provider (*settings.Manager) to a
// SettingsSource. Before the first snapshot it returns the defaults with SecretsLoaded
// false, so deliveries that no SCMProvider secret verifies are answered 503 until
// patrol-secret has been read.
func CurrentSettings(m interface{ Current() *settings.Settings }) SettingsSource {
	return func() settings.Webhook {
		if s := m.Current(); s != nil {
			return s.Webhook()
		}
		return settings.Webhook{MaxPayloadSize: DefaultMaxPayloadSize, RefreshJitterThreshold: settings.DefaultWebhookRefreshJitterThreshold}
	}
}

// GeneratorEvaluator evaluates ApplicationSet generators for Matrix webhook matching
// (ASET-107): the first child's parameter sets interpolate the second child, which is
// then checked against the event, as the ApplicationSet controller does. internal/appset
// provides the implementation. Without one, templated fields ("{{...}}") of the second
// child of a Matrix are treated as matching any value, so such ApplicationSets are
// refreshed on every event their non-templated fields allow (an over-approximation;
// refreshes are idempotent).
type GeneratorEvaluator interface {
	// GenerateParams returns the parameter sets gen produces for appSet.
	GenerateParams(ctx context.Context, appSet *v1alpha1.ApplicationSet, gen *v1alpha1.ApplicationSetGenerator) ([]map[string]any, error)
	// InterpolateGenerator renders the templated fields of gen with params, using
	// appSet's templating options (goTemplate, goTemplateOptions).
	InterpolateGenerator(appSet *v1alpha1.ApplicationSet, gen *v1alpha1.ApplicationSetGenerator, params map[string]any) (*v1alpha1.ApplicationSetGenerator, error)
}

// Config configures a Handler. Namespace, Client and Settings are required.
type Config struct {
	// Namespace is the control-plane namespace. Applications and ApplicationSets there are
	// always considered; SCMProvider webhook secrets are read there by default.
	Namespace string
	// ApplicationNamespaces are additional namespaces (globs, or /regex/) whose
	// Applications webhooks refresh (application.namespaces). Empty: the control-plane
	// namespace only.
	ApplicationNamespaces []string
	// ApplicationSetNamespaces are additional namespaces (same syntax) whose
	// ApplicationSets webhooks refresh. Empty: the control-plane namespace only.
	ApplicationSetNamespaces []string
	// SCMProviderNamespaces lists the namespaces whose SCMProvider webhook.secretRef
	// secrets verify deliveries (default: Namespace only). Every listed namespace is
	// trusted to authenticate deliveries for every repository of its provider type: do
	// not list tenant namespaces unless their owners are trusted with that.
	SCMProviderNamespaces []string

	// Client writes refresh annotations with JSON merge patches.
	Client client.Client
	// Reader lists Applications, ApplicationSets and SCMProviders and reads Secrets
	// (default Client). A cache-backed reader is recommended.
	Reader client.Reader
	// APIReader reads an Application right before its refresh patch, whose
	// metadata.resourceVersion makes the patch conditional (default Client). A live,
	// uncached reader (controller-runtime manager.GetAPIReader()) is recommended: with a
	// lagging cache the conditional patch conflicts until the cache catches up, and the
	// refresh fails after the retries (it is never applied over a newer value).
	APIReader client.Reader
	// Settings returns the current webhook settings (required).
	Settings SettingsSource

	// RequireSignatures rejects every delivery that cannot be verified, even when no
	// webhook secret is configured at all (Patrol extension).
	RequireSignatures bool
	// AllowUnsignedPerProvider restores upstream's per-provider policy: a delivery whose
	// provider has no configured secret is accepted unverified even when other providers
	// have secrets. By default (false) unsigned deliveries are accepted only while no
	// webhook secret is configured anywhere (patrol-secret or a trusted SCMProvider):
	// the provider is chosen by request headers and the repository by the unsigned
	// payload, so a forged delivery routed to a provider without a secret could
	// otherwise refresh repositories hosted on a provider that has one.
	AllowUnsignedPerProvider bool

	// Workers process accepted events (server.webhook.parallelism.limit, default 50).
	Workers int
	// RefreshWorkers apply refresh patches (server.webhook.refresh.workers, default 20).
	RefreshWorkers int
	// QueueSize bounds queued events (default 50000).
	QueueSize int
	// MaxQueuedBytes bounds the estimated memory of queued events (default 256 MiB).
	MaxQueuedBytes int64
	// MaxInFlightBodyBytes bounds the bytes of request bodies being read and verified
	// concurrently (default 128 MiB; at least MaxEffectivePayloadSize). A request is
	// charged its Content-Length before reading, or the bytes read so far without one.
	MaxInFlightBodyBytes int64
	// RateLimit configures per-source rate limiting.
	RateLimit RateLimit
	// SourceKey identifies the source of a request for rate limiting (default
	// RemoteAddrSource).
	SourceKey func(*http.Request) string
	// DeliveryCacheSize and DeliveryTTL bound the redelivery cache.
	DeliveryCacheSize int
	DeliveryTTL       time.Duration

	// GeneratorEvaluator evaluates Matrix generators (optional, see the type).
	GeneratorEvaluator GeneratorEvaluator
	// BitbucketCloud configures the changed-files callback for Bitbucket Cloud pushes.
	BitbucketCloud BitbucketCloudOptions

	// PathPrefix is the mount path (default /api/webhook); a further path segment selects
	// the provider explicitly.
	PathPrefix string
	// APITimeout bounds each Kubernetes API call (default 10s).
	APITimeout time.Duration
	// ShutdownTimeout bounds queue draining when Start's context ends (default 30s).
	ShutdownTimeout time.Duration

	// Registerer receives the webhook metrics (nil: not registered).
	Registerer prometheus.Registerer
	// Logger receives audit and diagnostic logs (default slog.Default()).
	Logger *slog.Logger

	// randFloat returns a number in [0, 1) for refresh jitter (tests).
	randFloat func() float64
}

func (c Config) withDefaults() (Config, error) {
	if c.Namespace == "" {
		return c, errors.New("webhook: Namespace is required")
	}
	if c.Client == nil {
		return c, errors.New("webhook: Client is required")
	}
	if c.Settings == nil {
		return c, errors.New("webhook: Settings is required")
	}
	if c.Reader == nil {
		c.Reader = c.Client
	}
	if c.APIReader == nil {
		c.APIReader = c.Client
	}
	if c.MaxInFlightBodyBytes == 0 {
		c.MaxInFlightBodyBytes = DefaultMaxInFlightBodyBytes
	}
	if c.MaxInFlightBodyBytes < MaxEffectivePayloadSize {
		return c, fmt.Errorf("webhook: MaxInFlightBodyBytes must be at least %d (one maximum-size delivery)", int64(MaxEffectivePayloadSize))
	}
	if c.SCMProviderNamespaces == nil {
		c.SCMProviderNamespaces = []string{c.Namespace}
	}
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.RefreshWorkers <= 0 {
		c.RefreshWorkers = DefaultRefreshWorkers
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.MaxQueuedBytes <= 0 {
		c.MaxQueuedBytes = DefaultMaxQueuedBytes
	}
	if c.SourceKey == nil {
		c.SourceKey = RemoteAddrSource
	}
	if c.DeliveryCacheSize <= 0 {
		c.DeliveryCacheSize = DefaultDeliveryCacheSize
	}
	if c.DeliveryTTL <= 0 {
		c.DeliveryTTL = DefaultDeliveryTTL
	}
	if c.PathPrefix == "" {
		c.PathPrefix = DefaultPathPrefix
	}
	if c.APITimeout <= 0 {
		c.APITimeout = DefaultAPITimeout
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c, nil
}
