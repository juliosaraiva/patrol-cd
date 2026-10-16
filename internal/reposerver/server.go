package reposerver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// HandlerOptions configure the HTTP handler.
type HandlerOptions struct {
	// Token is the shared internal token. Required.
	Token string
	// MaxRequestBytes caps JSON request bodies (default DefaultMaxRequestBytes).
	MaxRequestBytes int64
	// MaxStreamedTarBytes caps the files part of GenerateManifestWithFiles (default
	// DefaultStreamedManifestMaxTarSize).
	MaxStreamedTarBytes int64
	// RequestTimeout bounds every request (default DefaultRequestTimeout).
	RequestTimeout time.Duration
	// SpoolDir holds uploads while their checksum is verified (default os.TempDir()).
	SpoolDir string
	// Logger (default slog.Default()).
	Logger *slog.Logger
	// Observe, when set, receives every request's method, error code ("" on success)
	// and duration.
	Observe func(method, code string, d time.Duration)
	// Ready reports readiness for /readyz (nil = always ready).
	Ready func() bool
}

// NewHandler serves svc over HTTP: POST /repo/v1/<Method> (bearer token required),
// GET /healthz and GET /readyz (no authentication).
func NewHandler(svc apiclient.Client, opts HandlerOptions) (http.Handler, error) {
	if svc == nil {
		return nil, errors.New("reposerver: service is required")
	}
	if opts.Token == "" {
		return nil, errors.New("reposerver: an internal token is required")
	}
	if opts.MaxRequestBytes <= 0 {
		opts.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if opts.MaxStreamedTarBytes <= 0 {
		opts.MaxStreamedTarBytes = DefaultStreamedManifestMaxTarSize
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.SpoolDir == "" {
		opts.SpoolDir = os.TempDir()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	h := &handler{svc: svc, opts: opts, tokenSum: sha256.Sum256([]byte(opts.Token)), log: opts.Logger.With(slog.String("component", "reposerver"))}
	h.methods = map[string]jsonMethod{
		apiclient.MethodGenerateManifest:        bind(svc.GenerateManifest),
		apiclient.MethodResolveRevision:         bind(svc.ResolveRevision),
		apiclient.MethodTestRepository:          bind(svc.TestRepository),
		apiclient.MethodListRefs:                bind(svc.ListRefs),
		apiclient.MethodListOCITags:             bind(svc.ListOCITags),
		apiclient.MethodListApps:                bind(svc.ListApps),
		apiclient.MethodGetAppDetails:           bind(svc.GetAppDetails),
		apiclient.MethodGetRevisionMetadata:     bind(svc.GetRevisionMetadata),
		apiclient.MethodGetRevisionChartDetails: bind(svc.GetRevisionChartDetails),
		apiclient.MethodGetOCIMetadata:          bind(svc.GetOCIMetadata),
		apiclient.MethodGetHelmCharts:           bind(svc.GetHelmCharts),
		apiclient.MethodGetGitFiles:             bind(svc.GetGitFiles),
		apiclient.MethodGetGitDirectories:       bind(svc.GetGitDirectories),
		apiclient.MethodUpdateRevisionForPaths:  bind(svc.UpdateRevisionForPaths),
		apiclient.MethodListPlugins: func(ctx context.Context, decode func(any) error) (any, error) {
			var empty struct{}
			if err := decode(&empty); err != nil {
				return nil, err
			}
			return svc.ListPlugins(ctx)
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+apiclient.PathHealthz, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET "+apiclient.PathReadyz, func(w http.ResponseWriter, _ *http.Request) {
		if opts.Ready != nil && !opts.Ready() {
			writeProblem(w, http.StatusServiceUnavailable, apiclient.CodeUnavailable, "not ready")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST "+apiclient.PathPrefix+"{method}", h.serve)
	mux.HandleFunc(apiclient.PathPrefix+"{method}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		writeProblem(w, http.StatusMethodNotAllowed, apiclient.CodeInvalidArgument, "method not allowed")
	})
	return mux, nil
}

// Handler returns the HTTP handler of s (token from the configuration, metrics
// recorded).
func (s *Service) Handler() (http.Handler, error) {
	token, err := s.cfg.readToken()
	if err != nil {
		return nil, err
	}
	return NewHandler(s, HandlerOptions{
		Token:               token,
		MaxRequestBytes:     s.cfg.MaxRequestBytes,
		MaxStreamedTarBytes: s.cfg.StreamedManifestMaxTarSize,
		RequestTimeout:      s.cfg.RequestTimeout,
		SpoolDir:            s.root,
		Logger:              s.cfg.Logger,
		Observe:             s.observe,
		Ready:               s.ready,
	})
}

func (s *Service) observe(method, code string, d time.Duration) {
	if code == "" {
		code = "ok"
	}
	s.metrics.requests.WithLabelValues(method, code).Inc()
	s.metrics.requestDuration.WithLabelValues(method).Observe(d.Seconds())
}

// ready reports whether the working directory root is usable.
func (s *Service) ready() bool {
	if s.shuttingDown.Load() {
		return false
	}
	_, err := os.Stat(s.root)
	return err == nil
}

type jsonMethod func(ctx context.Context, decode func(any) error) (any, error)

// bind adapts a typed service method.
func bind[Req, Resp any](fn func(context.Context, *Req) (*Resp, error)) jsonMethod {
	return func(ctx context.Context, decode func(any) error) (any, error) {
		var req Req
		if err := decode(&req); err != nil {
			return nil, err
		}
		return fn(ctx, &req)
	}
}

type handler struct {
	svc      apiclient.Client
	opts     HandlerOptions
	tokenSum [sha256.Size]byte
	log      *slog.Logger
	methods  map[string]jsonMethod
}

func (h *handler) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], h.tokenSum[:]) == 1
}

// errBadRequest marks request decoding failures.
type errBadRequest struct {
	status int
	code   string
	msg    string
}

func (e *errBadRequest) Error() string { return e.msg }

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	method := r.PathValue("method")
	code := ""
	defer func() {
		if h.opts.Observe != nil {
			label := method
			if _, ok := h.methods[method]; !ok && method != apiclient.MethodGenerateManifestWithFiles {
				label = "unknown"
			}
			h.opts.Observe(label, code, time.Since(start))
		}
	}()
	if !h.authorized(r) {
		code = apiclient.CodeUnauthenticated
		w.Header().Set("WWW-Authenticate", `Bearer realm="patrol-repo-server"`)
		writeProblem(w, http.StatusUnauthorized, code, "missing or invalid internal token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.opts.RequestTimeout)
	defer cancel()
	// Bound body reads (uploads) by the request timeout as well.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.opts.RequestTimeout))
	var (
		resp any
		err  error
	)
	if method == apiclient.MethodGenerateManifestWithFiles {
		resp, err = h.withFiles(ctx, w, r)
	} else {
		fn, ok := h.methods[method]
		if !ok {
			code = apiclient.CodeNotFound
			writeProblem(w, http.StatusNotFound, code, fmt.Sprintf("unknown method %q", method))
			return
		}
		if mt, _, perr := mime.ParseMediaType(r.Header.Get("Content-Type")); perr != nil || mt != "application/json" {
			code = apiclient.CodeInvalidArgument
			writeProblem(w, http.StatusUnsupportedMediaType, code, "content type must be application/json")
			return
		}
		body := http.MaxBytesReader(w, r.Body, h.opts.MaxRequestBytes)
		resp, err = fn(ctx, func(v any) error { return decodeJSON(body, v) })
	}
	if err != nil {
		var bad *errBadRequest
		status := 0
		if errors.As(err, &bad) {
			status, code = bad.status, bad.code
		} else {
			code = ErrorCode(err)
			status = apiclient.StatusForCode(code)
			if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
				code, status = apiclient.CodeDeadlineExceeded, http.StatusGatewayTimeout
			}
		}
		if status >= http.StatusInternalServerError {
			h.log.Error("reposerver: request failed", slog.String("method", method), slog.String("code", code), slog.Any("error", err))
		} else {
			h.log.Debug("reposerver: request rejected", slog.String("method", method), slog.String("code", code), slog.Any("error", err))
		}
		writeProblem(w, status, code, err.Error())
		return
	}
	data, err := json.Marshal(resp)
	if err != nil {
		code = apiclient.CodeInternal
		writeProblem(w, http.StatusInternalServerError, code, "encode response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

// decodeJSON decodes exactly one JSON value.
func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &errBadRequest{status: http.StatusRequestEntityTooLarge, code: apiclient.CodeResourceExhausted, msg: fmt.Sprintf("request body exceeds %d bytes", mbe.Limit)}
		}
		return &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "malformed request body: " + err.Error()}
	}
	if dec.More() {
		return &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "malformed request body: trailing data"}
	}
	return nil
}

