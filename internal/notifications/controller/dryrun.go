package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// DefaultRecipient is the recipient of "template notify" when none is given: the rendered
// notification is printed (upstream default console:stdout).
const DefaultRecipient = services.TypeConsole + ":stdout"

// maxManifestBytes bounds an Application manifest read from a file or stdin.
const maxManifestBytes = 4 << 20

// DryRunOptions configure NewDryRun.
type DryRunOptions struct {
	// Projects resolves .appProject and project-level subscriptions (nil: none).
	Projects notifications.ProjectGetter
	// Operations completes status.operationState like the controller (nil: summary only).
	Operations OperationGetter
	// Repo serves the repo.* helpers (nil: they fail).
	Repo notifications.RepoService
	// Network configures real deliveries of TemplateNotify (non-console recipients).
	Network services.Network
	// Output receives console deliveries (default io.Discard).
	Output io.Writer
	// DisableDefaultCatalog stops merging the built-in catalog (as Config).
	DisableDefaultCatalog bool
	// TemplateLimits bounds each render.
	TemplateLimits notifications.TemplateLimits
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logger receives engine logs (default: discard).
	Logger *slog.Logger
}

// DryRun evaluates a notifications configuration outside the controller. It backs
// "patrol admin notifications trigger run", "template notify" and a dry evaluation of what the
// controller would deliver. Like upstream's CLI, .secrets is available to templates.
type DryRun struct {
	cfg  *notifications.Config
	opts DryRunOptions
}

// Evaluation is the outcome of DryRun.Evaluate.
type Evaluation struct {
	// Skipped is true when the controller would not evaluate the Application now (stale sync
	// status after an operation, NOTIF-010); SkipReason says why.
	Skipped    bool
	SkipReason string
	// Results has one entry per triggered or failed (condition, destination): StatusDryRun with
	// the rendered notification for what would be sent, StatusSkippedDuplicate for deliveries
	// already recorded in the Application's delivery state, StatusFailed otherwise.
	Results []notifications.DeliveryResult
}

