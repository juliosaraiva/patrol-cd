package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/catalog"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// Configuration keys of the notifications ConfigMap.
const (
	keySubscriptions   = "subscriptions"
	keyDefaultTriggers = "defaultTriggers"
	keyContext         = "context"
	keyLegacyConfig    = "config.yaml"
	keyLegacyNotifiers = "notifiers.yaml"
	prefixService      = "service."
	prefixTrigger      = "trigger."
	prefixTemplate     = "template."
	prefixDefaultTrig  = "defaultTriggers."
	// maxConfigValueBytes bounds a single ConfigMap value (ConfigMaps are capped at 1 MiB).
	maxConfigValueBytes = 1 << 20
)

// Errors returned by configuration parsing and the engine.
var (
	// ErrInvalidConfig reports an unparseable configuration entry.
	ErrInvalidConfig = errors.New("invalid notifications configuration")
	// ErrTriggerNotFound reports a subscription or request naming an undefined trigger.
	ErrTriggerNotFound = errors.New("trigger not found")
	// ErrTemplateNotFound reports a trigger or request naming an undefined template.
	ErrTemplateNotFound = errors.New("template not found")
	// ErrServiceNotConfigured reports a destination whose service is not configured.
	ErrServiceNotConfigured = errors.New("service not configured")
	// ErrInvalidApplication reports an application object that cannot be processed.
	ErrInvalidApplication = errors.New("invalid application object")
	// ErrRepoServiceUnavailable is returned by repo.* helpers when no RepoService is wired.
	ErrRepoServiceUnavailable = errors.New("repository service is not available")
	// ErrTemplateOutputTooLarge reports a template field rendering beyond the size limit.
	ErrTemplateOutputTooLarge = errors.New("rendered template exceeds size limit")
	// ErrTemplateLimitExceeded reports a template (or a strings.* helper) exceeding the
	// render resource limits (TemplateLimits) or building a value the sandbox refuses
	// (too large, too deep or cyclic).
	ErrTemplateLimitExceeded = errors.New("template resource limit exceeded")
)

// Condition is one condition of a trigger.
type Condition struct {
	When        string   `json:"when,omitempty"`
	OncePer     string   `json:"oncePer,omitempty"`
	Send        []string `json:"send,omitempty"`
	Description string   `json:"description,omitempty"`
}

// DefaultSubscription is a centrally configured subscription (ConfigMap key
// "subscriptions"). Recipients are "<service>:<recipient>" or "<service>".
type DefaultSubscription struct {
	Recipients []string `json:"recipients,omitempty"`
	Triggers   []string `json:"triggers,omitempty"`
	Selector   string   `json:"selector,omitempty"`

	selector labels.Selector
}

// Matches reports whether the subscription applies to an object with the given labels.
func (s DefaultSubscription) Matches(objLabels map[string]string) bool {
	if s.selector == nil {
		return true
	}
	return s.selector.Matches(labels.Set(objLabels))
}

// ServiceConfig is one configured notification service.
type ServiceConfig struct {
	// Name is the service name used in subscriptions (defaults to the type).
	Name string
	// Type is the service type (slack, email, webhook, ...).
	Type string
	// Options is the secret-substituted JSON form of the service options. It contains
	// credentials: never log it.
	Options json.RawMessage

	secretValues []string
}

// ServiceInfo describes a configured service for listing APIs.
type ServiceInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TriggerInfo describes a configured trigger for listing APIs.
type TriggerInfo struct {
	Name       string      `json:"name"`
	Conditions []Condition `json:"conditions"`
}

// Config is the parsed notifications configuration.
type Config struct {
	Services               map[string]ServiceConfig
	Templates              map[string]services.Notification
	Triggers               map[string][]Condition
	DefaultTriggers        []string
	ServiceDefaultTriggers map[string][]string
	Subscriptions          []DefaultSubscription
	Context                map[string]string
	// Secrets are the notifications Secret values exposed to templates as .secrets.
	Secrets map[string]string
	// Warnings lists non-fatal configuration issues (unknown keys, missing secret keys,
	// entries skipped in lenient mode).
	Warnings []string
}

