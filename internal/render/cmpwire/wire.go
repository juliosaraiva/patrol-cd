// Package cmpwire is the wire protocol between the repo-server (the client in
// internal/render) and Config Management Plugin sidecars (the server in internal/cmp).
//
// Transport (ADR-0006): HTTP/1.1 over a unix socket in a directory shared by the
// repo-server and its sidecars. Each sidecar serves one plugin on
// "<socket dir>/<metadata.name>[-<spec.version>].sock"; the repo-server discovers
// plugins only by the presence of socket files. There is no other authentication than
// the socket's file permissions.
//
// Endpoints (all under APIPrefix):
//
//	GET  /plugin/v1/config      -> ConfigResponse      (upstream CheckPluginConfiguration)
//	POST /plugin/v1/match       -> MatchResponse       (upstream MatchRepository)
//	POST /plugin/v1/generate    -> ManifestResponse    (upstream GenerateManifest)
//	POST /plugin/v1/parameters  -> ParametersResponse  (upstream GetParametersAnnouncement)
//
// The three POST endpoints take a stream body (Content-Type ContentTypeStream): a
// 4-byte big-endian length N (at most MaxMetadataBytes), N bytes of JSON Metadata, then
// exactly Metadata.Size bytes of a gzip-compressed tar of the source tree whose SHA-256
// is Metadata.Checksum (upstream: one metadata message followed by file chunks). Use
// WriteMetadata / ReadMetadata and PayloadReader to produce and consume it.
//
// Successful responses are JSON (ContentTypeJSON) with status 200. Failures carry a
// non-2xx status and an Error body (ContentTypeProblem); Error.Code uses the
// repo-server codes of internal/reposerver/apiclient (CodeInvalidArgument, ...).
//
// The client's deadline travels in HeaderDeadline so the sidecar can stop its commands
// slightly before the client gives up (upstream shortens the deadline by 100 ms).
package cmpwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// Protocol constants.
const (
	// APIPrefix is the path prefix of every endpoint.
	APIPrefix = "/plugin/v1"
	// PathConfig answers ConfigResponse (GET).
	PathConfig = APIPrefix + "/config"
	// PathMatch answers MatchResponse for a streamed source tree (POST).
	PathMatch = APIPrefix + "/match"
	// PathGenerate answers ManifestResponse for a streamed source tree (POST).
	PathGenerate = APIPrefix + "/generate"
	// PathParameters answers ParametersResponse for a streamed source tree (POST).
	PathParameters = APIPrefix + "/parameters"

	// ContentTypeStream is the media type of stream request bodies.
	ContentTypeStream = "application/vnd.patrol.cmp.stream.v1"
	// ContentTypeJSON is the media type of successful responses.
	ContentTypeJSON = "application/json"
	// ContentTypeProblem is the media type of error responses.
	ContentTypeProblem = "application/problem+json"

	// HeaderDeadline carries the client deadline (RFC 3339 with nanoseconds, UTC).
	HeaderDeadline = "Patrol-Cmp-Deadline"

	// SocketSuffix is the file name suffix of plugin sockets.
	SocketSuffix = ".sock"
	// DefaultSocketDir is the socket directory shared by the repo-server and sidecars
	// (upstream /home/argocd/cmp-server/plugins).
	DefaultSocketDir = "/home/patrol/cmp-server/plugins"

	// MaxMetadataBytes bounds the JSON metadata frame (environment and parameters).
	MaxMetadataBytes = 4 << 20
	// MaxResponseBytes is the default bound clients apply to response bodies.
	MaxResponseBytes = 256 << 20
)

// Errors of the stream framing. ErrChecksumMismatch keeps the upstream message.
var (
	// ErrMetadataMissing reports a stream without (or with an empty) metadata frame.
	ErrMetadataMissing = errors.New("error getting stream metadata: metadata is nil")
	// ErrMetadataTooLarge reports a metadata frame above MaxMetadataBytes.
	ErrMetadataTooLarge = errors.New("cmpwire: metadata frame too large")
	// ErrInvalidMetadata reports metadata that fails validation.
	ErrInvalidMetadata = errors.New("cmpwire: invalid metadata")
	// ErrChecksumMismatch reports a payload whose SHA-256 differs from the metadata.
	ErrChecksumMismatch = errors.New("file checksum validation error")
	// ErrSizeMismatch reports a payload shorter or longer than Metadata.Size.
	ErrSizeMismatch = errors.New("cmpwire: payload size does not match metadata")
)

// EnvEntry is one environment variable sent to the plugin.
type EnvEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Metadata describes a streamed source tree.
type Metadata struct {
	// AppName is the base name of the application directory (upstream semantics).
	AppName string `json:"appName"`
	// AppRelPath is the application directory relative to the root of the archive,
	// slash-separated ("." for the root). It never starts with "/" or "..".
	AppRelPath string `json:"appRelPath"`
	// Checksum is the lowercase hex SHA-256 of the payload.
	Checksum string `json:"checksum"`
	// Size is the payload length in bytes.
	Size int64 `json:"size"`
	// Env is the environment for discovery, init, generate and parameter commands.
	Env []EnvEntry `json:"env,omitempty"`
}

