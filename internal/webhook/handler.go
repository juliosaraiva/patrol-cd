package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
)

// Handler serves POST /api/webhook and /api/webhook/{provider} and applies the refresh
// effects of accepted events. Create it with NewHandler, mount it with Register (or as an
// http.Handler) and run its workers with Start.
type Handler struct {
	cfg        Config
	log        *slog.Logger
	metrics    *metrics
	limiter    *sourceLimiter
	deliveries *scm.DeliveryCache
	appNS      *namespaceMatcher
	appSetNS   *namespaceMatcher
	bitbucket  *bitbucketRefiner
	randFloat  func() float64

	events      chan *queuedEvent
	queuedBytes atomic.Int64
	// bodies bounds the request bodies being read and verified concurrently.
	bodies *byteBudget
	// clampWarned is set once the configured payload limit was reported as clamped.
	clampWarned atomic.Bool
	refreshQ    workqueue.TypedDelayingInterface[refreshRequest]
	closing     atomic.Bool
	started     atomic.Bool
}

// queuedEvent is an accepted delivery waiting for a worker.
type queuedEvent struct {
	event       *scm.Event
	registry    *registryEvent
	provider    string
	deliveryKey string
	size        int64
}

// refreshRequest is one refresh annotation to apply. It is comparable, so the refresh
// queue collapses duplicate requests for the same object.
type refreshRequest struct {
	kind      string
	namespace string
	name      string
	hydrate   bool
}

// NewHandler validates cfg and builds a Handler. It starts no goroutines (see Start).
func NewHandler(cfg Config) (*Handler, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	m, err := newMetrics(cfg.Registerer)
	if err != nil {
		return nil, err
	}
	limiter, err := newSourceLimiter(cfg.RateLimit)
	if err != nil {
		return nil, fmt.Errorf("webhook: rate limiter: %w", err)
	}
	log := cfg.Logger.With("component", "webhook")
	bb, err := newBitbucketRefiner(cfg.BitbucketCloud, log)
	if err != nil {
		return nil, err
	}
	appNS, warns := newNamespaceMatcher(cfg.Namespace, cfg.ApplicationNamespaces)
	appSetNS, warns2 := newNamespaceMatcher(cfg.Namespace, cfg.ApplicationSetNamespaces)
	for _, w := range append(warns, warns2...) {
		log.Warn("webhook: " + w)
	}
	rf := cfg.randFloat
	if rf == nil {
		rf = rand.Float64
	}
	h := &Handler{
		cfg:        cfg,
		log:        log,
		metrics:    m,
		limiter:    limiter,
		deliveries: scm.NewDeliveryCache(cfg.DeliveryCacheSize, cfg.DeliveryTTL),
		appNS:      appNS,
		appSetNS:   appSetNS,
		bitbucket:  bb,
		randFloat:  rf,
		events:     make(chan *queuedEvent, cfg.QueueSize),
		bodies:     &byteBudget{limit: cfg.MaxInFlightBodyBytes},
		refreshQ: workqueue.NewTypedDelayingQueueWithConfig(workqueue.TypedDelayingQueueConfig[refreshRequest]{
			Name: "webhook-refresh",
		}),
	}
	return h, nil
}

// Register mounts the handler on mux at the path prefix and below it.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle(h.cfg.PathPrefix, h)
	mux.Handle(h.cfg.PathPrefix+"/", h)
}

