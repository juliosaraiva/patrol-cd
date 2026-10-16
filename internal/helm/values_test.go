package helm_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/helm"
)

func TestValuesFileSchemes(t *testing.T) {
	t.Parallel()
	if got := helm.ValuesFileSchemes("", false); !slices.Equal(got, []string{"https", "http"}) {
		t.Fatalf("absent setting = %v", got)
	}
	if got := helm.ValuesFileSchemes("", true); len(got) != 0 {
		t.Fatalf("empty setting = %v", got)
	}
	if got := helm.ValuesFileSchemes(" s3 , https,, gs ", true); !slices.Equal(got, []string{"s3", "https", "gs"}) {
		t.Fatalf("list setting = %v", got)
	}
	if !helm.IsURLSchemeAllowed("S3", []string{"s3"}) || helm.IsURLSchemeAllowed("", []string{""}) || helm.IsURLSchemeAllowed("ftp", helm.DefaultValuesFileSchemes) {
		t.Fatal("IsURLSchemeAllowed")
	}
}

// repoTree builds:
//
//	root/values.yaml
//	root/charts/app/values.yaml
//	root/charts/app/envs/prod.yaml
//	root/charts/app/inlink.yaml -> ../../values.yaml        (inside)
//	root/charts/app/outlink.yaml -> <outside>/secret.yaml   (absolute, outside)
//	root/charts/app/rel-out.yaml -> ../../../secret.yaml    (relative, outside)
//	root/charts/app/dirlink -> ../../                       (inside)
//	root/charts/app/loop -> loop
func repoTree(t *testing.T) (root, app, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "repo")
	outside = base
	app = filepath.Join(root, "charts", "app")
	for _, d := range []string{filepath.Join(app, "envs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{
		filepath.Join(root, "values.yaml"):      "root: true\n",
		filepath.Join(app, "values.yaml"):       "app: true\n",
		filepath.Join(app, "envs", "prod.yaml"): "env: prod\n",
		filepath.Join(outside, "secret.yaml"):   "secret: true\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"inlink.yaml":  "../../values.yaml",
		"outlink.yaml": filepath.Join(outside, "secret.yaml"),
		"rel-out.yaml": "../../../secret.yaml",
		"dirlink":      "../../",
		"loop":         "loop",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(app, name)); err != nil {
			t.Fatal(err)
		}
	}
	return root, app, outside
}

func TestResolveValueFilePathOrURL(t *testing.T) {
	t.Parallel()
	root, app, _ := repoTree(t)
	tests := []struct {
		name       string
		appPath    string
		file       string
		schemes    []string
		want       string // relative to root, or the URL when remote
		wantRemote bool
		wantErr    error
		errText    string
	}{
		{name: "relative", appPath: app, file: "values.yaml", want: "charts/app/values.yaml"},
		{name: "nested relative", appPath: app, file: "envs/prod.yaml", want: "charts/app/envs/prod.yaml"},
		{name: "repo-root absolute", appPath: app, file: "/values.yaml", want: "values.yaml"},
		{name: "dot-dot inside", appPath: app, file: "../../values.yaml", want: "values.yaml"},
		{name: "missing file allowed", appPath: app, file: "missing.yaml", want: "charts/app/missing.yaml"},
		{name: "symlink inside resolved", appPath: app, file: "inlink.yaml", want: "values.yaml"},
		{name: "symlinked dir inside", appPath: app, file: "dirlink/values.yaml", want: "values.yaml"},
		{name: "https default", appPath: app, file: "https://example.com/values.yaml", schemes: helm.DefaultValuesFileSchemes, want: "https://example.com/values.yaml", wantRemote: true},
		{name: "custom scheme case-insensitive", appPath: app, file: "S3://bucket/values.yaml", schemes: []string{"s3"}, want: "S3://bucket/values.yaml", wantRemote: true},
		{name: "scheme not allowed", appPath: app, file: "ftp://example.com/values.yaml", schemes: helm.DefaultValuesFileSchemes, wantErr: helm.ErrURLSchemeNotAllowed, errText: "the URL scheme 'ftp' is not allowed"},
		{name: "empty allow-list", appPath: app, file: "https://example.com/v.yaml", schemes: nil, wantErr: helm.ErrURLSchemeNotAllowed},
		{name: "traversal", appPath: app, file: "../../../secret.yaml", wantErr: helm.ErrPathOutsideRoot, errText: "file '../../../secret.yaml' resolved to outside repository root"},
		{name: "absolute traversal", appPath: app, file: "/../secret.yaml", wantErr: helm.ErrPathOutsideRoot},
		{name: "absolute symlink out", appPath: app, file: "outlink.yaml", wantErr: helm.ErrPathOutsideRoot},
		{name: "relative symlink out", appPath: app, file: "rel-out.yaml", wantErr: helm.ErrPathOutsideRoot},
		{name: "symlink loop", appPath: app, file: "loop", wantErr: helm.ErrInvalidValueFile, errText: "internal error: failed to resolve path. Check logs for more details"},
		{name: "repo root itself", appPath: app, file: "../..", wantErr: helm.ErrInvalidValueFile},
		{name: "app outside root", appPath: filepath.Dir(root), file: "values.yaml", wantErr: helm.ErrInvalidValueFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, remote, err := helm.ResolveValueFilePathOrURL(tt.appPath, root, tt.file, tt.schemes)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("got %q, %v; want %v", got, err, tt.wantErr)
				}
				if tt.errText != "" && err.Error() != tt.errText {
					t.Fatalf("error text %q, want %q", err.Error(), tt.errText)
				}
				mustNotContain(t, err.Error(), root)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if remote != tt.wantRemote {
				t.Fatalf("remote = %v", remote)
			}
			want := tt.want
			if !tt.wantRemote {
				want = filepath.Join(root, tt.want)
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
	if got, err := helm.ResolveFileOrDirectoryPath(app, root, "../.."); err != nil || got != root {
		t.Fatalf("ResolveFileOrDirectoryPath(root) = %q, %v", got, err)
	}
}

// TestResolveValueFileConfinement checks with random relative paths over a tree
// with inside and outside symlinks that every successful resolution is a
// symlink-free path strictly inside the root.
func TestResolveValueFileConfinement(t *testing.T) {
	t.Parallel()
	root, app, _ := repoTree(t)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	comps := []string{"..", ".", "values.yaml", "envs", "prod.yaml", "inlink.yaml", "outlink.yaml", "rel-out.yaml", "dirlink", "loop", "charts", "app", "x"}
	rapid.Check(t, func(rt *rapid.T) {
		parts := rapid.SliceOfN(rapid.SampledFrom(comps), 1, 7).Draw(rt, "parts")
		p := strings.Join(parts, "/")
		if rapid.Bool().Draw(rt, "abs") {
			p = "/" + p
		}
		got, remote, err := helm.ResolveValueFilePathOrURL(app, root, p, nil)
		if err != nil {
			return
		}
		if remote {
			rt.Fatalf("local path %q treated as remote", p)
		}
		if got == root || !strings.HasPrefix(got, root+string(filepath.Separator)) {
			rt.Fatalf("%q resolved to %q outside %q", p, got, root)
		}
		// The deepest existing prefix must really be inside the root.
		probe := got
		for {
			if _, err := os.Lstat(probe); err == nil {
				break
			}
			probe = filepath.Dir(probe)
		}
		real, err := filepath.EvalSymlinks(probe)
		if err != nil {
			rt.Fatalf("eval %q: %v", probe, err)
		}
		if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
			rt.Fatalf("%q resolved to %q which really is %q", p, got, real)
		}
	})
}
