package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
)

// applyFlags parses args with the shared Application flags and applies them to spec.
func applyFlags(t *testing.T, spec *v1alpha1.ApplicationSpec, position int, args ...string) (int, error) {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	o := &appOptions{}
	addAppFlags(fs, o)
	require.NoError(t, fs.Parse(args))
	m := &specMapper{ctx: context.Background(), e: defaultTestEnv(), fs: fs, opts: o}
	return m.apply(spec, position)
}

func defaultTestEnv() *env {
	e := newEnv(nil)
	e.getenv = func(string) string { return "" }
	return e
}

func src(repo, path string) *v1alpha1.ApplicationSource {
	return &v1alpha1.ApplicationSource{RepoURL: repo, Path: path}
}

func TestSpecMapperSourceFlags(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valuesFile := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesFile, []byte("replicaCount: 2\nimage:\n  tag: v1\n"), 0o600))
	badValues := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(badValues, []byte("- a\n- b\n"), 0o600))

	tests := []struct {
		name string
		in   *v1alpha1.ApplicationSource
		args []string
		want *v1alpha1.ApplicationSource
		err  string
	}{
		{
			name: "core source fields",
			args: []string{"--repo", "https://r", "--path", "p", "--revision", "main", "--tag-prefix", "v", "--helm-chart", "c", "--ref", "values", "--source-name", "one"},
			want: &v1alpha1.ApplicationSource{RepoURL: "https://r", Path: "p", TargetRevision: "main", TagPrefix: "v", Chart: "c", Ref: "values", Name: "one"},
		},
		{
			name: "helm family",
			in:   src("https://r", "p"),
			args: []string{
				"--values", "a.yaml", "--values", "b.yaml", "--ignore-missing-value-files", "--release-name", "rel", "--helm-version", "v3",
				"--helm-pass-credentials", "--helm-set", "a=1", "--helm-set", "b=2", "--helm-set-string", "c=3", "--helm-set-file", "d=f.txt",
				"--helm-skip-crds", "--helm-skip-schema-validation", "--helm-skip-tests", "--helm-namespace", "hn", "--helm-kube-version", "1.30",
				"--helm-api-versions", "v1/Service", "--helm-api-versions", "apps/v1/Deployment", "--values-literal-file", valuesFile,
			},
			want: &v1alpha1.ApplicationSource{RepoURL: "https://r", Path: "p", Helm: &v1alpha1.ApplicationSourceHelm{
				ValueFiles: []string{"a.yaml", "b.yaml"}, IgnoreMissingValueFiles: true, ReleaseName: "rel", Version: "v3", PassCredentials: true,
				Parameters:     []v1alpha1.HelmParameter{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}, {Name: "c", Value: "3", ForceString: true}},
				FileParameters: []v1alpha1.HelmFileParameter{{Name: "d", Path: "f.txt"}},
				SkipCrds:       true, SkipSchemaValidation: true, SkipTests: true, Namespace: "hn", KubeVersion: "1.30",
				APIVersions: []string{"v1/Service", "apps/v1/Deployment"},
			}},
		},
		{
			name: "helm set replaces an existing parameter",
			in:   &v1alpha1.ApplicationSource{RepoURL: "r", Helm: &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{{Name: "a", Value: "old"}}}},
			args: []string{"--helm-set", "a=new"},
			want: &v1alpha1.ApplicationSource{RepoURL: "r", Helm: &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{{Name: "a", Value: "new"}}}},
		},
		{name: "helm set without value", args: []string{"--helm-set", "novalue"}, err: "expected helm parameter of the form: param=value"},
		{name: "helm set-file without path", args: []string{"--helm-set-file", "x"}, err: "expected helm file parameter of the form: param=path"},
		{name: "values literal must be an object", args: []string{"--values-literal-file", badValues}, err: "invalid type"},
		{name: "values literal file missing", args: []string{"--values-literal-file", filepath.Join(dir, "missing.yaml")}, err: "no such file"},
		{
			name: "kustomize family",
			in:   src("r", "p"),
			args: []string{
				"--nameprefix", "pre-", "--namesuffix", "-suf", "--kustomize-version", "v5", "--kustomize-namespace", "kn",
				"--kustomize-image", "nginx:1.25", "--kustomize-image", "redis=redis:7", "--kustomize-replica", "web=3",
				"--kustomize-common-label", "team=a", "--kustomize-common-annotation", "owner=b", "--kustomize-label-without-selector",
				"--kustomize-label-include-templates", "--kustomize-force-common-label", "--kustomize-force-common-annotation",
				"--kustomize-kube-version", "1.29", "--kustomize-api-versions", "v1/Pod", "--ignore-missing-components",
			},
			want: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "p", Kustomize: &v1alpha1.ApplicationSourceKustomize{
				NamePrefix: "pre-", NameSuffix: "-suf", Version: "v5", Namespace: "kn",
				Images:               v1alpha1.KustomizeImages{"nginx:1.25", "redis=redis:7"},
				Replicas:             v1alpha1.KustomizeReplicas{{Name: "web", Count: intstr.FromInt(3)}},
				CommonLabels:         map[string]string{"team": "a"},
				CommonAnnotations:    map[string]string{"owner": "b"},
				LabelWithoutSelector: true, LabelIncludeTemplates: true, ForceCommonLabels: true, ForceCommonAnnotations: true,
				KubeVersion: "1.29", APIVersions: []string{"v1/Pod"}, IgnoreMissingComponents: true,
			}},
		},
		{
			name: "kustomize image merges by name",
			in:   &v1alpha1.ApplicationSource{RepoURL: "r", Kustomize: &v1alpha1.ApplicationSourceKustomize{Images: v1alpha1.KustomizeImages{"nginx:1.0", "busybox:1"}}},
			args: []string{"--kustomize-image", "nginx:2.0"},
			want: &v1alpha1.ApplicationSource{RepoURL: "r", Kustomize: &v1alpha1.ApplicationSourceKustomize{Images: v1alpha1.KustomizeImages{"nginx:2.0", "busybox:1"}}},
		},
		{name: "kustomize replica needs a count", args: []string{"--kustomize-replica", "web"}, err: "name=count"},
		{name: "kustomize replica count must be numeric", args: []string{"--kustomize-replica", "web=x"}, err: "expected integer"},
		{name: "common label needs key=value", args: []string{"--kustomize-common-label", "a=b=c"}, err: "labels should have key=value"},
		{
			name: "directory and jsonnet family",
			in:   src("r", "p"),
			args: []string{
				"--directory-recurse", "--directory-include", "*.yaml", "--directory-exclude", "skip/*",
				"--jsonnet-tla-str", "a=1", "--jsonnet-tla-code", "b=2+2", "--jsonnet-ext-var-str", "c=x", "--jsonnet-ext-var-code", "d=true",
				"--jsonnet-libs", "vendor", "--jsonnet-libs", "lib",
			},
			want: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "p", Directory: &v1alpha1.ApplicationSourceDirectory{
				Recurse: true, Include: "*.yaml", Exclude: "skip/*",
				Jsonnet: v1alpha1.ApplicationSourceJsonnet{
					// Flags apply in lexical flag-name order (pflag Visit, as upstream): code before str.
					TLAs:    []v1alpha1.JsonnetVar{{Name: "b", Value: "2+2", Code: true}, {Name: "a", Value: "1"}},
					ExtVars: []v1alpha1.JsonnetVar{{Name: "d", Value: "true", Code: true}, {Name: "c", Value: "x"}},
					Libs:    []string{"vendor", "lib"},
				},
			}},
		},
		{
			name: "plugin family",
			in:   src("r", "p"),
			args: []string{"--config-management-plugin", "kasane", "--plugin-env", "FOO=bar", "--plugin-env", "FOO=baz", "--plugin-env", "X=1"},
			want: &v1alpha1.ApplicationSource{RepoURL: "r", Path: "p", Plugin: &v1alpha1.ApplicationSourcePlugin{
				Name: "kasane", Env: v1alpha1.Env{{Name: "FOO", Value: "baz"}, {Name: "X", Value: "1"}},
			}},
		},
		{name: "plugin env needs a value", args: []string{"--plugin-env", "FOO"}, err: "expected env entry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := &v1alpha1.ApplicationSpec{Source: tc.in}
			if spec.Source != nil {
				spec.Source = spec.Source.DeepCopy()
			}
			_, err := applyFlags(t, spec, 0, tc.args...)
			if tc.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.err)
				return
			}
			require.NoError(t, err)
			if tc.want.Helm != nil && spec.Source.Helm != nil && spec.Source.Helm.ValuesObject != nil {
				var v map[string]any
				require.NoError(t, json.Unmarshal(spec.Source.Helm.ValuesObject.Raw, &v))
				assert.Equal(t, map[string]any{"replicaCount": float64(2), "image": map[string]any{"tag": "v1"}}, v)
				spec.Source.Helm.ValuesObject = nil
			}
			assert.Equal(t, tc.want, spec.Source)
		})
	}
}

func TestSpecMapperSpecFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   v1alpha1.ApplicationSpec
		args []string
		want v1alpha1.ApplicationSpec
		err  string
		code int
	}{
		{
			name: "destination, project and history",
			args: []string{"--dest-server", "https://k", "--dest-name", "in-cluster", "--dest-namespace", "ns", "--project", "p", "--revision-history-limit", "3"},
			want: v1alpha1.ApplicationSpec{Destination: v1alpha1.ApplicationDestination{Server: "https://k", Name: "in-cluster", Namespace: "ns"},
				Project: "p", RevisionHistoryLimit: ptr.To(int64(3))},
		},
		{
			name: "sync policy automated resets the automated block",
			in:   v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{Prune: ptr.To(true)}}},
			args: []string{"--sync-policy", "auto"},
			want: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}},
		},
		{
			name: "automated with prune and self-heal",
			args: []string{"--sync-policy", "automated", "--auto-prune", "--self-heal", "--allow-empty"},
			want: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{
				Prune: ptr.To(true), SelfHeal: ptr.To(true), AllowEmpty: ptr.To(true)}}},
		},
		{
			name: "self-heal on a manual app keeps auto-sync disabled",
			args: []string{"--self-heal"},
			want: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{
				Enabled: ptr.To(false), SelfHeal: ptr.To(true)}}},
		},
		{
			name: "manual removes automation and normalizes an empty policy",
			in:   v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Automated: &v1alpha1.SyncPolicyAutomated{}}},
			args: []string{"--sync-policy", "none"},
			want: v1alpha1.ApplicationSpec{},
		},
		{name: "invalid sync policy", args: []string{"--sync-policy", "sometimes"}, err: "Invalid sync-policy: sometimes", code: 1},
		{
			name: "sync options add and remove",
			in:   v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"Prune=false"}}},
			args: []string{"--sync-option", "CreateNamespace=true", "--sync-option", "!Prune=false", "--sync-option", "CreateNamespace=true"},
			want: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"CreateNamespace=true"}}},
		},
		{
			name: "removing the last option normalizes the policy",
			in:   v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{SyncOptions: v1alpha1.SyncOptions{"Prune=false"}}},
			args: []string{"--sync-option", "!Prune=false"},
			want: v1alpha1.ApplicationSpec{},
		},
		{
			name: "retry with defaults",
			args: []string{"--sync-retry-limit", "4", "--sync-retry-refresh"},
			want: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Retry: &v1alpha1.RetryStrategy{
				Limit: 4, Refresh: true, Backoff: &v1alpha1.Backoff{Duration: "5s", MaxDuration: "3m0s", Factor: ptr.To(int64(2))}}}},
		},
		{
			name: "retry with custom backoff",
			args: []string{"--sync-retry-limit", "2", "--sync-retry-backoff-duration", "10s", "--sync-retry-backoff-max-duration", "1m", "--sync-retry-backoff-factor", "3"},
			want: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Retry: &v1alpha1.RetryStrategy{
				Limit: 2, Backoff: &v1alpha1.Backoff{Duration: "10s", MaxDuration: "1m0s", Factor: ptr.To(int64(3))}}}},
		},
		{
			name: "retry limit zero removes retry",
			in:   v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Retry: &v1alpha1.RetryStrategy{Limit: 3}}},
			args: []string{"--sync-retry-limit", "0"},
			want: v1alpha1.ApplicationSpec{},
		},
		{name: "negative retry limit", args: []string{"--sync-retry-limit", "-1"}, err: "Invalid sync-retry-limit [-1]", code: 1},
		{
			name: "retry refresh alone",
			args: []string{"--sync-retry-refresh"},
			want: v1alpha1.ApplicationSpec{SyncPolicy: &v1alpha1.SyncPolicy{Retry: &v1alpha1.RetryStrategy{Refresh: true}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := tc.in.DeepCopy()
			_, err := applyFlags(t, spec, 0, tc.args...)
			if tc.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.err)
				var coded interface{ ExitCode() int }
				require.True(t, errors.As(err, &coded))
				assert.Equal(t, tc.code, coded.ExitCode())
				return
			}
			require.NoError(t, err)
			// The source flags were not used: no source is created.
			spec.Source = nil
			assert.Equal(t, tc.want, *spec)
		})
	}
}

