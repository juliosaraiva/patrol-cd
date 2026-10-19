package cmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juliosaraiva/patrol-cd/internal/render/cmpwire"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/sandbox"
)

// Defaults applied when the corresponding Config field is zero.
const (
	// DefaultExecTimeout bounds each plugin command (upstream ARGOCD_EXEC_TIMEOUT).
	DefaultExecTimeout = sandbox.DefaultTimeout
	// DefaultKillGrace is the delay between SIGTERM and SIGKILL of a command's process
	// group on timeout or cancellation (upstream: 5 s).
	DefaultKillGrace = 5 * time.Second
	// DefaultMaxArchiveBytes bounds the compressed source archive of one request.
	DefaultMaxArchiveBytes int64 = 1 << 30
	// DefaultMaxStdout bounds a command's standard output (the generate response).
	DefaultMaxStdout int64 = cmpwire.MaxResponseBytes
	// DefaultShutdownTimeout is how long Serve waits for in-flight requests before
	// cancelling them.
	DefaultShutdownTimeout = 20 * time.Second
	// DefaultReceiveTimeout bounds the reception of one request stream (metadata and
	// archive) when the client sends no earlier deadline.
	DefaultReceiveTimeout = 5 * time.Minute
	// DefaultSocketMode is the permission of the plugin socket: the socket's file
	// permissions are the only authentication (ADR-0006), so only the owner (the UID
	// shared by the repo-server and its sidecars) may connect.
	DefaultSocketMode os.FileMode = 0o600

	// DeadlineBuffer is subtracted from the client deadline so the server can still
	// answer with a meaningful error (upstream cmpTimeoutBuffer).
	DeadlineBuffer = 100 * time.Millisecond
	// WorkDirName is the directory created (and wiped at startup) under Config.WorkDir
	// (upstream _cmp_server).
	WorkDirName = "_cmp_server"
)

// Config configures a Server.
type Config struct {
	// Plugin is the validated plugin configuration. Required.
	Plugin *PluginConfig
	// SocketDir is the directory of the plugin socket. Empty means
	// cmpwire.DefaultSocketDir.
	SocketDir string
	// SocketMode is the socket file permission. Zero means DefaultSocketMode.
	SocketMode os.FileMode
	// WorkDir is the absolute parent of the server work directory
	// "<WorkDir>/_cmp_server", which New removes and recreates (0700). Empty means
	// os.TempDir().
	WorkDir string
	// Exec bounds every command. A zero Timeout means DefaultExecTimeout, a zero
	// MaxStdout DefaultMaxStdout; rlimits require sandbox.MaybeHelperMain in the
	// binary. KillGrace is taken from the KillGrace field below.
	Exec sandbox.Limits
	// KillGrace is the SIGTERM-to-SIGKILL delay. Zero means DefaultKillGrace; a
	// negative value kills immediately.
	KillGrace time.Duration
	// PassEnv lists the server-process environment variables passed to plugin
	// commands in addition to PATH, HOME, TMPDIR and LANG: exact names, "PREFIX*"
	// patterns, or "*" for all (upstream behavior). Nothing else leaks into plugins.
	PassEnv []string
	// Environ is the server-process environment PassEnv selects from. Nil means
	// os.Environ() at New.
	Environ []string
	// MaxArchiveBytes bounds the compressed archive of one request. Zero means
	// DefaultMaxArchiveBytes; negative disables the limit.
	MaxArchiveBytes int64
	// Archive bounds extraction (entries, extracted bytes per file and in total).
	// StripExecBits is derived from spec.preserveFileMode and ignored here.
	Archive sandbox.ArchiveLimits
	// ReceiveTimeout bounds the reception of one request stream, counted from the
	// start of the request; the client deadline (minus DeadlineBuffer) applies when
	// earlier. Zero means DefaultReceiveTimeout; negative leaves only the client
	// deadline.
	ReceiveTimeout time.Duration
	// ShutdownTimeout bounds the graceful shutdown of Serve. Zero means
	// DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// Registerer receives the server metrics. Nil disables metrics.
	Registerer prometheus.Registerer
	// Logger receives structured logs. Nil means slog.Default().
	Logger *slog.Logger
}

