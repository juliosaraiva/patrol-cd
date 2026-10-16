package server

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func bind(t *testing.T, c settings.Component, args ...string) *ParamFlags {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	pf := BindParamFlags(fs, c)
	require.NoError(t, fs.Parse(args))
	return pf
}

// TestParamPrecedence checks flag > environment > patrol-cmd-params-cm > default for a
// string, a bool (flag without value) and an environment-only parameter.
func TestParamPrecedence(t *testing.T) {
	t.Parallel()
	const key, env = "server.listen.address", "PATROL_SERVER_LISTEN_ADDRESS"
	cases := []struct {
		name   string
		args   []string
		env    map[string]string
		cm     map[string]string
		want   string
		source string
	}{
		{name: "default", want: "0.0.0.0", source: "default"},
		{name: "configmap", cm: map[string]string{key: "10.0.0.1"}, want: "10.0.0.1", source: "configmap"},
		{name: "env over configmap", env: map[string]string{env: "10.0.0.2"}, cm: map[string]string{key: "10.0.0.1"}, want: "10.0.0.2", source: "env"},
		{name: "flag over env and configmap", args: []string{"--address=10.0.0.3"}, env: map[string]string{env: "10.0.0.2"}, cm: map[string]string{key: "10.0.0.1"}, want: "10.0.0.3", source: "flag"},
		{name: "empty env selects the default", env: map[string]string{env: ""}, cm: map[string]string{key: "10.0.0.1"}, want: "0.0.0.0", source: "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pf := bind(t, settings.ComponentServer, tc.args...)
			p, issues, err := pf.Resolve(tc.cm, envMap(tc.env))
			require.NoError(t, err)
			assert.Empty(t, issues)
			assert.Equal(t, tc.want, p.String(key))
			assert.Equal(t, tc.source, pf.paramSource(p, key))
		})
	}

	t.Run("bool flag without value", func(t *testing.T) {
		t.Parallel()
		pf := bind(t, settings.ComponentServer, "--insecure")
		p, _, err := pf.Resolve(map[string]string{"server.insecure": "false"}, envMap(map[string]string{"PATROL_SERVER_INSECURE": "false"}))
		require.NoError(t, err)
		assert.True(t, p.Bool("server.insecure"))
		assert.Equal(t, []string{"server.insecure"}, pf.ChangedKeys())
	})
	t.Run("bool from configmap", func(t *testing.T) {
		t.Parallel()
		pf := bind(t, settings.ComponentServer)
		p, _, err := pf.Resolve(map[string]string{"server.insecure": "true"}, nil)
		require.NoError(t, err)
		assert.True(t, p.Bool("server.insecure"))
	})
	t.Run("environment-only parameter", func(t *testing.T) {
		t.Parallel()
		pf := bind(t, settings.ComponentServer)
		p, _, err := pf.Resolve(map[string]string{"server.http.cookie.maxnumber": "30"}, envMap(map[string]string{"PATROL_MAX_COOKIE_NUMBER": "40"}))
		require.NoError(t, err)
		assert.EqualValues(t, 40, p.Int("server.http.cookie.maxnumber"))
		p, _, err = pf.Resolve(map[string]string{"server.http.cookie.maxnumber": "30"}, nil)
		require.NoError(t, err)
		assert.EqualValues(t, 30, p.Int("server.http.cookie.maxnumber"))
	})
	t.Run("list and map flags", func(t *testing.T) {
		t.Parallel()
		pf := bind(t, settings.ComponentController, "--application-namespaces=team-*,ops", "--otlp-headers=a=1,b=2")
		p, _, err := pf.Resolve(map[string]string{"application.namespaces": "ignored"}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"team-*", "ops"}, p.Strings("application.namespaces"))
		assert.Equal(t, map[string]string{"a": "1", "b": "2"}, p.StringMap("otlp.headers"))
	})
}

func TestParamValidation(t *testing.T) {
	t.Parallel()
	t.Run("invalid flag values are errors", func(t *testing.T) {
		t.Parallel()
		for _, args := range [][]string{
			{"--status-processors=abc"},
			{"--sharding-method=random"},
			{"--app-state-cache-expiration=11h"}, // above the 10h maximum
			{"--self-heal-timeout-seconds=-1"},
		} {
			pf := bind(t, settings.ComponentController, args...)
			_, _, err := pf.Resolve(nil, nil)
			require.Error(t, err, "%v", args)
			assert.Contains(t, err.Error(), "flag --", "%v", args)
		}
	})
	t.Run("invalid environment values fall back with a warning", func(t *testing.T) {
		t.Parallel()
		pf := bind(t, settings.ComponentController)
		p, issues, err := pf.Resolve(nil, envMap(map[string]string{"PATROL_APPLICATION_CONTROLLER_STATUS_PROCESSORS": "abc"}))
		require.NoError(t, err)
		assert.EqualValues(t, 20, p.Int("controller.status.processors"))
		require.Len(t, issues, 1)
		assert.Equal(t, settings.SeverityWarning, issues[0].Severity)
	})
	t.Run("fatal values from the configmap are errors", func(t *testing.T) {
		t.Parallel()
		pf := bind(t, settings.ComponentRepoServer)
		_, _, err := pf.Resolve(map[string]string{"reposerver.max.combined.directory.manifests.size": "lots"}, nil)
		require.Error(t, err)
		pf = bind(t, settings.ComponentServer)
		_, _, err = pf.Resolve(map[string]string{"server.tls.minversion": "9"}, nil)
		require.Error(t, err)
	})
	t.Run("unknown configmap keys are warnings", func(t *testing.T) {
		t.Parallel()
		pf := bind(t, settings.ComponentServer)
		_, issues, err := pf.Resolve(map[string]string{"no.such.key": "x"}, nil)
		require.NoError(t, err)
		require.NotEmpty(t, issues)
		assert.Equal(t, "no.such.key", issues[0].Key)
	})
}