// Validate checks the metadata invariants a server relies on.
func (m Metadata) Validate() error {
	if m.Size < 0 {
		return fmt.Errorf("%w: negative size", ErrInvalidMetadata)
	}
	if len(m.Checksum) != sha256.Size*2 {
		return fmt.Errorf("%w: checksum must be a hex SHA-256", ErrInvalidMetadata)
	}
	if _, err := hex.DecodeString(m.Checksum); err != nil {
		return fmt.Errorf("%w: checksum must be a hex SHA-256", ErrInvalidMetadata)
	}
	if !IsLocalRelPath(m.AppRelPath) {
		return fmt.Errorf("%w: illegal appPath: out of workDir bound", ErrInvalidMetadata)
	}
	for _, e := range m.Env {
		if e.Name == "" || strings.ContainsAny(e.Name, "=\x00") || strings.ContainsRune(e.Value, 0) {
			return fmt.Errorf("%w: invalid environment entry", ErrInvalidMetadata)
		}
	}
	return nil
}

// Environ returns Env as NAME=VALUE strings, in order.
func (m Metadata) Environ() []string {
	out := make([]string, 0, len(m.Env))
	for _, e := range m.Env {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}

// IsLocalRelPath reports whether p is a slash-separated relative path that stays
// inside its root ("." included): not absolute, no ".." component after cleaning, no
// backslashes or NUL bytes.
func IsLocalRelPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	c := path.Clean(p)
	return c != ".." && !strings.HasPrefix(c, "../")
}

// EnvEntries converts NAME=VALUE strings to entries, dropping strings without '='
// or with an empty name (upstream toEnvEntry).
func EnvEntries(env []string) []EnvEntry {
	out := make([]EnvEntry, 0, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		out = append(out, EnvEntry{Name: k, Value: v})
	}
	return out
}

// ConfigResponse is the plugin configuration summary.
type ConfigResponse struct {
	// IsDiscoveryConfigured is true when plugin.yaml has discovery rules.
	IsDiscoveryConfigured bool `json:"isDiscoveryConfigured"`
	// ProvideGitCreds mirrors spec.provideGitCreds. Patrol never forwards repository
	// credentials to sidecars (ADR-0007); clients only log it.
	ProvideGitCreds bool `json:"provideGitCreds"`
}

// MatchResponse is the discovery result for a streamed tree.
type MatchResponse struct {
	IsSupported        bool `json:"isSupported"`
	IsDiscoveryEnabled bool `json:"isDiscoveryEnabled"`
}

// ManifestResponse holds the generate output split into documents.
type ManifestResponse struct {
	Manifests []string `json:"manifests"`
}

// ParametersResponse holds parameter announcements (dynamic first, then static).
type ParametersResponse struct {
	ParameterAnnouncements []apiclient.ParameterAnnouncement `json:"parameterAnnouncements"`
}

// Error is the body of non-2xx responses.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// WriteMetadata writes the metadata frame (length prefix and JSON).
func WriteMetadata(w io.Writer, m Metadata) error {
	frame, err := EncodeMetadata(m)
	if err != nil {
		return err
	}
	_, err = w.Write(frame)
	return err
}

// EncodeMetadata returns the metadata frame (length prefix and JSON).
func EncodeMetadata(m Metadata) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("cmpwire: encode metadata: %w", err)
	}
	if len(data) > MaxMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	var buf bytes.Buffer
	buf.Grow(4 + len(data))
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	buf.Write(n[:])
	buf.Write(data)
	return buf.Bytes(), nil
}

// ReadMetadata reads and validates the metadata frame.
func ReadMetadata(r io.Reader) (Metadata, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Metadata{}, ErrMetadataMissing
		}
		return Metadata{}, fmt.Errorf("cmpwire: read metadata length: %w", err)
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 {
		return Metadata{}, ErrMetadataMissing
	}
	if size > MaxMetadataBytes {
		return Metadata{}, ErrMetadataTooLarge
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return Metadata{}, fmt.Errorf("cmpwire: read metadata: %w", err)
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Metadata{}, fmt.Errorf("%w: %w", ErrInvalidMetadata, err)
	}
	if err := m.Validate(); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

// PayloadReader returns a reader of exactly m.Size bytes of r. When those bytes have
// been read it returns io.EOF only if their SHA-256 equals m.Checksum and r holds no
// further data; otherwise ErrChecksumMismatch or ErrSizeMismatch. Consumers must read
// it to EOF (for example by extracting the archive to completion and then calling
// io.Copy(io.Discard, ...)) before trusting the content.
func PayloadReader(r io.Reader, m Metadata) io.Reader {
	return &payloadReader{r: r, remaining: m.Size, want: m.Checksum, h: sha256.New()}
}

type payloadReader struct {
	r         io.Reader
	remaining int64
	want      string
	h         hash.Hash
	err       error
}

func (p *payloadReader) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	if p.remaining == 0 {
		p.err = p.finish()
		return 0, p.err
	}
	if int64(len(b)) > p.remaining {
		b = b[:p.remaining]
	}
	n, err := p.r.Read(b)
	p.h.Write(b[:n])
	p.remaining -= int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			if p.remaining > 0 {
				p.err = ErrSizeMismatch
				return n, p.err
			}
			p.err = p.finish()
			return n, p.err
		}
		p.err = err
		return n, err
	}
	return n, nil
}

func (p *payloadReader) finish() error {
	var one [1]byte
	if n, _ := io.ReadFull(p.r, one[:]); n > 0 {
		return ErrSizeMismatch
	}
	if hex.EncodeToString(p.h.Sum(nil)) != p.want {
		return ErrChecksumMismatch
	}
	return io.EOF
}
