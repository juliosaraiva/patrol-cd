package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// Defaults.
const (
	DefaultSendTimeout    = 2 * time.Minute
	DefaultHelperTimeout  = 30 * time.Second
	DefaultProjectTimeout = 5 * time.Second
	defaultProject        = "default"
)

// Context keys aliased to each other so that Argo CD style configurations keep working.
const (
	ContextKeyPatrolURL = "patrolUrl"
	ContextKeyArgoCDURL = "argocdUrl"
	contextNotifType    = "notificationType"
)

// DeliveryStatus is the outcome for one destination.
type DeliveryStatus string

// Delivery outcomes.
const (
	// StatusSent: the notification was delivered.
	StatusSent DeliveryStatus = "Sent"
	// StatusSkippedDuplicate: skipped because this condition (and oncePer value) was
	// already delivered to the destination.
	StatusSkippedDuplicate DeliveryStatus = "SkippedDuplicate"
	// StatusFailed: rendering or delivery failed (Err is set). The state is not updated so
	// the next evaluation retries.
	StatusFailed DeliveryStatus = "Failed"
	// StatusDryRun: rendered but not sent (dry-run mode).
	StatusDryRun DeliveryStatus = "DryRun"
)

// DeliveryResult reports what happened for one (trigger condition, destination).
type DeliveryResult struct {
	Trigger      string
	ConditionKey string
	Templates    []string
	Destination  services.Destination
	Status       DeliveryStatus
	// Err is set for StatusFailed. It never contains configured secret values.
	Err error
	// Notification is the rendered notification (set in dry-run mode).
	Notification *services.Notification
}

// ServiceFactory builds a service; it defaults to services.New.
type ServiceFactory func(serviceType string, options []byte, deps services.Deps) (services.Service, error)

type engineOptions struct {
	repo           RepoService
	projects       ProjectGetter
	metrics        Metrics
	logger         *slog.Logger
	now            func() time.Time
	network        services.Network
	prefix         string
	namespace      string
	selfService    bool
	exposeSecrets  *bool
	dryRun         bool
	sendTimeout    time.Duration
	helperTimeout  time.Duration
	factory        ServiceFactory
	product        string
	serviceOverlay map[string]services.Service
}

// Option configures an Engine.
type Option func(*engineOptions)

// WithRepoService wires repo.GetCommitMetadata / repo.GetAppDetails.
func WithRepoService(r RepoService) Option { return func(o *engineOptions) { o.repo = r } }

// WithProjectGetter wires AppProject lookups (template var .appProject and project-level
// subscription annotations).
func WithProjectGetter(p ProjectGetter) Option { return func(o *engineOptions) { o.projects = p } }

// WithMetrics sets the metrics sink.
func WithMetrics(m Metrics) Option { return func(o *engineOptions) { o.metrics = m } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(o *engineOptions) { o.logger = l } }

// WithClock sets the clock used for time.Now in triggers/templates and state timestamps.
func WithClock(now func() time.Time) Option { return func(o *engineOptions) { o.now = now } }

// WithNetwork sets outbound network settings (dialer/SSRF guard, CA resolver, URL
// validation, timeouts, retry policy).
func WithNetwork(n services.Network) Option { return func(o *engineOptions) { o.network = n } }

// WithAnnotationPrefix overrides DefaultAnnotationPrefix.
func WithAnnotationPrefix(p string) Option { return func(o *engineOptions) { o.prefix = p } }

// WithSelfServiceNamespace marks the engine as built from an application-namespace
// configuration (self-service notifications): delivery-state keys are prefixed with the
// namespace and notification secrets are not exposed to templates.
func WithSelfServiceNamespace(ns string) Option {
	return func(o *engineOptions) {
		o.namespace = ns
		o.selfService = true
	}
}

// WithSecretsInTemplates controls whether .secrets is available to triggers and
// templates (default true, false in self-service mode).
func WithSecretsInTemplates(enabled bool) Option {
	return func(o *engineOptions) { o.exposeSecrets = &enabled }
}

// WithDryRun renders notifications without sending them.
func WithDryRun(enabled bool) Option { return func(o *engineOptions) { o.dryRun = enabled } }

