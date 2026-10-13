package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/catalog"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

func catalogEngine(t *testing.T, cm map[string]string, opts ...Option) *Engine {
	t.Helper()
	cfg, err := ParseConfig(cm, nil, WithDefaultCatalog())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("catalog config does not validate: %v", err)
	}
	eng, err := NewEngine(cfg, append([]Option{WithClock(fixedClock)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if w := eng.Warnings(); len(w) > 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
	return eng
}

func TestCatalogContents(t *testing.T) {
	t.Parallel()
	wantTriggers := []string{"on-created", "on-deleted", "on-deployed", "on-health-degraded", "on-sync-failed", "on-sync-running", "on-sync-status-unknown", "on-sync-succeeded"}
	if got := catalog.TriggerNames(); strings.Join(got, ",") != strings.Join(wantTriggers, ",") {
		t.Fatalf("catalog triggers = %v, want %v", got, wantTriggers)
	}
	if got := len(catalog.TemplateNames()); got != 8 {
		t.Fatalf("catalog templates = %d, want 8", got)
	}
}

func TestCatalogTriggers(t *testing.T) {
	t.Parallel()
	eng := catalogEngine(t, nil)
	deleted := func(app map[string]any) { setPath(app, "2026-10-12T11:59:00Z", "metadata", "deletionTimestamp") }
	health := func(status, transition string) func(map[string]any) {
		return func(app map[string]any) {
			setPath(app, status, "status", "health", "status")
			if transition != "" {
				setPath(app, transition, "status", "health", "lastTransitionTime")
			}
		}
	}
	syncStatus := func(s string) func(map[string]any) {
		return func(app map[string]any) { setPath(app, s, "status", "sync", "status") }
	}
	tests := []struct {
		name        string
		trigger     string
		app         map[string]any
		want        bool
		wantOncePer string
	}{
		{"created always", "on-created", appFixture(), true, "guestbook"},
		{"deleted without deletionTimestamp", "on-deleted", appFixture(), false, "guestbook"},
		{"deleted with deletionTimestamp", "on-deleted", appFixture(deleted), true, "guestbook"},
		{"deployed: healthy shortly after finish", "on-deployed",
			appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev1"), health("Healthy", "2026-10-12T11:01:30Z")), true, "rev1"},
		{"deployed: healthy since before the sync started", "on-deployed",
			appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev1"), health("Healthy", "2026-10-12T10:00:00Z")), true, "rev1"},
		{"deployed: health changed during a long sync", "on-deployed",
			appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:05:00Z", "rev1"), health("Healthy", "2026-10-12T11:00:30Z")), false, "rev1"},
		{"deployed: progressing", "on-deployed",
			appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev1"), health("Progressing", "2026-10-12T11:01:30Z")), false, "rev1"},
		{"deployed: no operation", "on-deployed", appFixture(), false, "<nil>"},
		{"degraded", "on-health-degraded", appFixture(health("Degraded", "")), true, "<nil>"},
		{"degraded: healthy", "on-health-degraded", appFixture(), false, "<nil>"},
		{"sync failed", "on-sync-failed", appFixture(withOperation("Failed", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev2")), true, "rev2"},
		{"sync error", "on-sync-failed", appFixture(withOperation("Error", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev2")), true, "rev2"},
		{"sync failed: succeeded", "on-sync-failed", appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev2")), false, "rev2"},
		{"sync failed: no operation", "on-sync-failed", appFixture(), false, "<nil>"},
		{"sync running", "on-sync-running", appFixture(withOperation("Running", "2026-10-12T11:00:00Z", "", "rev3")), true, "rev3"},
		{"sync running: finished", "on-sync-running", appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev3")), false, "rev3"},
		{"status unknown", "on-sync-status-unknown", appFixture(syncStatus("Unknown")), true, "<nil>"},
		{"status unknown: synced", "on-sync-status-unknown", appFixture(), false, "<nil>"},
		{"sync succeeded", "on-sync-succeeded", appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev4")), true, "rev4"},
		{"sync succeeded: running", "on-sync-succeeded", appFixture(withOperation("Running", "2026-10-12T11:00:00Z", "", "rev4")), false, "rev4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := eng.RunTrigger(context.Background(), tc.trigger, tc.app)
			if err != nil {
				t.Fatalf("RunTrigger: %v", err)
			}
			if len(res) != 1 {
				t.Fatalf("got %d condition results, want 1", len(res))
			}
			if res[0].Triggered != tc.want {
				t.Fatalf("triggered = %v (err %v), want %v", res[0].Triggered, res[0].Err, tc.want)
			}
			if res[0].OncePer != tc.wantOncePer {
				t.Fatalf("oncePer = %q, want %q", res[0].OncePer, tc.wantOncePer)
			}
			wantTemplate := "app-" + strings.TrimPrefix(tc.trigger, "on-")
			if len(res[0].Templates) != 1 || res[0].Templates[0] != wantTemplate {
				t.Fatalf("templates = %v, want [%s]", res[0].Templates, wantTemplate)
			}
		})
	}
}

// TestCatalogTemplatesRender renders every catalog template for the services it targets
// and checks that the JSON sections are well formed for single and multi-source apps.
func TestCatalogTemplatesRender(t *testing.T) {
	t.Parallel()
	eng := catalogEngine(t, map[string]string{"context": "argocdUrl: https://cd.example.com"})
	multi := appFixture(func(app map[string]any) {
		spec := app["spec"].(map[string]any)
		delete(spec, "source")
		spec["sources"] = []any{
			map[string]any{"repoURL": "https://github.com/example/a.git"},
			map[string]any{"repoURL": "https://github.com/example/b.git"},
		}
		setPath(app, []any{map[string]any{"type": "SyncError", "message": "boom"}}, "status", "conditions")
	})
	apps := map[string]map[string]any{
		"single": appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev")),
		"multi":  multi,
	}
	for _, tmpl := range catalog.TemplateNames() {
		for appName, app := range apps {
			for _, svc := range []string{"slack", "email", "teams"} {
				t.Run(tmpl+"/"+appName+"/"+svc, func(t *testing.T) {
					t.Parallel()
					n, err := eng.Render(context.Background(), app, []string{tmpl}, services.Destination{Service: svc, Recipient: "r"})
					if err != nil {
						t.Fatalf("Render: %v", err)
					}
					if !strings.Contains(n.Message, "guestbook") {
						t.Fatalf("message %q does not mention the app", n.Message)
					}
					if svc == "slack" && strings.Contains(n.Message, "slack") {
						t.Fatalf("message leaked template logic: %q", n.Message)
					}
					if n.Email == nil || n.Email.Subject == "" {
						t.Fatalf("email subject missing")
					}
					if strings.Contains(n.Message, "/applications/") && !strings.Contains(n.Message, "https://cd.example.com/applications/guestbook") {
						t.Fatalf("argocdUrl context alias not applied: %q", n.Message)
					}
					if n.Slack != nil && n.Slack.Attachments != "" {
						var v []any
						if err := json.Unmarshal([]byte(n.Slack.Attachments), &v); err != nil {
							t.Fatalf("slack attachments invalid JSON: %v\n%s", err, n.Slack.Attachments)
						}
					}
					if n.Teams != nil {
						for field, raw := range map[string]string{"facts": n.Teams.Facts, "potentialAction": n.Teams.PotentialAction} {
							if raw == "" {
								continue
							}
							var v []any
							if err := json.Unmarshal([]byte(raw), &v); err != nil {
								t.Fatalf("teams %s invalid JSON: %v\n%s", field, err, raw)
							}
						}
					}
				})
			}
		}
	}
}

func TestCatalogSlackEmoji(t *testing.T) {
	t.Parallel()
	eng := catalogEngine(t, nil)
	app := appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev"))
	slack, err := eng.Render(context.Background(), app, []string{"app-sync-succeeded"}, services.Destination{Service: "slack"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(slack.Message, ":white_check_mark:") {
		t.Fatalf("slack message should start with emoji: %q", slack.Message)
	}
	mail, err := eng.Render(context.Background(), app, []string{"app-sync-succeeded"}, services.Destination{Service: "email"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mail.Message, ":white_check_mark:") {
		t.Fatalf("email message must not contain slack emoji: %q", mail.Message)
	}
}