// Server serves one plugin over cmpwire. It is safe for concurrent use.
type Server struct {
	cfg       Config
	plugin    *PluginConfig
	socketDir string
	workDir   string
	extract   sandbox.ArchiveLimits
	runner    *runner
	log       *slog.Logger
	metrics   *metrics
	handler   http.Handler
	active    sync.WaitGroup
}

// New validates cfg, removes and recreates the work directory, and returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Plugin == nil {
		return nil, errors.New("cmp: plugin configuration is required")
	}
	if err := cfg.Plugin.Validate(); err != nil {
		return nil, err
	}
	if err := validatePassEnv(cfg.PassEnv); err != nil {
		return nil, err
	}
	if cfg.SocketDir == "" {
		cfg.SocketDir = cmpwire.DefaultSocketDir
	}
	if cfg.SocketMode == 0 {
		cfg.SocketMode = DefaultSocketMode
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = os.TempDir()
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		return nil, fmt.Errorf("cmp: work directory %q must be absolute", cfg.WorkDir)
	}
	if cfg.Exec.Timeout == 0 {
		cfg.Exec.Timeout = DefaultExecTimeout
	}
	if cfg.Exec.MaxStdout == 0 {
		cfg.Exec.MaxStdout = DefaultMaxStdout
	}
	switch {
	case cfg.KillGrace == 0:
		cfg.Exec.KillGrace = DefaultKillGrace
	case cfg.KillGrace < 0:
		cfg.Exec.KillGrace = 0
	default:
		cfg.Exec.KillGrace = cfg.KillGrace
	}
	if cfg.MaxArchiveBytes == 0 {
		cfg.MaxArchiveBytes = DefaultMaxArchiveBytes
	}
	if cfg.ReceiveTimeout == 0 {
		cfg.ReceiveTimeout = DefaultReceiveTimeout
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if cfg.Environ == nil {
		cfg.Environ = os.Environ()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "cmp-server", "plugin", cfg.Plugin.PluginName())
	m, err := newMetrics(cfg.Registerer, cfg.Plugin.PluginName())
	if err != nil {
		return nil, err
	}
	workDir, err := initWorkDir(filepath.Join(cfg.WorkDir, WorkDirName))
	if err != nil {
		return nil, err
	}
	pluginEnv := pluginEnvironment(cfg.Environ, cfg.PassEnv)
	extract := cfg.Archive
	extract.StripExecBits = !cfg.Plugin.Spec.PreserveFileMode
	s := &Server{
		cfg:       cfg,
		plugin:    cfg.Plugin,
		socketDir: cfg.SocketDir,
		workDir:   workDir,
		extract:   extract,
		runner:    &runner{limits: cfg.Exec, pluginEnv: pluginEnv, path: envValue(pluginEnv, "PATH"), log: logger},
		log:       logger,
		metrics:   m,
	}
	s.handler = s.routes()
	return s, nil
}

// initWorkDir removes dir and recreates it with mode 0700 (upstream Service.Init),
// returning its symlink-free absolute path.
func initWorkDir(dir string) (string, error) {
	if err := removeTree(filepath.Dir(dir), filepath.Base(dir)); err != nil {
		return "", fmt.Errorf("error removing workdir %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("error creating workdir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("error creating workdir %q: %w", dir, err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("error creating workdir %q: %w", dir, err)
	}
	return real, nil
}

// removeTree removes parent/name without following symlinks out of parent. Plugins
// can leave directories without write or search permission, so on failure every
// directory of the tree is made owner-accessible and removal is retried.
func removeTree(parent, name string) error {
	root, err := os.OpenRoot(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(name); err == nil {
		return nil
	}
	_ = fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = root.Chmod(p, 0o700)
		}
		return nil
	})
	return root.RemoveAll(name)
}

// SocketPath is the socket the server listens on.
func (s *Server) SocketPath() string { return filepath.Join(s.socketDir, s.plugin.SocketName()) }

// WorkDir is the server work directory holding per-request directories.
func (s *Server) WorkDir() string { return s.workDir }

// Handler returns the cmpwire HTTP handler (exposed for embedding and tests).
func (s *Server) Handler() http.Handler { return s.handler }

