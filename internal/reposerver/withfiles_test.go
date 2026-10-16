package reposerver

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func streamedRequest(path string) *apiclient.ManifestRequest {
	return &apiclient.ManifestRequest{
		Repo:      apiclient.Repository{URL: "https://git.example.com/repo.git"},
		AppName:   "local",
		Namespace: "default",
		Source:    v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/repo.git", Path: path},
	}
}

func TestGenerateManifestWithFilesRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestService(t)
	c := startServer(t, s)
	archive := tarGz(t, [][2]string{
		{"app/", ""},
		{"app/cm.yaml", configMap("streamed", map[string]string{"k": "v"})},
		{"app/notes.txt", "ignored"},
		{"app/link.yaml", "->cm.yaml"},
	})
	resp, err := c.GenerateManifestWithFiles(bg(), streamedRequest("app"), io.NopCloser(bytes.NewReader(archive)))
	require.NoError(t, err)
	assert.Equal(t, "streamed", resp.Revision)
	assert.Equal(t, "Directory", resp.SourceType)
	assert.Equal(t, []string{"ConfigMap/streamed", "ConfigMap/streamed"}, names(objects(t, resp)))

	// Helm chart sources render the uploaded directory, never a remote chart.
	if helmBin() != "" {
		chart := tarGz(t, [][2]string{
			{"Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\n"},
			{"templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n"},
		})
		req := streamedRequest("")
		req.Source.Chart = "remote-chart"
		resp, err := c.GenerateManifestWithFiles(bg(), req, bytes.NewReader(chart))
		require.NoError(t, err)
		assert.Equal(t, "Helm", resp.SourceType)
		assert.Equal(t, []string{"ConfigMap/local"}, names(objects(t, resp)))
	}
}

func TestGenerateManifestWithFilesRejectsTraversal(t *testing.T) {
	t.Parallel()
	s := newTestService(t)
	c := startServer(t, s)
	parent := filepath.Dir(s.root)
	for name, entries := range map[string][][2]string{
		"dot-dot":            {{"../evil.yaml", configMap("evil", nil)}},
		"nested dot-dot":     {{"app/../../evil.yaml", configMap("evil", nil)}},
		"absolute":           {{"/tmp/evil.yaml", configMap("evil", nil)}},
		"absolute symlink":   {{"app/", ""}, {"app/link", "->/etc"}},
		"escaping symlink":   {{"app/", ""}, {"app/link", "->../../../.."}},
		"write through link": {{"link", "->" + parent}, {"link/evil.yaml", configMap("evil", nil)}},
		"hard link outside":  {{"app/", ""}, {"app/h", "=>/etc/passwd"}},
		"backslash":          {{`..\evil.yaml`, configMap("evil", nil)}},
	} {
		_, err := c.GenerateManifestWithFiles(bg(), streamedRequest("app"), bytes.NewReader(tarGz(t, entries)))
		require.Error(t, err, name)
		assert.True(t, apiclient.IsCode(err, apiclient.CodeInvalidArgument), "%s: %v", name, err)
		_, statErr := os.Stat(filepath.Join(parent, "evil.yaml"))
		assert.True(t, os.IsNotExist(statErr), "%s: nothing written outside the upload", name)
		_, statErr = os.Stat("/tmp/evil.yaml")
		assert.True(t, os.IsNotExist(statErr), name)
	}
	// Symlink errors use the upstream wording.
	_, err := c.GenerateManifestWithFiles(bg(), streamedRequest("app"), bytes.NewReader(tarGz(t, [][2]string{{"app/", ""}, {"app/link", "->/etc/passwd"}})))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "streamed files contains out-of-bounds symlinks. file: app/link")
	// Upload directories are always removed.
	assert.Equal(t, 0, s.workdirs.count())
}

func TestGenerateManifestWithFilesLimits(t *testing.T) {
	t.Parallel()
	s := newTestService(t, func(c *Config) { c.StreamedManifestMaxTarSize = 2048; c.StreamedManifestMaxExtractedSize = 64 << 10 })
	c := startServer(t, s)
	random := make([]byte, 8<<10)
	_, err := rand.Read(random)
	require.NoError(t, err)
	incompressible := tarGz(t, [][2]string{{"app/blob", string(random)}})
	_, err = c.GenerateManifestWithFiles(bg(), streamedRequest("app"), bytes.NewReader(incompressible))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file exceeded max size of 2048 bytes")
	assert.True(t, apiclient.IsCode(err, apiclient.CodeResourceExhausted))

	// The service enforces the upload limit itself when called in process.
	_, err = s.GenerateManifestWithFiles(bg(), streamedRequest("app"), bytes.NewReader(incompressible))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file exceeded max size of 2048 bytes")

	// Compressible content within the upload limit but over the extraction limit.
	zeros := make([]byte, 256<<10)
	_, err = c.GenerateManifestWithFiles(bg(), streamedRequest("app"), bytes.NewReader(tarGz(t, [][2]string{{"app/zeros", string(zeros)}})))
	require.Error(t, err)
	assert.True(t, apiclient.IsCode(err, apiclient.CodeResourceExhausted), "%v", err)
	assert.Contains(t, err.Error(), "archive limit exceeded")
}

// rawFilesRequest posts a hand-built multipart body.
func rawFilesRequest(t *testing.T, url string, req *apiclient.ManifestRequest, archive []byte, checksum string) (*http.Response, apiclient.Problem) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part := func(name, ct string, data []byte) {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+name+`"`)
		h.Set("Content-Type", ct)
		w, err := mw.CreatePart(h)
		require.NoError(t, err)
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	reqJSON, err := json.Marshal(req)
	require.NoError(t, err)
	part(apiclient.PartRequest, "application/json", reqJSON)
	part(apiclient.PartFiles, "application/gzip", archive)
	part(apiclient.PartChecksum, "text/plain", []byte(checksum))
	require.NoError(t, mw.Close())
	httpReq, err := http.NewRequest(http.MethodPost, url+apiclient.PathPrefix+apiclient.MethodGenerateManifestWithFiles, &body)
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var p apiclient.Problem
	_ = json.NewDecoder(resp.Body).Decode(&p)
	return resp, p
}

func TestGenerateManifestWithFilesChecksum(t *testing.T) {
	t.Parallel()
	s := newTestService(t)
	h, err := s.Handler()
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	archive := tarGz(t, [][2]string{{"app/cm.yaml", configMap("cm", nil)}})
	sum := sha256.Sum256(archive)
	good := hex.EncodeToString(sum[:])

	resp, _ := rawFilesRequest(t, srv.URL, streamedRequest("app"), archive, good)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp, p := rawFilesRequest(t, srv.URL, streamedRequest("app"), archive, "deadbeef")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "file checksum validation error: calc "+good+" sent deadbeef", p.Detail)
	assert.Equal(t, apiclient.CodeInvalidArgument, p.Code)
}
