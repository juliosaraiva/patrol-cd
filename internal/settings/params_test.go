package settings

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readLines(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, "\t"))
	}
	require.NoError(t, sc.Err())
	return out
}

// TestParamKeysMatchUpstream: every argocd-cmd-params-cm key (reference document and
// manifest wiring) is a patrol-cmd-params-cm key and no key was invented (OPS-017..028).
func TestParamKeysMatchUpstream(t *testing.T) {
	t.Parallel()
	var want []string
	for _, l := range readLines(t, "testdata/upstream-cmd-params-keys.txt") {
		want = append(want, l[0])
	}
	assert.Equal(t, want, ParamKeys())
	for _, k := range want {
		spec, ok := LookupKey(SourceCmdParamsConfigMap, k)
		assert.True(t, ok, k)
		assert.Equal(t, k, spec.Key)
	}
}

// TestParamWiringMatchesManifests: each manifest configMapKeyRef (component, key, env) exists
// with the env var renamed ARGOCD_ → PATROL_.
func TestParamWiringMatchesManifests(t *testing.T) {
	t.Parallel()
	type wire struct{ c, key, env string }
	var want []wire
	for _, l := range readLines(t, "testdata/upstream-cmd-params-wiring.tsv") {
		require.Len(t, l, 3)
		want = append(want, wire{l[0], l[1], l[2]})
	}
	var got []wire
	for _, s := range ParamSpecs() {
		if s.UpstreamEnv == "" {
			assert.True(t, strings.HasSuffix(s.Key, ".profile.enabled"), "only profile.enabled keys are not env-wired: %s", s.Key)
			continue
		}
		got = append(got, wire{string(s.Component), s.Key, s.UpstreamEnv})
		assert.Equal(t, PatrolEnv(s.UpstreamEnv), s.Env)
		assert.False(t, strings.HasPrefix(s.Env, "ARGOCD_"), s.Env)
	}
	cmp := func(a, b wire) int {
		return strings.Compare(a.c+"\x00"+a.key+"\x00"+a.env, b.c+"\x00"+b.key+"\x00"+b.env)
	}
	slices.SortFunc(want, cmp)
	slices.SortFunc(got, cmp)
	assert.Equal(t, want, got)
}

func TestPatrolEnv(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "PATROL_APPLICATION_CONTROLLER_STATUS_PROCESSORS", PatrolEnv("ARGOCD_APPLICATION_CONTROLLER_STATUS_PROCESSORS"))
	assert.Equal(t, "PATROL_SYNC_WAVE_DELAY", PatrolEnv("ARGOCD_SYNC_WAVE_DELAY"))
	assert.Equal(t, "GRPC_ENABLE_TXT_SERVICE_CONFIG", PatrolEnv("GRPC_ENABLE_TXT_SERVICE_CONFIG"))
	assert.Equal(t, "DEX_CONTINUE_ON_CONNECTOR_FAILURE", PatrolEnv("DEX_CONTINUE_ON_CONNECTOR_FAILURE"))
	assert.Equal(t, "PATROL_REPO_SERVER_METRICS_LISTEN_ADDRESS", PatrolEnv("ARGOCD_REPO_SERVER_LISTEN_METRICS_ADDRESS"))
}

