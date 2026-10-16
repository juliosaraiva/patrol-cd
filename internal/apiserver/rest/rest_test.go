package rest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

type whoami struct {
	LoggedIn  bool     `json:"loggedIn"`
	Actor     string   `json:"actor"`
	Anonymous bool     `json:"anonymous"`
	Kind      string   `json:"kind"`
	Source    string   `json:"source"`
	Groups    []string `json:"groups,omitempty"`
}

func whoamiHandler(ctx context.Context, _ *apiclient.Empty) (*whoami, error) {
	id := rest.IdentityFrom(ctx)
	out := &whoami{LoggedIn: id.LoggedIn(), Actor: id.Actor(), Anonymous: id.Anonymous, Source: string(id.Source), Groups: id.Groups()}
	if id.Principal != nil {
		out.Kind = string(id.Principal.Kind)
	}
	return out, nil
}

type echoReq struct {
	Name  string   `path:"name" json:"-"`
	Q     string   `query:"q" json:"-"`
	N     *int     `query:"n" json:"-"`
	Tags  []string `query:"tag" json:"-"`
	Flag  bool     `query:"flag" json:"-"`
	Value string   `json:"value"`
	Count int      `json:"count,omitempty"`
}

type objectReq struct {
	Name string                `path:"name" json:"-"`
	App  *v1alpha1.Application `body:"true" json:"-"`
}

type objectResp struct {
	Name    string `json:"name"`
	Project string `json:"project"`
}

func registerTestRoutes(t *rest.Table) {
	rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/public", OperationID: "public", Access: rest.Public()}, whoamiHandler)
	rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/private", OperationID: "private", Access: rest.Authenticated()}, whoamiHandler)
	rest.Register(t, rest.Route{Method: http.MethodPost, Path: "/api/v1/private", OperationID: "privateWrite", Access: rest.Authenticated()}, whoamiHandler)
	rest.Register(t, rest.Route{Method: http.MethodDelete, Path: "/api/v1/private", OperationID: "privateEnd", Access: rest.Authenticated(), NoRenew: true}, whoamiHandler)
	rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/accounts/{name}", OperationID: "getThing",
		Access: rest.RBACPathValue(rbac.ResourceAccounts, rbac.ActionGet, "name")}, func(ctx context.Context, _ *struct {
		Name string `path:"name" json:"-"`
	}) (*whoami, error) {
		return whoamiHandler(ctx, nil)
	})
	rest.Register(t, rest.Route{Method: http.MethodDelete, Path: "/api/v1/apps/{name}", OperationID: "deleteApp",
		Access: rest.RBACInHandler(rbac.ResourceApplications, rbac.ActionDelete, "proj/{name}")}, func(ctx context.Context, req *struct {
		Name string `path:"name" json:"-"`
	}) (*apiclient.Empty, error) {
		rest.AuditFrom(ctx).SetTarget(rest.ObjectRef{APIVersion: "cd.patrol.example/v1alpha1", Kind: "Application", Namespace: "patrol", Name: req.Name})
		if err := rest.Authorize(ctx, rbac.ResourceApplications, rbac.ActionDelete, "proj/"+req.Name); err != nil {
			return nil, err
		}
		return &apiclient.Empty{}, nil
	})
	rest.Register(t, rest.Route{Method: http.MethodPost, Path: "/api/v1/login", OperationID: "login", Access: rest.Public(), RateLimited: true},
		func(context.Context, *apiclient.Empty) (*apiclient.Empty, error) { return &apiclient.Empty{}, nil })
	rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/panic", OperationID: "panic", Access: rest.Public()},
		func(context.Context, *apiclient.Empty) (*apiclient.Empty, error) { panic("boom") })
	rest.Register(t, rest.Route{Method: http.MethodPost, Path: "/api/v1/echo/{name}", OperationID: "echo", Access: rest.Public(), MaxBodyBytes: 256},
		func(_ context.Context, req *echoReq) (*echoReq, error) { return req, nil })
	rest.Register(t, rest.Route{Method: http.MethodPut, Path: "/api/v1/objects/{name}", OperationID: "putObject", Access: rest.Public()},
		func(_ context.Context, req *objectReq) (*objectResp, error) {
			if req.App == nil {
				return nil, rest.InvalidArgument("body required")
			}
			return &objectResp{Name: req.Name, Project: req.App.Spec.Project}, nil
		})
	rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/files/{path...}", OperationID: "files", Access: rest.Public()},
		func(_ context.Context, req *struct {
			Path string `path:"path" json:"-"`
		}) (*objectResp, error) {
			return &objectResp{Name: req.Path}, nil
		})
}