func TestSpecMapperSourcePositions(t *testing.T) {
	t.Parallel()
	multi := func() *v1alpha1.ApplicationSpec {
		return &v1alpha1.ApplicationSpec{Sources: v1alpha1.ApplicationSources{*src("a", "pa"), *src("b", "pb")}}
	}
	spec := multi()
	_, err := applyFlags(t, spec, 2, "--path", "changed")
	require.NoError(t, err)
	assert.Equal(t, "pa", spec.Sources[0].Path)
	assert.Equal(t, "changed", spec.Sources[1].Path)

	spec = multi()
	_, err = applyFlags(t, spec, -1, "--repo", "c", "--path", "pc")
	require.NoError(t, err)
	require.Len(t, spec.Sources, 3, "a negative position appends a source")
	assert.Equal(t, *src("c", "pc"), spec.Sources[2])

	spec = &v1alpha1.ApplicationSpec{Source: src("a", "p")}
	n, err := applyFlags(t, spec, 0, "--dest-namespace", "x")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, src("a", "p"), spec.Source)
}

func TestSpecMapperHydratorFlags(t *testing.T) {
	t.Parallel()
	hydratorArgs := []string{"--dry-source-repo", "https://g", "--dry-source-revision", "HEAD", "--dry-source-path", "app",
		"--sync-source-branch", "env/dev", "--sync-source-path", "out", "--hydrate-to-branch", "env/dev-next"}
	spec := &v1alpha1.ApplicationSpec{}
	_, err := applyFlags(t, spec, 0, append(hydratorArgs, "--helm-set", "image.tag=v2")...)
	require.NoError(t, err)
	assert.Nil(t, spec.Source, "a hydrator Application has no plain source")
	assert.Equal(t, &v1alpha1.SourceHydrator{
		DrySource: v1alpha1.DrySource{RepoURL: "https://g", TargetRevision: "HEAD", Path: "app",
			Helm: &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{{Name: "image.tag", Value: "v2"}}}},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env/dev", Path: "out"},
		HydrateTo:  &v1alpha1.HydrateTo{TargetBranch: "env/dev-next"},
	}, spec.SourceHydrator, "tool options given with the hydrator flags go to the dry source")

	// Location flags next to hydrator flags fail instead of being dropped.
	_, err = applyFlags(t, &v1alpha1.ApplicationSpec{}, 0, append(hydratorArgs, "--repo", "https://other")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--repo cannot be used with an application that uses a source hydrator")
	code, _ := cmdutil.ExitCodeOf(err)
	assert.Equal(t, 20, code)

	_, err = applyFlags(t, spec, 0, "--hydrate-to-branch", "")
	require.NoError(t, err)
	assert.Nil(t, spec.SourceHydrator.HydrateTo, `--hydrate-to-branch "" removes hydrateTo`)

	empty := &v1alpha1.ApplicationSpec{}
	_, err = applyFlags(t, empty, 0, "--hydrate-to-branch", "")
	require.NoError(t, err)
	assert.Nil(t, empty.SourceHydrator, "an empty value does not create a hydrator")

	// Without a hydrator the source flags still edit the plain source.
	plain := &v1alpha1.ApplicationSpec{Source: src("a", "p")}
	_, err = applyFlags(t, plain, -1, "--hydrate-to-branch", "", "--path", "q")
	require.NoError(t, err)
	assert.Nil(t, plain.SourceHydrator)
	assert.Equal(t, src("a", "q"), plain.Source)
}

