package git

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func requirePrivateFile(t *testing.T, path, wantContent string) {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), path)
	if wantContent != "" {
		assert.Equal(t, wantContent, readFile(t, path))
	}
}

func TestHTTPSCredsEnviron(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	creds := HTTPSCreds{
		Username:       "alice",
		Password:       "pw",
		BearerToken:    "tok",
		ClientCertData: "CERT",
		ClientCertKey:  "KEY",
		CAData:         []byte("CA"),
	}
	env, cleanup, err := creds.Environ(dir)
	require.NoError(t, err)
	m := envMap(env)
	for k, v := range m {
		assert.NotContains(t, v, "pw", "password must only be in the askpass file (%s)", k)
	}
	requirePrivateFile(t, m["GIT_SSL_CAINFO"], "CA")
	requirePrivateFile(t, m["GIT_SSL_CERT"], "CERT")
	requirePrivateFile(t, m["GIT_SSL_KEY"], "KEY")
	requirePrivateFile(t, m[askpassFileEnv], "")
	var payload askpassPayload
	require.NoError(t, json.Unmarshal([]byte(readFile(t, m[askpassFileEnv])), &payload))
	assert.Equal(t, askpassPayload{Username: "alice", Password: "pw"}, payload)
	exe, err := os.Executable()
	require.NoError(t, err)
	assert.Equal(t, exe, m["GIT_ASKPASS"])
	assert.Equal(t, "1", m["GIT_CONFIG_COUNT"])
	assert.Equal(t, "http.extraHeader", m["GIT_CONFIG_KEY_0"])
	assert.Equal(t, "Authorization: Bearer tok", m["GIT_CONFIG_VALUE_0"], "bearer wins without ForceBasicAuth")
	for _, k := range []string{"GIT_SSL_CAINFO", "GIT_SSL_CERT", "GIT_SSL_KEY", askpassFileEnv} {
		assert.Equal(t, dir, filepath.Dir(m[k]), "%s must live in the per-call directory", k)
	}

	cleanup()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "cleanup removes every credential file")
}

func TestHTTPSCredsHeaderPrecedence(t *testing.T) {
	t.Parallel()
	basic := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	tests := []struct {
		name  string
		creds HTTPSCreds
		want  string
	}{
		{"none", HTTPSCreds{Username: "u", Password: "p"}, ""},
		{"bearer", HTTPSCreds{BearerToken: "t"}, "Authorization: Bearer t"},
		{"force basic", HTTPSCreds{Username: "u", Password: "p", ForceBasicAuth: true}, basic},
		{"force basic beats bearer", HTTPSCreds{Username: "u", Password: "p", BearerToken: "t", ForceBasicAuth: true}, basic},
		{"force basic needs a password", HTTPSCreds{Username: "u", BearerToken: "t", ForceBasicAuth: true}, "Authorization: Bearer t"},
		{"default username", HTTPSCreds{Password: "p", ForceBasicAuth: true},
			"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(DefaultHTTPSUsername+":p"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.creds.authHeader())
		})
	}
}

func TestHTTPSCredsInsecureIgnoresCA(t *testing.T) {
	t.Parallel()
	env, cleanup, err := HTTPSCreds{Insecure: true, CAData: []byte("CA")}.Environ(t.TempDir())
	require.NoError(t, err)
	defer cleanup()
	m := envMap(env)
	assert.Equal(t, "true", m["GIT_SSL_NO_VERIFY"])
	assert.NotContains(t, m, "GIT_SSL_CAINFO")
	assert.NotContains(t, m, "GIT_SSL_CERT", "client cert requires both cert and key")
}

func TestSSHCredsEnviron(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env, cleanup, err := SSHCreds{PrivateKey: "KEYDATA", KnownHosts: []byte("host ssh-ed25519 AAAA\n")}.Environ(dir)
	require.NoError(t, err)
	cmd := envMap(env)["GIT_SSH_COMMAND"]
	assert.Contains(t, cmd, "-o StrictHostKeyChecking=yes")
	assert.Contains(t, cmd, "IdentitiesOnly=yes")
	assert.NotContains(t, cmd, "KEYDATA")
	var keyPath, khPath string
	for _, f := range strings.Fields(cmd) {
		f = strings.Trim(f, "'")
		if strings.HasPrefix(f, dir+"/ssh-key-") {
			keyPath = f
		}
		if p, ok := strings.CutPrefix(f, "UserKnownHostsFile="); ok {
			khPath = p
		}
	}
	requirePrivateFile(t, keyPath, "KEYDATA\n")
	requirePrivateFile(t, khPath, "host ssh-ed25519 AAAA\n")
	cleanup()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)

	env, cleanup, err = SSHCreds{PrivateKey: "K\n", InsecureIgnoreHostKey: true}.Environ(t.TempDir())
	require.NoError(t, err)
	defer cleanup()
	cmd = envMap(env)["GIT_SSH_COMMAND"]
	assert.Contains(t, cmd, "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null")
}