func TestRouteValidation(t *testing.T) {
	t.Parallel()
	h := func(context.Context, *apiclient.Empty) (*apiclient.Empty, error) { return nil, nil }
	type nameReq struct {
		Name string `path:"name" json:"-"`
	}
	type bodyReq struct {
		Value string `json:"value"`
	}
	tests := []struct {
		name string
		reg  func(t *rest.Table)
		want string
	}{
		{"bad operation id", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "get-a", Access: rest.Public()}, h)
		}, "operation ID"},
		{"no access", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a"}, h)
		}, "access mode not set"},
		{"rbac without resolve", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a", Access: rest.Access{Mode: rest.AccessRBAC, Resource: "x", Action: "y"}}, h)
		}, "Resolve or InHandler"},
		{"missing path field", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a/{name}", OperationID: "a", Access: rest.Public()}, h)
		}, `path parameter "name"`},
		{"path field without wildcard", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a", Access: rest.Public()},
				func(context.Context, *nameReq) (*apiclient.Empty, error) { return nil, nil })
		}, "which the pattern lacks"},
		{"GET with body", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a", Access: rest.Public()},
				func(context.Context, *bodyReq) (*apiclient.Empty, error) { return nil, nil })
		}, "cannot carry a JSON body"},
		{"duplicate operation", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a", Access: rest.Public()}, h)
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/b", OperationID: "a", Access: rest.Public()}, h)
		}, "duplicate operation ID"},
		{"duplicate pattern", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a", Access: rest.Public()}, h)
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "b", Access: rest.Public()}, h)
		}, "registered twice"},
		{"invalid pattern", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a/{x...}/b", OperationID: "a", Access: rest.Public()}, h)
		}, "invalid pattern"},
		{"skip auth needs public", func(t *rest.Table) {
			t.Add(rest.Route{Path: "/dex/", OperationID: "dex", Access: rest.Authenticated(), SkipAuth: true, Handler: http.NotFoundHandler()})
		}, "SkipAuth requires public"},
		{"raw route without handler", func(t *rest.Table) {
			t.Add(rest.Route{Method: http.MethodGet, Path: "/raw", OperationID: "raw", Access: rest.Public()})
		}, "no handler"},
		{"hidden documentation-only route", func(t *rest.Table) {
			t.Add(rest.Route{Method: http.MethodPost, Path: "/doc", OperationID: "doc", Access: rest.Public(), DocumentationOnly: true, Hidden: true})
		}, "cannot be hidden"},
		{"extension without x- prefix", func(t *rest.Table) {
			rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/a", OperationID: "a", Access: rest.Public(),
				Extensions: map[string]any{"receivers": []string{"a"}}}, h)
		}, "must start with x-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tbl := rest.NewTable()
			tc.reg(tbl)
			err := tbl.Err()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			_, err = tbl.Handler(&rest.Env{})
			require.Error(t, err, "an invalid table must not mount")
		})
	}
}

// TestDocumentationOnlyRoutesAreNotMounted checks that documentation-only routes need no
// handler, are listed for the OpenAPI document and never change routing.
func TestDocumentationOnlyRoutesAreNotMounted(t *testing.T) {
	t.Parallel()
	tbl := rest.NewTable()
	tbl.Add(rest.Route{Method: http.MethodPost, Path: "/hook", OperationID: "hook", Access: rest.Public(), SkipAuth: true, DocumentationOnly: true,
		Extensions: map[string]any{"x-note": "served elsewhere"}})
	rest.Register(tbl, rest.Route{Method: http.MethodGet, Path: "/served", OperationID: "served", Access: rest.Public()},
		func(context.Context, *apiclient.Empty) (*apiclient.Empty, error) { return &apiclient.Empty{}, nil })
	require.NoError(t, tbl.Err())
	ids := []string{}
	for _, r := range tbl.Routes() {
		ids = append(ids, r.OperationID)
	}
	assert.ElementsMatch(t, []string{"hook", "served"}, ids)

	mux := http.NewServeMux()
	require.NoError(t, tbl.Mount(mux, &rest.Env{DisableAuth: true}))
	_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/hook", nil))
	assert.Empty(t, pattern, "documentation-only routes are not mounted")
	_, pattern = mux.Handler(httptest.NewRequest(http.MethodGet, "/served", nil))
	assert.Equal(t, "GET /served", pattern)
}

