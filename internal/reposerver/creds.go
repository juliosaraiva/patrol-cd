package reposerver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/helm"
	"github.com/juliosaraiva/patrol-cd/internal/oci"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// sourceKind is how a source is obtained.
type sourceKind int

const (
	kindGit sourceKind = iota
	kindHelm
	kindOCI
)

func (k sourceKind) String() string {
	switch k {
	case kindHelm:
		return "helm"
	case kindOCI:
		return "oci"
	default:
		return "git"
	}
}

// kindOf classifies a source as upstream does (IsOCI: oci:// repoURL; IsHelm: chart
// set; otherwise git). The repository URL is used when the source has none.
func kindOf(repo apiclient.Repository, src v1alpha1.ApplicationSource) sourceKind {
	u := src.RepoURL
	if u == "" {
		u = repo.URL
	}
	switch {
	case strings.HasPrefix(u, "oci://"):
		return kindOCI
	case src.Chart != "":
		return kindHelm
	default:
		return kindGit
	}
}

// credFingerprinter computes process-local HMAC fingerprints of credential material so
// that working copies and cached repository reads are never shared between different
// credentials (the key is random per process; fingerprints reveal nothing).
type credFingerprinter struct {
	key [32]byte
}

func newCredFingerprinter() (*credFingerprinter, error) {
	f := &credFingerprinter{}
	if _, err := rand.Read(f.key[:]); err != nil {
		return nil, err
	}
	return f, nil
}

// of returns "anon" for repositories without credentials, else a hex HMAC.
func (f *credFingerprinter) of(r apiclient.Repository) string {
	if !r.HasCredentials() {
		return "anon"
	}
	m := hmac.New(sha256.New, f.key[:])
	for _, part := range []string{
		r.Username, r.Password, r.BearerToken, r.SSHPrivateKey, r.TLSClientCertData, r.TLSClientCertKey,
	} {
		m.Write([]byte(part))
		m.Write([]byte{0})
	}
	if r.ForceHTTPBasicAuth {
		m.Write([]byte("basic"))
	}
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// trust holds non-secret trust material read from mounted ConfigMaps.
type trust struct {
	knownHostsPath string
	tlsCertsDir    string
	log            *slog.Logger

	mu         sync.Mutex
	knownHosts []byte
	khStamp    string
}

const (
	maxKnownHostsBytes = 4 << 20
	maxCertFileBytes   = 1 << 20
)

var hostFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// KnownHosts returns the current known_hosts data (re-read when the file changes).
func (t *trust) KnownHosts() []byte {
	if t.knownHostsPath == "" {
		return nil
	}
	st, err := os.Stat(t.knownHostsPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			t.log.Warn("reposerver: cannot stat SSH known hosts", slog.Any("error", err))
		}
		return nil
	}
	stamp := st.ModTime().UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(st.Size(), 10)
	t.mu.Lock()
	defer t.mu.Unlock()
	if stamp == t.khStamp {
		return t.knownHosts
	}
	data, err := readFileLimited(t.knownHostsPath, maxKnownHostsBytes)
	if err != nil {
		t.log.Warn("reposerver: cannot read SSH known hosts", slog.Any("error", err))
		return t.knownHosts
	}
	t.knownHosts, t.khStamp = data, stamp
	return data
}

// CAData returns the PEM bundle configured for host (file <dir>/<host>), or nil.
func (t *trust) CAData(host string) []byte {
	if t.tlsCertsDir == "" || host == "" || !hostFileName.MatchString(host) || strings.Contains(host, "..") {
		return nil
	}
	data, err := readFileLimited(filepath.Join(t.tlsCertsDir, host), maxCertFileBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			t.log.Warn("reposerver: cannot read TLS certificates", slog.String("host", host), slog.Any("error", err))
		}
		return nil
	}
	return data
}