// ListenAndServe removes a stale socket, listens on SocketPath with SocketMode and
// serves until ctx is done; the socket is removed on return. It refuses to replace a
// socket another live server answers on, or a file that is not a socket.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := os.MkdirAll(s.socketDir, 0o755); err != nil {
		return fmt.Errorf("cmp: create socket directory: %w", err)
	}
	path := s.SocketPath()
	if err := removeStaleSocket(ctx, path); err != nil {
		return err
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("cmp: listen on %s: %w", path, err)
	}
	defer func() { _ = removeSocket(path) }()
	if err := os.Chmod(path, s.cfg.SocketMode); err != nil {
		_ = l.Close()
		return fmt.Errorf("cmp: set socket permissions: %w", err)
	}
	if !s.plugin.IsDiscoveryConfigured() {
		s.log.Info(fmt.Sprintf("No discovery configuration is defined for plugin %s. To use this plugin, specify %q in the Application's spec.source.plugin.name field.", s.plugin.Metadata.Name, s.plugin.PluginName()))
	}
	if len(s.cfg.PassEnv) == 0 {
		s.log.Info("Plugin commands receive only PATH, HOME, TMPDIR, LANG and the request environment, not the sidecar environment (upstream passes all of it). Set " +
			EnvPassEnv + " to names, PREFIX* patterns or * to pass sidecar variables.")
	} else {
		s.log.Info("sidecar variables passed to plugin commands", "passEnv", s.cfg.PassEnv)
	}
	s.log.Info("cmp-server serving", "socket", path, "workDir", s.workDir)
	return s.Serve(ctx, l)
}

// removeStaleSocket deletes a leftover socket at path. A socket that still accepts
// connections, or a non-socket file, is an error.
func removeStaleSocket(ctx context.Context, path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cmp: inspect socket %s: %w", path, err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("cmp: %s exists and is not a socket", path)
	}
	d := net.Dialer{Timeout: time.Second}
	if c, err := d.DialContext(ctx, "unix", path); err == nil {
		_ = c.Close()
		return fmt.Errorf("cmp: another server is listening on %s (duplicate plugin name and version?)", path)
	}
	return removeSocket(path)
}

func removeSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode().Type() != fs.ModeSocket {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cmp: remove socket %s: %w", path, err)
	}
	return nil
}

// Serve serves l until ctx is done, then shuts down gracefully: the listener is
// closed at once, in-flight requests get ShutdownTimeout to finish, and are then
// cancelled (their commands' process groups are terminated). It returns nil after a
// shutdown, once every request handler has returned and cleaned up.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	base, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return base },
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(l) }()
	select {
	case err := <-errCh:
		cancelBase()
		s.active.Wait()
		return fmt.Errorf("cmp: serve: %w", err)
	case <-ctx.Done():
	}
	s.log.Info("cmp-server shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		s.log.Warn("in-flight requests did not finish before the shutdown timeout; cancelling them",
			"shutdownTimeout", s.cfg.ShutdownTimeout)
		cancelBase()
		_ = srv.Close()
	}
	<-errCh //nolint:errcheck // Serve returns http.ErrServerClosed after Shutdown or Close
	cancelBase()
	s.active.Wait()
	return nil
}

// statusError is an error with its HTTP status and cmpwire error code.
type statusError struct {
	status int
	code   string
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func newStatusError(status int, code string, err error) *statusError {
	return &statusError{status: status, code: code, err: err}
}

var (
	// errArchiveTooLarge reports an archive above Config.MaxArchiveBytes.
	errArchiveTooLarge = errors.New("source archive exceeds the size limit")
	// errReceiveTimeout reports a request stream that did not arrive before the
	// receive deadline.
	errReceiveTimeout = errors.New("timed out receiving the request stream")
)

// receiveTimedOut reports whether err, returned while receiving a stream, was caused
// by the receive deadline: a read interrupted by the connection read deadline, a
// context that hit a deadline, or a context cancelled once the deadline had passed
// (a read that times out also cancels the connection context).
func receiveTimedOut(err error, deadline time.Time) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return errors.Is(err, context.Canceled) && !deadline.IsZero() && !time.Now().Before(deadline)
}