func TestRoutesSortedAndTyped(t *testing.T) {
	t.Parallel()
	tbl := rest.NewTable()
	registerTestRoutes(tbl)
	require.NoError(t, tbl.Err())
	routes := tbl.Routes()
	for i := 1; i < len(routes); i++ {
		assert.LessOrEqual(t, routes[i-1].Path, routes[i].Path)
	}
	for _, r := range routes {
		if r.OperationID == "echo" {
			assert.Equal(t, reflect.TypeFor[echoReq](), r.Request)
			assert.Equal(t, []string{"name"}, r.PathParams())
			b, err := rest.BindingOf(r.Request)
			require.NoError(t, err)
			assert.True(t, b.BodyIsStruct)
			assert.Len(t, b.QueryParams, 4)
		}
	}
}

func TestDecodeBindings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)

	resp := h.do(http.MethodPost, "/api/v1/echo/it%2Fem?q=hello&n=7&tag=a&tag=b&flag", `{"value":"v","count":3}`, jsonBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got := decodeJSON[map[string]any](t, resp)
	// Path, query and flag fields are json:"-": the response only echoes body fields.
	assert.Equal(t, map[string]any{"value": "v", "count": float64(3)}, got)

	// The typed request is visible to the handler: check through a dedicated route.
	tbl := rest.NewTable()
	var seen echoReq
	rest.Register(tbl, rest.Route{Method: http.MethodPost, Path: "/e/{name}", OperationID: "e", Access: rest.Public()},
		func(_ context.Context, req *echoReq) (*apiclient.Empty, error) {
			seen = *req
			return &apiclient.Empty{}, nil
		})
	handler, err := tbl.Handler(&rest.Env{DisableAuth: true})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/e/it%2Fem?q=hello&n=7&tag=a&tag=b&flag", strings.NewReader(`{"value":"v"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, seen.N)
	assert.Equal(t, echoReq{Name: "it/em", Q: "hello", N: seen.N, Tags: []string{"a", "b"}, Flag: true, Value: "v"}, seen)
	assert.Equal(t, 7, *seen.N)

	// Multi-segment wildcard.
	resp = h.do(http.MethodGet, "/api/v1/files/a/b/c.yaml", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "a/b/c.yaml", decodeJSON[objectResp](t, resp).Name)

	// Kubernetes object bodies accept unknown fields (newer API versions).
	resp = h.do(http.MethodPut, "/api/v1/objects/x", `{"apiVersion":"cd.patrol.example/v1alpha1","kind":"Application","spec":{"project":"p","futureField":1},"x":true}`, jsonBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, objectResp{Name: "x", Project: "p"}, decodeJSON[objectResp](t, resp))

	// An empty body is "no body".
	resp = h.do(http.MethodPost, "/api/v1/echo/x", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDecodeErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)
	tests := []struct {
		name, path, body string
		opts             []reqOpt
		status           int
		code             apiclient.ErrorCode
		detail           string
	}{
		{"unknown field", "/api/v1/echo/x", `{"value":"v","bogus":1}`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, `unknown field "bogus"`},
		{"wrong type", "/api/v1/echo/x", `{"value":1}`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, "field value must be string"},
		{"malformed", "/api/v1/echo/x", `{"value":`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, "truncated JSON"},
		{"syntax", "/api/v1/echo/x", `{value}`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, "malformed JSON"},
		{"trailing data", "/api/v1/echo/x", `{"value":"v"} {}`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, "unexpected data"},
		{"content type", "/api/v1/echo/x", `{"value":"v"}`, []reqOpt{header("Content-Type", "text/plain")}, 415, apiclient.CodeInvalidArgument, "Invalid content type"},
		{"missing content type", "/api/v1/echo/x", `{"value":"v"}`, nil, 415, apiclient.CodeInvalidArgument, "Invalid content type"},
		{"too large", "/api/v1/echo/x", `{"value":"` + strings.Repeat("x", 300) + `"}`, []reqOpt{jsonBody}, 413, apiclient.CodeResourceExhausted, "too large"},
		{"bad int query", "/api/v1/echo/x?n=abc", `{}`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, `query parameter "n"`},
		{"bad bool query", "/api/v1/echo/x?flag=maybe", `{}`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, `query parameter "flag"`},
		{"bad query string", "/api/v1/echo/x?q=%zz", `{}`, []reqOpt{jsonBody}, 400, apiclient.CodeInvalidArgument, "invalid query string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, tc.path, tc.body, tc.opts...)
			require.Equal(t, tc.status, resp.StatusCode)
			p := decodeProblem(t, resp)
			assert.Equal(t, tc.code, p.Code)
			assert.Equal(t, tc.status, p.Status)
			assert.Equal(t, apiclient.ProblemType(tc.code), p.Type)
			assert.Contains(t, p.Detail, tc.detail)
			assert.NotEmpty(t, p.RequestID)
			assert.Equal(t, resp.Header.Get(rest.RequestIDHeader), p.RequestID)
		})
	}
}

