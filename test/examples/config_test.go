package examples_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cmp"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	"github.com/juliosaraiva/patrol-cd/internal/lua"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

const (
	controlPlaneNamespace = "patrol"
	settingsDir           = "platform/settings/"
)

// settingsObjects returns the ConfigMaps and Secrets of examples/platform/settings.
func settingsObjects(t *testing.T, in *inventory) ([]*corev1.ConfigMap, []*corev1.Secret) {
	t.Helper()
	var cms []*corev1.ConfigMap
	var secrets []*corev1.Secret
	for _, d := range in.objects(roleObject) {
		if !strings.HasPrefix(d.file.rel, settingsDir) {
			continue
		}
		switch d.obj.GetKind() {
		case "ConfigMap":
			cm := &corev1.ConfigMap{}
			if err := decodeInto(d.obj, cm); err != nil {
				t.Fatalf("%s: %v", d, err)
			}
			cms = append(cms, cm)
		case "Secret":
			s := &corev1.Secret{}
			if err := decodeInto(d.obj, s); err != nil {
				t.Fatalf("%s: %v", d, err)
			}
			// stringData is merged into data by the API server.
			s.Data = map[string][]byte{}
			for k, v := range s.StringData {
				s.Data[k] = []byte(v)
			}
			secrets = append(secrets, s)
		}
	}
	return cms, secrets
}

func configMapByName(t *testing.T, in *inventory, rel, name string) *corev1.ConfigMap {
	t.Helper()
	for _, d := range in.objects(roleObject) {
		if d.file.rel == rel && d.obj.GetKind() == "ConfigMap" && d.obj.GetName() == name {
			cm := &corev1.ConfigMap{}
			if err := decodeInto(d.obj, cm); err != nil {
				t.Fatalf("%s: %v", d, err)
			}
			return cm
		}
	}
	t.Fatalf("ConfigMap %s not found in %s", name, rel)
	return nil
}

// TestSettings parses the settings ConfigMaps and Secrets with internal/settings, which
// reports every key that cannot be used (and unknown or deprecated keys as warnings).
func TestSettings(t *testing.T) {
	in := loadInventory(t)
	cms, secrets := settingsObjects(t, in)
	if len(cms) < 7 {
		t.Fatalf("expected the 7 settings ConfigMaps in %s, found %d", settingsDir, len(cms))
	}
	// server.secretkey is deliberately absent from the patrol-secret template: Patrol CD
	// generates it at first start (settings.EnsureServerSecretKey). Add one the same way.
	for _, s := range secrets {
		if s.Name == settings.DefaultSecretName {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				t.Fatal(err)
			}
			s.Data[settings.SecretKeyServerSecretKey] = key
		}
	}
	s := settings.Parse(settings.Input{Namespace: controlPlaneNamespace, ConfigMaps: cms, Secrets: secrets})
	for _, issue := range s.Validate() {
		t.Errorf("settings issue: %s", issue)
	}
	names := make([]string, 0, len(cms))
	for _, cm := range cms {
		names = append(names, cm.Name+"("+itoa(len(cm.Data))+" keys)")
	}
	t.Logf("settings parsed without issues: %s", strings.Join(names, ", "))

	// Every key of patrol-cmd-params-cm is known to at least one component.
	params := configMapByName(t, in, settingsDir+"patrol-cmd-params-cm.yaml", settings.DefaultCmdParamsConfigMapName)
	known := settings.ParamKeys()
	for k := range params.Data {
		if !slices.Contains(known, k) {
			t.Errorf("patrol-cmd-params-cm: unknown key %q", k)
		}
	}
	for _, c := range settings.Components() {
		_, issues := settings.ResolveParams(c, params.Data, func(string) (string, bool) { return "", false })
		for _, issue := range issues {
			t.Errorf("patrol-cmd-params-cm (%s): %s", c, issue)
		}
	}
}

