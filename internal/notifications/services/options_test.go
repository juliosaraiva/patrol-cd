package services

import (
	"testing"
)

// TestDecodeOptionsCoercesScalars: unquoted numbers and booleans given for string-typed
// options decode to their text (sigs.k8s.io/yaml type-hinted decoding, as in the baseline),
// both from JSON (the engine's canonical form) and YAML.
func TestDecodeOptionsCoercesScalars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		typ   string
		raw   string
		dst   func() any
		check func(t *testing.T, v any)
	}{
		{
			name: "awssqs account", typ: TypeAwsSqs, raw: `{"queue":"q","account":123456789012}`,
			dst: func() any { return &AwsSqsOptions{} },
			check: func(t *testing.T, v any) {
				if got := v.(*AwsSqsOptions).Account; got != "123456789012" {
					t.Fatalf("account %q", got)
				}
			},
		},
		{
			name: "webhook header values and basic auth", typ: TypeWebhook,
			raw: `{"url":"https://h.example.com","headers":[{"name":"X-Build-Id","value":12345},{"name":"X-Flag","value":true},{"name":"X-Big","value":12345678901234567890}],"basicAuth":{"username":42,"password":1234}}`,
			dst: func() any { return &WebhookOptions{} },
			check: func(t *testing.T, v any) {
				o := v.(*WebhookOptions)
				if o.Headers[0].Value != "12345" || o.Headers[1].Value != "true" || o.Headers[2].Value != "12345678901234567890" {
					t.Fatalf("headers %+v", o.Headers)
				}
				if o.BasicAuth.Username != "42" || o.BasicAuth.Password != "1234" {
					t.Fatalf("basicAuth %+v", o.BasicAuth)
				}
			},
		},
		{
			name: "opsgenie apiKeys map values", typ: TypeOpsgenie, raw: "apiUrl: api.opsgenie.com\napiKeys:\n  team: 12345678\n",
			dst: func() any { return &OpsgenieOptions{} },
			check: func(t *testing.T, v any) {
				if got := v.(*OpsgenieOptions).APIKeys["team"]; got != "12345678" {
					t.Fatalf("apiKeys %q", got)
				}
			},
		},
		{
			name: "pagerduty serviceID", typ: TypePagerDuty, raw: `{"token":"t","serviceID":1234567}`,
			dst: func() any { return &PagerDutyOptions{} },
			check: func(t *testing.T, v any) {
				if got := v.(*PagerDutyOptions).ServiceID; got != "1234567" {
					t.Fatalf("serviceID %q", got)
				}
			},
		},
		{
			name: "flex fields keep accepting numbers and strings", typ: TypeEmail, raw: `{"host":"smtp","port":587,"html":"true","password":2002}`,
			dst: func() any { return &EmailOptions{} },
			check: func(t *testing.T, v any) {
				o := v.(*EmailOptions)
				if o.Port != "587" || !bool(o.HTML) || o.Password != "2002" {
					t.Fatalf("email %+v", o)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dst := tc.dst()
			if err := decodeOptions(tc.typ, []byte(tc.raw), dst); err != nil {
				t.Fatalf("decodeOptions: %v", err)
			}
			tc.check(t, dst)
		})
	}
}

func TestDecodeOptionsRejectsWrongShapes(t *testing.T) {
	t.Parallel()
	var o WebhookOptions
	if err := decodeOptions(TypeWebhook, []byte(`{"headers":"not-a-list"}`), &o); err == nil {
		t.Fatal("a scalar for a list option must fail")
	}
	if err := decodeOptions(TypeWebhook, []byte(`{"url": [unclosed`), &o); err == nil {
		t.Fatal("malformed options must fail")
	}
}
