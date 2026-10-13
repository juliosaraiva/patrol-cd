package services

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// WebhookHeader is a static request header.
type WebhookHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// WebhookBasicAuth holds basic authentication credentials.
type WebhookBasicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// WebhookOptions configure a webhook service.
type WebhookOptions struct {
	URL          string            `json:"url"`
	Headers      []WebhookHeader   `json:"headers"`
	BasicAuth    *WebhookBasicAuth `json:"basicAuth"`
	RetryWaitMin flexString        `json:"retryWaitMin"`
	RetryWaitMax flexString        `json:"retryWaitMax"`
	RetryMax     *flexString       `json:"retryMax"`
	TransportOptions
}

type webhookService struct {
	opts WebhookOptions
	http *httpClient
}

func newWebhookService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts WebhookOptions
	if err := decodeOptions(TypeWebhook, raw, &opts); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("%w: webhook: url is required", ErrInvalidOptions)
	}
	if _, err := parseEndpoint(opts.URL); err != nil {
		return nil, fmt.Errorf("%w: webhook: %v", ErrInvalidOptions, err)
	}
	for _, h := range opts.Headers {
		if strings.TrimSpace(h.Name) == "" {
			return nil, fmt.Errorf("%w: webhook: header without name", ErrInvalidOptions)
		}
		if isSensitiveHeader(h.Name) {
			r.Add(h.Value)
			// "Bearer xyz" / "token xyz": also redact the bare credential.
			if _, cred, ok := strings.Cut(h.Value, " "); ok {
				r.Add(cred)
			}
		}
	}
	if opts.BasicAuth != nil {
		r.Add(opts.BasicAuth.Password)
	}
	policy := deps.retry()
	if opts.RetryMax != nil {
		v, err := opts.RetryMax.int64()
		if err != nil || v < 0 {
			return nil, fmt.Errorf("%w: webhook: invalid retryMax", ErrInvalidOptions)
		}
		policy.MaxRetries = int(v)
	}
	if v := strings.TrimSpace(string(opts.RetryWaitMin)); v != "" {
		d, err := parseFlexDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%w: webhook: invalid retryWaitMin: %v", ErrInvalidOptions, err)
		}
		policy.WaitMin = d
	}
	if v := strings.TrimSpace(string(opts.RetryWaitMax)); v != "" {
		d, err := parseFlexDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%w: webhook: invalid retryWaitMax: %v", ErrInvalidOptions, err)
		}
		policy.WaitMax = d
	}
	hc, err := newHTTPClient(TypeWebhook, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	hc.retry = policy.normalized()
	return &webhookService{opts: opts, http: hc}, nil
}

func isSensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "auth") || strings.Contains(n, "token") || strings.Contains(n, "key") ||
		strings.Contains(n, "secret") || strings.Contains(n, "cookie") || strings.Contains(n, "signature")
}

func (s *webhookService) Send(ctx context.Context, n Notification, dest Destination) error {
	method := http.MethodGet
	target := s.opts.URL
	var body []byte
	if wn, ok := n.Webhook[dest.Service]; ok {
		if m := strings.ToUpper(strings.TrimSpace(wn.Method)); m != "" {
			method = m
		}
		if wn.Path != "" {
			target = appendWebhookPath(target, wn.Path)
		}
		if wn.Body != "" {
			body = []byte(wn.Body)
		}
	}
	if !validMethod(method) {
		return fmt.Errorf("%w: webhook: invalid method %q", ErrInvalidNotification, method)
	}
	header := http.Header{}
	for _, h := range s.opts.Headers {
		header.Add(h.Name, h.Value)
	}
	if s.opts.BasicAuth != nil {
		req := &http.Request{Header: http.Header{}}
		req.SetBasicAuth(s.opts.BasicAuth.Username, s.opts.BasicAuth.Password)
		header.Set("Authorization", req.Header.Get("Authorization"))
	}
	resp, err := s.http.do(ctx, method, target, header, body)
	if err != nil {
		return err
	}
	return s.http.expect2xx(method, target, resp)
}

// appendWebhookPath appends the rendered template path to the configured URL. Like the
// baseline it concatenates verbatim, so a path may continue a query string
// ("...?app=" + "{{.app.metadata.name}}") or a path segment; only the doubled slash of a
// base ending in "/" and a path starting with "/" is collapsed.
func appendWebhookPath(base, p string) string {
	if strings.HasSuffix(base, "/") && strings.HasPrefix(p, "/") {
		return base + p[1:]
	}
	return base + p
}

func validMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}