// TestRBACPolicy validates patrol-rbac-cm with the policy parser and the dangling-role
// checks of `patrol admin settings rbac validate`.
func TestRBACPolicy(t *testing.T) {
	in := loadInventory(t)
	cm := configMapByName(t, in, settingsDir+"patrol-rbac-cm.yaml", settings.DefaultRBACConfigMapName)
	problems, err := rbac.Validate(rbac.PolicySources(cm.Data), rbac.ValidateOptions{
		MatchMode: cm.Data[rbac.ConfigMapMatchModeKey],
		Builtin:   rbac.BuiltinPolicy(),
	})
	if err != nil {
		t.Errorf("patrol-rbac-cm: %v", err)
	}
	for _, p := range problems {
		t.Errorf("patrol-rbac-cm: %s", p.Error())
	}
	if _, err := rbac.ParseScopes(cm.Data[rbac.ConfigMapScopesKey]); err != nil {
		t.Errorf("patrol-rbac-cm scopes: %v", err)
	}
	e := rbac.NewEnforcer()
	if err := e.ApplyConfigMap(rbac.BuiltinPolicy(), cm.Data); err != nil {
		t.Fatalf("patrol-rbac-cm does not load: %v", err)
	}
	// Spot checks of the documented intent.
	lead := rbac.Subjects{User: "alice", Groups: []string{"example-org:team-a-leads"}}
	for _, tc := range []struct {
		s                        rbac.Subjects
		resource, action, object string
		want                     bool
	}{
		{lead, rbac.ResourceApplications, rbac.ActionSync, "team-a/guestbook", true},
		{lead, rbac.ResourceApplications, rbac.ActionDelete, "team-a/guestbook-prod", false},
		{lead, rbac.ResourceApplications, rbac.ActionSync, "default/guestbook", false},
		{rbac.Subjects{User: "ci-bot"}, rbac.ResourceApplications, rbac.ActionSync, "default/guestbook-plain", true},
		{rbac.Subjects{User: "bob", Groups: []string{"example-org:platform"}}, rbac.ResourceClusters, rbac.ActionDelete, "https://prod-us.k8s.example.com", true},
	} {
		if got := e.Enforce(tc.s, tc.resource, tc.action, tc.object).Allowed; got != tc.want {
			t.Errorf("enforce(%v, %s, %s, %s) = %v, want %v", tc.s, tc.resource, tc.action, tc.object, got, tc.want)
		}
	}
}

// staticProjects decodes every AppProject of the examples.
func staticProjects(t *testing.T, in *inventory) map[string]*v1alpha1.AppProject {
	t.Helper()
	out := map[string]*v1alpha1.AppProject{}
	for _, d := range in.byKind(v1alpha1.GroupName, "AppProject") {
		p := &v1alpha1.AppProject{}
		if err := decodeInto(d.obj, p); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		out[p.Name] = p
	}
	return out
}