// TestSpecMapperHydratorSourceOptions covers source flags on an Application that already uses
// a source hydrator (app set / app create -f): every flag takes effect on the dry source or
// fails; none is dropped while the command reports success.
func TestSpecMapperHydratorSourceOptions(t *testing.T) {
	t.Parallel()
	hydrator := func() *v1alpha1.ApplicationSpec {
		return &v1alpha1.ApplicationSpec{SourceHydrator: &v1alpha1.SourceHydrator{
			DrySource: v1alpha1.DrySource{RepoURL: "https://g", TargetRevision: "HEAD", Path: "app",
				Kustomize: &v1alpha1.ApplicationSourceKustomize{NameSuffix: "-s"}},
			SyncSource: v1alpha1.SyncSource{TargetBranch: "env/dev", Path: "out"},
		}}
	}
	withDry := func(edit func(d *v1alpha1.DrySource)) *v1alpha1.ApplicationSpec {
		spec := hydrator()
		edit(&spec.SourceHydrator.DrySource)
		return spec
	}
	tests := []struct {
		name     string
		position int
		args     []string
		want     *v1alpha1.ApplicationSpec
		err      string
	}{
		{name: "kustomize option", position: -1, args: []string{"--nameprefix", "p-"},
			want: withDry(func(d *v1alpha1.DrySource) { d.Kustomize.NamePrefix = "p-" })},
		{name: "helm options", position: 0, args: []string{"--helm-set", "a=b", "--values", "v.yaml", "--release-name", "r"},
			want: withDry(func(d *v1alpha1.DrySource) {
				d.Helm = &v1alpha1.ApplicationSourceHelm{Parameters: []v1alpha1.HelmParameter{{Name: "a", Value: "b"}}, ValueFiles: []string{"v.yaml"}, ReleaseName: "r"}
			})},
		{name: "directory and plugin options", position: -1, args: []string{"--directory-recurse", "--jsonnet-tla-str", "x=1", "--config-management-plugin", "cmp"},
			want: withDry(func(d *v1alpha1.DrySource) {
				d.Directory = &v1alpha1.ApplicationSourceDirectory{Recurse: true, Jsonnet: v1alpha1.ApplicationSourceJsonnet{TLAs: []v1alpha1.JsonnetVar{{Name: "x", Value: "1"}}}}
				d.Plugin = &v1alpha1.ApplicationSourcePlugin{Name: "cmp"}
			})},
		{name: "spec flags only at a source position", position: 1, args: []string{"--dest-namespace", "x"},
			want: func() *v1alpha1.ApplicationSpec { s := hydrator(); s.Destination.Namespace = "x"; return s }()},
		{name: "path with a tool option", position: -1, args: []string{"--path", "new-path", "--helm-set", "a=b"},
			err: "--path cannot be used with an application that uses a source hydrator"},
		{name: "every location flag", position: -1,
			args: []string{"--repo", "r", "--path", "p", "--revision", "v", "--helm-chart", "c", "--tag-prefix", "t", "--ref", "f", "--source-name", "n"},
			err:  "--repo, --path, --revision, --helm-chart, --tag-prefix, --ref, --source-name cannot be used"},
		{name: "tool option for the sync source", position: 1, args: []string{"--helm-set", "a=b"},
			err: "has no source at position 1: --helm-set applies to the dry source"},
		{name: "invalid tool option", position: -1, args: []string{"--helm-set", "novalue"},
			err: "novalue"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := hydrator()
			_, err := applyFlags(t, spec, tc.position, tc.args...)
			if tc.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.err)
				return
			}
			require.NoError(t, err)
			assert.Nil(t, spec.Source)
			assert.Equal(t, tc.want, spec)
		})
	}

	// The edit is detached: a failing flag leaves the hydrator untouched.
	spec := hydrator()
	_, err := applyFlags(t, spec, -1, "--nameprefix", "p-", "--plugin-env", "bad")
	require.Error(t, err)
	assert.Equal(t, hydrator(), spec)
}