type parseOptions struct {
	catalog bool
	lenient bool
}

// ParseOption customizes ParseConfig.
type ParseOption func(*parseOptions)

// WithDefaultCatalog merges the built-in catalog triggers and templates; keys present in
// the ConfigMap take precedence.
func WithDefaultCatalog() ParseOption { return func(o *parseOptions) { o.catalog = true } }

// Lenient makes ParseConfig skip invalid entries (recording a warning) instead of failing.
// The default is strict, matching the baseline where one invalid key invalidates the
// whole configuration.
func Lenient() ParseOption { return func(o *parseOptions) { o.lenient = true } }

// ParseConfig parses the notifications ConfigMap data and Secret data.
func ParseConfig(cmData map[string]string, secretData map[string][]byte, opts ...ParseOption) (*Config, error) {
	var po parseOptions
	for _, o := range opts {
		o(&po)
	}
	data := make(map[string]string, len(cmData))
	if po.catalog {
		cat, err := catalog.Data()
		if err != nil {
			return nil, err
		}
		for k, v := range cat {
			data[k] = v
		}
	}
	for k, v := range cmData {
		data[k] = v
	}
	secrets := make(map[string]string, len(secretData))
	for k, v := range secretData {
		secrets[k] = string(v)
	}
	cfg := &Config{
		Services:               map[string]ServiceConfig{},
		Templates:              map[string]services.Notification{},
		Triggers:               map[string][]Condition{},
		ServiceDefaultTriggers: map[string][]string{},
		Context:                map[string]string{},
		Secrets:                secrets,
	}

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []error
	fail := func(key string, err error) {
		e := fmt.Errorf("%w: key %q: %v", ErrInvalidConfig, key, err)
		if po.lenient {
			cfg.Warnings = append(cfg.Warnings, e.Error())
			return
		}
		errs = append(errs, e)
	}
	for _, k := range keys {
		v := data[k]
		if len(v) > maxConfigValueBytes {
			fail(k, fmt.Errorf("value exceeds %d bytes", maxConfigValueBytes))
			continue
		}
		switch {
		case k == keySubscriptions:
			subs, err := parseDefaultSubscriptions(v)
			if err != nil {
				fail(k, err)
				continue
			}
			cfg.Subscriptions = subs
		case k == keyDefaultTriggers:
			var list []string
			if err := yaml.Unmarshal([]byte(v), &list); err != nil {
				fail(k, err)
				continue
			}
			cfg.DefaultTriggers = list
		case strings.HasPrefix(k, prefixDefaultTrig):
			var list []string
			if err := yaml.Unmarshal([]byte(v), &list); err != nil {
				fail(k, err)
				continue
			}
			cfg.ServiceDefaultTriggers[strings.TrimPrefix(k, prefixDefaultTrig)] = list
		case k == keyContext:
			ctxMap, err := parseContext(v)
			if err != nil {
				fail(k, err)
				continue
			}
			for ck, cv := range ctxMap {
				cfg.Context[ck] = cv
			}
		case strings.HasPrefix(k, prefixTrigger):
			conds, err := parseTrigger(v)
			if err != nil {
				fail(k, err)
				continue
			}
			cfg.Triggers[strings.TrimPrefix(k, prefixTrigger)] = conds
		case strings.HasPrefix(k, prefixTemplate):
			tmpl, err := parseTemplate(v)
			if err != nil {
				fail(k, err)
				continue
			}
			cfg.Templates[strings.TrimPrefix(k, prefixTemplate)] = tmpl
		case strings.HasPrefix(k, prefixService):
			sc, warnings, err := parseService(k, v, secretData)
			cfg.Warnings = append(cfg.Warnings, warnings...)
			if err != nil {
				fail(k, err)
				continue
			}
			if prev, dup := cfg.Services[sc.Name]; dup {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("service name %q is defined by more than one key (types %s and %s); the last key wins", sc.Name, prev.Type, sc.Type))
			}
			cfg.Services[sc.Name] = sc
		case k == keyLegacyConfig:
			// Applied after all other keys (see applyLegacyConfig).
		default:
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("key %q is not supported", k))
		}
	}
	if err := applyLegacy(cfg, data[keyLegacyConfig], secretData, &po); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func parseContext(v string) (map[string]string, error) {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(v), &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		switch t := val.(type) {
		case nil:
			out[k] = ""
		case string:
			out[k] = t
		case map[string]any, []any:
			return nil, fmt.Errorf("context value %q must be a scalar", k)
		default:
			out[k] = fmt.Sprint(t)
		}
	}
	return out, nil
}