func TestParamDefaultsAreValidAndDocumented(t *testing.T) {
	t.Parallel()
	for _, c := range Components() {
		p, issues := ResolveParams(c, nil, nil)
		assert.Empty(t, issues, c)
		for _, v := range p.Values() {
			assert.Equal(t, ParamFromDefault, v.Source, v.Spec.Key)
			assert.NotEmpty(t, v.Spec.Description, v.Spec.Key)
		}
	}
	ctrl, _ := ResolveParams(ComponentController, nil, nil)
	assert.Equal(t, int64(20), ctrl.Int("controller.status.processors"))
	assert.Equal(t, int64(10), ctrl.Int("controller.operation.processors"))
	assert.Equal(t, int64(5), ctrl.Int("controller.hydration.processors"))
	assert.Equal(t, int64(2), ctrl.Int("controller.sync.wave.delay.seconds"))
	assert.Equal(t, time.Hour, ctrl.Duration("controller.app.state.cache.expiration"))
	assert.Equal(t, 180*time.Second, ctrl.Duration("controller.repo.error.grace.period.seconds"))
	assert.Equal(t, 100*time.Millisecond, ctrl.Duration("controller.cluster.cache.events.processing.interval"))
	assert.Equal(t, "legacy", ctrl.String("controller.sharding.algorithm"))
	assert.Equal(t, 50.0, ctrl.Float("controller.k8s.client.qps"))
	assert.True(t, ctrl.Bool("controller.cluster.cache.batch.events.processing"))
	assert.Equal(t, "patrol-repo-server:8081", ctrl.String("repo.server"))
	assert.Equal(t, "/app/config/reposerver/mtls/client.crt", ctrl.String("controller.repo.server.client.cert.path"))
	assert.Equal(t, []string{}, ctrl.Strings("application.namespaces"))

	srv, _ := ResolveParams(ComponentServer, nil, nil)
	assert.Equal(t, int64(50), srv.Int("server.webhook.parallelism.limit"))
	assert.Equal(t, int64(20), srv.Int("server.webhook.refresh.workers"))
	assert.True(t, srv.Bool("server.sync.replace.allowed"))
	assert.Equal(t, "frame-ancestors 'self';", srv.String("server.content.security.policy"))
	assert.Equal(t, "1.2", srv.String("server.tls.minversion"))
	assert.True(t, srv.Bool("applicationsetcontroller.enable.scm.providers"), "the server also reads appset keys")

	repo, _ := ResolveParams(ComponentRepoServer, nil, nil)
	assert.Equal(t, int64(10_000_000), repo.Quantity("reposerver.max.combined.directory.manifests.size"))
	assert.Equal(t, int64(1_000_000_000), repo.Quantity("reposerver.oci.manifest.max.extracted.size"))
	assert.Len(t, repo.Strings("reposerver.oci.layer.media.types"), 3)
	assert.Equal(t, 15*time.Second, repo.Duration("reposerver.git.request.timeout"))

	appset, _ := ResolveParams(ComponentAppSetController, nil, nil)
	assert.Equal(t, 3*time.Minute, appset.Duration("applicationsetcontroller.requeue.after"))
	assert.Equal(t, int64(5000), appset.Int("applicationsetcontroller.status.max.resources.count"))
	notif, _ := ResolveParams(ComponentNotificationsController, nil, nil)
	assert.Equal(t, int64(1), notif.Int("notificationscontroller.processors.count"))
}

