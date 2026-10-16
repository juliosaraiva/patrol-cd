package treeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
)

// ShardResolver returns the shard serving an Application (sharding.StatusShardResolver).
type ShardResolver interface {
	ShardForApplication(ctx context.Context, app *v1alpha1.Application) (int, error)
}

// HolderResolver returns the pod currently holding a shard's Lease, "" when none
// (*sharding.LeaseHolders).
type HolderResolver interface {
	ShardHolder(ctx context.Context, shard int) (string, error)
}

// Router picks the controller replica for an Application.
type Router interface {
	// Shard returns the shard serving app.
	Shard(ctx context.Context, app *v1alpha1.Application) (int, error)
	// Endpoint returns the base URL ("scheme://host:port") of the replica serving shard.
	Endpoint(ctx context.Context, shard int) (string, error)
}

// invalidator is implemented by routers that cache shard holders.
type invalidator interface {
	Invalidate(shard int)
}

// StaticRouter sends every request to one URL (a single controller replica, tests).
type StaticRouter struct {
	URL string
}

// Shard implements Router.
func (StaticRouter) Shard(context.Context, *v1alpha1.Application) (int, error) { return 0, nil }

// Endpoint implements Router.
func (r StaticRouter) Endpoint(context.Context, int) (string, error) { return r.URL, nil }

// Defaults of ShardRouter.
const (
	DefaultStatefulSet = "patrol-controller"
	DefaultService     = "patrol-controller"
)

// ShardRouter routes to controller pods through the StatefulSet's headless Service:
// <pod>.<Service>.<Namespace>.svc:<Port>, where <pod> is the current Lease holder of the
// shard (when Holders knows one) or <StatefulSet>-<shard>.
type ShardRouter struct {
	// Shards resolves the Application's shard (nil: shard 0).
	Shards ShardResolver
	// Holders resolves the pod holding a shard (nil: the ordinal pod).
	Holders HolderResolver
	// StatefulSet is the controller StatefulSet name (DefaultStatefulSet).
	StatefulSet string
	// Service is the headless Service name (DefaultService).
	Service string
	// Namespace is the control-plane namespace (required).
	Namespace string
	// Port is the tree API port (DefaultPort).
	Port int
	// Scheme is "http" (default) or "https".
	Scheme string
	// Logger receives holder lookup failures (nil discards).
	Logger *slog.Logger
}

// Shard implements Router.
func (r *ShardRouter) Shard(ctx context.Context, app *v1alpha1.Application) (int, error) {
	if r.Shards == nil {
		return 0, nil
	}
	s, err := r.Shards.ShardForApplication(ctx, app)
	if err != nil {
		return 0, fmt.Errorf("resolve controller shard: %w", err)
	}
	return max(s, 0), nil
}

// Endpoint implements Router.
func (r *ShardRouter) Endpoint(ctx context.Context, shard int) (string, error) {
	if r.Namespace == "" {
		return "", errors.New("treeapi: router namespace is required")
	}
	sts := cmpOr(r.StatefulSet, DefaultStatefulSet)
	svc := cmpOr(r.Service, DefaultService)
	scheme := cmpOr(r.Scheme, "http")
	port := r.Port
	if port <= 0 {
		port = DefaultPort
	}
	pod := sts + "-" + strconv.Itoa(shard)
	if r.Holders != nil {
		holder, err := r.Holders.ShardHolder(ctx, shard)
		switch {
		case err != nil:
			if r.Logger != nil {
				r.Logger.Warn("cannot resolve the shard holder; using the ordinal pod", slog.Int("shard", shard), slog.Any("error", err))
			}
		case holder != "" && len(validation.IsDNS1123Label(holder)) == 0:
			pod = holder
		}
	}
	return fmt.Sprintf("%s://%s.%s.%s.svc:%d", scheme, pod, svc, r.Namespace, port), nil
}

