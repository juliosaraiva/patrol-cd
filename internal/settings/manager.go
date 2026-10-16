package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	informerscorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
)

// Event reasons emitted by the Manager on the settings object an issue concerns.
const (
	EventReasonInvalidSettings = "InvalidSettings"
	EventReasonSettingsWarning = "SettingsWarning"
)

// Manager defaults.
const (
	DefaultDebounce     = 200 * time.Millisecond
	DefaultResyncPeriod = 10 * time.Minute
	DefaultSyncTimeout  = 2 * time.Minute
	maxEventMessageLen  = 1024
)

// EventRecorder emits Kubernetes Events. client-go's record.EventRecorder implements it.
type EventRecorder interface {
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...any)
}

// ReloadResult describes one reload, for metrics.
type ReloadResult struct {
	// Duration is the time spent building the snapshot.
	Duration time.Duration
	// Published is true when a new snapshot replaced the current one.
	Published bool
	// Initial is true for the snapshot built by Start.
	Initial bool
	// Errors and Warnings count the snapshot's issues.
	Errors, Warnings int
}

// Metrics receives Manager instrumentation. Nil functions are ignored; callbacks must be
// fast and must not call back into the Manager.
type Metrics struct {
	Reloaded func(ReloadResult)
}

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Namespace is the control-plane namespace (required).
	Namespace string
	// Names selects the object names (defaults when empty).
	Names Names
	// Debounce coalesces the events of this window into one reload (default 200ms).
	Debounce time.Duration
	// ResyncPeriod is the informers' resync period (default 10m).
	ResyncPeriod time.Duration
	// SyncTimeout bounds the initial cache sync in Start (default 2m).
	SyncTimeout time.Duration
	// IgnorePartOfLabel disables the app.kubernetes.io/part-of label requirement.
	IgnorePartOfLabel bool
	// DisableReferencedSecrets stops watching labelled Secrets, so "$<secret>:<key>"
	// references cannot be resolved; the Manager then needs list/watch only on the named
	// objects (RBAC resourceNames).
	DisableReferencedSecrets bool
	// Recorder emits Events for invalid settings (optional).
	Recorder EventRecorder
	// Metrics receives reload instrumentation (optional).
	Metrics Metrics
	// Logger receives operational logs (default slog.Default()).
	Logger *slog.Logger
}

// Manager keeps the current Settings snapshot up to date from informers filtered to the
// control-plane namespace and the configured object names.
type Manager struct {
	client kubernetes.Interface
	opts   ManagerOptions
	log    *slog.Logger

	current atomic.Pointer[Settings]

	mu      sync.Mutex
	subs    map[uint64]func(old, new *Settings)
	nextSub uint64
	started bool

	trigger         chan struct{}
	cmInformers     map[Source]toolscache.SharedIndexInformer
	secretInformers map[Source]toolscache.SharedIndexInformer
	refInformer     toolscache.SharedIndexInformer
	reported        map[string]bool
}

// NewManager returns a Manager. Current returns a snapshot of defaults until Start loaded the
// objects.
func NewManager(client kubernetes.Interface, opts ManagerOptions) (*Manager, error) {
	if client == nil {
		return nil, errors.New("settings manager: nil Kubernetes client")
	}
	if opts.Namespace == "" {
		return nil, errors.New("settings manager: namespace is required")
	}
	opts.Names = opts.Names.WithDefaults()
	if err := opts.Names.Validate(); err != nil {
		return nil, fmt.Errorf("settings manager: %w", err)
	}
	if opts.Debounce <= 0 {
		opts.Debounce = DefaultDebounce
	}
	if opts.ResyncPeriod <= 0 {
		opts.ResyncPeriod = DefaultResyncPeriod
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = DefaultSyncTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		client:   client,
		opts:     opts,
		log:      log.With("component", "settings"),
		subs:     map[uint64]func(old, new *Settings){},
		trigger:  make(chan struct{}, 1),
		reported: map[string]bool{},
	}
	m.current.Store(Parse(Input{Namespace: opts.Namespace, Names: opts.Names, IgnorePartOfLabel: opts.IgnorePartOfLabel}))
	return m, nil
}

// Current returns the current snapshot (never nil).
func (m *Manager) Current() *Settings { return m.current.Load() }