func TestSetParameterOverrides(t *testing.T) {
	t.Parallel()
	app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Source: src("r", "p")}}
	require.NoError(t, setParameterOverrides(app, []string{"image.tag=v2", "replicas=3"}, 0))
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "image.tag", Value: "v2"}, {Name: "replicas", Value: "3"}}, app.Spec.Source.Helm.Parameters)

	kust := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Source: &v1alpha1.ApplicationSource{RepoURL: "r", Kustomize: &v1alpha1.ApplicationSourceKustomize{NamePrefix: "x"}}}}
	err := setParameterOverrides(kust, []string{"a=b"}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Parameters can only be set against Helm applications")

	byStatus := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Source: src("r", "p")}, Status: v1alpha1.ApplicationStatus{SourceType: v1alpha1.ApplicationSourceTypeHelm}}
	require.Error(t, setParameterOverrides(byStatus, []string{"novalue"}, 0))
	require.NoError(t, setParameterOverrides(byStatus, []string{"a=1"}, 0))
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "a", Value: "1"}}, byStatus.Spec.Source.Helm.Parameters)

	// Source hydrator: the overrides go to the dry source. status.sourceType (Directory: the
	// hydrated manifests) does not describe the dry source and is not consulted.
	hyd := func() *v1alpha1.Application {
		return &v1alpha1.Application{
			Spec: v1alpha1.ApplicationSpec{SourceHydrator: &v1alpha1.SourceHydrator{
				DrySource:  v1alpha1.DrySource{RepoURL: "https://g", TargetRevision: "HEAD", Path: "chart"},
				SyncSource: v1alpha1.SyncSource{TargetBranch: "env/dev", Path: "out"},
			}},
			Status: v1alpha1.ApplicationStatus{SourceType: v1alpha1.ApplicationSourceTypeDirectory},
		}
	}
	h := hyd()
	require.NoError(t, setParameterOverrides(h, []string{"image.tag=v2"}, -1))
	require.NotNil(t, h.Spec.SourceHydrator.DrySource.Helm)
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "image.tag", Value: "v2"}}, h.Spec.SourceHydrator.DrySource.Helm.Parameters)
	assert.Nil(t, h.Spec.Source)
	require.NoError(t, setParameterOverrides(h, []string{"image.tag=v3", "replicas=2"}, 0))
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "image.tag", Value: "v3"}, {Name: "replicas", Value: "2"}}, h.Spec.SourceHydrator.DrySource.Helm.Parameters)

	h = hyd()
	err = setParameterOverrides(h, []string{"a=b"}, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no source at position 1")
	assert.Equal(t, hyd(), h)

	h = hyd()
	h.Spec.SourceHydrator.DrySource.Kustomize = &v1alpha1.ApplicationSourceKustomize{NamePrefix: "x"}
	err = setParameterOverrides(h, []string{"a=b"}, -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Parameters can only be set against Helm applications")
	assert.Nil(t, h.Spec.SourceHydrator.DrySource.Helm)
}

func TestUnsetSource(t *testing.T) {
	t.Parallel()
	helm := func() *v1alpha1.ApplicationSource {
		s := &v1alpha1.ApplicationSource{RepoURL: "r", Helm: &v1alpha1.ApplicationSourceHelm{
			Parameters: []v1alpha1.HelmParameter{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}},
			ValueFiles: []string{"v1.yaml", "v2.yaml"}, IgnoreMissingValueFiles: true, PassCredentials: true,
		}}
		require.NoError(t, setHelmValues(s.Helm, "x: 1"))
		return s
	}
	s := helm()
	updated, nothing := unsetSource(s, &unsetOptions{parameters: []string{"a"}, valuesFiles: []string{"v1.yaml"}, valuesLiteral: true,
		ignoreMissingValueFiles: true, passCredentials: true})
	assert.True(t, updated)
	assert.False(t, nothing)
	assert.Equal(t, []v1alpha1.HelmParameter{{Name: "b", Value: "2"}}, s.Helm.Parameters)
	assert.Equal(t, []string{"v2.yaml"}, s.Helm.ValueFiles)
	assert.Nil(t, s.Helm.ValuesObject)
	assert.False(t, s.Helm.IgnoreMissingValueFiles)
	assert.False(t, s.Helm.PassCredentials)

	_, nothing = unsetSource(helm(), &unsetOptions{namePrefix: true})
	assert.True(t, nothing, "kustomize options do not apply to a Helm source")

	k := &v1alpha1.ApplicationSource{RepoURL: "r", Kustomize: &v1alpha1.ApplicationSourceKustomize{
		NamePrefix: "p-", NameSuffix: "-s", Version: "v5", Namespace: "n", IgnoreMissingComponents: true,
		Images:   v1alpha1.KustomizeImages{"nginx:1", "redis=redis:7"},
		Replicas: v1alpha1.KustomizeReplicas{{Name: "web", Count: intstr.FromInt(2)}},
	}}
	updated, _ = unsetSource(k, &unsetOptions{namePrefix: true, nameSuffix: true, kustomizeVersion: true, kustomizeNamespace: true,
		ignoreMissingComponents: true, kustomizeImages: []string{"nginx", "redis"}, kustomizeReplicas: []string{"web"}})
	assert.True(t, updated)
	assert.True(t, kustomizeIsZero(k.Kustomize))

	p := &v1alpha1.ApplicationSource{RepoURL: "r", Plugin: &v1alpha1.ApplicationSourcePlugin{Name: "x", Env: v1alpha1.Env{{Name: "A", Value: "1"}}}}
	updated, _ = unsetSource(p, &unsetOptions{pluginEnvs: []string{"A", "MISSING"}})
	assert.True(t, updated)
	assert.Empty(t, p.Plugin.Env)

	ref := &v1alpha1.ApplicationSource{RepoURL: "r", Ref: "values"}
	updated, nothing = unsetSource(ref, &unsetOptions{ref: true})
	assert.True(t, updated)
	assert.False(t, nothing)
	assert.Empty(t, ref.Ref)
}

func TestExitCodesFollowCmdutil(t *testing.T) {
	t.Parallel()
	code, _ := cmdutil.ExitCodeOf(fatal("x"))
	assert.Equal(t, 1, code)
	code, _ = cmdutil.ExitCodeOf(fatalGeneric("x"))
	assert.Equal(t, 20, code)
	code, printed := cmdutil.ExitCodeOf(silentExit(exitDiffFound))
	assert.Equal(t, 1, code)
	assert.False(t, printed)
	code, _ = cmdutil.ExitCodeOf(withExitCode(errors.New("boom"), exitDiffError))
	assert.Equal(t, 2, code)
}
