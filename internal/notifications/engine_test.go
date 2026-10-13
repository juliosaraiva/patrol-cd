package notifications

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

const testPrefix = DefaultAnnotationPrefix

func newTestEngine(t *testing.T, cm map[string]string, secret map[string][]byte, svcs map[string]services.Service, opts ...Option) *Engine {
	t.Helper()
	cfg, err := ParseConfig(cm, secret)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	all := append([]Option{WithClock(fixedClock), WithServices(svcs)}, opts...)
	eng, err := NewEngine(cfg, all...)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func process(t *testing.T, eng *Engine, app map[string]any, state DeliveryState) (DeliveryState, []DeliveryResult) {
	t.Helper()
	st, res, err := eng.Process(context.Background(), app, state)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return st, res
}

func statuses(res []DeliveryResult) string {
	var out []string
	for _, r := range res {
		out = append(out, fmt.Sprintf("%s/%s/%s", r.Trigger, r.Destination, r.Status))
	}
	return strings.Join(out, ",")
}

var unknownTriggerCM = map[string]string{
	"service.slack":          "token: xoxb-test",
	"trigger.on-unknown":     "- when: app.status.sync.status == 'Unknown'\n  send: [status]",
	"template.status":        "message: '{{.app.metadata.name}} is {{.app.status.sync.status}} via {{.serviceType}} to {{.recipient}} ({{.context.notificationType}})'",
	"trigger.on-rev":         "- when: app.status.health.status == 'Degraded'\n  oncePer: app.status.sync.revision\n  send: [status]",
	"trigger.on-failing-tpl": "- when: 'true'\n  send: [missing]",
}

func setSync(status string) func(map[string]any) {
	return func(app map[string]any) { setPath(app, status, "status", "sync", "status") }
}

func TestProcessDeliversOnceAndRearms(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": rec})
	sub := withAnnotation(testPrefix+"/subscribe.on-unknown.slack", "c1;c2")

	state, res := process(t, eng, appFixture(sub, setSync("Unknown")), nil)
	if got := statuses(res); got != "on-unknown/slack:c1/Sent,on-unknown/slack:c2/Sent" {
		t.Fatalf("first pass: %s", got)
	}
	if len(state) != 2 {
		t.Fatalf("state %v", state)
	}
	for k, v := range state {
		if !strings.HasPrefix(k, "on-unknown:[0].") || v != fixedNow.Unix() {
			t.Fatalf("state entry %s=%d", k, v)
		}
	}
	if msg := rec.deliveries()[0].n.Message; msg != "guestbook is Unknown via slack to c1 (slack)" {
		t.Fatalf("message %q", msg)
	}

	state, res = process(t, eng, appFixture(sub, setSync("Unknown")), state)
	if got := statuses(res); got != "on-unknown/slack:c1/SkippedDuplicate,on-unknown/slack:c2/SkippedDuplicate" {
		t.Fatalf("second pass: %s", got)
	}
	if len(rec.deliveries()) != 2 {
		t.Fatalf("duplicate deliveries: %d", len(rec.deliveries()))
	}

	state, res = process(t, eng, appFixture(sub, setSync("Synced")), state)
	if len(res) != 0 || len(state) != 0 {
		t.Fatalf("condition false must clear state: %v %v", res, state)
	}

	_, res = process(t, eng, appFixture(sub, setSync("Unknown")), state)
	if got := statuses(res); got != "on-unknown/slack:c1/Sent,on-unknown/slack:c2/Sent" {
		t.Fatalf("re-armed pass: %s", got)
	}
	if len(rec.deliveries()) != 4 {
		t.Fatalf("deliveries %d, want 4", len(rec.deliveries()))
	}
}

func TestProcessOncePer(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": rec})
	sub := withAnnotation(testPrefix+"/subscribe.on-rev.slack", "c")
	degraded := func(rev string) map[string]any {
		return appFixture(sub, func(app map[string]any) {
			setPath(app, "Degraded", "status", "health", "status")
			setPath(app, rev, "status", "sync", "revision")
		})
	}
	healthy := func(rev string) map[string]any {
		return appFixture(sub, func(app map[string]any) { setPath(app, rev, "status", "sync", "revision") })
	}
	state, res := process(t, eng, degraded("rev1"), nil)
	if statuses(res) != "on-rev/slack:c/Sent" {
		t.Fatalf("pass1 %s", statuses(res))
	}
	for k := range state {
		if !strings.HasPrefix(k, "rev1:on-rev:") {
			t.Fatalf("oncePer value must prefix the key: %s", k)
		}
	}
	state, _ = process(t, eng, healthy("rev1"), state)
	if len(state) != 1 {
		t.Fatalf("oncePer key must survive the condition turning false: %v", state)
	}
	state, res = process(t, eng, degraded("rev1"), state)
	if statuses(res) != "on-rev/slack:c/SkippedDuplicate" {
		t.Fatalf("same oncePer value must not re-notify: %s", statuses(res))
	}
	state, res = process(t, eng, degraded("rev2"), state)
	if statuses(res) != "on-rev/slack:c/Sent" || len(state) != 2 {
		t.Fatalf("new oncePer value must notify: %s %v", statuses(res), state)
	}
	if len(rec.deliveries()) != 2 {
		t.Fatalf("deliveries %d", len(rec.deliveries()))
	}
}

type countingMetrics struct {
	mu          sync.Mutex
	deliveries  map[string]int
	evaluations map[string]int
}

func (m *countingMetrics) IncDelivery(trigger, service string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveries[fmt.Sprintf("%s/%s/%v", trigger, service, ok)]++
}

func (m *countingMetrics) IncTriggerEvaluation(trigger string, triggered bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluations[fmt.Sprintf("%s/%v", trigger, triggered)]++
}

func TestProcessFailureIsVisibleAndRetried(t *testing.T) {
	t.Parallel()
	rec := &recordingService{fail: &services.StatusError{Service: "slack", StatusCode: 500, Body: "boom"}}
	m := &countingMetrics{deliveries: map[string]int{}, evaluations: map[string]int{}}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": rec}, WithMetrics(m))
	app := appFixture(withAnnotation(testPrefix+"/subscribe.on-rev.slack", "c"), func(app map[string]any) {
		setPath(app, "Degraded", "status", "health", "status")
	})
	state, res := process(t, eng, app, nil)
	if len(res) != 1 || res[0].Status != StatusFailed || !errors.Is(res[0].Err, services.ErrDeliveryFailed) {
		t.Fatalf("results %+v", res)
	}
	if len(state) != 0 {
		t.Fatalf("failed delivery must not be recorded (even with oncePer): %v", state)
	}
	if err := FailedResults(res); err == nil || !strings.Contains(err.Error(), "on-rev -> slack:c") {
		t.Fatalf("FailedResults %v", err)
	}
	rec.mu.Lock()
	rec.fail = nil
	rec.mu.Unlock()
	state, res = process(t, eng, app, state)
	if statuses(res) != "on-rev/slack:c/Sent" || len(state) != 1 {
		t.Fatalf("retry: %s %v", statuses(res), state)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deliveries["on-rev/slack/false"] != 1 || m.deliveries["on-rev/slack/true"] != 1 || m.evaluations["on-rev/true"] != 2 {
		t.Fatalf("metrics %v %v", m.deliveries, m.evaluations)
	}
}

func TestProcessUnknownTriggerServiceAndTemplate(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": rec})
	app := appFixture(
		withAnnotation(testPrefix+"/subscribe.on-missing.slack", "c"),
		withAnnotation(testPrefix+"/subscribe.on-unknown.nosuchservice", "c"),
		withAnnotation(testPrefix+"/subscribe.on-failing-tpl.slack", "c"),
		setSync("Unknown"),
	)
	state, res := process(t, eng, app, nil)
	byTrigger := map[string]DeliveryResult{}
	for _, r := range res {
		byTrigger[r.Trigger] = r
	}
	if r := byTrigger["on-missing"]; r.Status != StatusFailed || !errors.Is(r.Err, ErrTriggerNotFound) {
		t.Fatalf("missing trigger %+v", r)
	}
	if r := byTrigger["on-unknown"]; r.Status != StatusFailed || !errors.Is(r.Err, ErrServiceNotConfigured) {
		t.Fatalf("missing service %+v", r)
	}
	if r := byTrigger["on-failing-tpl"]; r.Status != StatusFailed || !errors.Is(r.Err, ErrTemplateNotFound) {
		t.Fatalf("missing template %+v", r)
	}
	if len(state) != 0 || len(rec.deliveries()) != 0 {
		t.Fatalf("nothing must be recorded or sent: %v", state)
	}
}

func TestProcessProjectAndDefaultSubscriptions(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	cm := map[string]string{
		"service.slack":      "token: x",
		"trigger.on-created": "- when: 'true'\n  oncePer: app.metadata.name\n  send: [created]",
		"template.created":   "message: '{{.app.metadata.name}} in {{.appProject.metadata.name}}: {{.appProject.spec.description}}'",
		"subscriptions":      "- recipients: [slack:team-a]\n  selector: team=a\n  triggers: [on-created]\n- recipients: [slack:team-b]\n  selector: team=b\n  triggers: [on-created]",
	}
	projects := fakeProjects{"default": {
		"metadata": map[string]any{"name": "default", "annotations": map[string]any{
			testPrefix + "/subscribe.on-created.slack":                   "project-channel",
			"on-created." + LegacyRecipientsAnnotationSuffix(testPrefix): "slack:legacy-channel",
		}},
		"spec": map[string]any{"description": "Default project"},
	}}
	eng := newTestEngine(t, cm, nil, map[string]services.Service{"slack": rec}, WithProjectGetter(projects))
	_, res := process(t, eng, appFixture(), nil)
	if got := statuses(res); got != "on-created/slack:legacy-channel/Sent,on-created/slack:project-channel/Sent,on-created/slack:team-a/Sent" {
		t.Fatalf("results %s", got)
	}
	if msg := rec.deliveries()[0].n.Message; msg != "guestbook in default: Default project" {
		t.Fatalf("appProject var: %q", msg)
	}
	dests, err := eng.Destinations(context.Background(), appFixture())
	if err != nil || len(dests["on-created"]) != 3 {
		t.Fatalf("Destinations %v %v", dests, err)
	}
	// Unknown project: appProject is an empty map and project subscriptions are skipped.
	app := appFixture(func(app map[string]any) { setPath(app, "missing", "spec", "project") })
	_, res = process(t, eng, app, nil)
	if got := statuses(res); got != "on-created/slack:team-a/Sent" {
		t.Fatalf("results %s", got)
	}
}

func TestProcessDryRun(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": rec}, WithDryRun(true))
	state, res := process(t, eng, appFixture(withAnnotation(testPrefix+"/subscribe.on-unknown.slack", "c"), setSync("Unknown")), nil)
	if len(res) != 1 || res[0].Status != StatusDryRun || res[0].Notification == nil || !strings.Contains(res[0].Notification.Message, "guestbook is Unknown") {
		t.Fatalf("dry run %+v", res)
	}
	if len(rec.deliveries()) != 0 || rec.calls != 0 {
		t.Fatal("dry run must not send")
	}
	if len(state) != 1 {
		t.Fatalf("dry run returns the would-be state: %v", state)
	}
	results, err := eng.NotifyTemplate(context.Background(), appFixture(), "status", []services.Destination{{Service: "slack", Recipient: "x"}})
	if err != nil || len(results) != 1 || results[0].Status != StatusDryRun {
		t.Fatalf("NotifyTemplate dry run %+v %v", results, err)
	}
}

func TestNotifyTemplate(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": rec})
	res, err := eng.NotifyTemplate(context.Background(), appFixture(), "status", []services.Destination{{Service: "slack", Recipient: "a"}, {Service: "nope", Recipient: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != StatusSent || res[1].Status != StatusFailed || !errors.Is(res[1].Err, ErrServiceNotConfigured) {
		t.Fatalf("results %+v", res)
	}
	if _, err := eng.NotifyTemplate(context.Background(), nil, "status", nil); !errors.Is(err, ErrInvalidApplication) {
		t.Fatalf("nil app: %v", err)
	}
}

func TestSelfServiceNamespaceAndSecrets(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"service.slack":  "token: x",
		"trigger.on-any": "- when: 'true'\n  send: [s]",
		"template.s":     "message: 'secret={{.secrets.token}}'",
	}
	secret := map[string][]byte{"token": []byte("s3cr3t")}
	sub := withAnnotation(testPrefix+"/subscribe.on-any.slack", "c")

	rec := &recordingService{}
	eng := newTestEngine(t, cm, secret, map[string]services.Service{"slack": rec})
	process(t, eng, appFixture(sub), nil)
	if msg := rec.deliveries()[0].n.Message; msg != "secret=s3cr3t" {
		t.Fatalf("controller-level config exposes secrets: %q", msg)
	}

	rec2 := &recordingService{}
	eng2 := newTestEngine(t, cm, secret, map[string]services.Service{"slack": rec2}, WithSelfServiceNamespace("team-ns"))
	state, _ := process(t, eng2, appFixture(sub), nil)
	if msg := rec2.deliveries()[0].n.Message; msg != "secret=<no value>" {
		t.Fatalf("self-service config must not expose secrets: %q", msg)
	}
	for k := range state {
		if !strings.HasPrefix(k, "team-ns:on-any:") {
			t.Fatalf("self-service keys must be namespaced: %s", k)
		}
	}
	rec3 := &recordingService{}
	eng3 := newTestEngine(t, cm, secret, map[string]services.Service{"slack": rec3}, WithSelfServiceNamespace("ns"), WithSecretsInTemplates(true))
	process(t, eng3, appFixture(sub), nil)
	if msg := rec3.deliveries()[0].n.Message; msg != "secret=s3cr3t" {
		t.Fatalf("explicit WithSecretsInTemplates(true) must win: %q", msg)
	}
}

func TestHelpersInTriggersAndTemplates(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		commits: map[string]*CommitMetadata{"https://github.com/example/guestbook.git@abc123": {Message: "fix: things", Author: "Alice <alice@example.com>", Date: fixedNow, Tags: []string{"v1"}}},
		details: &AppDetail{Type: "Helm", Helm: &CustomHelmAppSpec{
			HelmAppSpec:            HelmAppSpec{Name: "chart", Parameters: []HelmParameter{{Name: "image.tag", Value: "1.0"}, {Name: "replicas", Value: "2"}}},
			HelmParameterOverrides: []HelmParameter{{Name: "image.tag", Value: "2.0"}},
		}},
	}
	cm := map[string]string{
		"service.slack": "token: x",
		"trigger.on-author": "- when: repo.GetCommitMetadata(app.status.sync.revision).Author contains 'Alice' and strings.ToUpper(app.metadata.name) == 'GUESTBOOK'" +
			" and time.Now().Sub(time.Parse('2026-10-12T11:00:00Z')).Minutes() >= 59 and sync.GetInfoItem(app, 'reason') == 'manual'\n  send: [helpers]",
		"template.helpers": "message: |-\n" +
			"  {{(call .repo.GetCommitMetadata .app.status.sync.revision).Author}}|{{(call .repo.GetCommitMetadata .app.status.sync.revision).Message}}\n" +
			"  {{call .repo.RepoURLToHTTPS \"git@github.com:example/guestbook.git\"}}|{{call .repo.FullNameByRepoURL .app.spec.source.repoURL}}|{{call .repo.QueryEscape \"a b/c\"}}\n" +
			"  {{(call .repo.GetAppDetails).Type}}|{{(call .repo.GetAppDetails).Helm.GetParameterValueByName \"image.tag\"}}|{{(call .repo.GetAppDetails).Helm.GetParameterValueByName \"replicas\"}}\n" +
			"  {{(call .time.Now).Format .time.RFC3339}}|{{call .strings.ReplaceAll \"a-b\" \"-\" \"+\"}}|{{call .sync.GetInfoItem .app \"reason\"}}|{{ upper \"sprig\" }}",
	}
	rec := &recordingService{}
	eng := newTestEngine(t, cm, nil, map[string]services.Service{"slack": rec}, WithRepoService(repo))
	app := appFixture(withAnnotation(testPrefix+"/subscribe.on-author.slack", "c"), func(app map[string]any) {
		setPath(app, map[string]any{"info": []any{map[string]any{"name": "reason", "value": "manual"}}}, "status", "operationState", "operation")
	})
	_, res := process(t, eng, app, nil)
	if statuses(res) != "on-author/slack:c/Sent" {
		t.Fatalf("results %s %v", statuses(res), res)
	}
	want := "Alice <alice@example.com>|fix: things\n" +
		"https://github.com/example/guestbook.git|example/guestbook|a+b%2Fc\n" +
		"Helm|2.0|2\n" +
		"2026-10-12T12:00:00Z|a+b|manual|SPRIG"
	if got := rec.deliveries()[0].n.Message; got != want {
		t.Fatalf("message:\n%s\nwant:\n%s", got, want)
	}
	repo.mu.Lock()
	calls := repo.calls
	repo.mu.Unlock()
	if calls != 1 {
		t.Fatalf("commit metadata must be memoized per Process call: %d calls", calls)
	}

	// Without a repo service the helper fails and the delivery is reported as failed.
	eng2 := newTestEngine(t, map[string]string{
		"service.slack":  "token: x",
		"trigger.on-any": "- when: 'true'\n  send: [t]",
		"template.t":     "message: '{{(call .repo.GetCommitMetadata \"abc\").Author}}'",
	}, nil, map[string]services.Service{"slack": &recordingService{}})
	_, res = process(t, eng2, appFixture(withAnnotation(testPrefix+"/subscribe.on-any.slack", "c")), nil)
	if len(res) != 1 || res[0].Status != StatusFailed || !errors.Is(res[0].Err, ErrRepoServiceUnavailable) {
		t.Fatalf("results %+v", res)
	}
	// RunTrigger exposes evaluation errors.
	crs, err := eng.RunTrigger(context.Background(), "on-author", appFixture())
	if err != nil || len(crs) != 1 || crs[0].Triggered || crs[0].Err == nil {
		t.Fatalf("RunTrigger %+v %v", crs, err)
	}
}

func TestTemplateMergingAndDefaults(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"template.a":  "message: from-a\nemail:\n  subject: subject-a\nwebhook:\n  hook1:\n    method: POST\n    body: a\n",
		"template.b":  "slack:\n  attachments: '[{\"title\":\"{{.app.metadata.name}}\"}]'\nwebhook:\n  hook2:\n    path: /b\n",
		"template.c":  "message: from-c\nemail:\n  body: body-c\n",
		"template.gh": "github:\n  status:\n    state: success\nnewrelic:\n  description: d\nalertmanager:\n  labels:\n    severity: info\n",
	}
	repo := &fakeRepo{commits: map[string]*CommitMetadata{"https://github.com/example/guestbook.git@abc123": {Message: "msg", Author: "bob"}}}
	eng := newTestEngine(t, cm, nil, nil, WithRepoService(repo))
	app := appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "rev9"))
	n, err := eng.Render(context.Background(), app, []string{"a", "b"}, dest("slack", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Message != "from-a" || n.Email.Subject != "subject-a" || n.Slack.Attachments != `[{"title":"guestbook"}]` {
		t.Fatalf("merged %+v", n)
	}
	if len(n.Webhook) != 2 || n.Webhook["hook1"].Body != "a" || n.Webhook["hook2"].Path != "/b" {
		t.Fatalf("webhook entries must merge per name: %+v", n.Webhook)
	}
	n, err = eng.Render(context.Background(), app, []string{"a", "c"}, dest("email", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Message != "from-c" || n.Email.Subject != "" || n.Email.Body != "body-c" {
		t.Fatalf("later section replaces earlier section: %+v %+v", n, n.Email)
	}
	n, err = eng.Render(context.Background(), app, []string{"gh"}, dest("github", ""))
	if err != nil {
		t.Fatal(err)
	}
	if n.GitHub.RepoURLPath != "https://github.com/example/guestbook.git" || n.GitHub.RevisionPath != "rev9" {
		t.Fatalf("github defaults %+v", n.GitHub)
	}
	if n.Alertmanager.Labels["alertname"] != "gh" || n.Alertmanager.GeneratorURL != "https://github.com/example/guestbook.git" {
		t.Fatalf("alertmanager defaults %+v", n.Alertmanager)
	}
	if n.NewRelic.Revision != "rev9" || n.NewRelic.Changelog != "msg" || n.NewRelic.User != "bob" || n.NewRelic.Description != "d" {
		t.Fatalf("newrelic default revision %+v", n.NewRelic)
	}
}

func TestTemplateSafety(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"template.env":   "message: '{{ env \"HOME\" }}'",
		"template.dns":   "message: '{{ getHostByName \"example.com\" }}'",
		"template.big":   "message: '{{ repeat 2000000 \"x\" }}'",
		"template.nilop": "message: '{{ .app.status.operationState.phase }}'",
	}
	eng := newTestEngine(t, cm, nil, nil)
	if w := strings.Join(eng.Warnings(), "\n"); !strings.Contains(w, `function "env" not defined`) || !strings.Contains(w, `function "getHostByName" not defined`) {
		t.Fatalf("removed functions must not compile: %s", w)
	}
	if _, err := eng.Render(context.Background(), appFixture(), []string{"env"}, dest("slack", "")); err == nil {
		t.Fatal("env template rendered")
	}
	if _, err := eng.Render(context.Background(), appFixture(), []string{"big"}, dest("slack", "")); !errors.Is(err, ErrTemplateOutputTooLarge) {
		t.Fatalf("got %v", err)
	}
	n, err := eng.Render(context.Background(), appFixture(), []string{"nilop"}, dest("slack", ""))
	if err != nil || n.Message != "<no value>" {
		t.Fatalf("missing fields render <no value> like the baseline: %q %v", n.Message, err)
	}
}

func TestProcessContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	svc := &cancellingService{cancel: cancel}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": svc})
	app := appFixture(withAnnotation(testPrefix+"/subscribe.on-unknown.slack", "a;b;c"), setSync("Unknown"))
	state, res, err := eng.Process(ctx, app, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if len(res) != 1 || res[0].Status != StatusSent || len(state) != 1 || svc.calls.Load() != 1 {
		t.Fatalf("only the first destination must be attempted: %s state=%v", statuses(res), state)
	}
}

type cancellingService struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *cancellingService) Send(context.Context, services.Notification, services.Destination) error {
	c.calls.Add(1)
	c.cancel()
	return nil
}

func TestProcessConcurrent(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	eng := newTestEngine(t, unknownTriggerCM, nil, map[string]services.Service{"slack": rec}, WithRepoService(&fakeRepo{}))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			app := appFixture(withAnnotation(testPrefix+"/subscribe.on-unknown.slack", fmt.Sprintf("c%d", i)), setSync("Unknown"))
			if _, _, err := eng.Process(context.Background(), app, nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(rec.deliveries()) != 32 {
		t.Fatalf("deliveries %d", len(rec.deliveries()))
	}
}

func TestProcessInvalidApplication(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, unknownTriggerCM, nil, nil)
	for _, app := range []map[string]any{nil, {"kind": "Application"}} {
		if _, _, err := eng.Process(context.Background(), app, DeliveryState{"k": 1}); !errors.Is(err, ErrInvalidApplication) {
			t.Fatalf("got %v", err)
		}
	}
	// No subscriptions: nothing happens and the state is returned unchanged.
	state, res := process(t, eng, appFixture(), DeliveryState{"k": 1})
	if len(res) != 0 || state["k"] != 1 {
		t.Fatalf("state %v res %v", state, res)
	}
}

func TestEngineMisconfiguredService(t *testing.T) {
	t.Parallel()
	cfg, err := ParseConfig(map[string]string{
		"service.slack":  "username: no-token",
		"trigger.on-any": "- when: 'true'\n  send: [t]",
		"template.t":     "message: m",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(eng.Warnings(), "\n"), `service "slack"`) {
		t.Fatalf("warnings %v", eng.Warnings())
	}
	_, res, err := eng.Process(context.Background(), appFixture(withAnnotation(testPrefix+"/subscribe.on-any.slack", "c")), nil)
	if err != nil || len(res) != 1 || !errors.Is(res[0].Err, services.ErrInvalidOptions) {
		t.Fatalf("results %+v %v", res, err)
	}
}

// TestEndToEndWebhook drives configuration -> engine -> real webhook service -> HTTP.
func TestEndToEndWebhook(t *testing.T) {
	t.Parallel()
	type req struct {
		method, path, auth, body string
	}
	reqs := make(chan req, 4)
	fail := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs <- req{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)}
		if fail.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("rejected token ghp_e2e_secret"))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	cm := map[string]string{
		"service.webhook.github": "url: " + srv.URL + "\nheaders:\n- name: Authorization\n  value: token $github-token\n",
		"template.commit-status": "webhook:\n  github:\n    method: POST\n    path: /repos/{{call .repo.FullNameByRepoURL .app.spec.source.repoURL}}/statuses/{{.app.status.operationState.operation.sync.revision}}\n" +
			"    body: '{\"state\":\"success\",\"context\":\"cd/{{.app.metadata.name}}\",\"target_url\":\"{{.context.patrolUrl}}/applications/{{.app.metadata.name}}\"}'\n",
		"trigger.on-synced": "- when: app.status.operationState.phase in ['Succeeded']\n  oncePer: app.status.operationState.syncResult.revision\n  send: [commit-status]",
		"context":           "argocdUrl: https://cd.example.com",
	}
	cfg, err := ParseConfig(cm, map[string][]byte{"github-token": []byte("ghp_e2e_secret")})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg, WithClock(fixedClock), WithNetwork(services.Network{
		Retry: &services.RetryPolicy{MaxRetries: 1, WaitMin: time.Millisecond, WaitMax: time.Millisecond}, DisableProxyFromEnvironment: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	app := appFixture(withAnnotation(testPrefix+"/subscribe.on-synced.github", ""), withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "abc123"))
	state, res := process(t, eng, app, nil)
	if statuses(res) != "on-synced/github:/Sent" {
		t.Fatalf("results %s %v", statuses(res), res)
	}
	got := <-reqs
	want := req{"POST", "/repos/example/guestbook/statuses/abc123", "token ghp_e2e_secret",
		`{"state":"success","context":"cd/guestbook","target_url":"https://cd.example.com/applications/guestbook"}`}
	if got != want {
		t.Fatalf("request %+v\nwant %+v", got, want)
	}
	annotations, err := ApplyDeliveryState(nil, eng.AnnotationPrefix(), state)
	if err != nil || !strings.Contains(annotations["notified.notifications.cd.patrol.example"], "abc123:on-synced:[0].") {
		t.Fatalf("annotation %v %v", annotations, err)
	}

	// A rejected delivery is reported with the secret redacted.
	fail.Store(true)
	app2 := appFixture(withAnnotation(testPrefix+"/subscribe.on-synced.github", ""), withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "def456"))
	_, res = process(t, eng, app2, state)
	<-reqs
	if len(res) != 1 || res[0].Status != StatusFailed {
		t.Fatalf("results %+v", res)
	}
	if strings.Contains(res[0].Err.Error(), "ghp_e2e_secret") || !strings.Contains(res[0].Err.Error(), "400") {
		t.Fatalf("error must be redacted and informative: %v", res[0].Err)
	}
}

func TestPrometheusMetrics(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := NewPrometheusMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	m.IncDelivery("on-x", "slack", true)
	m.IncDelivery("on-x", "slack", true)
	m.IncTriggerEvaluation("on-x", false)
	again, err := NewPrometheusMetrics(reg)
	if err != nil {
		t.Fatalf("re-registration must reuse collectors: %v", err)
	}
	again.IncDelivery("on-x", "slack", true)
	if got := testutil.ToFloat64(again.deliveries.WithLabelValues("on-x", "slack", "true")); got != 3 {
		t.Fatalf("deliveries = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.evaluations.WithLabelValues("on-x", "false")); got != 1 {
		t.Fatalf("evaluations = %v, want 1", got)
	}
	if n, err := testutil.GatherAndCount(reg, "patrol_notifications_deliveries_total", "patrol_notifications_trigger_eval_total"); err != nil || n != 2 {
		t.Fatalf("registered series = %d, %v", n, err)
	}
}

func TestSyncStatusRefreshed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		app  map[string]any
		want bool
	}{
		{"no operation", appFixture(), true},
		{"running", appFixture(withOperation("Running", "2026-10-12T11:00:00Z", "", "r")), true},
		{"finished before reconcile", appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "r"), func(a map[string]any) {
			setPath(a, "2026-10-12T11:02:00Z", "status", "reconciledAt")
		}), true},
		{"finished after reconcile", appFixture(withOperation("Failed", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "r"), func(a map[string]any) {
			setPath(a, "2026-10-12T11:00:30Z", "status", "reconciledAt")
		}), false},
		{"observedAt counts", appFixture(withOperation("Error", "2026-10-12T11:00:00Z", "2026-10-12T11:01:00Z", "r"), func(a map[string]any) {
			setPath(a, "2026-10-12T11:05:00Z", "status", "observedAt")
		}), true},
		{"completed without finishedAt", appFixture(withOperation("Succeeded", "2026-10-12T11:00:00Z", "", "r")), false},
	}
	for _, tc := range tests {
		if got, reason := SyncStatusRefreshed(tc.app); got != tc.want {
			t.Fatalf("%s: got %v (%s), want %v", tc.name, got, reason, tc.want)
		}
	}
}

