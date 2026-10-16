package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Login metric labels.
const (
	loginLocal   = "local"
	loginSSO     = "sso"
	loginSuccess = "success"
	loginFailure = "failure"
)

// createSession serves POST /api/v1/session (local login, upstream SessionService.Create).
// Every failure that could reveal whether the account exists or is locked out returns
// the same 401 "Invalid username or password"; the internal reason is only logged and
// counted.
func (h *Handlers) createSession(ctx context.Context, req *apiclient.SessionCreateRequest) (*apiclient.SessionResponse, error) {
	if h.d.Login == nil || h.d.Tokens == nil {
		return nil, rest.Unavailable("local login is not available").WithCause(errNotConfigured)
	}
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	audit := rest.AuditFrom(ctx)
	if req.Username == "" && req.Password == "" {
		h.d.Metrics.Login(loginLocal, loginFailure)
		audit.SetReason("no credentials supplied")
		return nil, rest.Unauthenticated("no credentials supplied")
	}
	acct, err := h.d.Login.Authenticate(ctx, req.Username, req.Password)
	if err != nil {
		h.d.Metrics.Login(loginLocal, loginFailure)
		return nil, h.loginError(ctx, req.Username, err)
	}
	iss, err := h.d.Tokens.IssueLoginSession(ctx, acct, s.UserSessionDuration())
	if err != nil {
		h.d.Metrics.Login(loginLocal, loginFailure)
		return nil, rest.Internal(err)
	}
	if req.Cookie {
		if err := auth.SetSessionCookie(rest.ResponseWriter(ctx), rest.HTTPRequest(ctx), iss.Token, h.cookieOptions(rest.HTTPRequest(ctx), s)); err != nil {
			return nil, rest.InvalidArgument("%s", err.Error()).WithCause(err)
		}
	}
	h.d.Metrics.Login(loginLocal, loginSuccess)
	audit.SetActor(acct.Name)
	audit.SetMessage("%s logged in", acct.Name)
	h.log().LogAttrs(ctx, slog.LevelInfo, "user logged in",
		slog.String("requestId", rest.RequestID(ctx)), slog.String("user", acct.Name), slog.String("method", loginLocal))
	return &apiclient.SessionResponse{Token: iss.Token}, nil
}

// loginError maps an Authenticator error to the client response. The username of a
// failed attempt is logged only when the account exists (mistyped passwords end up in
// unknown usernames).
func (h *Handlers) loginError(ctx context.Context, username string, err error) error {
	audit := rest.AuditFrom(ctx)
	var le *auth.LoginError
	switch {
	case errors.As(err, &le):
		attrs := []slog.Attr{slog.String("requestId", rest.RequestID(ctx)), slog.String("reason", string(le.Reason))}
		if le.Reason != auth.ReasonUnknownUser {
			attrs = append(attrs, slog.String("user", username))
		}
		h.log().LogAttrs(ctx, slog.LevelInfo, "login failed", attrs...)
		audit.SetReason("login failed: " + string(le.Reason))
		return rest.Unauthenticated("Invalid username or password").WithCause(err)
	case errors.Is(err, auth.ErrInvalidCredentials):
		audit.SetReason("login failed")
		return rest.Unauthenticated("Invalid username or password").WithCause(err)
	case errors.Is(err, auth.ErrNoCredentials):
		return rest.Unauthenticated("no credentials supplied").WithCause(err)
	case errors.Is(err, auth.ErrBlankPassword):
		return rest.Unauthenticated("Blank passwords are not allowed").WithCause(err)
	case errors.Is(err, auth.ErrUsernameTooLong):
		return rest.InvalidArgument("Username is too long (%d bytes max)", auth.MaxUsernameLength).WithCause(err)
	case errors.Is(err, auth.ErrAccountDisabled):
		audit.SetReason("account disabled")
		return rest.Unauthenticated("Account %s is disabled", username).WithCause(err)
	case errors.Is(err, auth.ErrMissingCapability):
		audit.SetReason("missing login capability")
		return rest.Unauthenticated("Account %s does not have login capability", username).WithCause(err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return rest.Unavailable("login is temporarily unavailable").WithCause(err)
}

// deleteSession serves DELETE /api/v1/session (logout): it revokes the caller's login or
// SSO session until it expires, clears the session cookie, and returns the identity
// provider's end-session URL for SSO sessions when oidc.config.logoutURL is set. Account
// API and project tokens are not revoked by logout; delete their metadata instead.
func (h *Handlers) deleteSession(ctx context.Context, _ *apiclient.Empty) (*apiclient.LogoutResponse, error) {
	id := rest.IdentityFrom(ctx)
	r := rest.HTTPRequest(ctx)
	s, _ := h.settings()
	out := &apiclient.LogoutResponse{}
	if id.LoggedIn() {
		p := id.Principal
		if p.Kind == auth.KindLogin || p.Kind == auth.KindSSO {
			if h.d.Tokens == nil {
				return nil, rest.Unavailable("logout is not available").WithCause(errNotConfigured)
			}
			if err := h.d.Tokens.Revoke(ctx, p); err != nil && !errors.Is(err, auth.ErrNotRevocable) {
				if errors.Is(err, auth.ErrRevocationStoreFull) {
					return nil, rest.Unavailable("the session revocation store is full; retry later").WithCause(err)
				}
				return nil, rest.Unavailable("logout is temporarily unavailable").WithCause(err)
			}
			rest.AuditFrom(ctx).SetMessage("%s logged out", p.Subject)
		}
		if p.Kind == auth.KindSSO && s != nil {
			if cfg, ok, err := s.OIDCProviderConfig(); err == nil && ok && cfg.LogoutURL != "" {
				out.LogoutURL = cfg.BuildLogoutURL("", h.externalBaseURL(r, s))
			}
		}
	}
	if w := rest.ResponseWriter(ctx); w != nil {
		auth.ClearSessionCookie(w, r, h.cookieOptions(r, s))
	}
	return out, nil
}

// getUserInfo serves GET /api/v1/session/userinfo.
func (h *Handlers) getUserInfo(ctx context.Context, _ *apiclient.Empty) (*apiclient.UserInfo, error) {
	id := rest.IdentityFrom(ctx)
	if !id.LoggedIn() {
		return &apiclient.UserInfo{LoggedIn: false}, nil
	}
	p := id.Principal
	return &apiclient.UserInfo{
		LoggedIn: true,
		Username: p.Username(),
		Iss:      p.Issuer,
		Groups:   slices.Clone(id.Groups()),
	}, nil
}

// externalBaseURL returns the configured external URL matching the request host (url or
// one of additionalUrls), falling back to url, without a trailing slash.
func (h *Handlers) externalBaseURL(r *http.Request, s interface {
	URL() string
	AdditionalURLs() []string
}) string {
	if s == nil {
		return ""
	}
	base := s.URL()
	if r != nil {
		for _, u := range append([]string{base}, s.AdditionalURLs()...) {
			if hostOf(u) != "" && strings.EqualFold(hostOf(u), r.Host) {
				base = u
				break
			}
		}
	}
	return strings.TrimRight(base, "/")
}

func hostOf(u string) string {
	_, rest, ok := strings.Cut(u, "://")
	if !ok {
		return ""
	}
	host, _, _ := strings.Cut(rest, "/")
	return host
}