// TestAppProjects validates every AppProject as the API server does and compiles its role
// policies with the RBAC enforcer.
func TestAppProjects(t *testing.T) {
	in := loadInventory(t)
	projects := staticProjects(t, in)
	for _, name := range slices.Sorted(maps.Keys(projects)) {
		p := projects[name]
		if err := project.ValidateProject(p); err != nil {
			t.Errorf("AppProject %s: %v", name, err)
		}
		roles := make([]rbac.Role, 0, len(p.Spec.Roles))
		for _, r := range p.Spec.Roles {
			roles = append(roles, rbac.Role{Name: r.Name, Policies: r.Policies, Groups: r.Groups})
		}
		if err := rbac.NewEnforcer().SetProjectPolicies(name, roles); err != nil {
			t.Errorf("AppProject %s roles: %v", name, err)
		}
		for _, w := range p.Spec.SyncWindows {
			if _, err := project.GetSyncWindowTimes(w, time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)); err != nil {
				t.Errorf("AppProject %s sync window %q: %v", name, w.Schedule, err)
			}
		}
	}
	t.Logf("AppProjects validated: %d %v", len(projects), slices.Sorted(maps.Keys(projects)))

	// The sync windows of team-a do what its comments say: the night deny window only
	// applies to the prod-us cluster (by name or URL), the business-hours allow window only
	// to the guestbook applications.
	t.Run("team-a sync windows", func(t *testing.T) {
		p, ok := projects["team-a"]
		if !ok {
			t.Fatal("AppProject team-a not found")
		}
		berlin, err := time.LoadLocation("Europe/Berlin")
		if err != nil {
			t.Fatal(err)
		}
		wedNight := time.Date(2026, 10, 14, 21, 0, 0, 0, berlin)
		wedDay := time.Date(2026, 10, 14, 10, 0, 0, 0, berlin)
		satDay := time.Date(2026, 10, 17, 12, 0, 0, 0, berlin)
		app := func(name string, dst v1alpha1.ApplicationDestination) *v1alpha1.Application {
			a := &v1alpha1.Application{}
			a.Name, a.Namespace = name, controlPlaneNamespace
			a.Spec.Project, a.Spec.Destination = "team-a", dst
			return a
		}
		prodByName := app("team-a-frontend-prod-us", v1alpha1.ApplicationDestination{Name: "prod-us", Namespace: "team-a-frontend"})
		prodByURL := app("team-a-api", v1alpha1.ApplicationDestination{Server: "https://prod-us.k8s.example.com", Namespace: "team-a-api"})
		staging := app("team-a-backend-staging-eu", v1alpha1.ApplicationDestination{Name: "staging-eu", Namespace: "team-a-backend"})
		inCluster := app("team-a-frontend-dev", v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "team-a-frontend"})
		guestbook := app("team-a-guestbook", v1alpha1.ApplicationDestination{Name: "in-cluster", Namespace: "team-a-guestbook"})
		for _, tc := range []struct {
			app    *v1alpha1.Application
			manual bool
			at     time.Time
			want   bool
		}{
			{prodByName, false, wedNight, false},
			{prodByName, true, wedNight, true},
			{prodByName, false, wedDay, true},
			{prodByName, false, satDay, true},
			{prodByURL, false, wedNight, false},
			{staging, false, wedNight, true},
			{staging, false, satDay, true},
			{inCluster, false, wedNight, true},
			{inCluster, false, satDay, true},
			{guestbook, false, wedDay, true},
			{guestbook, false, wedNight, false},
			{guestbook, false, satDay, false},
		} {
			got, err := project.CanSync(p, tc.app, tc.manual, nil, tc.at)
			if err != nil {
				t.Errorf("CanSync(%s, manual=%v, %s): %v", tc.app.Name, tc.manual, tc.at.Format(time.RFC3339), err)
				continue
			}
			if got != tc.want {
				t.Errorf("CanSync(%s → %s%s, manual=%v, %s) = %v, want %v (windows %v)", tc.app.Name, tc.app.Spec.Destination.Name, tc.app.Spec.Destination.Server,
					tc.manual, tc.at.Format("Mon 15:04 MST"), got, tc.want, project.MatchSyncWindows(p.Spec.SyncWindows, tc.app))
			}
		}
	})
}

// notificationSecret returns patrol-notifications-secret with the REPLACE_ME placeholders
// replaced by syntactically valid test values (a generated RSA key for private keys), so
// that every service can be constructed.
func notificationSecret(t *testing.T, in *inventory) map[string][]byte {
	t.Helper()
	_, secrets := settingsObjects(t, in)
	for _, s := range secrets {
		if s.Name != settings.DefaultNotificationsSecretName {
			continue
		}
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
		out := map[string][]byte{}
		for k := range s.Data {
			if strings.Contains(strings.ToLower(k), "privatekey") {
				out[k] = []byte(pemKey)
				continue
			}
			out[k] = []byte("test-" + k)
		}
		return out
	}
	t.Fatalf("%s not found in %s", settings.DefaultNotificationsSecretName, settingsDir)
	return nil
}