// classify maps an error to its HTTP status and code.
func classify(err error) (int, string) {
	var se *statusError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &se):
		return se.status, se.code
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, sandbox.ErrTimeout):
		return http.StatusGatewayTimeout, apiclient.CodeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, apiclient.CodeUnavailable
	case errors.Is(err, errArchiveTooLarge), errors.Is(err, sandbox.ErrArchiveLimit), errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge, apiclient.CodeResourceExhausted
	case errors.Is(err, sandbox.ErrOutputLimit):
		return http.StatusInternalServerError, apiclient.CodeResourceExhausted
	}
	return http.StatusInternalServerError, apiclient.CodeInternal
}

// classifyReceive maps a stream reception error; malformed input is the client's.
func classifyReceive(err error) (int, string) {
	status, code := classify(err)
	if code == apiclient.CodeInternal {
		return http.StatusBadRequest, apiclient.CodeInvalidArgument
	}
	return status, code
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(cmpwire.PathConfig, s.handleConfig)
	mux.Handle(cmpwire.PathMatch, s.streamHandler(opMatch, s.match))
	mux.Handle(cmpwire.PathGenerate, s.streamHandler(opGenerate, s.generate))
	mux.Handle(cmpwire.PathParameters, s.streamHandler(opParameters, s.parameters))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, apiclient.CodeNotFound, fmt.Sprintf("unknown endpoint %s", r.URL.Path))
	})
	return mux
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.metrics.begin()
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, apiclient.CodeInvalidArgument, "method not allowed")
		s.metrics.end("config", apiclient.CodeInvalidArgument, time.Since(start))
		return
	}
	writeJSON(w, http.StatusOK, cmpwire.ConfigResponse{
		IsDiscoveryConfigured: s.plugin.IsDiscoveryConfigured(),
		ProvideGitCreds:       s.plugin.Spec.ProvideGitCreds,
	})
	s.metrics.end("config", "success", time.Since(start))
}

// operation describes one streamed endpoint.
type operation struct {
	// name is the metrics and log label.
	name string
	// receivePrefix and errorPrefix wrap reception and processing errors (upstream
	// messages).
	receivePrefix, errorPrefix string
}

var (
	opMatch = operation{
		name:          "match",
		receivePrefix: "match repository error receiving stream",
		errorPrefix:   "match repository error",
	}
	opGenerate = operation{
		name:          "generate",
		receivePrefix: "generate manifest error receiving stream",
		errorPrefix:   "error generating manifests",
	}
	opParameters = operation{
		name:          "parameters",
		receivePrefix: "parameters announcement error receiving stream",
		errorPrefix:   "get parameters announcement error",
	}
)

// request is one received source tree.
type request struct {
	meta cmpwire.Metadata
	// root is the per-request directory holding the extracted tree.
	root string
	// appPath is the application directory inside root (symlinks resolved within
	// root).
	appPath string
	// env is the request environment (NAME=VALUE), in order.
	env []string
}