// WithSendTimeout bounds one delivery including retries (default 2m).
func WithSendTimeout(d time.Duration) Option { return func(o *engineOptions) { o.sendTimeout = d } }

// WithServiceFactory replaces services.New (tests, custom services).
func WithServiceFactory(f ServiceFactory) Option { return func(o *engineOptions) { o.factory = f } }

// WithServices registers ready-made services by name, overriding configured ones.
func WithServices(m map[string]services.Service) Option {
	return func(o *engineOptions) { o.serviceOverlay = m }
}

type serviceEntry struct {
	typ string
	svc services.Service
	err error
}

// Engine evaluates triggers, renders templates and delivers notifications. It is
// immutable after construction and safe for concurrent use; build a new Engine when the
// configuration changes.
type Engine struct {
	cfg       *Config
	opts      engineOptions
	services  map[string]serviceEntry
	triggers  map[string]*compiledTrigger
	templates map[string]*compiledTemplate
	tmplErrs  map[string]error
	warnings  []string
}

// NewEngine builds an engine from a parsed configuration. Invalid services, triggers and
// templates do not prevent construction: deliveries that need them fail with the
// underlying error (see Warnings and Config.Validate).
func NewEngine(cfg *Config, opts ...Option) (*Engine, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: nil configuration", ErrInvalidConfig)
	}
	var o engineOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.exposeSecrets == nil {
		expose := !o.selfService
		o.exposeSecrets = &expose
	}
	if o.metrics == nil {
		o.metrics = noopMetrics{}
	}
	if o.logger == nil {
		o.logger = slog.New(slog.DiscardHandler)
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.prefix == "" {
		o.prefix = DefaultAnnotationPrefix
	}
	if o.sendTimeout <= 0 {
		o.sendTimeout = DefaultSendTimeout
	}
	if o.helperTimeout <= 0 {
		o.helperTimeout = DefaultHelperTimeout
	}
	if o.factory == nil {
		o.factory = services.New
	}
	if o.product == "" {
		o.product = "Patrol CD"
	}
	e := &Engine{
		cfg:       cfg,
		opts:      o,
		services:  map[string]serviceEntry{},
		triggers:  map[string]*compiledTrigger{},
		templates: map[string]*compiledTemplate{},
		tmplErrs:  map[string]error{},
		warnings:  append([]string(nil), cfg.Warnings...),
	}
	allSecrets := make([]string, 0, len(cfg.Secrets))
	for _, v := range cfg.Secrets {
		allSecrets = append(allSecrets, v)
	}
	for _, sc := range cfg.ListServices() {
		conf := cfg.Services[sc.Name]
		deps := services.Deps{
			Network:         o.network,
			Logger:          o.logger.With("service", sc.Name, "type", sc.Type),
			Now:             o.now,
			SensitiveValues: append(append([]string(nil), conf.secretValues...), allSecrets...),
			Product:         o.product,
		}
		svc, err := o.factory(conf.Type, conf.Options, deps)
		if err != nil {
			e.warnings = append(e.warnings, fmt.Sprintf("service %q: %v", sc.Name, err))
		}
		e.services[sc.Name] = serviceEntry{typ: conf.Type, svc: svc, err: err}
	}
	for name, svc := range o.serviceOverlay {
		typ := e.services[name].typ
		if typ == "" {
			typ = name
		}
		e.services[name] = serviceEntry{typ: typ, svc: svc}
	}
	for name, conds := range cfg.Triggers {
		ct := compileTrigger(name, conds)
		for _, c := range ct.conditions {
			if c.err != nil {
				e.warnings = append(e.warnings, fmt.Sprintf("trigger %q: %v", name, c.err))
			}
		}
		e.triggers[name] = ct
	}
	for name, def := range cfg.Templates {
		ct, err := compileTemplate(name, def)
		if err != nil {
			e.tmplErrs[name] = err
			e.warnings = append(e.warnings, fmt.Sprintf("template %q: %v", name, err))
			continue
		}
		e.templates[name] = ct
	}
	sort.Strings(e.warnings)
	return e, nil
}

// Config returns the configuration the engine was built from.
func (e *Engine) Config() *Config { return e.cfg }

// Warnings returns configuration problems detected while building the engine.
func (e *Engine) Warnings() []string { return append([]string(nil), e.warnings...) }

