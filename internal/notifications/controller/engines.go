package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// Event reasons and actions emitted by the controller. api/v1alpha1 may mirror them.
const (
	// ReasonNotificationDeliveryFailed is a Warning on an Application whose notification could
	// not be rendered or delivered (or names an undefined trigger, template or service).
	ReasonNotificationDeliveryFailed = "NotificationDeliveryFailed"
	// ReasonInvalidNotificationsConfiguration is a Warning on a notifications ConfigMap that
	// cannot be parsed; no notification is sent from that configuration until it is fixed.
	ReasonInvalidNotificationsConfiguration = "InvalidNotificationsConfiguration"

	actionNotify = "Notify"
	actionParse  = "ParseConfiguration"
	// maxEventNoteBytes keeps notes under the events API limit (1 KiB).
	maxEventNoteBytes = 1000
	// maxEventActionBytes is the events API limit of the action field.
	maxEventActionBytes = 128
)

// engineEntry is the engine of one configuration (ConfigMap + Secret of a namespace).
type engineEntry struct {
	// namespace is where the configuration lives.
	namespace string
	// tenant marks a self-service (application namespace) configuration.
	tenant   bool
	engine   *notifications.Engine
	services map[string]bool
	triggers map[string]bool
}

func (e *engineEntry) hasService(name string) bool { return e.services[name] }
func (e *engineEntry) hasTrigger(name string) bool { return e.triggers[name] }

type cachedEngine struct {
	fingerprint string
	entry       *engineEntry // nil when the configuration is unusable
}

// engineFactory builds engines from configuration objects read from the cache, rebuilding an
// engine when its ConfigMap or Secret changes (hot reload, NOTIF-014).
type engineFactory struct {
	cfg      *completed
	reader   client.Reader
	recorder events.EventRecorder
	opts     []notifications.Option
	log      *slog.Logger

	mu      sync.Mutex
	entries map[string]*cachedEngine
}

func newEngineFactory(cfg *completed, reader client.Reader, recorder events.EventRecorder, opts []notifications.Option) *engineFactory {
	return &engineFactory{
		cfg:      cfg,
		reader:   reader,
		recorder: recorder,
		opts:     opts,
		log:      cfg.Logger,
		entries:  map[string]*cachedEngine{},
	}
}

// forApp returns the engines that process an Application of namespace ns: the control-plane
// configuration and, with self-service, the configuration of ns. Unusable configurations are
// left out (their problem was logged and reported as an Event once).
func (f *engineFactory) forApp(ctx context.Context, ns string) ([]*engineEntry, error) {
	var out []*engineEntry
	cp, err := f.get(ctx, f.cfg.Namespace, false)
	if err != nil {
		return nil, err
	}
	if cp != nil {
		out = append(out, cp)
	}
	if f.cfg.SelfServiceEnabled && ns != f.cfg.Namespace {
		tenant, err := f.get(ctx, ns, true)
		if err != nil {
			return nil, err
		}
		if tenant != nil {
			out = append(out, tenant)
		}
	}
	return out, nil
}

func (f *engineFactory) get(ctx context.Context, ns string, tenant bool) (*engineEntry, error) {
	cm := &corev1.ConfigMap{}
	if err := f.reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: f.cfg.ConfigMapName}, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("getting notifications ConfigMap %s/%s: %w", ns, f.cfg.ConfigMapName, err)
		}
		cm = nil
	}
	if cm == nil && tenant {
		f.mu.Lock()
		delete(f.entries, ns)
		f.mu.Unlock()
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := f.reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: f.cfg.SecretName}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("getting notifications Secret %s/%s: %w", ns, f.cfg.SecretName, err)
		}
		secret = nil
	}
	fp := fingerprint(cm, secret)
	f.mu.Lock()
	ce, ok := f.entries[ns]
	f.mu.Unlock()
	if ok && ce.fingerprint == fp {
		return ce.entry, nil
	}
	entry := f.build(ns, tenant, cm, secret)
	f.mu.Lock()
	f.entries[ns] = &cachedEngine{fingerprint: fp, entry: entry}
	f.mu.Unlock()
	return entry, nil
}