func TestAuthMatrix(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)
	ctx := context.Background()

	revokedTok := h.loginToken("alice")
	p, err := h.tokens.Verify(ctx, revokedTok)
	require.NoError(t, err)
	require.NoError(t, h.tokens.Revoke(ctx, p))

	ci, err := h.accounts.LookupAccount(ctx, "ci")
	require.NoError(t, err)
	apiTok, meta, err := h.tokens.IssueAccountToken(ctx, ci, "t1", 0)
	require.NoError(t, err)
	deletedTok, _, err := h.tokens.IssueAccountToken(ctx, ci, "t2", 0)
	require.NoError(t, err)
	h.accounts.update("ci", func(a *auth.Account) { a.Tokens = []auth.TokenMeta{meta} })

	projTok, err := h.tokens.IssueProjectToken(ctx, "proj", "ci", "", 0)
	require.NoError(t, err)
	h.projects.Store(projTok.ID, true)
	unknownProjTok, err := h.tokens.IssueProjectToken(ctx, "proj", "ci", "", 0)
	require.NoError(t, err)

	staleTok := h.loginToken("bob")
	h.clock.Advance(2 * time.Second)
	h.accounts.update("bob", func(a *auth.Account) { a.PasswordMtime = h.clock.Now() })

	disabledTok := h.loginToken("admin")
	h.accounts.update("admin", func(a *auth.Account) { a.Enabled = false })

	tests := []struct {
		name     string
		path     string
		opts     []reqOpt
		status   int
		detail   string
		kind     string
		loggedIn bool
		source   string
	}{
		{"no token", "/api/v1/private", nil, 401, "no session information", "", false, ""},
		{"garbage token", "/api/v1/private", []reqOpt{bearer("a.b.c")}, 401, "invalid session", "", false, ""},
		{"forged signature", "/api/v1/private", []reqOpt{bearer(forge(t, h.loginToken("alice")))}, 401, "invalid session", "", false, ""},
		{"revoked", "/api/v1/private", []reqOpt{bearer(revokedTok)}, 401, "token revoked", "", false, ""},
		{"api token listed", "/api/v1/private", []reqOpt{bearer(apiTok.Token)}, 200, "", "apiKey", true, "header"},
		{"api token deleted", "/api/v1/private", []reqOpt{bearer(deletedTok.Token)}, 401, "invalid session", "", false, ""},
		{"project token", "/api/v1/private", []reqOpt{bearer(projTok.Token)}, 200, "", "project", true, "header"},
		{"project token not recorded", "/api/v1/private", []reqOpt{bearer(unknownProjTok.Token)}, 401, "invalid session", "", false, ""},
		{"password changed", "/api/v1/private", []reqOpt{bearer(staleTok)}, 401, "password changed", "", false, ""},
		{"account disabled", "/api/v1/private", []reqOpt{bearer(disabledTok)}, 401, "account disabled", "", false, ""},
		{"cookie", "/api/v1/private", []reqOpt{cookie(h.loginToken("alice"))}, 200, "", "login", true, "cookie"},
		{"bearer wins over cookie", "/api/v1/private", []reqOpt{bearer(h.loginToken("alice")), cookie("x.y.z")}, 200, "", "login", true, "header"},
		{"public with bad token", "/api/v1/public", []reqOpt{bearer(revokedTok)}, 200, "", "", false, ""},
		{"public without token", "/api/v1/public", nil, 200, "", "", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodGet, tc.path, "", tc.opts...)
			require.Equal(t, tc.status, resp.StatusCode)
			if tc.status != http.StatusOK {
				p := decodeProblem(t, resp)
				assert.Equal(t, apiclient.CodeUnauthenticated, p.Code)
				assert.Contains(t, p.Detail, tc.detail)
				assert.Equal(t, `Bearer realm="patrol"`, resp.Header.Get("WWW-Authenticate"))
				return
			}
			w := decodeJSON[whoami](t, resp)
			assert.Equal(t, tc.loggedIn, w.LoggedIn)
			assert.Equal(t, tc.kind, w.Kind)
			if tc.source != "" {
				assert.Equal(t, tc.source, w.Source)
			}
		})
	}

	t.Run("expired", func(t *testing.T) {
		h2 := newHarness(t, registerTestRoutes)
		tok := h2.loginToken("alice")
		h2.clock.Advance(2 * time.Hour)
		resp := h2.do(http.MethodGet, "/api/v1/private", "", bearer(tok))
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, decodeProblem(t, resp).Detail, "token expired")
	})
	t.Run("invalid cookie is cleared", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/api/v1/private", "", cookie(revokedTok))
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		var cleared bool
		for _, c := range resp.Cookies() {
			if c.Name == auth.SessionCookieName && c.MaxAge < 0 {
				cleared = true
			}
		}
		assert.True(t, cleared, "the stale session cookie must be expired")
	})
}

