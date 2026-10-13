package notifications

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func serviceOptions(t *testing.T, cfg *Config, name string) map[string]any {
	t.Helper()
	sc, ok := cfg.Services[name]
	if !ok {
		t.Fatalf("service %q not configured; have %v", name, cfg.ListServices())
	}
	var m map[string]any
	if err := json.Unmarshal(sc.Options, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseConfigServicesAndSecrets(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"service.slack":            "token: $slack-token\nusername: patrol\nicon: \":rocket:\"",
		"service.slack.workspace2": "token: $slack.token2",
		"service.webhook.github":   "url: https://api.github.com\nheaders:\n- name: Authorization\n  value: token $github-token\n",
		"service.opsgenie":         "apiUrl: api.opsgenie.com\napiKeys:\n  $opsgenie-team-id: $opsgenie-team-api-key\n",
		"service.email.gmail":      "host: smtp.gmail.com\nport: 465\nfrom: $email-username\nusername: $email-username\npassword: $missing-key\n",
		"service.webhook.my.hook":  "url: https://hooks.example.com/$hook-path.json",
		"service.github":           "appID: 1",
		"service.github.ghapp":     "appID: 12345\ninstallationID: $gh-installation\nprivateKey: $github-privateKey",
	}
	secret := map[string][]byte{
		"slack-token":           []byte("xoxb-1"),
		"slack.token2":          []byte("xoxb-2"),
		"github-token":          []byte("ghp_abc"),
		"opsgenie-team-id":      []byte("team-uuid"),
		"opsgenie-team-api-key": []byte("genie-key"),
		"email-username":        []byte("bot@example.com"),
		"hook-path":             []byte("abc"),
		"gh-installation":       []byte("777"),
		"github-privateKey":     []byte("-----BEGIN RSA PRIVATE KEY-----\nline: with colon\n  indented: value\n-----END RSA PRIVATE KEY-----\n"),
	}
	cfg, err := ParseConfig(cm, secret)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := serviceOptions(t, cfg, "slack"); got["token"] != "xoxb-1" || got["icon"] != ":rocket:" {
		t.Fatalf("slack options %v", got)
	}
	if cfg.Services["workspace2"].Type != "slack" || serviceOptions(t, cfg, "workspace2")["token"] != "xoxb-2" {
		t.Fatalf("named service with dotted secret key: %+v", cfg.Services["workspace2"])
	}
	gh := serviceOptions(t, cfg, "ghapp")
	if gh["privateKey"] != string(secret["github-privateKey"]) || gh["installationID"] != "777" || gh["appID"] != float64(12345) {
		t.Fatalf("secret values must be substituted verbatim without altering structure: %v", gh)
	}
	wh := cfg.Services["github"]
	if wh.Type != "webhook" {
		t.Fatalf("keys are applied in sorted order: service.webhook.github must win over service.github, got type %q", wh.Type)
	}
	if !containsWarning(cfg.Warnings, `service name "github" is defined by more than one key`) {
		t.Fatalf("duplicate service name warning missing: %v", cfg.Warnings)
	}
	og := serviceOptions(t, cfg, "opsgenie")
	if keys := og["apiKeys"].(map[string]any); keys["team-uuid"] != "genie-key" {
		t.Fatalf("map keys must be substituted: %v", og)
	}
	if cfg.Services["my.hook"].Type != "webhook" || serviceOptions(t, cfg, "my.hook")["url"] != "https://hooks.example.com/abc.json" {
		t.Fatalf("dotted service name / partial reference: %v", serviceOptions(t, cfg, "my.hook"))
	}
	mail := serviceOptions(t, cfg, "gmail")
	if mail["password"] != "$missing-key" || mail["from"] != "bot@example.com" {
		t.Fatalf("missing secret keys stay literal: %v", mail)
	}
	if !containsWarning(cfg.Warnings, `secret key "missing-key"`) {
		t.Fatalf("missing secret key warning absent: %v", cfg.Warnings)
	}
	if got := cfg.Services["gmail"].secretValues; !contains(got, "bot@example.com") {
		t.Fatalf("substituted values must be tracked for redaction: %v", got)
	}
	if cfg.Secrets["slack-token"] != "xoxb-1" {
		t.Fatalf("secrets exposed to templates: %v", cfg.Secrets)
	}
}

func containsWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestParseConfigTriggersTemplatesAndDefaults(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"trigger.on-list":            "- when: app.status.sync.status == 'Unknown'\n  send: [t1]\n- when: 'true'\n  oncePer: app.metadata.name\n  send: [t1, t2]\n  description: second",
		"trigger.on-single":          "when: app.status.health.status == 'Degraded'\noncePer: app.status.sync.revision\nsend: [t1]",
		"template.t1":                "message: hello {{.app.metadata.name}}\nemail:\n  subject: s\nslack:\n  attachments: '[]'\n  notifyBroadcast: true\n",
		"template.t2":                "messageGroupId: '{{.app.metadata.name}}-deployment'\nmessage: m\n",
		"defaultTriggers":            "- on-list\n",
		"defaultTriggers.mattermost": "- on-single\n",
		"context":                    "argocdUrl: https://cd.example.com\nport: 8080\nenabled: true\n",
		"subscriptions":              "- recipients: [slack:test2, email:test@gmail.com]\n  triggers: [on-list]\n- recipients: [slack:test3]\n  selector: test=true\n  triggers: [on-single]\n",
		"unknown.key":                "x",
	}
	cfg, err := ParseConfig(cm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if l := cfg.Triggers["on-list"]; len(l) != 2 || l[1].OncePer != "app.metadata.name" || l[1].Description != "second" || len(l[1].Send) != 2 {
		t.Fatalf("on-list %+v", l)
	}
	if s := cfg.Triggers["on-single"]; len(s) != 1 || s[0].OncePer != "app.status.sync.revision" {
		t.Fatalf("single-object trigger %+v", s)
	}
	t1 := cfg.Templates["t1"]
	if t1.Email == nil || t1.Email.Subject != "s" || t1.Slack == nil || !t1.Slack.NotifyBroadcast {
		t.Fatalf("t1 %+v", t1)
	}
	if t2 := cfg.Templates["t2"]; t2.AwsSqs == nil || t2.AwsSqs.MessageGroupID != "{{.app.metadata.name}}-deployment" {
		t.Fatalf("top-level messageGroupId must be hoisted: %+v", t2.AwsSqs)
	}
	if strings.Join(cfg.DefaultTriggers, ",") != "on-list" || strings.Join(cfg.ServiceDefaultTriggers["mattermost"], ",") != "on-single" {
		t.Fatalf("default triggers %v %v", cfg.DefaultTriggers, cfg.ServiceDefaultTriggers)
	}
	if cfg.Context["port"] != "8080" || cfg.Context["enabled"] != "true" || cfg.Context["argocdUrl"] != "https://cd.example.com" {
		t.Fatalf("context %v", cfg.Context)
	}
	if len(cfg.Subscriptions) != 2 || !cfg.Subscriptions[0].Matches(nil) || cfg.Subscriptions[1].Matches(map[string]string{"test": "false"}) || !cfg.Subscriptions[1].Matches(map[string]string{"test": "true"}) {
		t.Fatalf("subscriptions %+v", cfg.Subscriptions)
	}
	if !containsWarning(cfg.Warnings, `"unknown.key" is not supported`) {
		t.Fatalf("warnings %v", cfg.Warnings)
	}
	if names := cfg.ListTemplates(); strings.Join(names, ",") != "t1,t2" {
		t.Fatalf("ListTemplates %v", names)
	}
	if tr := cfg.ListTriggers(); len(tr) != 2 || tr[0].Name != "on-list" {
		t.Fatalf("ListTriggers %v", tr)
	}
}

func TestParseConfigErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]map[string]string{
		"invalid service yaml":   {"service.slack": "token: [unclosed"},
		"invalid trigger yaml":   {"trigger.x": "- when: [", "template.ok": "message: x"},
		"invalid template yaml":  {"template.x": "message: [1, 2"},
		"invalid selector":       {"subscriptions": "- recipients: [slack:x]\n  selector: 'a in (b'"},
		"invalid defaultTrigger": {"defaultTriggers": "a: b"},
		"nested context":         {"context": "a:\n  b: c"},
		"empty service type":     {"service.": "x: y"},
	}
	for name, cm := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseConfig(cm, nil); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("strict: want ErrInvalidConfig, got %v", err)
			}
			cfg, err := ParseConfig(cm, nil, Lenient())
			if err != nil {
				t.Fatalf("lenient: %v", err)
			}
			if len(cfg.Warnings) == 0 {
				t.Fatal("lenient parse must record a warning")
			}
		})
	}
}