// Invalidate drops cached holder information of shard.
func (r *ShardRouter) Invalidate(shard int) {
	if inv, ok := r.Holders.(invalidator); ok {
		inv.Invalidate(shard)
	}
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// DefaultClientTimeout bounds one request.
const DefaultClientTimeout = 60 * time.Second

// Client calls the tree API of the replica serving each Application.
type Client struct {
	router           Router
	token            string
	hc               *http.Client
	maxResponseBytes int64
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithHTTPClient sets the HTTP client (TLS/mTLS transports); its Timeout bounds requests.
func WithHTTPClient(hc *http.Client) ClientOption { return func(c *Client) { c.hc = hc } }

// WithMaxResponseBytes caps response bodies (DefaultMaxResponseBytes).
func WithMaxResponseBytes(n int64) ClientOption { return func(c *Client) { c.maxResponseBytes = n } }

// NewClient returns a client authenticating with the internal token.
func NewClient(router Router, token string, opts ...ClientOption) (*Client, error) {
	if router == nil {
		return nil, errors.New("treeapi client: a router is required")
	}
	if token == "" {
		return nil, errors.New("treeapi client: an internal token is required")
	}
	c := &Client{router: router, token: token, hc: &http.Client{Timeout: DefaultClientTimeout}, maxResponseBytes: DefaultMaxResponseBytes}
	for _, o := range opts {
		o(c)
	}
	if c.maxResponseBytes <= 0 {
		c.maxResponseBytes = DefaultMaxResponseBytes
	}
	return c, nil
}

func appQuery(app *v1alpha1.Application) url.Values {
	return url.Values{"namespace": {app.Namespace}, "name": {app.Name}}
}

// Tree returns the Application's resource tree.
func (c *Client) Tree(ctx context.Context, app *v1alpha1.Application) (*compare.ApplicationTree, error) {
	tree, _, _, err := c.TreeIfChanged(ctx, app, "")
	return tree, err
}

// TreeIfChanged returns the tree and its ETag; when etag matches the current tree, changed
// is false and tree is nil.
func (c *Client) TreeIfChanged(ctx context.Context, app *v1alpha1.Application, etag string) (tree *compare.ApplicationTree, newETag string, changed bool, err error) {
	var hdr http.Header
	if etag != "" {
		hdr = http.Header{"If-None-Match": {etag}}
	}
	resp, err := c.do(ctx, app, http.MethodGet, PathTree, appQuery(app), nil, hdr)
	if err != nil {
		return nil, "", false, err
	}
	if resp.status == http.StatusNotModified {
		return nil, etag, false, nil
	}
	var out compare.ApplicationTree
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return nil, "", false, fmt.Errorf("treeapi client: decode tree: %w", err)
	}
	return &out, resp.header.Get("ETag"), true, nil
}

// ManagedResources returns the Application's managed resources (hooks excluded, Secrets
// masked) matching q.
func (c *Client) ManagedResources(ctx context.Context, app *v1alpha1.Application, q ManagedResourcesQuery) ([]ResourceDiff, error) {
	v := appQuery(app)
	setNonEmpty(v, "group", q.Group)
	setNonEmpty(v, "kind", q.Kind)
	setNonEmpty(v, "resourceNamespace", q.ResourceNamespace)
	setNonEmpty(v, "resourceName", q.ResourceName)
	resp, err := c.do(ctx, app, http.MethodGet, PathManagedResources, v, nil, nil)
	if err != nil {
		return nil, err
	}
	var out ManagedResourcesResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return nil, fmt.Errorf("treeapi client: decode managed resources: %w", err)
	}
	return out.Items, nil
}

// ServerSideDiff runs a server-side diff of index-aligned live resources and target
// manifests.
func (c *Client) ServerSideDiff(ctx context.Context, app *v1alpha1.Application, live []ResourceDiff, targets []string) (*ServerSideDiffResponse, error) {
	body, err := json.Marshal(ServerSideDiffRequest{Namespace: app.Namespace, Name: app.Name, LiveResources: live, TargetManifests: targets})
	if err != nil {
		return nil, fmt.Errorf("treeapi client: encode request: %w", err)
	}
	resp, err := c.do(ctx, app, http.MethodPost, PathServerSideDiff, nil, body, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return nil, err
	}
	var out ServerSideDiffResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return nil, fmt.Errorf("treeapi client: decode server-side diff: %w", err)
	}
	return &out, nil
}