// parseTrigger accepts a list of conditions or a single condition object.
func parseTrigger(v string) ([]Condition, error) {
	var list []Condition
	errList := yaml.Unmarshal([]byte(v), &list)
	if errList == nil {
		return list, nil
	}
	var single Condition
	if err := yaml.Unmarshal([]byte(v), &single); err != nil {
		return nil, errList
	}
	return []Condition{single}, nil
}

func parseTemplate(v string) (services.Notification, error) {
	var n services.Notification
	if err := yaml.Unmarshal([]byte(v), &n); err != nil {
		return n, err
	}
	// The AWS SQS documentation places messageGroupId at the top level of the template.
	var top struct {
		MessageGroupID string `json:"messageGroupId"`
	}
	if err := yaml.Unmarshal([]byte(v), &top); err == nil && top.MessageGroupID != "" {
		if n.AwsSqs == nil {
			n.AwsSqs = &services.AwsSqsNotification{}
		}
		if n.AwsSqs.MessageGroupID == "" {
			n.AwsSqs.MessageGroupID = top.MessageGroupID
		}
	}
	return n, nil
}

func parseDefaultSubscriptions(v string) ([]DefaultSubscription, error) {
	var subs []DefaultSubscription
	if err := yaml.Unmarshal([]byte(v), &subs); err != nil {
		return nil, err
	}
	for i := range subs {
		if strings.TrimSpace(subs[i].Selector) == "" {
			continue
		}
		sel, err := labels.Parse(subs[i].Selector)
		if err != nil {
			return nil, fmt.Errorf("subscription %d: invalid selector: %w", i, err)
		}
		subs[i].selector = sel
	}
	return subs, nil
}

// parseService parses a service.<type>[.<name>] entry, substituting $secret references
// in keys and string values of the parsed document (so secret values can never alter the
// YAML structure).
func parseService(key, v string, secretData map[string][]byte) (ServiceConfig, []string, error) {
	rest := strings.TrimPrefix(key, prefixService)
	typ, name, hasName := strings.Cut(rest, ".")
	if typ == "" {
		return ServiceConfig{}, nil, errors.New("service type is empty")
	}
	if !hasName || name == "" {
		name = typ
	}
	// UseNumber keeps numeric scalars verbatim (no float64 rounding) so that the service
	// decodes them with the type hints of its option struct, like the baseline does.
	var tree any
	if err := yaml.Unmarshal([]byte(v), &tree, useNumber); err != nil {
		return ServiceConfig{}, nil, err
	}
	sub := &secretSubstituter{data: secretData}
	tree = sub.walk(tree)
	opts, err := json.Marshal(tree)
	if err != nil {
		return ServiceConfig{}, nil, err
	}
	var warnings []string
	for _, missing := range sub.missingKeys() {
		warnings = append(warnings, fmt.Sprintf("service %q references secret key %q which does not exist", name, missing))
	}
	return ServiceConfig{Name: name, Type: typ, Options: opts, secretValues: sub.used()}, warnings, nil
}

func useNumber(d *json.Decoder) *json.Decoder {
	d.UseNumber()
	return d
}

var secretRef = regexp.MustCompile(`\$[\w-]+(?:\.[\w-]+)*`)

type secretSubstituter struct {
	data    map[string][]byte
	values  map[string]struct{}
	missing map[string]struct{}
}