func TestSSHCredsProxy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env, cleanup, err := SSHCreds{PrivateKey: "K", Proxy: "socks5://user:proxy-pw@proxy.local:1080"}.Environ(dir)
	require.NoError(t, err)
	m := envMap(env)
	assert.Equal(t, "user", m["SOCKS5_USER"])
	assert.Equal(t, "proxy-pw", m["SOCKS5_PASSWD"])
	assert.NotContains(t, m["GIT_SSH_COMMAND"], "proxy-pw")
	assert.Contains(t, m["GIT_SSH_COMMAND"], "ProxyCommand=")
	cleanup()

	for _, bad := range []string{"http://proxy:80", "socks5://a;b:1080", "socks5://proxy", "socks5://proxy:0", "socks5://proxy:99999", "socks5://[zz::1]:1080"} {
		_, _, err := SSHCreds{PrivateKey: "K", Proxy: bad}.Environ(t.TempDir())
		assert.ErrorIs(t, err, ErrInvalidCredentials, bad)
	}
	assert.NoError(t, ValidateSSHProxyURL("socks5h://[::1]:1080"))
	assert.NoError(t, ValidateHTTPProxyURL("http://proxy.example.com:3128"))
	assert.Error(t, ValidateHTTPProxyURL("http://proxy.example.com"))
}

func TestCredsRedactionsAndFingerprints(t *testing.T) {
	t.Parallel()
	h := HTTPSCreds{Username: "u", Password: "pw", BearerToken: "tok", ClientCertKey: "key"}
	r := credsRedactions(h)
	assert.Contains(t, r, "pw")
	assert.Contains(t, r, "tok")
	assert.Contains(t, r, "key")
	assert.Contains(t, r, base64.StdEncoding.EncodeToString([]byte("u:pw")))
	assert.Contains(t, credsRedactions(SSHCreds{PrivateKey: "k", Proxy: "socks5://a:secret@h:1"}), "secret")
	assert.Empty(t, credsRedactions(NopCreds{}))

	assert.Equal(t, credsFingerprint(h), credsFingerprint(h))
	assert.NotEqual(t, credsFingerprint(h), credsFingerprint(HTTPSCreds{Username: "u", Password: "pw2"}))
	assert.NotEqual(t, credsFingerprint(NopCreds{}), credsFingerprint(HTTPSCreds{}))
	assert.NotContains(t, credsFingerprint(h), "pw")
	assert.Empty(t, credsFingerprint(customCreds{}), "unknown credential types bypass the ref cache")
}

type customCreds struct{}

func (customCreds) Environ(string) ([]string, func(), error) { return nil, func() {}, nil }

func TestScopeHeaders(t *testing.T) {
	t.Parallel()
	const origin = "https://example.com:8443"
	bearer := gitConfig{"http.extraHeader", "Authorization: Bearer x"}
	other := gitConfig{"other.key", "v"}
	tests := map[string]struct {
		in     []gitConfig
		origin string
		want   []gitConfig
	}{
		"unscoped header is scoped to the origin and redirects are disabled": {
			in:     []gitConfig{bearer, other},
			origin: origin,
			want: []gitConfig{
				{"http." + origin + "/.extraHeader", bearer.value},
				other,
				{"http." + origin + "/.followRedirects", "false"},
			},
		},
		"key case is ignored and one guard per scope": {
			in:     []gitConfig{{"HTTP.ExtraHeader", "A: 1"}, {"http.extraheader", "B: 2"}},
			origin: origin,
			want: []gitConfig{
				{"http." + origin + "/.extraHeader", "A: 1"},
				{"http." + origin + "/.extraHeader", "B: 2"},
				{"http." + origin + "/.followRedirects", "false"},
			},
		},
		"pre-scoped header keeps its scope and gets a guard": {
			in:     []gitConfig{{"http.https://other.example/.extraHeader", "A: 1"}},
			origin: origin,
			want: []gitConfig{
				{"http.https://other.example/.extraHeader", "A: 1"},
				{"http.https://other.example/.followRedirects", "false"},
			},
		},
		"no http origin drops unscoped headers": {
			in:     []gitConfig{bearer, other},
			origin: "",
			want:   []gitConfig{other},
		},
		"no headers: no guard": {
			in:     []gitConfig{other, {"http.sslVerify", "false"}},
			origin: origin,
			want:   []gitConfig{other, {"http.sslVerify", "false"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, scopeHeaders(tt.in, tt.origin))
		})
	}
}

func TestGitConfigEnvRoundTripProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		gen := rapid.Custom(func(t *rapid.T) gitConfig {
			return gitConfig{
				key:   rapid.StringMatching(`[a-z]{1,6}\.[a-zA-Z]{1,8}`).Draw(t, "key"),
				value: rapid.String().Draw(t, "value"),
			}
		})
		first := rapid.SliceOfN(gen, 0, 5).Draw(t, "first")
		second := rapid.SliceOfN(gen, 0, 5).Draw(t, "second")
		other := rapid.SliceOfN(rapid.StringMatching(`[A-Z]{1,5}=[a-z]{0,5}`), 0, 4).Draw(t, "other")
		env := appendGitConfig(appendGitConfig(other, first...), second...)
		rest, cfg := splitGitConfig(env)
		want := append(append([]gitConfig{}, first...), second...)
		if len(want) == 0 {
			want = []gitConfig{}
		}
		if fmt.Sprint(cfg) != fmt.Sprint(want) {
			t.Fatalf("config round trip: got %v want %v", cfg, want)
		}
		if fmt.Sprint(rest) != fmt.Sprint(other) {
			t.Fatalf("other entries: got %v want %v", rest, other)
		}
		counts := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
				counts++
			}
		}
		if len(want) > 0 && counts != 1 {
			t.Fatalf("expected exactly one GIT_CONFIG_COUNT, got %d", counts)
		}
	})
}