// forge re-signs the payload of tok with another key.
func forge(t *testing.T, tok string) string {
	t.Helper()
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)
	return parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
}

func TestAnonymousAccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)
	require.NoError(t, h.enforcer.SetPolicy(rbac.BuiltinPolicy(), userPolicy, rbac.RoleReadOnly, rbac.GlobMatchMode))

	resp := h.do(http.MethodGet, "/api/v1/accounts/x", "")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "anonymous disabled")

	h.anonymous.Store(true)
	resp = h.do(http.MethodGet, "/api/v1/accounts/x", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "policy.default role:readonly allows get")
	w := decodeJSON[whoami](t, resp)
	assert.True(t, w.Anonymous)
	assert.False(t, w.LoggedIn)
	assert.Equal(t, "anonymous", w.Actor)

	// Invalid tokens are downgraded to anonymous.
	resp = h.do(http.MethodGet, "/api/v1/private", "", bearer("a.b.c"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, decodeJSON[whoami](t, resp).Anonymous)

	// Writes beyond the default role are denied.
	resp = h.do(http.MethodDelete, "/api/v1/apps/allowed", "")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestDisableAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes, withDisableAuth())
	resp := h.do(http.MethodDelete, "/api/v1/apps/anything", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = h.do(http.MethodGet, "/api/v1/accounts/x", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSessionRenewal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)
	tok := h.loginToken("alice")

	resp := h.do(http.MethodGet, "/api/v1/private", "", bearer(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get(apiclient.RenewTokenHeader), "not close to expiry")

	h.clock.Advance(58 * time.Minute)
	resp = h.do(http.MethodGet, "/api/v1/private", "", bearer(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	renewed := resp.Header.Get(apiclient.RenewTokenHeader)
	require.NotEmpty(t, renewed)
	p, err := h.tokens.Verify(context.Background(), renewed)
	require.NoError(t, err)
	assert.Equal(t, "alice", p.Subject)

	resp = h.do(http.MethodGet, "/api/v1/private", "", cookie(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var c *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.SessionCookieName {
			c = ck
		}
	}
	require.NotNil(t, c, "renewed session cookie")
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, c.SameSite)
	assert.Empty(t, resp.Header.Get(apiclient.RenewTokenHeader))

	// NoRenew routes (logout, password change) never hand out a replacement session.
	resp = h.do(http.MethodDelete, "/api/v1/private", "", bearer(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, resp.Header.Get(apiclient.RenewTokenHeader) != "", "no renewed bearer token on a NoRenew route")
	resp = h.do(http.MethodDelete, "/api/v1/private", "", cookie(tok), header(rest.CSRFHeader, rest.CSRFHeaderValue))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	for _, ck := range resp.Cookies() {
		assert.NotEqual(t, auth.SessionCookieName, ck.Name, "no renewed session cookie on a NoRenew route")
	}
}

func TestCSRF(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)
	tok := h.loginToken("alice")
	xrw := header(rest.CSRFHeader, rest.CSRFHeaderValue)
	tests := []struct {
		name   string
		method string
		opts   []reqOpt
		status int
	}{
		{"cookie mutation without header", http.MethodPost, []reqOpt{cookie(tok)}, 403},
		{"cookie mutation with wrong header", http.MethodPost, []reqOpt{cookie(tok), header(rest.CSRFHeader, "XMLHttpRequest")}, 403},
		{"cookie mutation with header", http.MethodPost, []reqOpt{cookie(tok), xrw}, 200},
		{"cookie mutation same origin", http.MethodPost, []reqOpt{cookie(tok), xrw, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }}, 200},
		{"cookie mutation trusted origin", http.MethodPost, []reqOpt{cookie(tok), xrw, header("Origin", "https://patrol.example.com")}, 200},
		{"cookie mutation foreign origin", http.MethodPost, []reqOpt{cookie(tok), xrw, header("Origin", "https://evil.example.com")}, 403},
		{"cookie mutation null origin", http.MethodPost, []reqOpt{cookie(tok), xrw, header("Origin", "null")}, 403},
		{"cookie mutation foreign referer", http.MethodPost, []reqOpt{cookie(tok), xrw, header("Referer", "https://evil.example.com/page")}, 403},
		{"cookie mutation same referer", http.MethodPost, []reqOpt{cookie(tok), xrw, func(r *http.Request) { r.Header.Set("Referer", "http://"+r.Host+"/applications") }}, 200},
		{"cookie read without header", http.MethodGet, []reqOpt{cookie(tok)}, 200},
		{"bearer mutation without header", http.MethodPost, []reqOpt{bearer(tok)}, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(tc.method, "/api/v1/private", "", tc.opts...)
			require.Equal(t, tc.status, resp.StatusCode)
			if tc.status == http.StatusForbidden {
				assert.Equal(t, apiclient.CodePermissionDenied, decodeProblem(t, resp).Code)
			}
		})
	}
}

func TestRBACDenialIsAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)

	resp := h.do(http.MethodGet, "/api/v1/accounts/x", "", bearer(h.loginToken("alice")))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, h.audit.all(), "successful reads are not audited")

	resp = h.do(http.MethodGet, "/api/v1/accounts/x", "", bearer(h.loginToken("bob")), header(rest.RequestIDHeader, "req-1"))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	p := decodeProblem(t, resp)
	assert.Equal(t, apiclient.CodePermissionDenied, p.Code)
	assert.True(t, strings.HasPrefix(p.Detail, "permission denied: accounts, get, x, sub: bob, iat: "), p.Detail)
	entries := h.audit.all()
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, rest.AuditDenied, e.Result)
	assert.Equal(t, "bob", e.Actor)
	assert.Equal(t, "login", e.ActorKind)
	assert.Equal(t, []string{"accounts", "get", "x"}, []string{e.Resource, e.Action, e.Object})
	assert.Equal(t, http.StatusForbidden, e.Status)
	assert.Equal(t, "req-1", e.RequestID)
	assert.Equal(t, "getThing", e.Operation)
	assert.Equal(t, "GET /api/v1/accounts/{name}", e.Route)

	// Handler-enforced check: allowed object succeeds and is audited with its target;
	// another object is denied and audited.
	bob := h.loginToken("bob")
	resp = h.do(http.MethodDelete, "/api/v1/apps/allowed", "", bearer(bob))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = h.do(http.MethodDelete, "/api/v1/apps/other", "", bearer(bob))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	entries = h.audit.all()
	require.Len(t, entries, 3)
	assert.Equal(t, rest.AuditSucceeded, entries[1].Result)
	require.NotNil(t, entries[1].Target)
	assert.Equal(t, "Application", entries[1].Target.Kind)
	assert.Equal(t, rest.AuditDenied, entries[2].Result)
	assert.Equal(t, "proj/other", entries[2].Object)
	require.NotNil(t, entries[2].Target)

	// Failed mutations are audited as failed.
	resp = h.do(http.MethodPost, "/api/v1/echo/x", `{"bogus":1}`, jsonBody)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	entries = h.audit.all()
	require.Len(t, entries, 4)
	assert.Equal(t, rest.AuditFailed, entries[3].Result)
	assert.Equal(t, http.StatusBadRequest, entries[3].Status)
}