// hostOf returns the host name of a repository URL (scp-like and scheme-less forms
// included).
func hostOf(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "://") {
		if ok, _ := git.IsSSHURL(s); ok {
			if at := strings.Index(s, "@"); at >= 0 {
				s = s[at+1:]
			}
			if c := strings.Index(s, ":"); c >= 0 {
				return s[:c]
			}
			return s
		}
		s = "//" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// gitCreds maps resolved access to git credentials.
func (s *Service) gitCreds(r apiclient.Repository) git.Creds {
	isSSH, _ := git.IsSSHURL(r.URL)
	if isSSH {
		if r.SSHPrivateKey == "" && !r.InsecureIgnoreHostKey && !r.Insecure {
			// Anonymous SSH still needs host key verification material.
			if kh := s.trust.KnownHosts(); len(kh) > 0 {
				return git.SSHCreds{KnownHosts: kh, Proxy: r.Proxy}
			}
			return git.NopCreds{}
		}
		return git.SSHCreds{
			PrivateKey:            r.SSHPrivateKey,
			InsecureIgnoreHostKey: r.Insecure || r.InsecureIgnoreHostKey,
			KnownHosts:            s.trust.KnownHosts(),
			Proxy:                 r.Proxy,
		}
	}
	var ca []byte
	if git.IsHTTPSURL(r.URL) {
		ca = s.trust.CAData(hostOf(r.URL))
	}
	if !r.HasCredentials() && len(ca) == 0 && !r.Insecure {
		return git.NopCreds{}
	}
	return git.HTTPSCreds{
		Username:       r.Username,
		Password:       r.Password,
		BearerToken:    r.BearerToken,
		ClientCertData: r.TLSClientCertData,
		ClientCertKey:  r.TLSClientCertKey,
		CAData:         ca,
		Insecure:       r.Insecure,
		ForceBasicAuth: r.ForceHTTPBasicAuth,
	}
}

// remoteBaseCreds returns the credentials offered to Kustomize remote-base git fetches
// of a source: the repository's own git credentials for git sources, none for Helm and
// OCI sources (registry credentials must never reach git hosts named by untrusted
// artifact content).
func (s *Service) remoteBaseCreds(kind sourceKind, r apiclient.Repository) git.Creds {
	if kind != kindGit {
		return nil
	}
	return s.gitCreds(r)
}

// helmCreds maps resolved access to Helm repository credentials.
func (s *Service) helmCreds(r apiclient.Repository, passCredentials bool) helm.Creds {
	c := helm.Creds{
		Username:           r.Username,
		Password:           r.Password,
		InsecureSkipVerify: r.Insecure,
		PassCredentials:    passCredentials,
		PlainHTTP:          r.InsecureOCIForceHTTP,
	}
	if c.Password == "" && r.BearerToken != "" {
		c.Password = r.BearerToken
	}
	if r.TLSClientCertData != "" && r.TLSClientCertKey != "" {
		c.CertData = []byte(r.TLSClientCertData)
		c.KeyData = []byte(r.TLSClientCertKey)
	}
	c.CAData = s.trust.CAData(hostOf(oci.TrimScheme(r.URL)))
	return c
}

// ociClient returns an OCI client for one repository's credentials.
func (s *Service) ociClient(r apiclient.Repository) (*oci.Client, error) {
	opts := oci.Options{
		Username:           r.Username,
		Password:           r.Password,
		AccessToken:        r.BearerToken,
		InsecureSkipVerify: r.Insecure,
		Proxy:              r.Proxy,
		NoProxy:            r.NoProxy,
		PlainHTTP:          r.InsecureOCIForceHTTP,
		UserAgent:          s.cfg.HelmUserAgent,
		CacheDir:           s.ociCacheDir,
	}
	if r.TLSClientCertData != "" && r.TLSClientCertKey != "" {
		opts.CertData = []byte(r.TLSClientCertData)
		opts.KeyData = []byte(r.TLSClientCertKey)
	}
	opts.CAData = s.trust.CAData(hostOf(oci.TrimScheme(r.URL)))
	return oci.NewClient(opts)
}