func fingerprint(cm *corev1.ConfigMap, secret *corev1.Secret) string {
	fp := "cm:-"
	if cm != nil {
		fp = "cm:" + string(cm.UID) + "/" + cm.ResourceVersion
	}
	if secret != nil {
		fp += "|secret:" + string(secret.UID) + "/" + secret.ResourceVersion
	} else {
		fp += "|secret:-"
	}
	return fp
}

// build parses one configuration. A namespace configuration resolves $references only against
// the Secret of its own namespace, never exposes .secrets and delivers through the tenant
// network (guarded dialer, TenantEgressAllowedPrefixes only); with self-service enabled the
// control-plane configuration does not expose .secrets either (upstream).
func (f *engineFactory) build(ns string, tenant bool, cm *corev1.ConfigMap, secret *corev1.Secret) *engineEntry {
	var cmData map[string]string
	if cm != nil {
		cmData = cm.Data
	}
	var secretData map[string][]byte
	if secret != nil {
		secretData = secret.Data
	}
	redactor := secretRedactor(secretData)
	log := f.log.With("configNamespace", ns, "configMap", f.cfg.ConfigMapName)
	var parseOpts []notifications.ParseOption
	if !f.cfg.DisableDefaultCatalog {
		parseOpts = append(parseOpts, notifications.WithDefaultCatalog())
	}
	invalid := func(err error) *engineEntry {
		msg := redactor.Redact(err.Error())
		log.Error("notifications configuration is invalid; no notification is sent from it until it is fixed", "error", msg)
		if cm != nil {
			f.event(cm, corev1.EventTypeWarning, ReasonInvalidNotificationsConfiguration, actionParse, msg)
		}
		return nil
	}
	parsed, err := notifications.ParseConfig(cmData, secretData, parseOpts...)
	if err != nil {
		return invalid(err)
	}
	opts := append([]notifications.Option(nil), f.opts...)
	switch {
	case tenant:
		// A namespace configuration never gets the control-plane egress policy (threat model
		// T8): its webhooks would otherwise reach every range opened for the control plane.
		opts = append(opts, notifications.WithNetwork(f.cfg.tenantNetwork), notifications.WithSelfServiceNamespace(ns))
	case f.cfg.SelfServiceEnabled:
		opts = append(opts, notifications.WithNetwork(f.cfg.Network), notifications.WithSecretsInTemplates(false))
	default:
		opts = append(opts, notifications.WithNetwork(f.cfg.Network))
	}
	eng, err := notifications.NewEngine(parsed, opts...)
	if err != nil {
		return invalid(err)
	}
	for _, w := range eng.Warnings() {
		log.Warn("notifications configuration warning", "warning", redactor.Redact(w))
	}
	entry := &engineEntry{namespace: ns, tenant: tenant, engine: eng, services: map[string]bool{}, triggers: map[string]bool{}}
	for name := range parsed.Services {
		entry.services[name] = true
	}
	for name := range parsed.Triggers {
		entry.triggers[name] = true
	}
	log.Info("loaded notifications configuration", "services", len(entry.services), "triggers", len(entry.triggers), "templates", len(parsed.Templates), "selfService", tenant)
	return entry
}

func (f *engineFactory) event(obj runtime.Object, eventType, reason, action, note string) {
	if f.recorder == nil {
		return
	}
	f.recorder.Eventf(obj, nil, eventType, reason, action, "%s", truncateUTF8(note, maxEventNoteBytes))
}

func secretRedactor(data map[string][]byte) *services.Redactor {
	values := make([]string, 0, len(data))
	for _, v := range data {
		values = append(values, string(v))
	}
	return services.NewRedactor(values...)
}

// truncateUTF8 shortens s to at most n bytes without splitting a rune, marking the cut with
// "..." when n leaves room for it.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	const ellipsis = "..."
	suffix := ellipsis
	if n <= len(ellipsis) {
		suffix = ""
	}
	cut := n - len(suffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