func TestProcessDoesNotMutateCallerObjects(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"service.slack":   "token: x",
		"trigger.on-any":  "- when: 'true'\n  send: [mutate]",
		"template.mutate": "message: '{{ $_ := set .app.metadata \"name\" \"hijacked\" }}{{ $_ := set .appProject \"x\" 1 }}{{ $_ := unset .strings \"ToUpper\" }}{{ .app.metadata.name }}'",
	}
	project := map[string]any{"metadata": map[string]any{"name": "default"}}
	rec := &recordingService{}
	eng := newTestEngine(t, cm, nil, map[string]services.Service{"slack": rec}, WithProjectGetter(fakeProjects{"default": project}))
	app := appFixture(withAnnotation(testPrefix+"/subscribe.on-any.slack", "c"))
	state := DeliveryState{"other": 1}
	process(t, eng, app, state)
	if rec.deliveries()[0].n.Message != "hijacked" {
		t.Fatalf("template ran on a copy: %q", rec.deliveries()[0].n.Message)
	}
	if name := app["metadata"].(map[string]any)["name"]; name != "guestbook" {
		t.Fatalf("caller app mutated: %v", name)
	}
	if _, ok := project["x"]; ok {
		t.Fatal("caller project mutated")
	}
	if len(state) != 1 {
		t.Fatalf("caller state mutated: %v", state)
	}
	if _, ok := stringHelpers["ToUpper"]; !ok {
		t.Fatal("package-level helper namespace mutated by a template")
	}
}
