package services

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpsgenie(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) {
		writeJSON(w, 202, map[string]any{"result": "Request will be processed"})
	})
	// apiUrl without scheme is accepted (baseline examples use a bare host).
	host := strings.TrimPrefix(srv.URL, "http://")
	svc := mustNew(t, TypeOpsgenie, map[string]any{"apiUrl": "http://" + host, "apiKeys": map[string]string{"team-1": "genie-key"}}, testDeps())
	long := strings.Repeat("x", 200)
	n := Notification{Message: long, Opsgenie: &OpsgenieNotification{
		Description: "desc", Priority: "P1", Alias: "guestbook", Note: "note",
		Actions: []string{"Restart"}, Tags: []string{"Critical"},
		VisibleTo: []OpsgenieResponder{{Type: "team", ID: "t1"}, {Type: "user", Username: "trinity@example.com"}},
		Details:   map[string]string{"environment": "production"}, Entity: "app", User: "patrol",
	}}
	mustSend(t, svc, n, Destination{Recipient: "team-1"})
	r := rec.all()[0]
	if r.Path != "/v2/alerts" || r.Header.Get("Authorization") != "GenieKey genie-key" {
		t.Fatalf("request %s auth %q", r.Path, r.Header.Get("Authorization"))
	}
	b := r.json(t)
	if len([]rune(str(t, b, "message"))) != 130 {
		t.Fatalf("message must be truncated to 130 runes")
	}
	for k, v := range map[string]string{"description": "desc", "priority": "P1", "alias": "guestbook", "note": "note", "entity": "app", "user": "patrol", "source": "Patrol CD"} {
		if str(t, b, k) != v {
			t.Fatalf("%s = %v", k, b[k])
		}
	}
	resp := b["responders"].([]any)[0].(map[string]any)
	if resp["type"] != "team" || resp["id"] != "team-1" {
		t.Fatalf("responders %v", b["responders"])
	}
	vis := b["visibleTo"].([]any)
	if len(vis) != 2 || vis[1].(map[string]any)["username"] != "trinity@example.com" {
		t.Fatalf("visibleTo %v", vis)
	}
	if b["details"].(map[string]any)["environment"] != "production" {
		t.Fatalf("details %v", b["details"])
	}
	if err := send(t, svc, n, Destination{Recipient: "unknown"}); !errors.Is(err, ErrRecipientNotConfigured) {
		t.Fatalf("got %v", err)
	}
	bad := Notification{Message: "m", Opsgenie: &OpsgenieNotification{Priority: "P9"}}
	if err := send(t, svc, bad, Destination{Recipient: "team-1"}); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("got %v", err)
	}
}

func TestPagerDuty(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) {
		writeJSON(w, 201, map[string]any{"incident": map[string]any{"id": "Q1"}})
	})
	svc := mustNew(t, TypePagerDuty, map[string]any{"token": "pd-token", "from": "ops@example.com", "apiURL": srv.URL}, testDeps())
	n := Notification{Message: "fallback", PagerDuty: &PagerDutyNotification{Title: "Rollout aborted", Body: "details", Urgency: "high", PriorityID: "PRIO"}}
	mustSend(t, svc, n, Destination{Recipient: "PSERVICE"})
	r := rec.all()[0]
	if r.Path != "/incidents" || r.Header.Get("Authorization") != "Token token=pd-token" || r.Header.Get("From") != "ops@example.com" ||
		r.Header.Get("Accept") != "application/vnd.pagerduty+json;version=2" {
		t.Fatalf("request %s %v", r.Path, r.Header)
	}
	inc := r.json(t)["incident"].(map[string]any)
	if inc["type"] != "incident" || inc["title"] != "Rollout aborted" || inc["urgency"] != "high" {
		t.Fatalf("incident %v", inc)
	}
	if s := inc["service"].(map[string]any); s["id"] != "PSERVICE" || s["type"] != "service_reference" {
		t.Fatalf("service %v", s)
	}
	if b := inc["body"].(map[string]any); b["details"] != "details" || b["type"] != "incident_body" {
		t.Fatalf("body %v", b)
	}
	if p := inc["priority"].(map[string]any); p["id"] != "PRIO" || p["type"] != "priority_reference" {
		t.Fatalf("priority %v", p)
	}
}