// AnnotationPrefix returns the subscription annotation prefix.
func (e *Engine) AnnotationPrefix() string { return e.opts.prefix }

// ListServices returns the configured services.
func (e *Engine) ListServices() []ServiceInfo { return e.cfg.ListServices() }

// ListTriggers returns the configured triggers.
func (e *Engine) ListTriggers() []TriggerInfo { return e.cfg.ListTriggers() }

// ListTemplates returns the configured template names.
func (e *Engine) ListTemplates() []string { return e.cfg.ListTemplates() }

// processing holds per-call state shared by evaluation and rendering.
type processing struct {
	app     map[string]any
	project map[string]any
	base    map[string]any
}

func (e *Engine) prepare(ctx context.Context, app map[string]any) (*processing, error) {
	if app == nil {
		return nil, fmt.Errorf("%w: nil object", ErrInvalidApplication)
	}
	if _, ok := app["metadata"].(map[string]any); !ok {
		return nil, fmt.Errorf("%w: missing metadata", ErrInvalidApplication)
	}
	app = deepCopyJSON(app).(map[string]any)
	p := &processing{app: app}
	if proj := e.lookupProject(ctx, app); proj != nil {
		p.project = deepCopyJSON(proj).(map[string]any)
	}
	ctxVars := make(map[string]string, len(e.cfg.Context)+2)
	for k, v := range e.cfg.Context {
		ctxVars[k] = v
	}
	if _, ok := ctxVars[ContextKeyPatrolURL]; !ok {
		if v, ok := ctxVars[ContextKeyArgoCDURL]; ok {
			ctxVars[ContextKeyPatrolURL] = v
		}
	}
	if _, ok := ctxVars[ContextKeyArgoCDURL]; !ok {
		if v, ok := ctxVars[ContextKeyPatrolURL]; ok {
			ctxVars[ContextKeyArgoCDURL] = v
		}
	}
	repo := &repoHelpers{ctx: ctx, app: app, svc: e.opts.repo, timeout: e.opts.helperTimeout}
	project := p.project
	if project == nil {
		project = map[string]any{}
	}
	p.base = map[string]any{
		"app":        app,
		"appProject": project,
		"context":    ctxVars,
		"time":       timeHelpers(e.opts.now),
		// Fresh maps per call: sprig's set/unset mutate map[string]any in place.
		"strings": cloneHelpers(stringHelpers),
		"sync":    cloneHelpers(syncHelpers),
		"repo":    repo.namespace(),
	}
	if *e.opts.exposeSecrets {
		p.base["secrets"] = e.cfg.Secrets
	}
	return p, nil
}

// vars returns the variables for a destination ("" service for trigger evaluation).
func (e *Engine) vars(p *processing, dest services.Destination) map[string]any {
	out := make(map[string]any, len(p.base)+2)
	for k, v := range p.base {
		out[k] = v
	}
	base := p.base["context"].(map[string]string)
	ctxVars := make(map[string]string, len(base)+1)
	ctxVars[contextNotifType] = dest.Service
	for k, v := range base {
		ctxVars[k] = v
	}
	out["context"] = ctxVars
	if dest.Service != "" {
		out["serviceType"] = e.serviceType(dest.Service)
		out["recipient"] = dest.Recipient
	}
	return out
}

func (e *Engine) serviceType(name string) string {
	if s, ok := e.services[name]; ok && s.typ != "" {
		return s.typ
	}
	return name
}

func (e *Engine) lookupProject(ctx context.Context, app map[string]any) map[string]any {
	if e.opts.projects == nil {
		return nil
	}
	name, _ := nestedString(app, "spec", "project")
	if name == "" {
		name = defaultProject
	}
	pctx, cancel := context.WithTimeout(ctx, DefaultProjectTimeout)
	defer cancel()
	proj, err := e.opts.projects.GetAppProject(pctx, name)
	if err != nil {
		e.opts.logger.Warn("notifications: failed to get AppProject", "project", name, "error", err)
		return nil
	}
	return proj
}