// Resource returns the live manifest (JSON, Secret data masked) of a resource of the
// Application's tree.
func (c *Client) Resource(ctx context.Context, app *v1alpha1.Application, q ResourceQuery) (string, error) {
	v := appQuery(app)
	setNonEmpty(v, "group", q.Group)
	setNonEmpty(v, "version", q.Version)
	setNonEmpty(v, "kind", q.Kind)
	setNonEmpty(v, "resourceNamespace", q.ResourceNamespace)
	setNonEmpty(v, "resourceName", q.ResourceName)
	resp, err := c.do(ctx, app, http.MethodGet, PathResource, v, nil, nil)
	if err != nil {
		return "", err
	}
	var out ResourceResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return "", fmt.Errorf("treeapi client: decode resource: %w", err)
	}
	return out.Manifest, nil
}

func setNonEmpty(v url.Values, k, val string) {
	if val != "" {
		v.Set(k, val)
	}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends a request to the replica serving app. A 421 answer naming another shard, or a
// transport error, is retried once against a freshly resolved endpoint.
func (c *Client) do(ctx context.Context, app *v1alpha1.Application, method, path string, query url.Values, body []byte, hdr http.Header) (*response, error) {
	if app == nil {
		return nil, errors.New("treeapi client: application is required")
	}
	shard, err := c.router.Shard(ctx, app)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := range 2 {
		base, err := c.router.Endpoint(ctx, shard)
		if err != nil {
			return nil, fmt.Errorf("treeapi client: %w", err)
		}
		resp, err := c.send(ctx, base, method, path, query, body, hdr)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("treeapi client: %w", err)
			}
			lastErr = fmt.Errorf("treeapi client: %w: %w", ErrUnavailable, err)
			c.invalidate(shard)
			continue
		}
		if resp.status == http.StatusOK || resp.status == http.StatusNotModified {
			return resp, nil
		}
		apiErr := decodeError(resp)
		if resp.status == http.StatusMisdirectedRequest && attempt == 0 {
			c.invalidate(shard)
			if apiErr.Shard != nil && *apiErr.Shard >= 0 {
				shard = *apiErr.Shard
			}
			lastErr = apiErr
			continue
		}
		return nil, apiErr
	}
	return nil, lastErr
}

func (c *Client) invalidate(shard int) {
	if inv, ok := c.router.(invalidator); ok {
		inv.Invalidate(shard)
	}
}

func (c *Client) send(ctx context.Context, base, method, path string, query url.Values, body []byte, hdr http.Header) (*response, error) {
	u := strings.TrimSuffix(base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json, application/problem+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(data)) > c.maxResponseBytes {
		return &response{status: http.StatusRequestEntityTooLarge, header: resp.Header,
			body: problemBody(http.StatusRequestEntityTooLarge, CodeTooLarge, fmt.Sprintf("response exceeds %d bytes", c.maxResponseBytes))}, nil
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func problemBody(status int, code, detail string) []byte {
	b, _ := json.Marshal(problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail, Code: code})
	return b
}

// decodeError turns a non-success response into *Error.
func decodeError(resp *response) *Error {
	e := &Error{}
	if json.Unmarshal(resp.body, e) != nil || e.Code == "" {
		e = &Error{Code: codeForStatus(resp.status), Message: fmt.Sprintf("unexpected status %d", resp.status)}
	}
	e.Status = resp.status
	if e.Shard == nil {
		if s, err := strconv.Atoi(resp.header.Get(HeaderShard)); err == nil {
			e.Shard = &s
		}
	}
	return e
}

func codeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusBadRequest:
		return CodeInvalidArgument
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusMisdirectedRequest:
		return CodeMisdirected
	case http.StatusRequestEntityTooLarge:
		return CodeTooLarge
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return CodeUnavailable
	case http.StatusGatewayTimeout:
		return CodeDeadlineExceeded
	default:
		return CodeInternal
	}
}