func TestLogAuditorEvents(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rec := record.NewFakeRecorder(10)
	a := &rest.LogAuditor{Logger: slog.New(slog.NewJSONHandler(&buf, nil)), Recorder: rec}
	app := &rest.ObjectRef{APIVersion: "cd.patrol.example/v1alpha1", Kind: "Application", Namespace: "patrol", Name: "guestbook"}
	ctx := context.Background()
	a.Audit(ctx, rest.AuditEntry{Actor: "bob", Method: http.MethodDelete, Result: rest.AuditSucceeded, Status: 200, Target: app})
	a.Audit(ctx, rest.AuditEntry{Actor: "eve", Method: http.MethodPost, Action: "sync", Result: rest.AuditDenied, Status: 403, Target: app})
	a.Audit(ctx, rest.AuditEntry{Actor: "bob", Method: http.MethodPost, Result: rest.AuditFailed, Status: 500, Target: app})
	a.Audit(ctx, rest.AuditEntry{Actor: "bob", Method: http.MethodPost, Result: rest.AuditSucceeded, Target: &rest.ObjectRef{Kind: "Cluster", Name: "c"}})
	close(rec.Events)
	var events []string
	for e := range rec.Events {
		events = append(events, e)
	}
	assert.Equal(t, []string{
		"Normal ResourceDeleted bob deleted application guestbook",
		"Warning AccessDenied eve was denied sync on application guestbook",
	}, events)
	assert.Equal(t, 4, strings.Count(buf.String(), `"msg":"audit"`))
}