// Start runs the event and refresh workers until ctx ends, then stops accepting events
// (503), drains the queues for at most Config.ShutdownTimeout and returns. It satisfies
// controller-runtime's manager.Runnable. A Handler can be started once.
func (h *Handler) Start(ctx context.Context) error {
	if !h.started.CompareAndSwap(false, true) {
		return errors.New("webhook: handler already started")
	}
	// Workers keep a live context while draining; it is cancelled when draining ends or
	// times out.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	stop := make(chan struct{})
	var events, refreshes sync.WaitGroup
	for range h.cfg.Workers {
		events.Go(func() { h.eventWorker(workCtx, stop) })
	}
	for range h.cfg.RefreshWorkers {
		refreshes.Go(func() { h.refreshWorker(workCtx) })
	}
	h.log.Info("webhook: workers started", "workers", h.cfg.Workers, "refreshWorkers", h.cfg.RefreshWorkers)
	<-ctx.Done()
	h.closing.Store(true)
	close(stop)
	drained := make(chan struct{})
	go func() {
		events.Wait()
		h.refreshQ.ShutDownWithDrain()
		refreshes.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(h.cfg.ShutdownTimeout):
		h.log.Warn("webhook: shutdown timeout; abandoning queued refreshes (polling recovers them)")
		cancelWork()
		h.refreshQ.ShutDown()
		<-drained
	}
	h.log.Info("webhook: workers stopped")
	return nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	providerName := providerUnknown
	result := resultError
	defer func() { h.metrics.requests.WithLabelValues(providerName, result).Inc() }()

	if !h.limiter.allow(h.cfg.SourceKey(r)) {
		result = resultRateLimited
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Too many webhook requests", http.StatusTooManyRequests)
		return
	}
	p, known := h.resolveProvider(r)
	if !known {
		result = resultUnknownProvider
		http.Error(w, "Unknown webhook provider", http.StatusNotFound)
		return
	}
	if p == nil {
		result = resultUnknownProvider
		http.Error(w, "Unknown webhook event", http.StatusBadRequest)
		return
	}
	providerName = p.name
	if r.Method != http.MethodPost {
		result = resultMethodNotAllowed
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Webhook processing failed", http.StatusMethodNotAllowed)
		return
	}
	ws := h.cfg.Settings()
	maxBytes := h.payloadLimit(ws.MaxPayloadSize)
	tooLarge := fmt.Sprintf("Webhook processing failed: payload must be valid JSON under %d MB", maxBytes/1024/1024)
	if r.ContentLength > maxBytes {
		result = resultTooLarge
		h.log.Warn("webhook: payload too large", "provider", p.name, "limitBytes", maxBytes, "contentLength", r.ContentLength, "security", "high")
		http.Error(w, tooLarge, http.StatusBadRequest)
		return
	}
	// The in-flight body budget bounds the memory (and HMAC work) of concurrent
	// deliveries across all sources: a known Content-Length is charged before reading,
	// an unknown one while reading. Everything is released when the request ends.
	if !h.bodies.tryAcquire(r.ContentLength) {
		result = resultBusy
		h.log.Warn("webhook: in-flight body budget exhausted", "provider", p.name, "contentLength", r.ContentLength, "budgetBytes", h.bodies.limit)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Webhook server is busy", http.StatusServiceUnavailable)
		return
	}
	defer h.bodies.release(r.ContentLength)
	// A body never exceeds its Content-Length (net/http enforces it; the limit keeps the
	// charge exact for requests built elsewhere).
	src := io.LimitReader(r.Body, r.ContentLength)
	if r.ContentLength < 0 {
		br := &budgetReader{r: r.Body, budget: h.bodies}
		defer func() { h.bodies.release(br.charged) }()
		src = br
	}
	body, err := scm.ReadWebhookBody(src, maxBytes)
	if err != nil {
		switch {
		case errors.Is(err, scm.ErrPayloadTooLarge):
			result = resultTooLarge
			h.log.Warn("webhook: payload too large", "provider", p.name, "limitBytes", maxBytes, "security", "high")
			http.Error(w, tooLarge, http.StatusBadRequest)
		case errors.Is(err, errBodyBudget):
			result = resultBusy
			h.log.Warn("webhook: in-flight body budget exhausted", "provider", p.name, "budgetBytes", h.bodies.limit)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Webhook server is busy", http.StatusServiceUnavailable)
		default:
			result = resultInvalid
			h.log.Info("webhook: reading body failed", "provider", p.name, "error", logErr(err))
			http.Error(w, "Webhook processing failed", http.StatusBadRequest)
		}
		return
	}
	secrets, err := h.secretsFor(r.Context(), p, ws)
	if err != nil {
		result = resultError
		h.log.Error("webhook: cannot determine webhook secrets", "provider", p.name, "error", logErr(err))
		http.Error(w, "Webhook processing failed", http.StatusServiceUnavailable)
		return
	}
	if !secrets.configured {
		// The delivery cannot be verified. It is accepted only when no webhook secret is
		// configured anywhere (or the operator chose upstream's per-provider policy): a
		// secret configured for one provider must not be bypassable by routing a forged
		// delivery for the same repository through another provider.
		reason := ""
		switch {
		case h.cfg.RequireSignatures:
			reason = "no secret is configured for the provider and signatures are required"
		case secrets.otherConfigured && !h.cfg.AllowUnsignedPerProvider:
			reason = "no secret is configured for the provider but webhook secrets are configured for other providers"
		}
		if reason != "" {
			result = resultUnsigned
			h.log.Warn("webhook: delivery rejected: "+reason, "provider", p.name, "security", "high")
			h.unauthorized(w, p)
			return
		}
	}
	if secrets.configured && len(secrets.candidates) == 0 && secrets.notLoaded {
		// patrol-secret has not been loaded yet (and no SCMProvider secret can verify the
		// delivery): answer a retriable 503 rather than accepting it unverified.
		result = resultSecretsUnavailable
		h.log.Warn("webhook: delivery rejected: webhook secrets are not loaded yet", "provider", p.name, "security", "high")
		w.Header().Set("Retry-After", "10")
		http.Error(w, "Webhook secrets are not loaded yet", http.StatusServiceUnavailable)
		return
	}
	if secrets.configured && len(secrets.candidates) == 0 {
		// Every configured secret is unavailable: fail closed instead of accepting
		// unsigned deliveries.
		result = resultUnauthorized
		h.log.Warn("webhook: delivery rejected: the configured webhook secrets are unavailable", "provider", p.name, "unresolved", secrets.unresolved, "security", "high")
		h.unauthorized(w, p)
		return
	}
	if p.verifier == nil {
		result = h.serveRegistry(w, r, p, body, secrets.candidates)
		return
	}
	ev, err := verify(p, r.Header, body, secrets.candidates)
	if err != nil {
		result = h.reject(w, p, err, tooLarge)
		return
	}
	result = h.accept(w, p, ev, int64(len(body)))
}

