package reposerver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// keyring maintains a private GnuPG home with the public keys of the mounted GnuPG keys
// ConfigMap directory (one ASCII-armored key per file, named by key ID, as upstream
// SyncKeyRingFromDirectory). The home is rebuilt when the directory content changes;
// verifications hold a read lock on the current home.
type keyring struct {
	srcDir  string
	parent  string
	gpgPath string
	path    string // PATH for gpg
	timeout time.Duration
	log     *slog.Logger

	syncMu sync.Mutex   // serializes rebuilds
	mu     sync.RWMutex // guards home/stamp; readers hold it during verification
	home   string
	stamp  string
}

const (
	maxGPGKeyFiles     = 1024
	maxGPGKeyFileBytes = 1 << 20
)

// acquire returns the GnuPG home matching the current key directory and a release
// function (the home stays valid until release). With no keys an empty keyring is
// used, so every signature verifies as made with an unknown key.
func (k *keyring) acquire(ctx context.Context) (string, func(), error) {
	keys, stamp, err := k.readKeys()
	if err != nil {
		return "", nil, err
	}
	k.mu.RLock()
	if k.home != "" && k.stamp == stamp {
		return k.home, k.mu.RUnlock, nil
	}
	k.mu.RUnlock()

	k.syncMu.Lock()
	defer k.syncMu.Unlock()
	k.mu.RLock()
	if k.home != "" && k.stamp == stamp {
		return k.home, k.mu.RUnlock, nil
	}
	k.mu.RUnlock()

	home, err := k.build(ctx, keys)
	if err != nil {
		return "", nil, err
	}
	k.mu.Lock()
	old := k.home
	k.home, k.stamp = home, stamp
	k.mu.Unlock()
	if old != "" {
		_ = os.RemoveAll(old)
	}
	k.mu.RLock()
	if k.home != home {
		k.mu.RUnlock()
		return "", nil, errors.New("gnupg keyring changed concurrently")
	}
	return home, k.mu.RUnlock, nil
}

// close removes the current home.
func (k *keyring) close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.home != "" {
		_ = os.RemoveAll(k.home)
		k.home, k.stamp = "", ""
	}
}

type gpgKeyFile struct {
	name string
	data []byte
}

// readKeys reads the key files (names that are 16-hex key IDs, as upstream) and a
// content stamp.
func (k *keyring) readKeys() ([]gpgKeyFile, string, error) {
	if k.srcDir == "" {
		return nil, "none", nil
	}
	entries, err := os.ReadDir(k.srcDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "none", nil
		}
		return nil, "", fmt.Errorf("read GnuPG keys directory: %w", err)
	}
	h := sha256.New()
	var keys []gpgKeyFile
	for _, e := range entries {
		name := e.Name()
		if _, err := settings.GPGKeyID(name); err != nil || len(name) != 16 {
			continue
		}
		if len(keys) >= maxGPGKeyFiles {
			k.log.Warn("reposerver: too many GnuPG key files; extra files ignored", slog.Int("max", maxGPGKeyFiles))
			break
		}
		data, err := readFileLimited(filepath.Join(k.srcDir, name), maxGPGKeyFileBytes)
		if err != nil {
			k.log.Warn("reposerver: cannot read GnuPG key file", slog.String("key", name), slog.Any("error", err))
			continue
		}
		keys = append(keys, gpgKeyFile{name: name, data: data})
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	slices.SortFunc(keys, func(a, b gpgKeyFile) int { return strings.Compare(a.name, b.name) })
	return keys, hex.EncodeToString(h.Sum(nil)), nil
}

// build creates a fresh GnuPG home with keys imported and ultimately trusted.
func (k *keyring) build(ctx context.Context, keys []gpgKeyFile) (string, error) {
	home, err := os.MkdirTemp(k.parent, "pgk")
	if err != nil {
		return "", fmt.Errorf("create GnuPG home: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		_ = os.RemoveAll(home)
		return "", fmt.Errorf("create GnuPG home: %w", err)
	}
	var trust strings.Builder
	for _, key := range keys {
		parsed, err := settings.ParseGPGPublicKey(string(key.data))
		if err != nil {
			k.log.Warn("reposerver: invalid GnuPG key file ignored", slog.String("key", key.name), slog.Any("error", err))
			continue
		}
		if !strings.EqualFold(parsed.KeyID, key.name) {
			k.log.Warn("reposerver: GnuPG key file name does not match its key ID; imported anyway",
				slog.String("file", key.name), slog.String("keyID", parsed.KeyID))
		}
		if err := k.gpg(ctx, home, string(key.data), "--import"); err != nil {
			k.log.Warn("reposerver: GnuPG key import failed", slog.String("key", key.name), slog.Any("error", err))
			continue
		}
		fmt.Fprintf(&trust, "%s:6:\n", parsed.Fingerprint)
	}
	if trust.Len() > 0 {
		if err := k.gpg(ctx, home, trust.String(), "--import-ownertrust"); err != nil {
			k.log.Warn("reposerver: setting GnuPG owner trust failed", slog.Any("error", err))
		}
	}
	return home, nil
}

func (k *keyring) gpg(ctx context.Context, home, stdin string, args ...string) error {
	env := []string{"GNUPGHOME=" + home, "HOME=" + home, "LANG=C", "LC_ALL=C"}
	if k.path != "" {
		env = append(env, "PATH="+k.path)
	} else {
		env = sandbox.BaseEnv(env...)
	}
	full := append([]string{"--batch", "--no-tty", "--no-autostart", "--no-permission-warning"}, args...)
	_, err := sandbox.Run(ctx, sandbox.Cmd{
		Path:   k.gpgPath,
		Args:   full,
		Dir:    home,
		Env:    env,
		Stdin:  strings.NewReader(stdin),
		Limits: sandbox.Limits{Timeout: k.timeout, MaxStdout: 1 << 20, MaxStderr: 1 << 20},
	})
	return err
}