func TestPagerDutyV2(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) {
		writeJSON(w, 202, map[string]any{"status": "success", "message": "Event processed", "dedup_key": "k"})
	})
	svc := mustNew(t, TypePagerDutyV2, map[string]any{"serviceKeys": map[string]string{"my-service": "routing-key-123"}, "apiURL": srv.URL}, testDeps())
	n := Notification{PagerDutyV2: &PagerDutyV2Notification{Summary: "s", Severity: "critical", Source: "guestbook", Component: "c", Group: "g", Class: "cl", URL: "https://cd.example.com/app", DedupKey: "dk"}}
	mustSend(t, svc, n, Destination{Recipient: "my-service"})
	r := rec.all()[0]
	b := r.json(t)
	if r.Path != "/v2/enqueue" || str(t, b, "routing_key") != "routing-key-123" || str(t, b, "event_action") != "trigger" ||
		str(t, b, "dedup_key") != "dk" || str(t, b, "client") != "Patrol CD" || str(t, b, "client_url") != "https://cd.example.com/app" {
		t.Fatalf("event %s %v", r.Path, b)
	}
	p := b["payload"].(map[string]any)
	if p["summary"] != "s" || p["severity"] != "critical" || p["source"] != "guestbook" || p["component"] != "c" || p["group"] != "g" || p["class"] != "cl" {
		t.Fatalf("payload %v", p)
	}
	if err := send(t, svc, Notification{Message: "m"}, Destination{Recipient: "my-service"}); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("missing section: got %v", err)
	}
	if err := send(t, svc, n, Destination{Recipient: "other"}); !errors.Is(err, ErrRecipientNotConfigured) {
		t.Fatalf("got %v", err)
	}
}

func TestNewRelic(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) {
		writeJSON(w, 201, map[string]any{"deployment": map[string]any{"id": 1}})
	})
	svc := mustNew(t, TypeNewRelic, map[string]any{"apiURL": srv.URL, "apiKey": "NRAK-key"}, testDeps())
	n := Notification{Message: "deployed", NewRelic: &NewRelicNotification{Revision: "abc", Changelog: "fix", User: "alice"}}
	mustSend(t, svc, n, Destination{Recipient: "12345"})
	r := rec.all()[0]
	if r.Path != "/v2/applications/12345/deployments.json" || r.Header.Get("X-Api-Key") != "NRAK-key" {
		t.Fatalf("request %s %v", r.Path, r.Header)
	}
	d := r.json(t)["deployment"].(map[string]any)
	if d["revision"] != "abc" || d["changelog"] != "fix" || d["user"] != "alice" || d["description"] != "deployed" {
		t.Fatalf("deployment %v", d)
	}
	if err := send(t, svc, Notification{Message: "m"}, Destination{Recipient: "1"}); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("missing revision: got %v", err)
	}
}

func TestAlertmanager(t *testing.T) {
	t.Parallel()
	var failing atomic.Int32
	bad, badRec := newServer(t, func(w http.ResponseWriter, _ captured) {
		failing.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	good, goodRec := newServer(t, func(w http.ResponseWriter, _ captured) { w.WriteHeader(200) })
	targets := []string{strings.TrimPrefix(bad.URL, "http://"), strings.TrimPrefix(good.URL, "http://")}
	svc := mustNew(t, TypeAlertmanager, map[string]any{
		"targets": targets, "apiPath": "/api/v2/alerts",
		"basicAuth":   map[string]string{"username": "am", "password": "am-pass"},
		"bearerToken": "ignored-when-basic-auth",
	}, testDeps())
	n := Notification{Alertmanager: &AlertmanagerNotification{
		Labels:       map[string]string{"alertname": "app-deployed", "severity": "info"},
		Annotations:  map[string]string{"summary": "deployed"},
		GeneratorURL: "https://github.com/example/guestbook",
	}}
	mustSend(t, svc, n, Destination{})
	if failing.Load() != 3 {
		t.Fatalf("failing target attempts = %d, want 3 (1 + 2 retries)", failing.Load())
	}
	r := goodRec.all()[0]
	if r.Path != "/api/v2/alerts" {
		t.Fatalf("path %q", r.Path)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("am:am-pass"))
	if r.Header.Get("Authorization") != wantAuth {
		t.Fatalf("Authorization = %q, want basic auth precedence", r.Header.Get("Authorization"))
	}
	if !strings.HasPrefix(strings.TrimSpace(string(r.Body)), "[") {
		t.Fatalf("body must be a JSON array: %s", r.Body)
	}
	if !strings.Contains(string(r.Body), `"generatorURL":"https://github.com/example/guestbook"`) || !strings.Contains(string(r.Body), `"alertname":"app-deployed"`) {
		t.Fatalf("body %s", r.Body)
	}
	_ = badRec

	// All targets failing is an error.
	svc2 := mustNew(t, TypeAlertmanager, map[string]any{"targets": targets[:1], "bearerToken": "tok"}, testDeps())
	err := send(t, svc2, n, Destination{})
	var se *StatusError
	if err == nil || !errors.As(err, &se) || se.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %v", err)
	}
	if err := send(t, svc2, Notification{Alertmanager: &AlertmanagerNotification{}}, Destination{}); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("no labels: got %v", err)
	}
}