// Destinations returns the destinations subscribed to each trigger for app: default
// subscriptions matching the app labels, app annotations and project annotations.
func (e *Engine) Destinations(ctx context.Context, app map[string]any) (Destinations, error) {
	p, err := e.prepare(ctx, app)
	if err != nil {
		return nil, err
	}
	return e.destinations(p), nil
}

func (e *Engine) destinations(p *processing) Destinations {
	meta, _ := p.app["metadata"].(map[string]any)
	dests := e.cfg.GlobalDestinations(stringMap(meta["labels"]))
	appDests, warnings := AnnotationDestinations(e.opts.prefix, stringMap(meta["annotations"]), e.cfg.DefaultTriggers, e.cfg.ServiceDefaultTriggers)
	dests.Merge(appDests)
	if p.project != nil {
		pmeta, _ := p.project["metadata"].(map[string]any)
		projAnn := stringMap(pmeta["annotations"])
		projDests, pw := AnnotationDestinations(e.opts.prefix, projAnn, e.cfg.DefaultTriggers, e.cfg.ServiceDefaultTriggers)
		dests.Merge(projDests)
		dests.Merge(LegacyRecipientsDestinations(LegacyRecipientsAnnotationSuffix(e.opts.prefix), projAnn, e.cfg.DefaultTriggers, e.cfg.ServiceDefaultTriggers))
		warnings = append(warnings, pw...)
	}
	for _, w := range warnings {
		e.opts.logger.Warn("notifications: invalid subscription", "detail", w)
	}
	dests.sorted()
	return dests
}

// RunTrigger evaluates a trigger against app without sending anything.
func (e *Engine) RunTrigger(ctx context.Context, trigger string, app map[string]any) ([]ConditionResult, error) {
	p, err := e.prepare(ctx, app)
	if err != nil {
		return nil, err
	}
	return e.runTrigger(p, trigger)
}

func (e *Engine) runTrigger(p *processing, trigger string) ([]ConditionResult, error) {
	ct, ok := e.triggers[trigger]
	if !ok {
		return nil, fmt.Errorf("%w: trigger with name '%s' is not defined", ErrTriggerNotFound, trigger)
	}
	return ct.run(e.vars(p, services.Destination{})), nil
}

// Render renders the named templates (merged in order) for a destination without
// sending.
func (e *Engine) Render(ctx context.Context, app map[string]any, templates []string, dest services.Destination) (*services.Notification, error) {
	p, err := e.prepare(ctx, app)
	if err != nil {
		return nil, err
	}
	return e.render(p, templates, dest)
}

func (e *Engine) render(p *processing, templates []string, dest services.Destination) (*services.Notification, error) {
	vars := e.vars(p, dest)
	var out services.Notification
	for _, name := range templates {
		if err, ok := e.tmplErrs[name]; ok {
			return nil, fmt.Errorf("template %q: %w", name, err)
		}
		ct, ok := e.templates[name]
		if !ok {
			return nil, fmt.Errorf("%w: template '%s' is not supported", ErrTemplateNotFound, name)
		}
		if err := ct.apply(&out, vars); err != nil {
			return nil, fmt.Errorf("rendering template %q: %w", name, err)
		}
	}
	return &out, nil
}

func (e *Engine) send(ctx context.Context, n *services.Notification, dest services.Destination) error {
	entry, ok := e.services[dest.Service]
	if !ok {
		return fmt.Errorf("%w: notification service '%s' is not supported", ErrServiceNotConfigured, dest.Service)
	}
	if entry.err != nil {
		return fmt.Errorf("service %q is misconfigured: %w", dest.Service, entry.err)
	}
	if entry.svc == nil {
		return fmt.Errorf("%w: notification service '%s' is not supported", ErrServiceNotConfigured, dest.Service)
	}
	sctx, cancel := context.WithTimeout(ctx, e.opts.sendTimeout)
	defer cancel()
	return entry.svc.Send(sctx, *n, dest)
}