func TestLoginRateLimit(t *testing.T) {
	t.Parallel()
	l, err := rest.NewRateLimiter(rest.RateLimiterConfig{Limit: rate.Every(time.Hour), Burst: 2})
	require.NoError(t, err)
	h := newHarness(t, registerTestRoutes, withLimiter(l))
	for range 2 {
		resp := h.do(http.MethodPost, "/api/v1/login", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	resp := h.do(http.MethodPost, "/api/v1/login", "")
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	assert.Equal(t, apiclient.CodeResourceExhausted, decodeProblem(t, resp).Code)
	// Other routes are not limited.
	resp = h.do(http.MethodGet, "/api/v1/public", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted, err := rest.ParseTrustedProxies([]string{"10.0.0.0/8", "192.168.1.1"})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 192.168.1.1")
	assert.Equal(t, "203.0.113.9", rest.ClientIP(r, trusted))
	r.RemoteAddr = "198.51.100.1:1234"
	assert.Equal(t, "198.51.100.1", rest.ClientIP(r, trusted), "untrusted peers cannot spoof X-Forwarded-For")
	_, err = rest.ParseTrustedProxies([]string{"nope"})
	require.Error(t, err)
}

func TestPanicRecovery(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)
	resp := h.do(http.MethodGet, "/api/v1/panic", "")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	p := decodeProblem(t, resp)
	assert.Equal(t, apiclient.CodeInternal, p.Code)
	assert.Equal(t, "internal error", p.Detail)
	assert.NotContains(t, p.Detail, "boom")
	assert.NotEmpty(t, p.RequestID)
	resp = h.do(http.MethodGet, "/api/v1/public", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the server keeps serving")
}

func TestNotMatched(t *testing.T) {
	t.Parallel()
	h := newHarness(t, registerTestRoutes)
	resp := h.do(http.MethodGet, "/api/v1/nope", "")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, apiclient.CodeNotFound, decodeProblem(t, resp).Code)

	resp = h.do(http.MethodPut, "/api/v1/public", "")
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
}

func TestProblemFor(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: "cd.patrol.example", Resource: "applications"}
	tests := []struct {
		name   string
		err    error
		status int
		code   apiclient.ErrorCode
		detail string
	}{
		{"api error", rest.Conflict("stale"), 409, apiclient.CodeConflict, "stale"},
		{"wrapped api error", fmt.Errorf("ctx: %w", rest.NotFound("nope")), 404, apiclient.CodeNotFound, "nope"},
		{"k8s not found", apierrors.NewNotFound(gr, "a"), 404, apiclient.CodeNotFound, `"a" not found`},
		{"k8s already exists", apierrors.NewAlreadyExists(gr, "a"), 409, apiclient.CodeAlreadyExists, "already exists"},
		{"k8s conflict", apierrors.NewConflict(gr, "a", errors.New("rv")), 409, apiclient.CodeConflict, "cannot be fulfilled"},
		{"k8s forbidden hides details", apierrors.NewForbidden(gr, "a", errors.New("serviceaccount patrol-server cannot")), 403, apiclient.CodePermissionDenied, "permission denied"},
		{"k8s invalid", apierrors.NewInvalid(schema.GroupKind{Group: "cd.patrol.example", Kind: "Application"}, "a", field.ErrorList{field.Required(field.NewPath("spec"), "x")}), 422, apiclient.CodeInvalidArgument, "spec"},
		{"k8s bad request", apierrors.NewBadRequest("bad"), 400, apiclient.CodeInvalidArgument, "bad"},
		{"k8s too many", apierrors.NewTooManyRequests("slow down", 3), 429, apiclient.CodeResourceExhausted, "too many"},
		{"k8s timeout", apierrors.NewServerTimeout(gr, "get", 1), 503, apiclient.CodeUnavailable, "unavailable"},
		{"k8s gone", apierrors.NewResourceExpired("expired"), 410, apiclient.CodeFailedPrecondition, "expired"},
		{"rbac", &rbac.PermissionDeniedError{Resource: "applications", Action: "get", Object: "p/a"}, 403, apiclient.CodePermissionDenied, "permission denied"},
		{"rbac sentinel", fmt.Errorf("x: %w", rbac.ErrPermissionDenied), 403, apiclient.CodePermissionDenied, "permission denied"},
		{"token", fmt.Errorf("verify: %w", auth.ErrInvalidToken), 401, apiclient.CodeUnauthenticated, "invalid session"},
		{"deadline", context.DeadlineExceeded, 504, apiclient.CodeUnavailable, "deadline"},
		{"canceled", context.Canceled, 503, apiclient.CodeUnavailable, "canceled"},
		{"unknown hides cause", errors.New("db password is hunter2"), 500, apiclient.CodeInternal, "internal error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := rest.ProblemFor(tc.err)
			require.NotNil(t, e)
			assert.Equal(t, tc.status, e.Status)
			assert.Equal(t, tc.code, e.Code)
			assert.Contains(t, e.Detail, tc.detail)
			assert.NotContains(t, e.Detail, "hunter2")
			assert.NotContains(t, e.Detail, "serviceaccount")
		})
	}
	assert.Nil(t, rest.ProblemFor(nil))
	e := rest.ProblemFor(apierrors.NewTooManyRequests("slow down", 3))
	assert.Equal(t, 3*time.Second, e.RetryAfter)

	rec := httptest.NewRecorder()
	rest.WriteError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), apierrors.NewTooManyRequests("x", 3))
	assert.Equal(t, "3", rec.Header().Get("Retry-After"))
	assert.Equal(t, apiclient.ProblemContentType, rec.Header().Get("Content-Type"))
}