// sampleApplication is an Application in the JSON shape the notifications engine receives:
// synced, healthy, with a finished sync operation.
func sampleApplication() map[string]any {
	app := map[string]any{}
	_ = yaml.Unmarshal([]byte(`
apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata:
  name: guestbook-pr-42
  namespace: patrol
  labels: {env: prod}
spec:
  project: previews
  source:
    repoURL: https://github.com/example-org/guestbook.git
    targetRevision: 0123456789abcdef0123456789abcdef01234567
    path: chart
  destination:
    server: https://kubernetes.default.svc
    namespace: guestbook-pr-42
status:
  sync:
    status: Synced
    revision: 0123456789abcdef0123456789abcdef01234567
  health:
    status: Healthy
    lastTransitionTime: "2026-10-16T10:00:00Z"
  operationState:
    phase: Succeeded
    message: successfully synced (all tasks run)
    startedAt: "2026-10-16T10:00:00Z"
    finishedAt: "2026-10-16T10:00:30Z"
    syncResult:
      revision: 0123456789abcdef0123456789abcdef01234567
`), &app)
	return app
}

// noValue is what text/template prints for a missing map key: a template field that renders
// it refers to a context key, label or field that does not exist.
const noValue = "<no value>"

// checkRendered fails when a rendered notification contains noValue.
func checkRendered(t *testing.T, what string, n *services.Notification) {
	t.Helper()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "<no value>" searchable
	if err := enc.Encode(n); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if strings.Contains(buf.String(), noValue) {
		t.Errorf("%s renders %q (missing context key or field): %s", what, noValue, buf.String())
	}
}