// Process evaluates every subscribed trigger for app, delivers notifications for newly
// triggered conditions and returns the updated delivery state (to persist with
// ApplyDeliveryState) and one result per (condition, destination) that was triggered or
// failed. Neither app nor state is modified (templates see a private deep copy). The
// error is non-nil only for an invalid application or when ctx is done; partial results
// and state are returned in the latter case, and destinations that were not attempted are
// left untouched so they are retried.
func (e *Engine) Process(ctx context.Context, app map[string]any, state DeliveryState) (DeliveryState, []DeliveryResult, error) {
	newState := state.Clone()
	p, err := e.prepare(ctx, app)
	if err != nil {
		return newState, nil, err
	}
	dests := e.destinations(p)
	if len(dests) == 0 {
		return newState, nil, nil
	}
	var results []DeliveryResult
	for _, trigger := range dests.Triggers() {
		destList := dests[trigger]
		crs, err := e.runTrigger(p, trigger)
		if err != nil {
			e.opts.logger.Debug("notifications: trigger evaluation failed", "trigger", trigger, "error", err)
			for _, d := range destList {
				results = append(results, DeliveryResult{Trigger: trigger, Destination: d, Status: StatusFailed, Err: err})
			}
			continue
		}
		for _, cr := range crs {
			e.opts.metrics.IncTriggerEvaluation(trigger, cr.Triggered)
			if cr.Err != nil {
				e.opts.logger.Debug("notifications: condition evaluation error", "trigger", trigger, "condition", cr.Key, "error", cr.Err)
			}
			oncePer := cr.OncePer != ""
			if !cr.Triggered {
				for _, d := range destList {
					newState.setNotified(StateKey(e.opts.namespace, trigger, cr, d), oncePer, false, 0)
				}
				continue
			}
			for _, d := range destList {
				if err := ctx.Err(); err != nil {
					newState.Truncate(MaxDeliveryStateEntries)
					return newState, results, err
				}
				key := StateKey(e.opts.namespace, trigger, cr, d)
				res := DeliveryResult{Trigger: trigger, ConditionKey: cr.Key, Templates: cr.Templates, Destination: d}
				if !newState.setNotified(key, oncePer, true, e.opts.now().Unix()) {
					res.Status = StatusSkippedDuplicate
					results = append(results, res)
					continue
				}
				n, err := e.render(p, cr.Templates, d)
				if err == nil && e.opts.dryRun {
					res.Status = StatusDryRun
					res.Notification = n
					results = append(results, res)
					continue
				}
				if err == nil {
					err = e.send(ctx, n, d)
				}
				if err != nil {
					// Not recorded: retried on the next evaluation (also for oncePer).
					delete(newState, key)
					res.Status = StatusFailed
					res.Err = err
					e.opts.metrics.IncDelivery(trigger, d.Service, false)
					e.opts.logger.Warn("notifications: delivery failed", "trigger", trigger, "service", d.Service, "error", err)
				} else {
					res.Status = StatusSent
					e.opts.metrics.IncDelivery(trigger, d.Service, true)
				}
				results = append(results, res)
			}
		}
	}
	newState.Truncate(MaxDeliveryStateEntries)
	return newState, results, nil
}

// NotifyTemplate renders template for each destination and sends it (or only renders in
// dry-run mode), ignoring triggers and delivery state. It backs "admin notifications
// template notify".
func (e *Engine) NotifyTemplate(ctx context.Context, app map[string]any, template string, dests []services.Destination) ([]DeliveryResult, error) {
	p, err := e.prepare(ctx, app)
	if err != nil {
		return nil, err
	}
	results := make([]DeliveryResult, 0, len(dests))
	for _, d := range dests {
		res := DeliveryResult{Templates: []string{template}, Destination: d}
		n, err := e.render(p, []string{template}, d)
		switch {
		case err != nil:
		case e.opts.dryRun:
			res.Status = StatusDryRun
			res.Notification = n
			results = append(results, res)
			continue
		default:
			err = e.send(ctx, n, d)
		}
		if err != nil {
			res.Status = StatusFailed
			res.Err = err
		} else {
			res.Status = StatusSent
		}
		results = append(results, res)
	}
	return results, nil
}

// FailedResults filters results with StatusFailed and joins their errors.
func FailedResults(results []DeliveryResult) error {
	var errs []error
	for _, r := range results {
		if r.Status == StatusFailed && r.Err != nil {
			errs = append(errs, fmt.Errorf("%s -> %s: %w", r.Trigger, r.Destination, r.Err))
		}
	}
	return errors.Join(errs...)
}
