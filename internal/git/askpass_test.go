package git

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeAskpassFile(t *testing.T, p askpassPayload, mode os.FileMode) string {
	t.Helper()
	data, err := json.Marshal(p)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "askpass")
	require.NoError(t, os.WriteFile(path, data, mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestAskpassMain(t *testing.T) {
	t.Parallel()
	good := writeAskpassFile(t, askpassPayload{Username: "alice", Password: "s3cret"}, 0o600)
	const origin = "https://git.example.com"
	tests := []struct {
		name, prompt, file, origin string
		wantCode                   int
		wantOut                    string
	}{
		{"username", "Username for 'https://git.example.com': ", good, origin, 0, "alice\n"},
		{"password", "Password for 'https://alice@git.example.com': ", good, origin, 0, "s3cret\n"},
		{"double quotes (git-lfs)", `Password for "https://git.example.com"`, good, origin, 0, "s3cret\n"},
		{"explicit default port", "Password for 'https://git.example.com:443': ", good, origin, 0, "s3cret\n"},
		{"other host", "Password for 'https://evil.example.com': ", good, origin, 1, ""},
		{"other scheme", "Password for 'http://git.example.com': ", good, origin, 1, ""},
		{"other port", "Password for 'https://git.example.com:8443': ", good, origin, 1, ""},
		{"no origin configured", "Password for 'https://git.example.com': ", good, "", 1, ""},
		{"unknown prompt", "Enter passphrase for key '/tmp/k': ", good, origin, 1, ""},
		{"missing file", "Password for 'https://git.example.com': ", filepath.Join(t.TempDir(), "nope"), origin, 1, ""},
		{"relative file", "Password for 'https://git.example.com': ", "askpass", origin, 1, ""},
		{"group readable file", "Password for 'https://git.example.com': ",
			writeAskpassFile(t, askpassPayload{Password: "x"}, 0o640), origin, 1, ""},
		{"line break in secret", "Password for 'https://git.example.com': ",
			writeAskpassFile(t, askpassPayload{Password: "a\nb"}, 0o600), origin, 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := askpassMain(tt.prompt, tt.file, tt.origin, &out, &errOut)
			assert.Equal(t, tt.wantCode, code, errOut.String())
			assert.Equal(t, tt.wantOut, out.String())
			assert.NotContains(t, errOut.String(), "s3cret")
		})
	}
}

func TestAskpassFileTooLarge(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "big")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), maxAskpassFile+1), 0o600))
	_, err := readAskpassFile(path)
	assert.Error(t, err)
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(path, link))
	_, err = readAskpassFile(link)
	assert.Error(t, err, "symlinks are refused")
}

// TestAskpassHelperProcess runs the test binary exactly as git runs GIT_ASKPASS.
func TestAskpassHelperProcess(t *testing.T) {
	t.Parallel()
	exe, err := askpassExecutable()
	require.NoError(t, err)
	file := writeAskpassFile(t, askpassPayload{Username: "bob", Password: "pw-xyz"}, 0o600)
	cmd := exec.Command(exe, "Password for 'https://bob@git.example.com': ")
	cmd.Env = []string{askpassFileEnv + "=" + file, askpassOriginEnv + "=https://git.example.com"}
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, "pw-xyz\n", string(out))

	cmd = exec.Command(exe, "Password for 'https://other.example.com': ")
	cmd.Env = []string{askpassFileEnv + "=" + file, askpassOriginEnv + "=https://git.example.com"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	require.Error(t, err)
	assert.Empty(t, out)
	assert.True(t, strings.Contains(stderr.String(), "only offered to the repository origin"))
}

func TestSetAskpassPath(t *testing.T) {
	// Not parallel: mutates package state.
	SetAskpassPath("relative/helper")
	_, err := askpassExecutable()
	assert.Error(t, err)
	SetAskpassPath("/usr/local/bin/patrolcd")
	p, err := askpassExecutable()
	require.NoError(t, err)
	assert.Equal(t, "/usr/local/bin/patrolcd", p)
	SetAskpassPath("")
	p, err = askpassExecutable()
	require.NoError(t, err)
	exe, _ := os.Executable()
	assert.Equal(t, exe, p)
}
