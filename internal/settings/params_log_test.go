package settings

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveParamsLogSettingsAcceptUpstreamSpellings: upstream passes *.log.level to
// logrus.ParseLevel and *.log.format through strings.ToLower, so every logrus level name in
// any case, and the formats in any case, are valid; a migrated patrol-cmd-params-cm must not
// fail startup.
func TestResolveParamsLogSettingsAcceptUpstreamSpellings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, value, want, warning string
	}{
		{"controller.log.level", "warning", "warn", ""},
		{"controller.log.level", "WARN", "warn", ""},
		{"controller.log.level", "INFO", "info", ""},
		{"controller.log.level", "Debug", "debug", ""},
		{"controller.log.level", "error", "error", ""},
		{"controller.log.level", "trace", "debug", "log level 'trace' in controller.log.level is used as 'debug'"},
		{"controller.log.level", "fatal", "error", "log level 'fatal' in controller.log.level is used as 'error'"},
		{"controller.log.level", "PANIC", "error", "log level 'PANIC' in controller.log.level is used as 'error'"},
		{"controller.log.format", "JSON", "json", ""},
		{"controller.log.format", "Text", "text", ""},
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Parallel()
			p, issues := ResolveParams(ComponentController, map[string]string{tc.key: tc.value}, nil)
			assert.False(t, HasErrors(issues), "issues: %v", issues)
			assert.Equal(t, tc.want, p.String(tc.key))
			v, _ := p.Lookup(tc.key)
			assert.Equal(t, ParamFromConfigMap, v.Source)
			assert.Equal(t, tc.value, v.Raw)
			assert.Equal(t, tc.want, p.FlagDefault(tc.key))
			if tc.warning == "" {
				assert.Empty(t, issues)
			} else {
				require.Len(t, issues, 1)
				assert.Equal(t, SeverityWarning, issues[0].Severity)
				assert.Contains(t, issues[0].Message, tc.warning)
			}
			if strings.HasSuffix(tc.key, ".log.level") {
				var lvl slog.Level
				assert.NoError(t, lvl.UnmarshalText([]byte(p.String(tc.key))), "the value is a slog level name")
			}
		})
	}

	env := map[string]string{"PATROL_SERVER_LOG_LEVEL": "Warning"}
	p, issues := ResolveParams(ComponentServer, nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	assert.Empty(t, issues)
	assert.Equal(t, "warn", p.String("server.log.level"))

	for _, c := range Components() {
		for _, spec := range ParamsForComponent(c) {
			if strings.HasSuffix(spec.Key, ".log.level") {
				assert.Equal(t, ParamLogLevel, spec.Type, spec.Key)
				assert.Equal(t, []string{"panic", "fatal", "error", "warn", "warning", "info", "debug", "trace"}, spec.Allowed, spec.Key)
			}
			if strings.HasSuffix(spec.Key, ".log.format") {
				assert.True(t, spec.CaseInsensitive, spec.Key)
			}
		}
	}
}