// Subscribe registers fn to be called with the previous and the new snapshot after every
// change. Calls are sequential, in snapshot order, from the Manager's goroutine; a slow fn
// delays later notifications. The returned function unregisters fn.
func (m *Manager) Subscribe(fn func(old, new *Settings)) (unsubscribe func()) {
	if fn == nil {
		return func() {}
	}
	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = fn
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subs, id)
			m.mu.Unlock()
		})
	}
}

// Start starts the informers, waits for their caches, publishes the first snapshot and keeps
// it current until ctx is done. It returns after the first snapshot is available.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("settings manager already started")
	}
	m.started = true
	m.mu.Unlock()

	m.cmInformers = map[Source]toolscache.SharedIndexInformer{}
	m.secretInformers = map[Source]toolscache.SharedIndexInformer{}
	var all []toolscache.SharedIndexInformer
	byName := func(name string) func(*metav1.ListOptions) {
		sel := fields.OneTermEqualSelector("metadata.name", name).String()
		return func(o *metav1.ListOptions) { o.FieldSelector = sel }
	}
	for _, src := range configMapSources {
		inf := informerscorev1.NewFilteredConfigMapInformer(m.client, m.opts.Namespace, m.opts.ResyncPeriod, toolscache.Indexers{}, byName(m.opts.Names.Name(src)))
		m.cmInformers[src] = inf
		all = append(all, inf)
	}
	for _, src := range secretSources {
		inf := informerscorev1.NewFilteredSecretInformer(m.client, m.opts.Namespace, m.opts.ResyncPeriod, toolscache.Indexers{}, byName(m.opts.Names.Name(src)))
		m.secretInformers[src] = inf
		all = append(all, inf)
	}
	if !m.opts.DisableReferencedSecrets {
		sel := labels.SelectorFromSet(labels.Set{LabelKeyPartOf: LabelValuePartOf}).String()
		m.refInformer = informerscorev1.NewFilteredSecretInformer(m.client, m.opts.Namespace, m.opts.ResyncPeriod, toolscache.Indexers{}, func(o *metav1.ListOptions) { o.LabelSelector = sel })
		all = append(all, m.refInformer)
	}
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { m.kick() },
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(metav1.Object)
			n, ok2 := newObj.(metav1.Object)
			if ok1 && ok2 && o.GetResourceVersion() == n.GetResourceVersion() {
				return // periodic resync
			}
			m.kick()
		},
		DeleteFunc: func(any) { m.kick() },
	}
	for _, inf := range all {
		if _, err := inf.AddEventHandler(handler); err != nil {
			return fmt.Errorf("settings manager: add event handler: %w", err)
		}
	}
	// The informers stop with ctx, or right away when the initial sync fails.
	runCtx, stop := context.WithCancel(ctx)
	for _, inf := range all {
		go inf.Run(runCtx.Done())
	}
	syncCtx, cancel := context.WithTimeout(runCtx, m.opts.SyncTimeout)
	defer cancel()
	synced := make([]toolscache.InformerSynced, len(all))
	for i, inf := range all {
		synced[i] = inf.HasSynced
	}
	if !toolscache.WaitForCacheSync(syncCtx.Done(), synced...) {
		stop()
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("settings manager: waiting for caches: %w", err)
		}
		return fmt.Errorf("settings manager: caches did not sync within %s", m.opts.SyncTimeout)
	}
	// Drain the events of the initial list; the first snapshot below covers them.
	select {
	case <-m.trigger:
	default:
	}
	m.reload(true)
	go func() {
		defer stop()
		m.loop(runCtx)
	}()
	return nil
}

// kick schedules a reload.
func (m *Manager) kick() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *Manager) loop(ctx context.Context) {
	var timerC <-chan time.Time
	var timer *time.Timer
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-m.trigger:
			if timerC == nil {
				timer = time.NewTimer(m.opts.Debounce)
				timerC = timer.C
			}
		case <-timerC:
			timerC = nil
			m.reload(false)
		}
	}
}