func TestRequestIDs(t *testing.T) {
	t.Parallel()
	var seen string
	h := rest.RequestIDs()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = rest.RequestID(r.Context()) }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(rest.RequestIDHeader, "abc-123")
	h.ServeHTTP(rec, req)
	assert.Equal(t, "abc-123", seen)
	assert.Equal(t, "abc-123", rec.Header().Get(rest.RequestIDHeader))

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(rest.RequestIDHeader, "bad id\nwith newline")
	h.ServeHTTP(rec, req)
	assert.Len(t, seen, 36, "malformed IDs are replaced by a UUID")
	assert.Equal(t, seen, rec.Header().Get(rest.RequestIDHeader))
}

func TestSecurityHeadersAndMaxBody(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			if _, err := io_ReadAll(r); err != nil {
				rest.WriteError(w, r, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := rest.Chain(ok,
		rest.SecurityHeaders(rest.SecurityHeadersConfig{FrameOptions: rest.DefaultFrameOptions, ContentSecurityPolicy: rest.DefaultContentSecurityPolicy, HSTS: true}),
		rest.MaxBody(8))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "sameorigin", rec.Header().Get("X-Frame-Options"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'self'")
	assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
	assert.Empty(t, rec.Header().Get("Strict-Transport-Security"), "HSTS only over TLS")

	req := httptest.NewRequest(http.MethodGet, "https://x/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, "max-age=31536000", rec.Header().Get("Strict-Transport-Security"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789")))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

	off := rest.SecurityHeaders(rest.SecurityHeadersConfig{})(ok)
	rec = httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Empty(t, rec.Header().Get("X-Frame-Options"))
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
}

func io_ReadAll(r *http.Request) ([]byte, error) { //nolint:revive // test helper
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func TestAccessLogOmitsSecrets(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, nil))
	h := newHarness(t, registerTestRoutes)
	tok := h.loginToken("alice")
	handler := rest.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.srv.Config.Handler.ServeHTTP(w, r) }),
		rest.RequestIDs(), rest.AccessLog(rest.AccessLogConfig{Logger: lg}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/echo/x?q=s3cr3t-query", strings.NewReader(`{"value":"s3cr3t-body"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	out := buf.String()
	assert.Contains(t, out, `"route":"POST /api/v1/echo/{name}"`)
	assert.Contains(t, out, `"actor":"alice"`)
	assert.NotContains(t, out, "s3cr3t")
	assert.NotContains(t, out, tok)
}