func TestValidateReportsProblems(t *testing.T) {
	t.Parallel()
	cfg, err := ParseConfig(map[string]string{
		"service.carrier-pigeon": "x: y",
		"trigger.bad-expr":       "- when: app.status ==\n  send: [missing-template]",
		"template.bad":           "message: '{{ .app.metadata.name '",
		"template.env":           "message: '{{ env \"HOME\" }}'",
		"defaultTriggers":        "- nope",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{`service type 'carrier-pigeon' is not supported`, `trigger "bad-expr"`, `undefined template "missing-template"`, `template "bad"`, `function "env" not defined`, `undefined trigger "nope"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validation error %q missing %q", err, want)
		}
	}
}

func TestDefaultCatalogMerge(t *testing.T) {
	t.Parallel()
	cfg, err := ParseConfig(map[string]string{"trigger.on-created": "- when: 'false'\n  send: [app-created]"}, nil, WithDefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Triggers) != 8 || len(cfg.Templates) != 8 {
		t.Fatalf("catalog not merged: %d triggers %d templates", len(cfg.Triggers), len(cfg.Templates))
	}
	if cfg.Triggers["on-created"][0].When != "false" {
		t.Fatal("user keys must override catalog keys")
	}
}

func TestLegacyConfig(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"trigger.on-sync-status-unknown": "- when: app.status.sync.status == 'Unknown'\n  send: [old]",
		"config.yaml": `
triggers:
- name: on-sync-status-unknown
  template: legacy-template
  enabled: true
- name: on-new
  condition: app.status.health.status == 'Missing'
  template: legacy-template
templates:
- name: legacy-template
  subject: Legacy subject {{.app.metadata.name}}
  body: Legacy body
  slack:
    attachments: '[]'
context:
  argocdUrl: https://legacy.example.com
subscriptions:
- recipients: [slack:legacy]
  triggers: [on-new]
`,
	}
	secret := map[string][]byte{
		"notifiers.yaml": []byte("slack:\n  token: xoxb-legacy\nwebhook:\n- name: jenkins\n  url: https://jenkins.example.com/job\n"),
	}
	cfg, err := ParseConfig(cm, secret)
	if err != nil {
		t.Fatal(err)
	}
	tr := cfg.Triggers["on-sync-status-unknown"][0]
	if tr.When != "app.status.sync.status == 'Unknown'" || strings.Join(tr.Send, ",") != "legacy-template" {
		t.Fatalf("legacy trigger merge %+v", tr)
	}
	if cfg.Triggers["on-new"][0].When != "app.status.health.status == 'Missing'" {
		t.Fatalf("legacy new trigger %+v", cfg.Triggers["on-new"])
	}
	if !contains(cfg.DefaultTriggers, "on-sync-status-unknown") {
		t.Fatalf("enabled legacy trigger must become a default trigger: %v", cfg.DefaultTriggers)
	}
	tmpl := cfg.Templates["legacy-template"]
	if tmpl.Message != "Legacy body" || tmpl.Email == nil || tmpl.Email.Subject != "Legacy subject {{.app.metadata.name}}" || tmpl.Slack == nil {
		t.Fatalf("legacy template %+v", tmpl)
	}
	if cfg.Context["argocdUrl"] != "https://legacy.example.com" || len(cfg.Subscriptions) != 1 {
		t.Fatalf("legacy context/subscriptions %v %v", cfg.Context, cfg.Subscriptions)
	}
	if cfg.Services["slack"].Type != "slack" || cfg.Services["jenkins"].Type != "webhook" {
		t.Fatalf("legacy notifiers %v", cfg.ListServices())
	}
	if !contains(cfg.Services["slack"].secretValues, "xoxb-legacy") {
		t.Fatal("legacy notifier values must be treated as secrets")
	}
}

// TestSecretSubstitutionProperties checks that substituted values never change the YAML
// structure and are inserted verbatim, whatever characters they contain.
func TestSecretSubstitutionProperties(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		value := rapid.String().Draw(t, "value")
		key := rapid.StringMatching(`[a-z][a-z0-9-]{0,15}`).Draw(t, "key")
		cm := map[string]string{"service.webhook.h": "url: $" + key + "\nheaders:\n- name: X\n  value: token $" + key + " end\n"}
		cfg, err := ParseConfig(cm, map[string][]byte{key: []byte(value)})
		if err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		var opts struct {
			URL     string `json:"url"`
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		}
		if err := json.Unmarshal(cfg.Services["h"].Options, &opts); err != nil {
			t.Fatal(err)
		}
		if opts.URL != value || len(opts.Headers) != 1 || opts.Headers[0].Value != "token "+value+" end" {
			t.Fatalf("substitution of %q produced %+v", value, opts)
		}
	})
}