// input collects the cached objects.
func (m *Manager) input() Input {
	in := Input{Namespace: m.opts.Namespace, Names: m.opts.Names, IgnorePartOfLabel: m.opts.IgnorePartOfLabel}
	for _, src := range configMapSources {
		name := m.opts.Names.Name(src)
		for _, obj := range m.cmInformers[src].GetStore().List() {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == name && cm.Namespace == m.opts.Namespace {
				in.ConfigMaps = append(in.ConfigMaps, cm)
			}
		}
	}
	seen := map[string]bool{}
	for _, src := range secretSources {
		name := m.opts.Names.Name(src)
		for _, obj := range m.secretInformers[src].GetStore().List() {
			if sec, ok := obj.(*corev1.Secret); ok && sec.Name == name && sec.Namespace == m.opts.Namespace && !seen[sec.Name] {
				seen[sec.Name] = true
				in.Secrets = append(in.Secrets, sec)
			}
		}
	}
	if m.refInformer != nil {
		for _, obj := range m.refInformer.GetStore().List() {
			sec, ok := obj.(*corev1.Secret)
			if !ok || sec.Namespace != m.opts.Namespace || seen[sec.Name] || sec.Labels[LabelKeyPartOf] != LabelValuePartOf {
				continue
			}
			seen[sec.Name] = true
			in.Secrets = append(in.Secrets, sec)
		}
	}
	return in
}

// reload builds a snapshot and publishes it when the input changed.
func (m *Manager) reload(initial bool) {
	start := time.Now()
	in := m.input()
	prev := m.current.Load()
	next := Parse(in, WithPrevious(prev))
	res := ReloadResult{Duration: time.Since(start), Initial: initial}
	for _, is := range next.issues {
		if is.IsError() {
			res.Errors++
		} else {
			res.Warnings++
		}
	}
	if !initial && next.Fingerprint() == prev.Fingerprint() {
		m.observe(res)
		return
	}
	m.current.Store(next)
	res.Published = true
	m.report(in, next)
	m.log.Info("settings loaded", "initial", initial, "errors", res.Errors, "warnings", res.Warnings, "duration", res.Duration)
	m.observe(res)
	if initial {
		return
	}
	m.mu.Lock()
	ids := make([]uint64, 0, len(m.subs))
	for id := range m.subs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	subs := make([]func(old, new *Settings), 0, len(ids))
	for _, id := range ids {
		subs = append(subs, m.subs[id])
	}
	m.mu.Unlock()
	for _, fn := range subs {
		m.notify(fn, prev, next)
	}
}

func (m *Manager) notify(fn func(old, new *Settings), prev, next *Settings) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("settings subscriber panicked", "panic", fmt.Sprint(r))
		}
	}()
	fn(prev, next)
}

func (m *Manager) observe(res ReloadResult) {
	if m.opts.Metrics.Reloaded != nil {
		m.opts.Metrics.Reloaded(res)
	}
}

// report logs and emits Events for issues that were not reported for the previous
// snapshot. Messages never contain Secret values.
func (m *Manager) report(in Input, next *Settings) {
	current := map[string]bool{}
	for _, is := range next.issues {
		id := is.String()
		current[id] = true
		if m.reported[id] {
			continue
		}
		level := slog.LevelWarn
		if is.IsError() {
			level = slog.LevelError
		}
		m.log.Log(context.Background(), level, "invalid settings", "object", is.Object, "key", is.Key, "severity", string(is.Severity), "message", is.Message)
		if m.opts.Recorder == nil {
			continue
		}
		obj := findObject(in, is)
		if obj == nil {
			continue
		}
		reason, typ := EventReasonSettingsWarning, corev1.EventTypeWarning
		if is.IsError() {
			reason = EventReasonInvalidSettings
		}
		msg := is.Message
		if is.Key != "" {
			msg = is.Key + ": " + msg
		}
		if len(msg) > maxEventMessageLen {
			msg = msg[:maxEventMessageLen] + "..."
		}
		m.opts.Recorder.Eventf(obj, typ, reason, "%s", msg)
	}
	m.reported = current
}

// findObject returns the input object an issue concerns.
func findObject(in Input, is Issue) runtime.Object {
	if is.Object == "" {
		return nil
	}
	switch is.Source {
	case SourceSecret, SourceNotificationsSecret, SourceReferencedSecret:
		for _, s := range in.Secrets {
			if s.Name == is.Object {
				return s
			}
		}
	default:
		for _, c := range in.ConfigMaps {
			if c.Name == is.Object {
				return c
			}
		}
	}
	return nil
}