// streamHandler receives the stream into a fresh directory, runs fn and answers with
// its result.
func (s *Server) streamHandler(op operation, fn func(context.Context, *request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.active.Add(1)
		defer s.active.Done()
		start := time.Now()
		s.metrics.begin()
		log := s.log.With("operation", op.name)
		// root is the per-request directory once created; error messages name files
		// relative to it rather than by their temporary absolute path.
		var root string
		fail := func(status int, code string, err error, attrs ...slog.Attr) {
			msg := err.Error()
			if root != "" {
				msg = strings.ReplaceAll(msg, root+string(filepath.Separator), "")
			}
			writeError(w, status, code, msg)
			level := slog.LevelWarn
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			attrs = append(attrs, slog.String("code", code), slog.String("error", loggableError(err)), slog.Duration("duration", time.Since(start)))
			log.LogAttrs(r.Context(), level, "cmp request failed", attrs...)
			s.metrics.end(op.name, code, time.Since(start))
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			fail(http.StatusMethodNotAllowed, apiclient.CodeInvalidArgument, errors.New("method not allowed"))
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != cmpwire.ContentTypeStream {
			fail(http.StatusUnsupportedMediaType, apiclient.CodeInvalidArgument, fmt.Errorf("unsupported content type %q, expected %s", r.Header.Get("Content-Type"), cmpwire.ContentTypeStream))
			return
		}
		ctx := r.Context()
		var deadline time.Time // client deadline minus DeadlineBuffer
		if v := r.Header.Get(cmpwire.HeaderDeadline); v != "" {
			dl, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				fail(http.StatusBadRequest, apiclient.CodeInvalidArgument, fmt.Errorf("invalid %s header: %w", cmpwire.HeaderDeadline, err))
				return
			}
			deadline = dl.Add(-DeadlineBuffer)
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
			log.LogAttrs(ctx, slog.LevelDebug, "request deadline", slog.Duration("remaining", time.Until(deadline)))
		}

		// The receive phase ends at the client deadline or after ReceiveTimeout,
		// whichever is earlier. The context stops extraction between entries; the
		// connection read deadline interrupts a read blocked on a client that stopped
		// sending, which nothing else can (upstream: the gRPC stream deadline).
		recvDeadline := deadline
		if s.cfg.ReceiveTimeout > 0 {
			if t := start.Add(s.cfg.ReceiveTimeout); recvDeadline.IsZero() || t.Before(recvDeadline) {
				recvDeadline = t
			}
		}
		rctx := ctx
		rc := http.NewResponseController(w)
		readDeadlineSet := false
		if !recvDeadline.IsZero() {
			var cancel context.CancelFunc
			rctx, cancel = context.WithDeadline(ctx, recvDeadline)
			defer cancel()
			if err := rc.SetReadDeadline(recvDeadline); err != nil {
				log.LogAttrs(ctx, slog.LevelDebug, "connection read deadline not supported", slog.String("error", err.Error()))
			} else {
				readDeadlineSet = true
			}
		}
		receiveFailed := func(err error, attrs ...slog.Attr) {
			var se *statusError
			switch {
			case receiveTimedOut(err, recvDeadline):
				attrs = append(attrs, slog.String("cause", err.Error()))
				fail(http.StatusGatewayTimeout, apiclient.CodeDeadlineExceeded, fmt.Errorf("%s: %w", op.receivePrefix, errReceiveTimeout), attrs...)
			case errors.As(err, &se):
				fail(se.status, se.code, err, attrs...)
			default:
				status, code := classifyReceive(err)
				fail(status, code, fmt.Errorf("%s: %w", op.receivePrefix, err), attrs...)
			}
		}

		body := r.Body
		if s.cfg.MaxArchiveBytes > 0 {
			body = http.MaxBytesReader(w, r.Body, 4+cmpwire.MaxMetadataBytes+s.cfg.MaxArchiveBytes)
		}
		meta, err := cmpwire.ReadMetadata(body)
		if err != nil {
			receiveFailed(err)
			return
		}
		attrs := []slog.Attr{slog.String("app", meta.AppName), slog.String("appRelPath", meta.AppRelPath), slog.Int64("archiveBytes", meta.Size)}
		if s.cfg.MaxArchiveBytes > 0 && meta.Size > s.cfg.MaxArchiveBytes {
			err := fmt.Errorf("%w: %d bytes, limit %d", errArchiveTooLarge, meta.Size, s.cfg.MaxArchiveBytes)
			fail(http.StatusRequestEntityTooLarge, apiclient.CodeResourceExhausted, fmt.Errorf("%s: %w", op.receivePrefix, err), attrs...)
			return
		}
		req, cleanup, err := s.receive(rctx, body, meta)
		if cleanup != nil {
			defer cleanup()
		}
		if req != nil {
			root = req.root
		}
		if err != nil {
			// The read deadline stays: it also bounds the server's discard of an
			// unread body before the error response.
			receiveFailed(err, attrs...)
			return
		}
		// The body is consumed: the read deadline must not outlive it, or the
		// server's background read (which detects a client disconnect) would time out
		// and cancel the commands, which the exec timeout and the client deadline
		// bound instead. net/http clears it when that read starts; clearing it here
		// as well does not depend on that.
		if readDeadlineSet {
			if err := rc.SetReadDeadline(time.Time{}); err != nil {
				log.LogAttrs(ctx, slog.LevelDebug, "clear connection read deadline", slog.String("error", err.Error()))
			}
		}
		res, err := fn(ctx, req)
		if err != nil {
			status, code := classify(err)
			fail(status, code, fmt.Errorf("%s: %w", op.errorPrefix, err), attrs...)
			return
		}
		writeJSON(w, http.StatusOK, res)
		attrs = append(attrs, slog.Duration("duration", time.Since(start)))
		if mr, ok := res.(cmpwire.ManifestResponse); ok {
			attrs = append(attrs, slog.Int("manifests", len(mr.Manifests)))
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, "cmp request served", attrs...)
		s.metrics.end(op.name, "success", time.Since(start))
	})
}

