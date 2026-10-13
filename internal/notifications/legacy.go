package notifications

import (
	"encoding/json"
	"fmt"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// Deprecated configuration formats of the baseline (argocd-notifications v0.x), still
// honored by Argo CD v3.5.5: ConfigMap key "config.yaml" (triggers, templates, context,
// subscriptions) and Secret key "notifiers.yaml" (email, slack, opsgenie, grafana and a
// list of webhooks).

type legacyTemplate struct {
	Name  string `json:"name,omitempty"`
	Title string `json:"subject,omitempty"`
	Body  string `json:"body,omitempty"`
	services.Notification
}

type legacyTrigger struct {
	Name        string `json:"name,omitempty"`
	Condition   string `json:"condition,omitempty"`
	Description string `json:"description,omitempty"`
	Template    string `json:"template,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type legacyConfig struct {
	Triggers      []legacyTrigger       `json:"triggers,omitempty"`
	Templates     []legacyTemplate      `json:"templates,omitempty"`
	Context       map[string]string     `json:"context,omitempty"`
	Subscriptions []DefaultSubscription `json:"subscriptions,omitempty"`
}

func applyLegacy(cfg *Config, configYAML string, secretData map[string][]byte, po *parseOptions) error {
	fail := func(key string, err error) error {
		e := fmt.Errorf("%w: key %q: %v", ErrInvalidConfig, key, err)
		if po.lenient {
			cfg.Warnings = append(cfg.Warnings, e.Error())
			return nil
		}
		return e
	}
	if notifiers, ok := secretData[keyLegacyNotifiers]; ok && len(notifiers) > 0 {
		cfg.Warnings = append(cfg.Warnings, "Secret key \"notifiers.yaml\" is deprecated; migrate to service.<type> ConfigMap keys")
		if err := applyLegacyNotifiers(cfg, notifiers); err != nil {
			if e := fail(keyLegacyNotifiers, err); e != nil {
				return e
			}
		}
	}
	if configYAML == "" {
		return nil
	}
	cfg.Warnings = append(cfg.Warnings, "ConfigMap key \"config.yaml\" is deprecated; migrate to trigger.*, template.*, context and subscriptions keys")
	var legacy legacyConfig
	if err := yaml.Unmarshal([]byte(configYAML), &legacy); err != nil {
		return fail(keyLegacyConfig, err)
	}
	for k, v := range legacy.Context {
		cfg.Context[k] = v
	}
	if legacy.Subscriptions != nil {
		subs, err := reparseSubscriptions(legacy.Subscriptions)
		if err != nil {
			return fail(keyLegacyConfig, err)
		}
		cfg.Subscriptions = subs
	}
	for _, t := range legacy.Templates {
		n := t.Notification
		if t.Title != "" {
			if n.Email == nil {
				n.Email = &services.EmailNotification{}
			}
			n.Email.Subject = t.Title
		}
		if t.Body != "" {
			n.Message = t.Body
		}
		cfg.Templates[t.Name] = n
	}
	for _, t := range legacy.Triggers {
		if t.Enabled != nil && *t.Enabled {
			cfg.DefaultTriggers = append(cfg.DefaultTriggers, t.Name)
		}
		conds := append([]Condition(nil), cfg.Triggers[t.Name]...)
		if len(conds) == 0 {
			conds = []Condition{{}}
		}
		first := conds[0]
		if t.Condition != "" {
			first.When = t.Condition
		}
		if t.Template != "" {
			first.Send = []string{t.Template}
		}
		if t.Description != "" {
			first.Description = t.Description
		}
		conds[0] = first
		cfg.Triggers[t.Name] = conds
	}
	return nil
}

func reparseSubscriptions(subs []DefaultSubscription) ([]DefaultSubscription, error) {
	b, err := json.Marshal(subs)
	if err != nil {
		return nil, err
	}
	return parseDefaultSubscriptions(string(b))
}

func applyLegacyNotifiers(cfg *Config, data []byte) error {
	var raw map[string]json.RawMessage
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, typ := range keys {
		body := raw[typ]
		switch typ {
		case services.TypeEmail, services.TypeSlack, services.TypeOpsgenie, services.TypeGrafana:
			if string(body) == "null" {
				continue
			}
			cfg.Services[typ] = ServiceConfig{Name: typ, Type: typ, Options: body, secretValues: stringLeaves(body)}
		case services.TypeWebhook:
			var hooks []map[string]any
			if err := json.Unmarshal(body, &hooks); err != nil {
				return fmt.Errorf("webhook: %w", err)
			}
			for _, h := range hooks {
				name, _ := h["name"].(string)
				if name == "" {
					return fmt.Errorf("webhook: entry without name")
				}
				delete(h, "name")
				opts, err := json.Marshal(h)
				if err != nil {
					return err
				}
				cfg.Services[name] = ServiceConfig{Name: name, Type: services.TypeWebhook, Options: opts, secretValues: stringLeaves(opts)}
			}
		default:
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("notifiers.yaml: section %q is not supported", typ))
		}
	}
	return nil
}

// stringLeaves returns every string value of a JSON document; legacy notifier settings live
// in a Secret, so all of them are treated as sensitive.
func stringLeaves(doc []byte) []string {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			for _, val := range t {
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
	return out
}