// TestBindParamFlagsEveryComponent checks every flagged parameter of every component is a
// flag, and that flags defined by the command itself win.
func TestBindParamFlagsEveryComponent(t *testing.T) {
	t.Parallel()
	for _, c := range settings.Components() {
		fs := pflag.NewFlagSet(string(c), pflag.ContinueOnError)
		fs.String("repo-server", "own", "defined by the command")
		pf := BindParamFlags(fs, c)
		for _, spec := range settings.ParamsForComponent(c) {
			if spec.Flag == "" {
				continue
			}
			f := fs.Lookup(spec.Flag)
			require.NotNil(t, f, "%s: --%s", c, spec.Flag)
			if spec.Flag == "repo-server" {
				assert.Equal(t, "own", f.DefValue)
				continue
			}
			assert.Equal(t, spec.Default, f.DefValue, "%s: --%s", c, spec.Flag)
		}
		p, _, err := pf.Resolve(nil, nil)
		require.NoError(t, err, c)
		require.NotNil(t, p)
	}
}

func TestParamEnvLookup(t *testing.T) {
	t.Parallel()
	pf := bind(t, settings.ComponentController, "--sharding-method=round-robin")
	p, _, err := pf.Resolve(map[string]string{"controller.k8s.client.qps": "7"}, nil)
	require.NoError(t, err)
	lookup := paramEnvLookup(p, envMap(map[string]string{"PATROL_OTHER": "x"}))
	v, ok := lookup("PATROL_CONTROLLER_SHARDING_ALGORITHM")
	assert.True(t, ok)
	assert.Equal(t, "round-robin", v)
	v, ok = lookup("PATROL_K8S_CLIENT_QPS")
	assert.True(t, ok)
	assert.Equal(t, "7", v)
	_, ok = lookup("PATROL_K8S_CLIENT_BURST") // default: not reported
	assert.False(t, ok)
	v, ok = lookup("PATROL_OTHER")
	assert.True(t, ok)
	assert.Equal(t, "x", v)
}

func TestReadCmdParams(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	cs := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultCmdParamsConfigMapName, Namespace: "patrol"},
		Data:       map[string]string{"server.insecure": "true", "server.rootpath": "/patrol"},
	})
	data, err := readCmdParams(ctx, cs, "patrol", log)
	require.NoError(t, err)
	pf := bind(t, settings.ComponentServer, "--rootpath=/flag")
	p, _, err := pf.Resolve(data, envMap(map[string]string{"PATROL_SERVER_INSECURE": "false"}))
	require.NoError(t, err)
	assert.False(t, p.Bool("server.insecure"), "environment wins over the ConfigMap")
	assert.Equal(t, "/flag", p.String("server.rootpath"), "flag wins over the ConfigMap")

	missing, err := readCmdParams(ctx, fake.NewClientset(), "patrol", log)
	require.NoError(t, err)
	assert.Nil(t, missing)

	forbidden := fake.NewClientset()
	forbidden.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, settings.DefaultCmdParamsConfigMapName, errors.New("denied"))
	})
	got, err := readCmdParams(ctx, forbidden, "patrol", log)
	require.NoError(t, err, "a ConfigMap the component may not read is ignored")
	assert.Nil(t, got)

	failing := fake.NewClientset()
	failing.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	_, err = readCmdParams(ctx, failing, "patrol", log)
	require.Error(t, err)
}

func TestLogOptions(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	pf := bind(t, settings.ComponentAppSetController, "--loglevel=WARNING")
	p, _, err := pf.Resolve(nil, nil)
	require.NoError(t, err)
	o := logOptionsFor(bootstrapInput{pf: pf}, p)
	assert.Equal(t, "warn", o.Level)
	assert.Equal(t, LogFormatText, o.Format, "local default")

	pf = bind(t, settings.ComponentAppSetController, "--debug", "--loglevel=error", "--logformat=JSON")
	p, _, err = pf.Resolve(nil, nil)
	require.NoError(t, err)
	o = logOptionsFor(bootstrapInput{pf: pf}, p)
	assert.Equal(t, "debug", o.Level, "--debug overrides the level")
	assert.Equal(t, LogFormatJSON, o.Format)

	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	pf = bind(t, settings.ComponentServer)
	p, _, err = pf.Resolve(nil, nil)
	require.NoError(t, err)
	assert.Equal(t, LogFormatJSON, logOptionsFor(bootstrapInput{pf: pf}, p).Format, "in-cluster default")

	_, err = NewLogger(nil, LogOptions{Level: "info", Format: "xml"})
	require.Error(t, err)
}
