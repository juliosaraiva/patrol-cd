package commitserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/git"
)

// DefaultMaxRequestBytes caps request bodies (upstream's default gRPC message limit is
// 100 MiB).
const DefaultMaxRequestBytes = 100 << 20

// HandlerOptions configure the HTTP handler.
type HandlerOptions struct {
	// Token is the shared internal token (Secret patrol-internal-token, ADR-0006). Required.
	Token string
	// MaxRequestBytes caps the request body (default DefaultMaxRequestBytes).
	MaxRequestBytes int64
	// Logger is the handler logger (default slog.Default()).
	Logger *slog.Logger
}

// NewHandler serves the commit-server API: POST PathCommitHydratedManifests (bearer token
// required) and GET PathHealthz.
func NewHandler(svc Client, opts HandlerOptions) (http.Handler, error) {
	if svc == nil {
		return nil, errors.New("commit-server: service is required")
	}
	if opts.Token == "" {
		return nil, errors.New("commit-server: an internal token is required")
	}
	if opts.MaxRequestBytes <= 0 {
		opts.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	h := &handler{svc: svc, opts: opts, tokenSum: sha256.Sum256([]byte(opts.Token))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathHealthz, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST "+PathCommitHydratedManifests, h.commit)
	return mux, nil
}

type handler struct {
	svc      Client
	opts     HandlerOptions
	tokenSum [sha256.Size]byte
}

func (h *handler) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], h.tokenSum[:]) == 1
}

func (h *handler) commit(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="patrol-commit-server"`)
		writeProblem(w, http.StatusUnauthorized, CodeUnauthenticated, "missing or invalid internal token")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeProblem(w, http.StatusUnsupportedMediaType, CodeInvalidArgument, "content type must be application/json")
		return
	}
	body := http.MaxBytesReader(w, r.Body, h.opts.MaxRequestBytes)
	var req CommitHydratedManifestsRequest
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeProblem(w, http.StatusRequestEntityTooLarge, CodeTooLarge, fmt.Sprintf("request body exceeds %d bytes", mbe.Limit))
			return
		}
		writeProblem(w, http.StatusBadRequest, CodeInvalidArgument, "malformed request body: "+err.Error())
		return
	}
	if dec.More() {
		writeProblem(w, http.StatusBadRequest, CodeInvalidArgument, "malformed request body: trailing data")
		return
	}
	resp, err := h.svc.CommitHydratedManifests(r.Context(), &req)
	if err != nil {
		status, code := problemFor(err)
		if status >= http.StatusInternalServerError {
			h.opts.Logger.Error("commit-server: request failed", slog.Any("request", &req), slog.String("code", code), slog.Any("error", err))
		}
		writeProblem(w, status, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// problemFor maps service errors to HTTP status and problem codes.
func problemFor(err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, git.ErrInvalidRevision),
		errors.Is(err, git.ErrInvalidURL), errors.Is(err, git.ErrInvalidCredentials):
		return http.StatusBadRequest, CodeInvalidArgument
	case errors.Is(err, git.ErrAuthentication):
		return http.StatusForbidden, CodePermissionDenied
	case errors.Is(err, ErrConcurrentUpdate):
		return http.StatusConflict, CodeAborted
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, CodeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, CodeUnavailable
	default:
		return http.StatusInternalServerError, CodeInternal
	}
}

type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
}

// writeProblem writes an RFC 9457 problem details response.
func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
	})
}
