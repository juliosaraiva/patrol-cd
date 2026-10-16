// Package notifications implements the notification configuration API of patrol-server
// (api-contract.md, "Notifications, webhooks, badges, extensions"; parity NOTIF-101..103,
// API-032): the names of the services, triggers and templates configured in
// patrol-notifications-cm of the control-plane namespace, for the UI's subscription editor.
//
// Every authenticated caller may list them (upstream: no RBAC check). Only names (and the
// descriptions of trigger conditions) are returned: service configuration, recipients,
// template bodies and Secret values never leave the server. The configuration is parsed with
// the same catalog default as the notifications controller (internal/notifications), so the
// lists name what the controller can actually use.
package notifications

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// Config is the static configuration of the service.
type Config struct {
	// DisableDefaultCatalog stops merging the built-in trigger and template catalog, as the
	// notifications controller's option of the same name.
	DisableDefaultCatalog bool
}

// Deps are the collaborators of the service.
type Deps struct {
	Config   Config
	Settings SettingsSource
	Logger   *slog.Logger
}

// Service serves the notification routes; it implements apiserver.Service.
type Service struct {
	d Deps

	mu     sync.Mutex
	cached *catalog // parsed lists of the settings fingerprint cached.fingerprint
}

// catalog holds the parsed names of one configuration.
type catalog struct {
	fingerprint string
	services    []apiclient.NotificationService
	triggers    []apiclient.NotificationTrigger
	templates   []apiclient.NotificationTemplate
	err         error
}

// New returns the service.
func New(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{d: d}
}

const tag = "notifications"

// Register adds the notification routes to t.
func (s *Service) Register(t *rest.Table) {
	note := func(a rest.Access) rest.Access {
		a.Note = "any authenticated caller (no RBAC check, as upstream)"
		return a
	}
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathNotificationServices, OperationID: "listNotificationServices",
		Summary: "Configured notification services", Tags: []string{tag}, Access: note(rest.Authenticated()),
		Description: "Names of the services of patrol-notifications-cm (never their configuration); empty when the ConfigMap does not exist.",
	}, s.listServices)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathNotificationTriggers, OperationID: "listNotificationTriggers",
		Summary: "Configured notification triggers", Tags: []string{tag}, Access: note(rest.Authenticated()),
		Description: "Names (and condition descriptions) of the triggers of patrol-notifications-cm, including the built-in catalog; empty when the ConfigMap does not exist.",
	}, s.listTriggers)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathNotificationTemplates, OperationID: "listNotificationTemplates",
		Summary: "Configured notification templates", Tags: []string{tag}, Access: note(rest.Authenticated()),
		Description: "Names of the templates of patrol-notifications-cm, including the built-in catalog; empty when the ConfigMap does not exist.",
	}, s.listTemplates)
}

var (
	errNotConfigured = errors.New("not configured")
	errDoesNotParse  = errors.New("the notifications configuration does not parse")
)

// errInvalidConfig is returned (500) when the configuration does not parse. Its detail is
// fixed: parse errors may quote configuration values.
var errInvalidConfig = &rest.Error{
	Status: http.StatusInternalServerError, Code: apiclient.CodeInternal,
	Detail: "the notifications configuration is invalid; see the settings issues or the notifications controller logs",
}

// placeholder replaces Secret values: only the key names reach the API server's settings
// snapshot, and only names are listed.
const placeholder = "placeholder"

// current returns the parsed configuration of the current settings, reusing the previous
// parse while the settings fingerprint is unchanged.
func (s *Service) current(ctx context.Context) (*catalog, error) {
	if s.d.Settings == nil {
		return nil, rest.Unavailable("settings are not available").WithCause(errNotConfigured)
	}
	st := s.d.Settings.Current()
	if st == nil {
		return nil, rest.Unavailable("settings are not loaded yet")
	}
	fp := st.Fingerprint()
	s.mu.Lock()
	if c := s.cached; c != nil && c.fingerprint == fp && fp != "" {
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	c := s.parse(ctx, st.Notifications())
	c.fingerprint = fp
	s.mu.Lock()
	s.cached = c
	s.mu.Unlock()
	return c, nil
}

func (s *Service) parse(ctx context.Context, n settings.Notifications) *catalog {
	c := &catalog{
		services:  []apiclient.NotificationService{},
		triggers:  []apiclient.NotificationTrigger{},
		templates: []apiclient.NotificationTemplate{},
	}
	if n.ConfigMap == nil {
		// Upstream: a missing ConfigMap is an empty configuration, not an error.
		return c
	}
	secret := make(map[string][]byte, len(n.SecretKeys))
	for _, k := range n.SecretKeys {
		secret[k] = []byte(placeholder)
	}
	var opts []notifications.ParseOption
	if !s.d.Config.DisableDefaultCatalog {
		opts = append(opts, notifications.WithDefaultCatalog())
	}
	cfg, err := notifications.ParseConfig(n.ConfigMap, secret, opts...)
	if err != nil {
		// Parse errors may quote configuration values: only their number is logged (the
		// settings issues name the keys).
		s.d.Logger.LogAttrs(ctx, slog.LevelWarn, "notifications configuration does not parse",
			slog.String("requestId", rest.RequestID(ctx)), slog.Int("problems", countErrors(err)))
		c.err = errInvalidConfig.WithCause(errDoesNotParse)
		return c
	}
	for _, svc := range cfg.ListServices() {
		c.services = append(c.services, apiclient.NotificationService{Name: svc.Name})
	}
	for _, tr := range cfg.ListTriggers() {
		c.triggers = append(c.triggers, apiclient.NotificationTrigger{Name: tr.Name, Description: describe(tr.Conditions)})
	}
	for _, name := range cfg.ListTemplates() {
		c.templates = append(c.templates, apiclient.NotificationTemplate{Name: name})
	}
	return c
}

// describe joins the distinct non-empty condition descriptions of a trigger.
func describe(conds []notifications.Condition) string {
	var out []string
	for _, c := range conds {
		d := strings.TrimSpace(c.Description)
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return strings.Join(out, "; ")
}

func countErrors(err error) int {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return len(j.Unwrap())
	}
	return 1
}

// listServices serves GET /api/v1/notifications/services.
func (s *Service) listServices(ctx context.Context, _ *apiclient.Empty) (*apiclient.NotificationServiceList, error) {
	c, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	if c.err != nil {
		return nil, c.err
	}
	return &apiclient.NotificationServiceList{Items: slices.Clone(c.services)}, nil
}

// listTriggers serves GET /api/v1/notifications/triggers.
func (s *Service) listTriggers(ctx context.Context, _ *apiclient.Empty) (*apiclient.NotificationTriggerList, error) {
	c, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	if c.err != nil {
		return nil, c.err
	}
	return &apiclient.NotificationTriggerList{Items: slices.Clone(c.triggers)}, nil
}

// listTemplates serves GET /api/v1/notifications/templates.
func (s *Service) listTemplates(ctx context.Context, _ *apiclient.Empty) (*apiclient.NotificationTemplateList, error) {
	c, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	if c.err != nil {
		return nil, c.err
	}
	return &apiclient.NotificationTemplateList{Items: slices.Clone(c.templates)}, nil
}