// NewDryRun parses a ConfigMap/Secret pair (as read by LoadConfig or from files).
func NewDryRun(cmData map[string]string, secretData map[string][]byte, opts DryRunOptions) (*DryRun, error) {
	var parseOpts []notifications.ParseOption
	if !opts.DisableDefaultCatalog {
		parseOpts = append(parseOpts, notifications.WithDefaultCatalog())
	}
	cfg, err := notifications.ParseConfig(cmData, secretData, parseOpts...)
	if err != nil {
		return nil, errors.New(secretRedactor(secretData).Redact(err.Error()))
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &DryRun{cfg: cfg, opts: opts}, nil
}

// Config returns the parsed configuration (listings of "template get" / "trigger get").
func (d *DryRun) Config() *notifications.Config { return d.cfg }

func (d *DryRun) engine(dryRun bool, overlay map[string]services.Service) (*notifications.Engine, error) {
	opts := []notifications.Option{
		notifications.WithDryRun(dryRun),
		notifications.WithLogger(d.opts.Logger),
		notifications.WithClock(d.opts.Now),
		notifications.WithNetwork(d.opts.Network),
		notifications.WithTemplateLimits(d.opts.TemplateLimits),
	}
	if d.opts.Projects != nil {
		opts = append(opts, notifications.WithProjectGetter(d.opts.Projects))
	}
	if d.opts.Repo != nil {
		opts = append(opts, notifications.WithRepoService(d.opts.Repo))
	}
	if overlay != nil {
		opts = append(opts, notifications.WithServices(overlay))
	}
	return notifications.NewEngine(d.cfg, opts...)
}

// TriggerRun evaluates every condition of trigger against app without sending anything
// ("admin notifications trigger run"). Condition evaluation errors are in each result's Err.
func (d *DryRun) TriggerRun(ctx context.Context, trigger string, app map[string]any) ([]notifications.ConditionResult, error) {
	tmpl, err := TemplateApplication(ctx, app, d.opts.Operations)
	if err != nil {
		return nil, err
	}
	eng, err := d.engine(true, nil)
	if err != nil {
		return nil, err
	}
	return eng.RunTrigger(ctx, trigger, tmpl)
}

// TemplateNotify renders template for app and delivers it to each recipient
// ("<service>:<recipient>", or "<service>" for none), ignoring triggers and the delivery state
// ("admin notifications template notify"). The default recipient DefaultRecipient prints to
// Output. Other services are really contacted.
func (d *DryRun) TemplateNotify(ctx context.Context, template string, app map[string]any, recipients []string) ([]notifications.DeliveryResult, error) {
	if _, ok := d.cfg.Templates[template]; !ok {
		return nil, fmt.Errorf("%w: template '%s' is not supported", notifications.ErrTemplateNotFound, template)
	}
	if len(recipients) == 0 {
		recipients = []string{DefaultRecipient}
	}
	dests := make([]services.Destination, 0, len(recipients))
	for _, r := range recipients {
		dest, err := ParseRecipient(r)
		if err != nil {
			return nil, err
		}
		dests = append(dests, dest)
	}
	tmpl, err := TemplateApplication(ctx, app, d.opts.Operations)
	if err != nil {
		return nil, err
	}
	eng, err := d.engine(false, map[string]services.Service{services.TypeConsole: services.NewConsole(d.opts.Output)})
	if err != nil {
		return nil, err
	}
	return eng.NotifyTemplate(ctx, tmpl, template, dests)
}

// Evaluate reports what the controller would deliver for app now, rendering every notification
// without sending it and without changing the delivery state.
func (d *DryRun) Evaluate(ctx context.Context, app map[string]any) (*Evaluation, error) {
	tmpl, err := TemplateApplication(ctx, app, d.opts.Operations)
	if err != nil {
		return nil, err
	}
	if ok, reason := notifications.SyncStatusRefreshed(tmpl); !ok {
		return &Evaluation{Skipped: true, SkipReason: reason}, nil
	}
	eng, err := d.engine(true, nil)
	if err != nil {
		return nil, err
	}
	meta, _ := app["metadata"].(map[string]any)
	annotations := map[string]string{}
	if a, ok := meta["annotations"].(map[string]any); ok {
		for k, v := range a {
			if s, ok := v.(string); ok {
				annotations[k] = s
			}
		}
	}
	state := notifications.DeliveryStateFromAnnotations(annotations, eng.AnnotationPrefix())
	_, results, err := eng.Process(ctx, tmpl, state)
	if err != nil {
		return nil, err
	}
	return &Evaluation{Results: results}, nil
}

// ParseRecipient parses "<service>:<recipient>" (split at the first colon) or "<service>".
func ParseRecipient(s string) (services.Destination, error) {
	service, recipient, _ := strings.Cut(strings.TrimSpace(s), ":")
	if service == "" {
		return services.Destination{}, fmt.Errorf("invalid recipient %q: expected <service>:<recipient>", s)
	}
	return services.Destination{Service: service, Recipient: recipient}, nil
}

// LoadConfig reads the notifications ConfigMap and Secret of namespace. Missing objects yield
// empty data, as upstream.
func LoadConfig(ctx context.Context, reader client.Reader, namespace, cmName, secretName string) (map[string]string, map[string][]byte, error) {
	if cmName == "" {
		cmName = DefaultConfigMapName
	}
	if secretName == "" {
		secretName = DefaultSecretName
	}
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, cm); err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("getting ConfigMap %s/%s: %w", namespace, cmName, err)
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("getting Secret %s/%s: %w", namespace, secretName, err)
	}
	return cm.Data, secret.Data, nil
}

// GetApplication reads an Application as an unstructured map.
func GetApplication(ctx context.Context, reader client.Reader, namespace, name string) (map[string]any, error) {
	u := newApplication()
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		return nil, fmt.Errorf("getting application %s/%s: %w", namespace, name, err)
	}
	return u.Object, nil
}

// ParseApplication decodes an Application manifest (YAML or JSON, at most 4 MiB), e.g. the
// RESOURCE argument of "trigger run" given as a file.
func ParseApplication(data []byte) (map[string]any, error) {
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("application manifest exceeds %d bytes", maxManifestBytes)
	}
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("decoding application manifest: %w", err)
	}
	var obj map[string]any
	if err := utiljson.Unmarshal(j, &obj); err != nil {
		return nil, fmt.Errorf("decoding application manifest: %w", err)
	}
	if obj == nil {
		return nil, errors.New("application manifest is empty")
	}
	if kind, ok := obj["kind"].(string); ok && kind != "Application" {
		return nil, fmt.Errorf("manifest is a %s, not an Application", kind)
	}
	if _, ok := obj["metadata"].(map[string]any); !ok {
		return nil, errors.New("application manifest has no metadata")
	}
	return obj, nil
}