// TestNotifications parses the notification configurations (with the built-in catalog, as
// the controller does), validates every cross reference, builds every service, renders
// every template (no field may render "<no value>"), evaluates every trigger and resolves
// every subscription of the examples (annotations and default subscriptions); every
// subscribed (trigger, destination) is rendered, and webhook destinations need a
// webhook.<service> section in every template the trigger sends.
func TestNotifications(t *testing.T) {
	in := loadInventory(t)
	secret := notificationSecret(t, in)
	platform := configMapByName(t, in, settingsDir+"patrol-notifications-cm.yaml", settings.DefaultNotificationsConfigMapName)
	github := configMapByName(t, in, "github/04-notifications.yaml", settings.DefaultNotificationsConfigMapName)
	// The GitHub fragment is applied with a JSON merge patch, which replaces whole values:
	// redefining a platform key would drop the platform's content of that key.
	for _, k := range slices.Sorted(maps.Keys(github.Data)) {
		if _, ok := platform.Data[k]; ok {
			t.Errorf("github/04-notifications.yaml redefines key %q of patrol-notifications-cm; a merge patch would replace the platform value", k)
		}
	}
	merged := maps.Clone(platform.Data)
	maps.Copy(merged, github.Data)

	app := sampleApplication()
	const mergedName = "platform settings merged with github/04-notifications.yaml"
	var mergedCfg *notifications.Config
	var mergedEng *notifications.Engine
	for _, tc := range []struct {
		name string
		data map[string]string
	}{
		{"platform/settings/patrol-notifications-cm.yaml", platform.Data},
		{"github/04-notifications.yaml", github.Data},
		{mergedName, merged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := notifications.ParseConfig(tc.data, secret, notifications.WithDefaultCatalog())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			for _, w := range cfg.Warnings {
				t.Errorf("warning: %s", w)
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("validate: %v", err)
			}
			eng, err := notifications.NewEngine(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range eng.Warnings() {
				t.Errorf("engine warning: %s", w)
			}
			ctx := context.Background()
			rendered := 0
			for name := range tc.data {
				tmpl, ok := strings.CutPrefix(name, "template.")
				if !ok {
					continue
				}
				for _, svc := range cfg.ListServices() {
					n, err := eng.Render(ctx, app, []string{tmpl}, services.Destination{Service: svc.Name, Recipient: "example"})
					if err != nil {
						t.Errorf("render %s for %s: %v", tmpl, svc.Name, err)
						continue
					}
					if svc.Type == "github" && n.GitHub != nil && n.GitHub.Status != nil && n.GitHub.Status.State != "success" {
						t.Errorf("render %s: commit status %q, want success for a succeeded sync", tmpl, n.GitHub.Status.State)
					}
					checkRendered(t, "template "+tmpl+" for service "+svc.Name, n)
					rendered++
				}
			}
			triggered := 0
			for _, tr := range cfg.ListTriggers() {
				res, err := eng.RunTrigger(ctx, tr.Name, app)
				if err != nil {
					t.Errorf("trigger %s: %v", tr.Name, err)
				}
				for _, r := range res {
					if r.Triggered {
						triggered++
					}
				}
			}
			t.Logf("services=%d templates=%d triggers=%d renders=%d triggered conditions on the sample app=%d",
				len(cfg.ListServices()), len(cfg.ListTemplates()), len(cfg.ListTriggers()), rendered, triggered)
			// The fragment alone configures no service: it is only usable merged.
			if len(cfg.ListServices()) > 0 && rendered == 0 {
				t.Error("no template rendered")
			}
			if tc.name == mergedName {
				mergedCfg, mergedEng = cfg, eng
			}
		})
	}
	if mergedCfg == nil || mergedEng == nil {
		t.Fatal("merged configuration did not parse")
	}

	// Subscription annotations of every Application, AppProject and ApplicationSet template,
	// plus the default subscriptions matching their labels.
	t.Run("subscriptions", func(t *testing.T) {
		checked, renders := 0, 0
		ctx := context.Background()
		// The sample Application is the same for every object: render each subscribed
		// (trigger, destination, template) once.
		rendered := map[string]bool{}
		checkDests := func(where string, dests notifications.Destinations) {
			for _, trigger := range slices.Sorted(maps.Keys(dests)) {
				conds, ok := mergedCfg.Triggers[trigger]
				if !ok {
					t.Errorf("%s: subscription to undefined trigger %q", where, trigger)
				}
				for _, d := range dests[trigger] {
					svc, ok := mergedCfg.Services[d.Service]
					if !ok {
						t.Errorf("%s: subscription to unconfigured service %q", where, d.Service)
						continue
					}
					checked++
					for _, c := range conds {
						for _, tmpl := range c.Send {
							key := trigger + "|" + d.String() + "|" + tmpl
							if rendered[key] {
								continue
							}
							rendered[key] = true
							what := fmt.Sprintf("%s: trigger %s → %s (template %s)", where, trigger, d, tmpl)
							if svc.Type == "webhook" {
								if _, ok := mergedCfg.Templates[tmpl].Webhook[d.Service]; !ok {
									t.Errorf("%s: the template has no webhook.%s section; the webhook service would send a GET request without a body", what, d.Service)
								}
							}
							n, err := mergedEng.Render(ctx, app, []string{tmpl}, d)
							if err != nil {
								t.Errorf("%s: render: %v", what, err)
								continue
							}
							checkRendered(t, what, n)
							renders++
						}
					}
				}
			}
		}
		// isApp: default subscriptions apply to Applications (and generated ones).
		check := func(where string, annotations, objLabels map[string]string, isApp bool) {
			dests, warnings := notifications.AnnotationDestinations(notifications.DefaultAnnotationPrefix, annotations, mergedCfg.DefaultTriggers, mergedCfg.ServiceDefaultTriggers)
			for _, w := range warnings {
				t.Errorf("%s: %s", where, w)
			}
			checkDests(where, dests)
			if isApp {
				checkDests(where+" (default subscriptions)", mergedCfg.GlobalDestinations(objLabels))
			}
		}
		for _, d := range in.objects(roleObject) {
			if d.obj.GroupVersionKind().Group != v1alpha1.GroupName {
				continue
			}
			check(d.String(), d.obj.GetAnnotations(), d.obj.GetLabels(), d.obj.GetKind() == "Application")
			if d.obj.GetKind() == "ApplicationSet" {
				ann, _, _ := unstructured.NestedStringMap(d.obj.Object, "spec", "template", "metadata", "annotations")
				lbl, _, _ := unstructured.NestedStringMap(d.obj.Object, "spec", "template", "metadata", "labels")
				check(d.String()+" template", ann, lbl, true)
			}
		}
		for i, s := range mergedCfg.Subscriptions {
			for _, r := range s.Recipients {
				svc, _, _ := strings.Cut(r, ":")
				if _, ok := mergedCfg.Services[svc]; !ok {
					t.Errorf("default subscription %d: unconfigured service %q", i, svc)
				}
			}
		}
		if checked == 0 {
			t.Error("no subscription annotation found in the examples")
		}
		t.Logf("subscription destinations resolved: %d, distinct subscribed (trigger, destination, template) rendered: %d", checked, renders)
	})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

const luaDir = "plugins/lua"

type healthTests struct {
	Tests []struct {
		InputPath    string              `json:"inputPath"`
		HealthStatus health.HealthStatus `json:"healthStatus"`
	} `json:"tests"`
}

type actionTests struct {
	DiscoveryTests []struct {
		InputPath string       `json:"inputPath"`
		Result    []lua.Action `json:"result"`
	} `json:"discoveryTests"`
	ActionTests []struct {
		Action               string            `json:"action"`
		InputPath            string            `json:"inputPath"`
		ExpectedOutputPath   string            `json:"expectedOutputPath"`
		ExpectedErrorMessage string            `json:"expectedErrorMessage"`
		Parameters           map[string]string `json:"parameters"`
	} `json:"actionTests"`
}

func loadObject(t *testing.T, p string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return &unstructured.Unstructured{Object: m}
}

// normalized round-trips through JSON so that numbers compare equal regardless of their
// Go type.
func normalized(t *testing.T, obj map[string]any) any {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLuaCustomizations runs the health_test.yaml and action_test.yaml cases of
// examples/plugins/lua (the internal/lua format) against the customizations as Patrol CD
// loads them from patrol-cm, and checks that patrol-cm embeds exactly the scripts of
// examples/plugins/lua.
func TestLuaCustomizations(t *testing.T) {
	in := loadInventory(t)
	cm := configMapByName(t, in, settingsDir+"patrol-cm.yaml", settings.DefaultConfigMapName)
	overrides, err := lua.ParseResourceOverrides(cm.Data)
	if err != nil {
		t.Fatalf("patrol-cm resource customizations: %v", err)
	}
	rt := lua.New(overrides, lua.WithTimeout(30*time.Second))
	ctx := context.Background()

	var healthFiles, healthCases, actionFiles, discoveryCases, actionCases int
	usedLua := map[string]bool{}
	for _, f := range in.filesOf(classLua) {
		if !strings.HasPrefix(f.rel, luaDir+"/") {
			t.Errorf("%s: Lua scripts belong in %s", f.rel, luaDir)
		}
	}
	for _, f := range in.files {
		base := path.Base(f.rel)
		if base != "health_test.yaml" && base != "action_test.yaml" {
			continue
		}
		dir := path.Dir(f.rel)
		// <luaDir>/<group>/<Kind>[/actions]
		kindDir := strings.TrimSuffix(dir, "/actions")
		parts := strings.Split(strings.TrimPrefix(kindDir, luaDir+"/"), "/")
		if len(parts) != 2 {
			t.Errorf("%s: expected %s/<group>/<Kind>", f.rel, luaDir)
			continue
		}
		key := parts[0] + "/" + parts[1]
		override, ok := overrides[key]
		if !ok {
			t.Errorf("%s: patrol-cm has no resource customization for %s", f.rel, key)
			continue
		}
		data, err := os.ReadFile(f.abs)
		if err != nil {
			t.Fatal(err)
		}
		absDir := filepath.Dir(f.abs)
		switch base {
		case "health_test.yaml":
			healthFiles++
			script, err := os.ReadFile(filepath.Join(absDir, "health.lua"))
			if err != nil {
				t.Fatal(err)
			}
			usedLua[dir+"/health.lua"] = true
			if strings.TrimSpace(override.HealthLua) != strings.TrimSpace(string(script)) {
				t.Errorf("patrol-cm health script for %s differs from %s/health.lua", key, dir)
			}
			var tf healthTests
			if err := yaml.UnmarshalStrict(data, &tf); err != nil {
				t.Fatalf("%s: %v", f.rel, err)
			}
			for _, tc := range tf.Tests {
				healthCases++
				obj := loadObject(t, filepath.Join(absDir, tc.InputPath))
				got, err := rt.GetResourceHealthContext(ctx, obj)
				if err != nil {
					t.Errorf("%s %s: %v", f.rel, tc.InputPath, err)
					continue
				}
				if got == nil || *got != tc.HealthStatus {
					t.Errorf("%s %s: health %+v, want %+v", f.rel, tc.InputPath, got, tc.HealthStatus)
				}
			}
		case "action_test.yaml":
			actionFiles++
			defs, err := override.GetActions()
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			disc, err := os.ReadFile(filepath.Join(absDir, "discovery.lua"))
			if err != nil {
				t.Fatal(err)
			}
			usedLua[dir+"/discovery.lua"] = true
			if strings.TrimSpace(defs.ActionDiscoveryLua) != strings.TrimSpace(string(disc)) {
				t.Errorf("patrol-cm discovery script for %s differs from %s/discovery.lua", key, dir)
			}
			for _, def := range defs.Definitions {
				script, err := os.ReadFile(filepath.Join(absDir, def.Name, "action.lua"))
				if err != nil {
					t.Errorf("patrol-cm action %q of %s has no %s/%s/action.lua", def.Name, key, dir, def.Name)
					continue
				}
				usedLua[dir+"/"+def.Name+"/action.lua"] = true
				if strings.TrimSpace(def.ActionLua) != strings.TrimSpace(string(script)) {
					t.Errorf("patrol-cm action %q of %s differs from %s/%s/action.lua", def.Name, key, dir, def.Name)
				}
			}
			var tf actionTests
			if err := yaml.UnmarshalStrict(data, &tf); err != nil {
				t.Fatalf("%s: %v", f.rel, err)
			}
			for _, tc := range tf.DiscoveryTests {
				discoveryCases++
				obj := loadObject(t, filepath.Join(absDir, tc.InputPath))
				got, err := rt.ListActions(ctx, obj)
				if err != nil {
					t.Errorf("%s discovery %s: %v", f.rel, tc.InputPath, err)
					continue
				}
				slices.SortFunc(got, func(a, b lua.Action) int { return strings.Compare(a.Name, b.Name) })
				want := slices.Clone(tc.Result)
				slices.SortFunc(want, func(a, b lua.Action) int { return strings.Compare(a.Name, b.Name) })
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("%s discovery %s:\n got %s\nwant %s", f.rel, tc.InputPath, gotJSON, wantJSON)
				}
			}
			for _, tc := range tf.ActionTests {
				actionCases++
				src := loadObject(t, filepath.Join(absDir, tc.InputPath))
				var params []lua.ActionParameter
				for _, k := range slices.Sorted(maps.Keys(tc.Parameters)) {
					params = append(params, lua.ActionParameter{Name: k, Value: tc.Parameters[k]})
				}
				impacted, err := rt.ExecuteAction(ctx, src, tc.Action, params)
				if tc.ExpectedErrorMessage != "" {
					if err == nil || !strings.Contains(err.Error(), tc.ExpectedErrorMessage) {
						t.Errorf("%s action %s on %s: error %v, want one containing %q", f.rel, tc.Action, tc.InputPath, err, tc.ExpectedErrorMessage)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s action %s on %s: %v", f.rel, tc.Action, tc.InputPath, err)
					continue
				}
				want := loadObject(t, filepath.Join(absDir, tc.ExpectedOutputPath))
				if len(impacted) != 1 || impacted[0].Operation != lua.OperationPatch {
					t.Errorf("%s action %s: expected one patched object, got %d", f.rel, tc.Action, len(impacted))
					continue
				}
				gotJSON, _ := json.Marshal(normalized(t, impacted[0].Object.Object))
				wantJSON, _ := json.Marshal(normalized(t, want.Object))
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("%s action %s on %s:\n got %s\nwant %s", f.rel, tc.Action, tc.InputPath, gotJSON, wantJSON)
				}
			}
		}
	}
	for _, f := range in.filesOf(classLua) {
		if !usedLua[path.Dir(f.rel)+"/"+path.Base(f.rel)] {
			t.Errorf("%s is not exercised by any health_test.yaml/action_test.yaml", f.rel)
		}
	}
	if healthCases == 0 || actionCases == 0 || discoveryCases == 0 {
		t.Error("no Lua test cases found")
	}

	// Application health (app-of-apps waves wait for it): the documented script, giving the
	// same result as health.ApplicationHealth.
	appKey := health.ApplicationGroup + "/" + health.ApplicationKind
	appHealthCases := 0
	if o, ok := overrides[appKey]; !ok {
		t.Errorf("patrol-cm has no health customization for %s (examples/apps/app-of-apps relies on it)", appKey)
	} else {
		if strings.TrimSpace(o.HealthLua) != strings.TrimSpace(lua.ApplicationHealthLua) {
			t.Errorf("patrol-cm health script for %s differs from lua.ApplicationHealthLua", appKey)
		}
		for _, status := range []map[string]any{nil, {}, {"status": "Healthy"}, {"status": "Degraded", "message": "child failed"}, {"status": "Progressing"}} {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": v1alpha1.GroupVersion.String(), "kind": health.ApplicationKind,
				"metadata": map[string]any{"name": "child", "namespace": controlPlaneNamespace},
			}}
			if status != nil {
				obj.Object["status"] = map[string]any{"health": status}
			}
			got, err := rt.GetResourceHealthContext(ctx, obj)
			if err != nil {
				t.Errorf("Application health %v: %v", status, err)
				continue
			}
			want := health.ApplicationHealth(obj)
			if got == nil || want == nil || *got != *want {
				t.Errorf("Application health %v: patrol-cm script gives %+v, health.ApplicationHealth %+v", status, got, want)
			}
			appHealthCases++
		}
	}
	t.Logf("Lua: %d health test files (%d cases), %d action test files (%d discovery, %d action cases), %d scripts; %s health cases: %d",
		healthFiles, healthCases, actionFiles, discoveryCases, actionCases, len(usedLua), appKey, appHealthCases)
}

// TestCMPPluginConfigs parses every ConfigManagementPlugin document with internal/cmp.
func TestCMPPluginConfigs(t *testing.T) {
	in := loadInventory(t)
	docs := in.objects(roleCMPConfig)
	if len(docs) == 0 {
		t.Fatal("no ConfigManagementPlugin example")
	}
	for _, d := range docs {
		data, err := os.ReadFile(d.file.abs)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := cmp.ParsePluginConfig(data)
		if err != nil {
			t.Errorf("%s: %v", d, err)
			continue
		}
		for _, w := range cfg.Warnings() {
			t.Errorf("%s: warning: %s", d, w)
		}
		if !cfg.IsDiscoveryConfigured() {
			t.Errorf("%s: no discovery rule", d)
		}
		t.Logf("%s: plugin %s, %d static parameter announcements", d, cfg.PluginName(), len(cfg.Spec.Parameters.Static))
	}
}
