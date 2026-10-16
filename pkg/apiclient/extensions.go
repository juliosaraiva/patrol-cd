package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PathExtension is the proxy extension route (outside /api/v1, as upstream): the rest of
// the path after the extension name is forwarded to the extension's backend.
const PathExtension = "/extensions/{extension}/{path...}"

// Proxy extension headers. Requests name the Application they act for; the server adds
// the caller and destination headers to the forwarded request (Argo CD's Argocd-* headers).
const (
	// HeaderExtensionApplication is "<application namespace>:<application name>".
	HeaderExtensionApplication = "Patrol-Application-Name"
	// HeaderExtensionProject is the Application's project.
	HeaderExtensionProject = "Patrol-Project-Name"

	// Set by the server on forwarded requests (client-supplied values are dropped).
	HeaderExtensionNamespace         = "Patrol-Namespace"
	HeaderExtensionTargetClusterName = "Patrol-Target-Cluster-Name"
	HeaderExtensionTargetClusterURL  = "Patrol-Target-Cluster-URL"
	HeaderExtensionUserID            = "Patrol-User-Id"
	HeaderExtensionUsername          = "Patrol-Username"
	HeaderExtensionUserGroups        = "Patrol-User-Groups"
)

// ExtensionRequest documents the path parameters of the proxy extension route.
type ExtensionRequest struct {
	// Extension is the extension name.
	Extension string `path:"extension" json:"-"`
	// Path is forwarded to the backend (may contain slashes).
	Path string `path:"path" json:"-"`
}

// ExtensionCall is one proxy extension request.
type ExtensionCall struct {
	// Method defaults to GET.
	Method    string
	Extension string
	// Path is forwarded to the backend; a leading slash is optional.
	Path  string
	Query url.Values
	// AppNamespace, AppName and Project identify the Application the call is made for.
	AppNamespace string
	AppName      string
	Project      string
	// Header holds additional request headers (only allow-listed ones are forwarded).
	Header http.Header
	// Body and ContentType are the request body.
	Body        io.Reader
	ContentType string
}

// InvokeExtension sends a proxy extension request and returns the response whatever its
// status: backend responses pass through unchanged, while failures produced by the API
// server itself (authorization, unknown extension, unreachable backend) are problem
// documents that ResponseError decodes. The caller closes the response body.
func (c *Client) InvokeExtension(ctx context.Context, call *ExtensionCall) (*http.Response, error) {
	if call == nil || call.Extension == "" {
		return nil, errors.New("apiclient: extension name is required")
	}
	method := call.Method
	if method == "" {
		method = http.MethodGet
	}
	segs := strings.Split(strings.TrimPrefix(call.Path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := *c.base
	u.Path = c.base.Path + "/extensions/" + call.Extension + "/" + strings.TrimPrefix(call.Path, "/")
	u.RawPath = c.base.Path + "/extensions/" + url.PathEscape(call.Extension) + "/" + strings.Join(segs, "/")
	u.RawQuery = call.Query.Encode()
	hr, err := http.NewRequestWithContext(ctx, method, u.String(), call.Body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, vs := range c.headers {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	for k, vs := range call.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	hr.Header.Set("User-Agent", c.userAgent)
	hr.Header.Set(CSRFHeader, CSRFHeaderValue)
	if call.AppName != "" {
		hr.Header.Set(HeaderExtensionApplication, call.AppNamespace+":"+call.AppName)
	}
	if call.Project != "" {
		hr.Header.Set(HeaderExtensionProject, call.Project)
	}
	if call.Body != nil && call.ContentType != "" {
		hr.Header.Set("Content-Type", call.ContentType)
	}
	if tok := c.Token(); tok != "" {
		hr.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.hc.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("%s /extensions/%s: %w", method, call.Extension, err)
	}
	c.captureRenewal(resp)
	return resp, nil
}

// ResponseError returns the *APIError of a non-2xx response (decoding a problem document
// when present), or nil for 2xx responses. It reads (part of) the body.
func ResponseError(resp *http.Response) error {
	if resp == nil {
		return errors.New("apiclient: nil response")
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	return errorFromResponse(resp)
}