func (s *secretSubstituter) walk(v any) any {
	switch t := v.(type) {
	case string:
		return s.replace(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[s.replace(k)] = s.walk(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = s.walk(val)
		}
		return out
	default:
		return v
	}
}

// replace substitutes every $key reference. A reference may contain dots (secret keys
// may); when the full name does not exist the longest existing dot-separated prefix is
// used and the remainder kept literally. Unknown references are left untouched.
func (s *secretSubstituter) replace(in string) string {
	if !strings.Contains(in, "$") {
		return in
	}
	return secretRef.ReplaceAllStringFunc(in, func(ref string) string {
		name := ref[1:]
		for candidate, suffix := name, ""; ; {
			if val, ok := s.data[candidate]; ok {
				if s.values == nil {
					s.values = map[string]struct{}{}
				}
				s.values[string(val)] = struct{}{}
				return string(val) + suffix
			}
			i := strings.LastIndex(candidate, ".")
			if i < 0 {
				break
			}
			suffix = candidate[i:] + suffix
			candidate = candidate[:i]
		}
		if s.missing == nil {
			s.missing = map[string]struct{}{}
		}
		s.missing[name] = struct{}{}
		return ref
	})
}

func (s *secretSubstituter) used() []string {
	out := make([]string, 0, len(s.values))
	for v := range s.values {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (s *secretSubstituter) missingKeys() []string {
	out := make([]string, 0, len(s.missing))
	for v := range s.missing {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ListServices returns the configured services sorted by name.
func (c *Config) ListServices() []ServiceInfo {
	out := make([]ServiceInfo, 0, len(c.Services))
	for name, sc := range c.Services {
		out = append(out, ServiceInfo{Name: name, Type: sc.Type})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListTriggers returns the configured triggers sorted by name.
func (c *Config) ListTriggers() []TriggerInfo {
	out := make([]TriggerInfo, 0, len(c.Triggers))
	for name, conds := range c.Triggers {
		out = append(out, TriggerInfo{Name: name, Conditions: append([]Condition(nil), conds...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListTemplates returns the configured template names sorted.
func (c *Config) ListTemplates() []string {
	out := make([]string, 0, len(c.Templates))
	for name := range c.Templates {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Validate checks cross references and compiles every trigger condition and template. It
// returns all problems joined; a nil error means the configuration is fully usable.
func (c *Config) Validate() error {
	var errs []error
	for _, svc := range c.ListServices() {
		if !services.IsSupportedType(svc.Type) {
			errs = append(errs, fmt.Errorf("%w: service %q: service type '%s' is not supported", ErrInvalidConfig, svc.Name, svc.Type))
		}
	}
	for _, tr := range c.ListTriggers() {
		ct := compileTrigger(tr.Name, tr.Conditions)
		for _, cc := range ct.conditions {
			if cc.err != nil {
				errs = append(errs, fmt.Errorf("%w: trigger %q: %v", ErrInvalidConfig, tr.Name, cc.err))
			}
			for _, tmpl := range cc.Send {
				if _, ok := c.Templates[tmpl]; !ok {
					errs = append(errs, fmt.Errorf("%w: trigger %q sends undefined template %q", ErrInvalidConfig, tr.Name, tmpl))
				}
			}
		}
	}
	for _, name := range c.ListTemplates() {
		if _, err := compileTemplate(name, c.Templates[name]); err != nil {
			errs = append(errs, fmt.Errorf("%w: template %q: %v", ErrInvalidConfig, name, err))
		}
	}
	checkTriggers := func(where string, names []string) {
		for _, t := range names {
			if _, ok := c.Triggers[t]; !ok {
				errs = append(errs, fmt.Errorf("%w: %s references undefined trigger %q", ErrInvalidConfig, where, t))
			}
		}
	}
	checkTriggers("defaultTriggers", c.DefaultTriggers)
	for svc, names := range c.ServiceDefaultTriggers {
		checkTriggers("defaultTriggers."+svc, names)
	}
	for i, s := range c.Subscriptions {
		checkTriggers(fmt.Sprintf("subscriptions[%d]", i), s.Triggers)
	}
	return errors.Join(errs...)
}