func TestResolveParamsPrecedence(t *testing.T) {
	t.Parallel()
	cm := map[string]string{"controller.status.processors": "7", "controller.operation.processors": "3"}
	env := map[string]string{"PATROL_APPLICATION_CONTROLLER_STATUS_PROCESSORS": "9"}
	p, issues := ResolveParams(ComponentController, cm, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	assert.Empty(t, issues)
	assert.Equal(t, int64(9), p.Int("controller.status.processors"), "env wins over the ConfigMap")
	v, _ := p.Lookup("controller.status.processors")
	assert.Equal(t, ParamFromEnv, v.Source)
	assert.Equal(t, int64(3), p.Int("controller.operation.processors"))
	v, _ = p.Lookup("controller.operation.processors")
	assert.Equal(t, ParamFromConfigMap, v.Source)
	assert.Equal(t, int64(5), p.Int("controller.hydration.processors"))
}

func TestResolveParamsInvalidValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		c     Component
		key   string
		value string
		sev   Severity
		msg   string
		check func(t *testing.T, p *Params)
	}{
		{ComponentController, "controller.status.processors", "abc", SeverityWarning, "Could not parse 'abc' as a number from environment controller.status.processors", func(t *testing.T, p *Params) {
			assert.Equal(t, int64(20), p.Int("controller.status.processors"))
		}},
		{ComponentController, "controller.hydration.processors", "0", SeverityWarning, "Value in controller.hydration.processors is 0, which is less than minimum 1 allowed", func(t *testing.T, p *Params) {
			assert.Equal(t, int64(5), p.Int("controller.hydration.processors"))
		}},
		{ComponentServer, "server.webhook.parallelism.limit", "5000", SeverityWarning, "which is greater than maximum 1000 allowed", nil},
		{ComponentController, "controller.app.state.cache.expiration", "11h", SeverityWarning, "which is greater than maximum 10h0m0s allowed", func(t *testing.T, p *Params) {
			assert.Equal(t, time.Hour, p.Duration("controller.app.state.cache.expiration"))
		}},
		{ComponentController, "controller.sync.wave.delay.seconds", "x", SeverityWarning, "Could not parse 'x'", func(t *testing.T, p *Params) {
			assert.Equal(t, int64(2), p.Int("controller.sync.wave.delay.seconds"))
		}},
		{ComponentController, "controller.metrics.cache.expiration", "soon", SeverityWarning, "Could not parse 'soon' as a duration", nil},
		{ComponentController, "controller.k8s.client.qps", "fast", SeverityWarning, "Could not parse 'fast' as a float64", nil},
		{ComponentController, "controller.diff.server.side", "yes", SeverityWarning, "Could not parse 'yes' as a boolean", nil},
		{ComponentController, "otlp.headers", "a=1,b", SeverityWarning, "Invalid key-value pair", nil},
		{ComponentController, "otlp.headers", "a=1,a=2", SeverityWarning, "Duplicate key 'a'", nil},
		{ComponentController, "controller.sharding.algorithm", "random", SeverityError, `must be one of "legacy", "round-robin", "consistent-hashing"`, func(t *testing.T, p *Params) {
			assert.Equal(t, "legacy", p.String("controller.sharding.algorithm"))
		}},
		{ComponentRepoServer, "reposerver.max.combined.directory.manifests.size", "abc", SeverityError, "invalid quantity 'abc'", nil},
		{ComponentNotificationsController, "notificationscontroller.log.format", "xml", SeverityError, `must be one of "json", "text"`, nil},
		{ComponentController, "controller.log.level", "verbose", SeverityError, `invalid value 'verbose' in controller.log.level: not a valid logrus Level: "verbose"`, func(t *testing.T, p *Params) {
			assert.Equal(t, "info", p.String("controller.log.level"))
		}},
		{ComponentRepoServer, "reposerver.log.level", "warnings", SeverityError, `not a valid logrus Level: "warnings"`, nil},
		{ComponentController, "redis.server", "redis:6379", SeverityWarning, "no effect in Patrol CD: Patrol CD does not use Redis", nil},
		{ComponentController, "controller.grpc.enable.txt.service.config", "true", SeverityWarning, "no effect in Patrol CD", nil},
		{ComponentController, "controller.self.heal.backoff.cooldown.seconds", "100", SeverityWarning, "deprecated", nil},
		{ComponentController, "made.up.key", "1", SeverityWarning, "unknown key is ignored", nil},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Parallel()
			p, issues := ResolveParams(tc.c, map[string]string{tc.key: tc.value}, nil)
			found := false
			for _, is := range issues {
				if is.Key == tc.key && is.Severity == tc.sev && strings.Contains(is.Message, tc.msg) {
					found = true
				}
			}
			assert.True(t, found, "issues: %v", issues)
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

func TestResolveParamsEmptyValues(t *testing.T) {
	t.Parallel()
	p, issues := ResolveParams(ComponentServer, map[string]string{
		"server.x.frame.options":           "",
		"server.webhook.parallelism.limit": "",
		"server.api.content.types":         "",
	}, nil)
	assert.Empty(t, issues)
	assert.Equal(t, "", p.String("server.x.frame.options"), "allowEmpty: empty disables the header")
	assert.Equal(t, "", p.String("server.api.content.types"))
	assert.Equal(t, int64(50), p.Int("server.webhook.parallelism.limit"), "empty selects the default")
}

func TestParamsFlagDefaultAndSettings(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{}, newCM(DefaultCmdParamsConfigMapName, map[string]string{
		"application.namespaces":         "ns1, ns2",
		"otlp.headers":                   "b=2,a=1",
		"controller.status.processors":   "nope",
		"applicationsetcontroller.debug": "true",
	})))
	requireIssue(t, s, DefaultCmdParamsConfigMapName, "controller.status.processors", SeverityWarning, "Could not parse 'nope'")
	p, _ := s.Params(ComponentController, nil)
	assert.Equal(t, "ns1,ns2", p.FlagDefault("application.namespaces"))
	assert.Equal(t, "a=1,b=2", p.FlagDefault("otlp.headers"))
	assert.Equal(t, "20", p.FlagDefault("controller.status.processors"))
	assert.Equal(t, "", p.FlagDefault("unknown"))
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, p.StringMap("otlp.headers"))
	a, _ := s.Params(ComponentAppSetController, nil)
	assert.True(t, a.Bool("applicationsetcontroller.debug"))
	assert.Equal(t, ComponentAppSetController, a.Component())
}

func TestParamsForComponentSorted(t *testing.T) {
	t.Parallel()
	for _, c := range Components() {
		specs := ParamsForComponent(c)
		require.NotEmpty(t, specs, c)
		assert.True(t, slices.IsSortedFunc(specs, func(a, b ParamSpec) int { return strings.Compare(a.Key, b.Key) }), c)
		seen := map[string]bool{}
		for _, s := range specs {
			assert.False(t, seen[s.Key], "duplicate %s for %s", s.Key, c)
			seen[s.Key] = true
			assert.Equal(t, c, s.Component)
		}
	}
}
