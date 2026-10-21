package reposerver

import (
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// mountRootFS stands in for the file system under the root of a Kubernetes volume: the directory
// belongs to root, so the process is not permitted to change its mode. rootOwner is the uid the
// mount root reports.
func mountRootFS(mount string, rootOwner uint32) rootFS {
	real := defaultRootFS()
	return rootFS{
		euid: real.euid,
		chmod: func(name string, mode fs.FileMode) error {
			if filepath.Clean(name) == filepath.Clean(mount) {
				return &fs.PathError{Op: "chmod", Path: name, Err: syscall.EPERM}
			}
			return real.chmod(name, mode)
		},
		owner: func(fi fs.FileInfo) (uint32, bool) {
			if os.SameFile(fi, mustStat(mount)) {
				return rootOwner, true
			}
			return real.owner(fi)
		},
	}
}

func mustStat(p string) fs.FileInfo {
	fi, err := os.Stat(p)
	if err != nil {
		panic(err)
	}
	return fi
}

func mode(t *testing.T, p string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	require.NoError(t, err)
	return fi.Mode().Perm()
}

func TestPrepareRootOwnedDirectory(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	got, err := prepareRoot(root, defaultRootFS(), discardLog())
	require.NoError(t, err)
	assert.Equal(t, root, got, "a directory the process can chmod is used as is")
	assert.Equal(t, fs.FileMode(0o700), mode(t, root))
}

func TestPrepareRootMountRoot(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	// What the kubelet creates for an emptyDir: owned by root, world-writable.
	require.NoError(t, os.Chmod(mount, 0o777))
	fsys := mountRootFS(mount, 0)

	got, err := prepareRoot(mount, fsys, discardLog())
	require.NoError(t, err)
	want := filepath.Join(mount, privateRootName)
	assert.Equal(t, want, got, "the service works in a private subdirectory")
	assert.Equal(t, fs.FileMode(0o700), mode(t, got))
	assert.Equal(t, fs.FileMode(0o777), mode(t, mount), "the mount root is left as it is")

	// A container restart reuses the same emptyDir: the previous process left its directory
	// locked (0300) and the new one must be able to take it over.
	require.NoError(t, os.Chmod(got, 0o300))
	again, err := prepareRoot(mount, fsys, discardLog())
	require.NoError(t, err)
	assert.Equal(t, want, again)
	assert.Equal(t, fs.FileMode(0o700), mode(t, again))
}

// A root dir that is not ours and not root's is refused, as it was before the fallback existed:
// whoever owns it could move the private directory elsewhere.
func TestPrepareRootForeignOwner(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	fsys := mountRootFS(mount, defaultRootFS().euid+1)
	if fsys.euid+1 == 0 {
		t.Skip("uid arithmetic wrapped")
	}
	_, err := prepareRoot(mount, fsys, discardLog())
	require.ErrorIs(t, err, syscall.EPERM)
	assert.ErrorContains(t, err, "owned by another user")
	_, statErr := os.Lstat(filepath.Join(mount, privateRootName))
	assert.ErrorIs(t, statErr, fs.ErrNotExist, "nothing is created in a directory that is not ours")

	unknown := mountRootFS(mount, 0)
	unknown.owner = func(fs.FileInfo) (uint32, bool) { return 0, false }
	_, err = prepareRoot(mount, unknown, discardLog())
	require.ErrorContains(t, err, "owned by another user", "an owner that cannot be determined is refused")
}

// The private directory must be a real directory of this process: a symlink (even to a directory
// the process owns) or a file planted under the name is never adopted.
func TestPrepareRootRefusesPlantedEntry(t *testing.T) {
	t.Parallel()
	for name, plant := range map[string]func(t *testing.T, sub, outside string){
		"symlink to a directory": func(t *testing.T, sub, outside string) { require.NoError(t, os.Symlink(outside, sub)) },
		"regular file":           func(t *testing.T, sub, _ string) { require.NoError(t, os.WriteFile(sub, nil, 0o600)) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mount, outside := t.TempDir(), t.TempDir()
			victim := filepath.Join(outside, "r-victim")
			require.NoError(t, os.Mkdir(victim, 0o700))
			require.NoError(t, os.Chmod(outside, 0o755))
			plant(t, filepath.Join(mount, privateRootName), outside)

			_, err := prepareRoot(mount, mountRootFS(mount, 0), discardLog())
			require.Error(t, err)
			assert.ErrorContains(t, err, "private work dir")
			assert.Equal(t, fs.FileMode(0o755), mode(t, outside), "the target of the link keeps its mode")
			assert.DirExists(t, victim)
		})
	}

	t.Run("directory of another owner", func(t *testing.T) {
		t.Parallel()
		mount := t.TempDir()
		sub := filepath.Join(mount, privateRootName)
		require.NoError(t, os.Mkdir(sub, 0o700))
		require.NoError(t, os.Chmod(sub, 0o777))
		fsys := mountRootFS(mount, 0)
		real := fsys.owner
		fsys.owner = func(fi fs.FileInfo) (uint32, bool) {
			if os.SameFile(fi, mustStat(sub)) {
				return fsys.euid + 1, true
			}
			return real(fi)
		}
		_, err := prepareRoot(mount, fsys, discardLog())
		require.ErrorContains(t, err, "not owned by this process")
		assert.Equal(t, fs.FileMode(0o777), mode(t, sub), "its mode is not touched")
	})
}

func TestPrepareRootOtherErrors(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	fsys := defaultRootFS()
	fsys.chmod = func(string, fs.FileMode) error { return &fs.PathError{Op: "chmod", Path: root, Err: syscall.EIO} }
	_, err := prepareRoot(root, fsys, discardLog())
	require.ErrorIs(t, err, syscall.EIO, "only permission errors select the subdirectory")
	assert.ErrorContains(t, err, "root dir permissions")
	_, statErr := os.Stat(filepath.Join(root, privateRootName))
	assert.ErrorIs(t, statErr, fs.ErrNotExist)
}

// TestNewOnMountRoot builds a whole Service on a mount root: it must start, keep its state in
// the private subdirectory and leave the mount root untouched.
func TestNewOnMountRoot(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	require.NoError(t, os.Chmod(mount, 0o777))
	s := newTestService(t, func(c *Config) {
		c.RootDir = mount
		fsys := mountRootFS(mount, 0)
		c.rootFS = &fsys
	})
	t.Cleanup(func() { _ = s.Close() })

	private := filepath.Join(mount, privateRootName)
	wantRoot, err := filepath.EvalSymlinks(private)
	require.NoError(t, err)
	assert.Equal(t, wantRoot, s.root)
	assert.Equal(t, fs.FileMode(0o300), mode(t, private), "the work root may be traversed but not listed")
	assert.Equal(t, fs.FileMode(0o777), mode(t, mount))
	for _, d := range []string{"helm", "render", "oci-cache"} {
		_, statErr := os.Stat(filepath.Join(private, d))
		assert.NoError(t, statErr, d)
	}
}
