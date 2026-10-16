package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// decodeSpec splits generate-spec output into the Patrol object and its Secret.
func decodeSpec(t *testing.T, out string) (map[string]any, *corev1.Secret) {
	t.Helper()
	objs, err := splitObjects([]byte(out))
	require.NoError(t, err, out)
	var obj map[string]any
	var sec *corev1.Secret
	for _, o := range objs {
		if o.GetKind() == "Secret" {
			sec = &corev1.Secret{}
			require.NoError(t, runtimeFromUnstructured(o, sec))
			continue
		}
		obj = o.Object
	}
	return obj, sec
}

func TestRepoGenerateSpec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, []byte("fake-ssh-key-for-repo-test\n"), 0o600))
	appKey := filepath.Join(dir, "app.pem")
	require.NoError(t, os.WriteFile(appKey, []byte("fake-github-app-key-for-repo-test"), 0o600))
	certPath := filepath.Join(dir, "client.crt")
	require.NoError(t, os.WriteFile(certPath, []byte("fake-client-cert"), 0o600))
	certKeyPath := filepath.Join(dir, "client.key")
	require.NoError(t, os.WriteFile(certKeyPath, []byte("fake-client-cert-key"), 0o600))

	for _, tc := range []struct {
		name       string
		args       []string
		stdin      string
		secret     bool
		shown      map[string]string // stringData printed on stdout
		hidden     []string          // values that must not be printed
		checkSpec  func(t *testing.T, spec map[string]any)
		wantPrompt bool
	}{
		{
			name: "https basic auth", args: []string{"https://github.com/example/apps.git", "--username", "deploy", "--password", "fake-repo-test-password", "--project", "team-a"},
			secret: true, shown: map[string]string{"username": "deploy", "password": redactedValue}, hidden: []string{"fake-repo-test-password"},
			checkSpec: func(t *testing.T, spec map[string]any) {
				assert.Equal(t, "team-a", spec["project"])
				assert.Equal(t, "git", spec["type"])
			},
		},
		{
			name: "password prompt", args: []string{"https://github.com/example/apps.git", "--username", "deploy"}, stdin: "fake-prompted-password\n",
			secret: true, shown: map[string]string{"username": "deploy", "password": redactedValue}, hidden: []string{"fake-prompted-password"}, wantPrompt: true,
		},
		{
			name: "ssh key", args: []string{"git@github.com:example/private.git", "--ssh-private-key-path", keyPath, "--insecure-ignore-host-key"},
			secret: true, shown: map[string]string{"sshPrivateKey": redactedValue}, hidden: []string{"fake-ssh-key-for-repo-test"},
			checkSpec: func(t *testing.T, spec map[string]any) { assert.Equal(t, true, spec["insecureIgnoreHostKey"]) },
		},
		{
			name: "tls client certificate", args: []string{"https://git.example.com/repo.git", "--tls-client-cert-path", certPath, "--tls-client-cert-key-path", certKeyPath},
			secret: true, shown: map[string]string{"tlsClientCertData": "fake-client-cert", "tlsClientCertKey": redactedValue}, hidden: []string{"fake-client-cert-key"},
		},
		{
			name: "github app", args: []string{"https://github.com/example/apps.git", "--github-app-id", "42", "--github-app-installation-id", "7", "--github-app-private-key-path", appKey},
			secret: true, shown: map[string]string{"githubAppPrivateKey": redactedValue}, hidden: []string{"fake-github-app-key-for-repo-test"},
			checkSpec: func(t *testing.T, spec map[string]any) {
				app := spec["githubApp"].(map[string]any)
				assert.EqualValues(t, 42, app["appID"])
				assert.EqualValues(t, 7, app["installationID"])
			},
		},
		{
			name: "bearer token", args: []string{"https://bitbucket.example.com/scm/x/apps.git", "--bearer-token", "fake-bearer-for-repo-test"},
			secret: true, shown: map[string]string{"bearerToken": redactedValue}, hidden: []string{"fake-bearer-for-repo-test"},
		},
		{
			name: "helm without credentials", args: []string{"https://charts.example.com", "--type", "helm", "--name", "charts", "--enable-oci"},
			checkSpec: func(t *testing.T, spec map[string]any) {
				assert.Equal(t, "helm", spec["type"])
				assert.Equal(t, "charts", spec["name"])
				assert.Equal(t, true, spec["enableOCI"])
				assert.NotContains(t, spec, "credentialsRef")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tio := newTestIO("default")
			tio.f.RESTConfig = nil
			tio.f.In = strings.NewReader(tc.stdin)
			require.NoError(t, tio.run(t.Context(), append([]string{"admin", "repo", "generate-spec"}, tc.args...)...))
			out, errOut := tio.out.String(), tio.errOut.String()
			for _, h := range tc.hidden {
				assert.NotContains(t, out+errOut, h)
			}
			obj, sec := decodeSpec(t, out)
			require.NotNil(t, obj)
			assert.Equal(t, "Repository", obj["kind"])
			assert.Equal(t, v1alpha1.GroupVersion.String(), obj["apiVersion"])
			name := obj["metadata"].(map[string]any)["name"].(string)
			assert.True(t, strings.HasPrefix(name, "repo-"), name)
			spec := obj["spec"].(map[string]any)
			assert.Equal(t, tc.args[0], spec["url"])
			if tc.checkSpec != nil {
				tc.checkSpec(t, spec)
			}
			if tc.wantPrompt {
				assert.Contains(t, errOut, "Password: ")
			}
			if !tc.secret {
				assert.Nil(t, sec)
				return
			}
			require.NotNil(t, sec)
			assert.Equal(t, name+"-creds", sec.Name)
			assert.Equal(t, map[string]any{"name": name + "-creds"}, spec["credentialsRef"])
			assert.Equal(t, v1alpha1.LabelValueSecretTypeRepository, sec.Labels[v1alpha1.LabelKeySecretType])
			assert.Equal(t, tc.shown, sec.StringData)
			assert.Contains(t, errOut, "note: credentials are redacted; use --output-file")
		})
	}
}

func TestRepoGenerateSpecOutputFileAndErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo.yaml")
	out, errOut, err := offline(t, "admin", "repo", "generate-spec", "https://github.com/example/apps.git", "--username", "deploy",
		"--password", "fake-file-password", "--output-file", path)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Equal(t, "Wrote "+path+"\n", errOut)
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	_, sec := decodeSpec(t, string(data))
	require.NotNil(t, sec)
	assert.Equal(t, "fake-file-password", sec.StringData["password"])

	// Names follow the API: same for equivalent URLs, different per project.
	assert.Equal(t, repositoryObjectName("https://github.com/example/apps.git", ""), repositoryObjectName("https://github.com/example/apps", ""))
	assert.NotEqual(t, repositoryObjectName("https://github.com/example/apps.git", ""), repositoryObjectName("https://github.com/example/apps.git", "team-a"))

	keyPath := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyPath, []byte("k"), 0o600))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"https://charts.example.com", "--type", "helm"}, "must specify --name for repos of type 'helm'"},
		{[]string{"https://github.com/example/apps.git", "--ssh-private-key-path", keyPath}, "--ssh-private-key-path is only supported for SSH repositories"},
		{[]string{"https://github.com/example/apps.git", "--tls-client-cert-path", keyPath}, "--tls-client-cert-path and --tls-client-cert-key-path must be specified together"},
		{[]string{"git@github.com:example/apps.git", "--tls-client-cert-path", keyPath, "--tls-client-cert-key-path", keyPath}, "--tls-client-cert-path is only supported for HTTPS repositories"},
		{[]string{"https://github.com/example/apps.git", "--bearer-token", "t", "--username", "u", "--password", "p"}, "only --bearer-token or --password is allowed, not both"},
		{[]string{"git@github.com:example/apps.git", "--bearer-token", "t"}, "--bearer-token is only supported for HTTPS repositories"},
		{[]string{"https://charts.example.com", "--type", "helm", "--name", "c", "--bearer-token", "t"}, "--bearer-token is only supported for Git repositories"},
		{[]string{"https://example.com/x", "--type", "svn"}, `unsupported repository type "svn"`},
	} {
		_, _, err := offline(t, append([]string{"admin", "repo", "generate-spec"}, tc.args...)...)
		require.ErrorContains(t, err, tc.want, tc.args)
	}
}