// withFiles handles the multipart GenerateManifestWithFiles request: the request part,
// the files part (spooled to a private temporary file within the size limit) and the
// checksum part, verified before anything is extracted.
func (h *handler) withFiles(ctx context.Context, w http.ResponseWriter, r *http.Request) (any, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return nil, &errBadRequest{status: http.StatusUnsupportedMediaType, code: apiclient.CodeInvalidArgument, msg: "content type must be multipart/form-data"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxRequestBytes+h.opts.MaxStreamedTarBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "malformed multipart body: " + err.Error()}
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != apiclient.PartRequest {
		return nil, &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "the first part must be the request"}
	}
	var req apiclient.ManifestRequest
	if err := decodeJSON(io.LimitReader(part, h.opts.MaxRequestBytes+1), &req); err != nil {
		return nil, err
	}
	if part, err = mr.NextPart(); err != nil || part.FormName() != apiclient.PartFiles {
		return nil, &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "the second part must be the files"}
	}
	spool, err := os.CreateTemp(h.opts.SpoolDir, "t-upload-")
	if err != nil {
		return nil, fmt.Errorf("error creating temp file: %w", err)
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(spool, sum), io.LimitReader(part, h.opts.MaxStreamedTarBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &errBadRequest{status: http.StatusRequestEntityTooLarge, code: apiclient.CodeResourceExhausted, msg: fmt.Sprintf("file exceeded max size of %d bytes", h.opts.MaxStreamedTarBytes)}
		}
		return nil, &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "error receiving files: " + err.Error()}
	}
	if n > h.opts.MaxStreamedTarBytes {
		return nil, &errBadRequest{status: http.StatusRequestEntityTooLarge, code: apiclient.CodeResourceExhausted, msg: fmt.Sprintf("file exceeded max size of %d bytes", h.opts.MaxStreamedTarBytes)}
	}
	if part, err = mr.NextPart(); err != nil || part.FormName() != apiclient.PartChecksum {
		return nil, &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "the third part must be the checksum"}
	}
	sent, err := io.ReadAll(io.LimitReader(part, 256))
	if err != nil {
		return nil, &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument, msg: "error receiving checksum: " + err.Error()}
	}
	calc := hex.EncodeToString(sum.Sum(nil))
	if !bytes.Equal(bytes.TrimSpace(sent), []byte(calc)) {
		return nil, &errBadRequest{status: http.StatusBadRequest, code: apiclient.CodeInvalidArgument,
			msg: fmt.Sprintf("file checksum validation error: calc %s sent %s", calc, strings.TrimSpace(string(sent)))}
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return h.svc.GenerateManifestWithFiles(ctx, &req, io.NopCloser(spool))
}

// writeProblem writes an RFC 9457 problem details response.
func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", apiclient.ContentTypeProblem)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiclient.Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
	})
}
