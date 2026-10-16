package git

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sshServer is a throwaway sshd on 127.0.0.1 that accepts one user key and runs
// git commands with the test's git in PATH. Tests are skipped when sshd cannot
// be started (no sshd binary, sandboxed environment).
type sshServer struct {
	port       int
	hostPub    string // "type base64"
	userKey    string // private key PEM
	otherKey   string // a key that is not authorized
	knownHosts string // known_hosts line for the server
}

func findSSHD() string {
	for _, p := range []string{"/usr/sbin/sshd", "/opt/homebrew/sbin/sshd", "/usr/local/sbin/sshd"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath("sshd")
	return p
}

func startSSHServer(t *testing.T) *sshServer {
	t.Helper()
	sshd := findSSHD()
	keygen, err := exec.LookPath("ssh-keygen")
	if sshd == "" || err != nil {
		t.Skip("sshd or ssh-keygen not available")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not available")
	}
	u, err := user.Current()
	require.NoError(t, err)
	dir, err := os.MkdirTemp("", "psshd") // short path for sshd
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	gen := func(name string) {
		out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, name)).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	gen("host_key")
	gen("user_key")
	gen("other_key")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(readFile(t, filepath.Join(dir, "user_key.pub"))), 0o600))
	wrapper := fmt.Sprintf("#!/bin/sh\nPATH=%s:/usr/bin:/bin\nexport PATH\neval \"exec $SSH_ORIGINAL_COMMAND\"\n", filepath.Dir(gitBin(t)))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wrap.sh"), []byte(wrapper), 0o755))

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	cfg := strings.Join([]string{
		fmt.Sprintf("Port %d", port),
		"ListenAddress 127.0.0.1",
		"HostKey " + filepath.Join(dir, "host_key"),
		"AuthorizedKeysFile " + filepath.Join(dir, "authorized_keys"),
		"AllowUsers " + u.Username,
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"UsePAM no",
		"StrictModes no",
		"PidFile " + filepath.Join(dir, "sshd.pid"),
		"ForceCommand " + filepath.Join(dir, "wrap.sh"),
		"LogLevel ERROR",
	}, "\n") + "\n"
	cfgPath := filepath.Join(dir, "sshd_config")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))
	if out, err := exec.Command(sshd, "-t", "-f", cfgPath).CombinedOutput(); err != nil {
		t.Skipf("sshd rejects test configuration: %s", out)
	}
	cmd := exec.Command(sshd, "-D", "-e", "-f", cfgPath)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Skip("sshd did not start listening")
		}
		time.Sleep(50 * time.Millisecond)
	}
	hostPub := strings.Join(strings.Fields(readFile(t, filepath.Join(dir, "host_key.pub")))[:2], " ")
	return &sshServer{
		port:       port,
		hostPub:    hostPub,
		userKey:    readFile(t, filepath.Join(dir, "user_key")),
		otherKey:   readFile(t, filepath.Join(dir, "other_key")),
		knownHosts: fmt.Sprintf("[127.0.0.1]:%d %s\n", port, hostPub),
	}
}

func TestSSHTransport(t *testing.T) {
	t.Parallel()
	srv := startSSHServer(t)
	f := newRevisionFixture(t)
	u, err := user.Current()
	require.NoError(t, err)
	repoURL := fmt.Sprintf("ssh://%s@127.0.0.1:%d%s", u.Username, srv.port, f.bare)
	ctx := testCtx(t)

	t.Run("known host", func(t *testing.T) {
		t.Parallel()
		rec := &cmdRecorder{}
		c := newTestClient(t, repoURL, SSHCreds{PrivateKey: srv.userKey, KnownHosts: []byte(srv.knownHosts)}, withRunHook(rec.hook))
		sha, err := c.ResolveRevision(ctx, "feature")
		require.NoError(t, err)
		assert.Equal(t, f.c2, sha)
		require.NoError(t, c.CheckoutRevision(ctx, "main", false))
		head, err := c.CommitSHA(ctx)
		require.NoError(t, err)
		assert.Equal(t, f.c4, head)
		keyBody := strings.Split(strings.TrimSpace(srv.userKey), "\n")[1]
		assertNoSecretInArgs(t, rec, keyBody)
		for _, cmd := range rec.all() {
			for _, kv := range cmd.Env {
				assert.NotContains(t, kv, keyBody, "private key material must only be in the key file")
				if v, ok := strings.CutPrefix(kv, "GIT_SSH_COMMAND="); ok {
					for _, field := range strings.Fields(v) {
						if p := strings.Trim(field, "'"); strings.Contains(p, "ssh-key-") {
							assert.NoFileExists(t, p, "key file removed after the command")
						}
					}
				}
			}
		}
	})
	t.Run("unknown host", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, repoURL, SSHCreds{PrivateKey: srv.userKey})
		_, err := c.LsRemote(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAuthentication)
		assert.Contains(t, strings.ToLower(err.Error()), "host key verification failed")
	})
	t.Run("insecure ignores host key", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, repoURL, SSHCreds{PrivateKey: srv.userKey, InsecureIgnoreHostKey: true})
		_, err := c.LsRemote(ctx)
		require.NoError(t, err)
	})
	t.Run("unauthorized key", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, repoURL, SSHCreds{PrivateKey: srv.otherKey, KnownHosts: []byte(srv.knownHosts)})
		_, err := c.LsRemote(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAuthentication)
		assert.NotContains(t, err.Error(), strings.Split(strings.TrimSpace(srv.otherKey), "\n")[1])
	})
}