// unauthorized answers a delivery that cannot be authenticated like upstream answers
// signature failures: 400 "Webhook processing failed", or 401 "Unauthorized" for
// registry (GHCR) events.
func (h *Handler) unauthorized(w http.ResponseWriter, p *provider) {
	if p.name == ProviderGHCR {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	http.Error(w, "Webhook processing failed", http.StatusBadRequest)
}

// payloadLimit returns the effective body limit: webhook.maxPayloadSizeMB (default
// 50 MB, as upstream) clamped to MaxEffectivePayloadSize, the cap every pkg/scm verifier
// enforces. The clamp is reported once.
func (h *Handler) payloadLimit(configured int64) int64 {
	if configured <= 0 {
		configured = DefaultMaxPayloadSize
	}
	if configured <= MaxEffectivePayloadSize {
		return configured
	}
	if h.clampWarned.CompareAndSwap(false, true) {
		h.log.Warn("webhook: webhook.maxPayloadSizeMB exceeds the largest payload the providers accept; using the provider limit",
			"configuredBytes", configured, "limitBytes", int64(MaxEffectivePayloadSize))
	}
	return MaxEffectivePayloadSize
}

// resolveProvider returns the provider selected by the path (known=false for an unknown
// path name) or detected from the headers (nil when none matches). An explicit provider
// whose headers do not match yields nil.
func (h *Handler) resolveProvider(r *http.Request) (p *provider, known bool) {
	name := r.PathValue("provider")
	if name == "" {
		rest := strings.TrimPrefix(r.URL.Path, h.cfg.PathPrefix)
		if rest != r.URL.Path {
			name = strings.Trim(rest, "/")
		}
	}
	if name == "" {
		return detectProvider(r.Header), true
	}
	p = providerByName(strings.ToLower(name))
	if p == nil {
		return nil, false
	}
	if !p.matchesHeaders(r.Header) {
		return nil, true
	}
	return p, true
}

// verify tries the candidate secrets in order; a mismatch moves on to the next one, any
// other error (missing signature, malformed payload, unsupported event) is final.
func verify(p *provider, h http.Header, body []byte, candidates [][]byte) (*scm.Event, error) {
	if len(candidates) == 0 {
		return p.verifier.VerifyAndParse(h, body, nil)
	}
	var lastErr error
	for _, s := range candidates {
		ev, err := p.verifier.VerifyAndParse(h, body, s)
		if err == nil {
			return ev, nil
		}
		if !errors.Is(err, scm.ErrSignatureMismatch) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// reject answers a verification or parsing failure like upstream: 400 with a generic
// body (signature failures are logged as security events), or the "valid JSON under N
// MB" body for oversize and malformed payloads.
func (h *Handler) reject(w http.ResponseWriter, p *provider, err error, tooLarge string) string {
	switch {
	case errors.Is(err, scm.ErrSignatureMissing), errors.Is(err, scm.ErrSignatureMismatch):
		h.log.Warn("webhook: signature verification failed", "provider", p.name, "error", logErr(err), "security", "high")
		http.Error(w, "Webhook processing failed", http.StatusBadRequest)
		return resultUnauthorized
	case errors.Is(err, scm.ErrPayloadTooLarge):
		h.log.Warn("webhook: payload too large or corrupted", "provider", p.name, "error", logErr(err), "security", "high")
		http.Error(w, tooLarge, http.StatusBadRequest)
		return resultTooLarge
	case errors.Is(err, scm.ErrMalformedPayload):
		h.log.Info("webhook: payload too large or corrupted", "provider", p.name, "error", logErr(err))
		http.Error(w, tooLarge, http.StatusBadRequest)
		return resultInvalid
	case errors.Is(err, scm.ErrUnsupportedEvent):
		h.log.Info("webhook: unsupported event", "provider", p.name, "error", logErr(err))
		http.Error(w, "Webhook processing failed", http.StatusBadRequest)
		return resultUnsupported
	default:
		h.log.Info("webhook: processing failed", "provider", p.name, "error", logErr(err))
		http.Error(w, "Webhook processing failed", http.StatusBadRequest)
		return resultInvalid
	}
}

// relevant reports whether an event can refresh anything: pushes with a repository URL
// and pull request events with a refreshing action.
func relevant(ev *scm.Event) bool {
	switch ev.Type {
	case scm.EventPush:
		return len(pushWebURLs(ev)) > 0
	case scm.EventPullRequest:
		return newPRInfo(ev) != nil
	default:
		return false
	}
}

// accept de-duplicates and enqueues a verified (or unsigned but accepted) SCM event.
func (h *Handler) accept(w http.ResponseWriter, p *provider, ev *scm.Event, size int64) string {
	log := h.log.With("provider", p.name, "event", string(ev.Type), "deliveryID", ev.DeliveryID, "verified", ev.Verified)
	if h.deliveries.SeenEvent(ev) {
		log.Info("webhook: duplicate delivery ignored")
		w.WriteHeader(http.StatusOK)
		return resultDuplicate
	}
	if !relevant(ev) {
		log.Info("webhook: ignoring webhook event", "action", logValue(ev.Action))
		w.WriteHeader(http.StatusOK)
		return resultIgnored
	}
	key := ""
	if ev.Verified {
		key = scm.DeliveryKey(ev.Provider, ev.DeliveryID)
	}
	if res, ok := h.enqueue(&queuedEvent{event: ev, provider: p.name, deliveryKey: key, size: size}); !ok {
		log.Warn("webhook: queue is full, discarding webhook payload")
		http.Error(w, res, http.StatusServiceUnavailable)
		return resultQueueFull
	}
	w.WriteHeader(http.StatusOK)
	return resultAccepted
}

// serveRegistry handles GHCR package events: signature failures answer 401, skipped
// events 200 (REPO-123).
func (h *Handler) serveRegistry(w http.ResponseWriter, r *http.Request, p *provider, body []byte, candidates [][]byte) string {
	verified, err := verifyGHCR(r.Header, body, candidates)
	if err != nil {
		h.log.Warn("webhook: registry webhook HMAC verification failed", "provider", p.name, "error", logErr(err), "security", "high")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return resultUnauthorized
	}
	deliveryID := r.Header.Get(github.HeaderDelivery)
	if err := scm.ValidateDeliveryID(deliveryID); err != nil {
		h.log.Info("webhook: processing failed", "provider", p.name, "error", logErr(err))
		http.Error(w, "Webhook processing failed", http.StatusBadRequest)
		return resultInvalid
	}
	ev, err := parseGHCR(body)
	if err != nil {
		h.log.Info("webhook: processing failed", "provider", p.name, "error", logErr(err))
		http.Error(w, "Webhook processing failed", http.StatusBadRequest)
		return resultInvalid
	}
	log := h.log.With("provider", p.name, "deliveryID", deliveryID, "verified", verified)
	if ev == nil {
		log.Info("webhook: skipping registry event (not a published container tag)")
		w.WriteHeader(http.StatusOK)
		return resultIgnored
	}
	ev.DeliveryID, ev.Verified = deliveryID, verified
	key := ""
	if verified {
		key = scm.DeliveryKey(ProviderGHCR, deliveryID)
		if h.deliveries.Seen(key) {
			log.Info("webhook: duplicate delivery ignored")
			w.WriteHeader(http.StatusOK)
			return resultDuplicate
		}
	}
	if res, ok := h.enqueue(&queuedEvent{registry: ev, provider: p.name, deliveryKey: key, size: int64(len(body))}); !ok {
		log.Warn("webhook: queue is full, discarding webhook payload")
		http.Error(w, res, http.StatusServiceUnavailable)
		return resultQueueFull
	}
	w.WriteHeader(http.StatusOK)
	return resultAccepted
}

// eventOverhead is added to the raw body size when estimating a queued event's memory.
const eventOverhead = 1024

// enqueue adds an event within the queue and memory bounds. On failure the delivery is
// forgotten so a redelivery is processed, and the response body is returned.
func (h *Handler) enqueue(qe *queuedEvent) (string, bool) {
	if h.closing.Load() {
		h.deliveries.Forget(qe.deliveryKey)
		return "Webhook handler is shutting down", false
	}
	qe.size += eventOverhead
	if h.queuedBytes.Add(qe.size) > h.cfg.MaxQueuedBytes {
		h.queuedBytes.Add(-qe.size)
		h.deliveries.Forget(qe.deliveryKey)
		return "Queue is full, discarding webhook payload", false
	}
	select {
	case h.events <- qe:
		return "", true
	default:
		h.queuedBytes.Add(-qe.size)
		h.deliveries.Forget(qe.deliveryKey)
		return "Queue is full, discarding webhook payload", false
	}
}

func (h *Handler) eventWorker(ctx context.Context, stop <-chan struct{}) {
	for {
		select {
		case qe := <-h.events:
			h.handleQueued(ctx, qe)
		case <-stop:
			for {
				select {
				case qe := <-h.events:
					h.handleQueued(ctx, qe)
				default:
					return
				}
			}
		}
	}
}

// handleQueued processes one event, recovering from panics so a malformed event cannot
// stop a worker.
func (h *Handler) handleQueued(ctx context.Context, qe *queuedEvent) {
	h.queuedBytes.Add(-qe.size)
	h.metrics.inFlight.Inc()
	start := time.Now()
	defer func() {
		h.metrics.inFlight.Dec()
		h.metrics.duration.Observe(time.Since(start).Seconds())
		if rec := recover(); rec != nil {
			h.deliveries.Forget(qe.deliveryKey)
			h.log.Error("webhook: panic while processing webhook event", "provider", qe.provider, "panic", clip(fmt.Sprint(rec), maxLogErrorBytes))
		}
	}()
	h.process(ctx, qe, start)
}
