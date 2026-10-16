package reposerver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigTrustMaterialDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		in                   Config
		knownHosts, tls, gpg string
	}{
		{
			name:       "zero values take the mounted ConfigMap locations",
			in:         Config{RootDir: "/tmp/x"},
			knownHosts: DefaultSSHKnownHostsPath, tls: DefaultTLSCertsDir, gpg: DefaultGPGKeysDir,
		},
		{
			name:       "explicit paths are kept",
			in:         Config{RootDir: "/tmp/x", SSHKnownHostsPath: "/k", TLSCertsDir: "/t", GPGKeysDir: "/g"},
			knownHosts: "/k", tls: "/t", gpg: "/g",
		},
		{
			name:       "the disabled marker turns trust material off one by one",
			in:         Config{RootDir: "/tmp/x", SSHKnownHostsPath: TrustMaterialDisabled, GPGKeysDir: TrustMaterialDisabled},
			knownHosts: "", tls: DefaultTLSCertsDir, gpg: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.in.withDefaults()
			require.NoError(t, err)
			assert.Equal(t, tt.knownHosts, got.SSHKnownHostsPath)
			assert.Equal(t, tt.tls, got.TLSCertsDir)
			assert.Equal(t, tt.gpg, got.GPGKeysDir)
		})
	}
}

// TestServiceUsesTrustMaterialDefaults checks that a zero-valued configuration (as
// cmd/patrolcd builds it) reads trust material from the default mount points.
func TestServiceUsesTrustMaterialDefaults(t *testing.T) {
	t.Parallel()
	s := newTestService(t, func(c *Config) { c.SSHKnownHostsPath, c.TLSCertsDir, c.GPGKeysDir = "", "", "" })
	assert.Equal(t, DefaultSSHKnownHostsPath, s.trust.knownHostsPath)
	assert.Equal(t, DefaultTLSCertsDir, s.trust.tlsCertsDir)
	assert.Equal(t, DefaultGPGKeysDir, s.keyring.srcDir)
}