// receive extracts the payload of body into a new per-request directory and verifies
// its size and checksum; ctx bounds the reception. cleanup, when non-nil, removes the
// directory.
func (s *Server) receive(ctx context.Context, body io.Reader, meta cmpwire.Metadata) (*request, func(), error) {
	dir, err := os.MkdirTemp(s.workDir, "req-")
	if err != nil {
		return nil, nil, newStatusError(http.StatusInternalServerError, apiclient.CodeInternal, fmt.Errorf("error creating temp dir: %w", err))
	}
	cleanup := func() {
		if err := removeTree(s.workDir, filepath.Base(dir)); err != nil {
			s.log.Error("Failed to clean up temp directory", "dir", dir, "error", err.Error(),
				"security", 3, "CWE", 459)
		}
	}
	payload := cmpwire.PayloadReader(body, meta)
	extractErr := sandbox.ExtractTarGzContext(ctx, payload, dir, s.extract)
	// Read the rest of the payload (bounded by metadata.size, the context and the
	// connection read deadline) so tampering is reported as such rather than as an
	// archive error, and a client still sending gets the answer instead of a broken
	// pipe. Once the receive deadline has passed it is skipped; the payload is then
	// unverified, so the request fails.
	var drainErr error
	switch {
	case ctx.Err() != nil:
		drainErr = ctx.Err()
	case errors.Is(extractErr, os.ErrDeadlineExceeded):
		drainErr = extractErr
	default:
		_, drainErr = io.Copy(io.Discard, ctxReader{ctx: ctx, r: payload})
	}
	switch {
	case errors.Is(drainErr, cmpwire.ErrChecksumMismatch), errors.Is(drainErr, cmpwire.ErrSizeMismatch):
		return nil, cleanup, drainErr
	case extractErr != nil:
		return nil, cleanup, fmt.Errorf("error decompressing tgz file: %w", extractErr)
	case drainErr != nil:
		return nil, cleanup, drainErr
	}
	appPath, err := sandbox.SecureJoin(dir, filepath.FromSlash(meta.AppRelPath))
	if err != nil {
		return nil, cleanup, newStatusError(http.StatusBadRequest, apiclient.CodeInvalidArgument, fmt.Errorf("illegal appPath: out of workDir bound: %w", err))
	}
	return &request{meta: meta, root: dir, appPath: appPath, env: meta.Environ()}, cleanup, nil
}

// ctxReader is a reader that fails with the context error once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// requireAppDir fails unless the application directory exists in the received tree.
func requireAppDir(req *request) error {
	fi, err := os.Stat(req.appPath)
	if err != nil || !fi.IsDir() {
		return newStatusError(http.StatusBadRequest, apiclient.CodeInvalidArgument,
			fmt.Errorf("application path %q is not a directory of the received source", req.meta.AppRelPath))
	}
	return nil
}

// loggableError returns err's message without plugin stderr, which may echo
// sensitive input.
func loggableError(err error) string {
	msg := err.Error()
	var ce *CommandError
	if errors.As(err, &ce) && ce.Stderr != "" {
		msg = strings.Replace(msg, ": "+ce.Stderr, " (stderr omitted)", 1)
	}
	return msg
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	ct := cmpwire.ContentTypeJSON
	if status != http.StatusOK {
		ct = cmpwire.ContentTypeProblem
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, cmpwire.Error{Code: code, Message: msg})
}
