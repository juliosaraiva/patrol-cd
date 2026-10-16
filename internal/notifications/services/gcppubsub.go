package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	pubsubScope           = "https://www.googleapis.com/auth/pubsub"
	defaultPubSubEndpoint = "https://pubsub.googleapis.com"
	maxKeyFileBytes       = 64 << 10
)

// GCPPubSubOptions configure a gcppubsub service. KeyJSON (inline service account key,
// normally a $secret reference) and Endpoint are Patrol extensions. Without KeyFile or
// KeyJSON, Application Default Credentials (e.g. GKE Workload Identity) are used. Only
// service_account keys are accepted (external_account keys can name arbitrary token and
// credential-source URLs). The key's token_uri must be https and pass Network.ValidateURL,
// and the token exchange client validates every request it sends, not only redirects.
type GCPPubSubOptions struct {
	Project  string `json:"project"`
	Topic    string `json:"topic"`
	KeyFile  string `json:"keyFile"`
	KeyJSON  string `json:"keyJSON"`
	Endpoint string `json:"endpoint"`
	TransportOptions
}

type gcpPubSubService struct {
	opts GCPPubSubOptions
	http *httpClient

	mu     sync.Mutex
	tokens oauth2.TokenSource
}

func newGCPPubSubService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts GCPPubSubOptions
	if err := decodeOptions(TypeGCPPubSub, raw, &opts); err != nil {
		return nil, err
	}
	r.Add(opts.KeyJSON)
	if strings.TrimSpace(opts.Project) == "" {
		return nil, fmt.Errorf("%w: gcppubsub: project is required", ErrInvalidOptions)
	}
	if opts.Endpoint == "" {
		opts.Endpoint = defaultPubSubEndpoint
	}
	hc, err := newHTTPClient(TypeGCPPubSub, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &gcpPubSubService{opts: opts, http: hc}, nil
}

// tokenSource lazily builds a caching token source. Failures are not cached.
func (s *gcpPubSubService) tokenSource() (oauth2.TokenSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens != nil {
		return s.tokens, nil
	}
	// The token source outlives any single delivery, so it gets a background context that
	// carries the guarded, validating HTTP client for token exchange.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, s.tokenHTTPClient())
	var creds *google.Credentials
	var err error
	switch {
	case s.opts.KeyJSON != "":
		if err := s.checkServiceAccountKey([]byte(s.opts.KeyJSON)); err != nil {
			return nil, err
		}
		creds, err = google.CredentialsFromJSONWithType(ctx, []byte(s.opts.KeyJSON), google.ServiceAccount, pubsubScope)
	case s.opts.KeyFile != "":
		data, rerr := readBoundedFile(s.opts.KeyFile, maxKeyFileBytes)
		if rerr != nil {
			return nil, fmt.Errorf("%w: gcppubsub: reading keyFile: %v", ErrInvalidOptions, rerr)
		}
		if err := s.checkServiceAccountKey(data); err != nil {
			return nil, err
		}
		creds, err = google.CredentialsFromJSONWithType(ctx, data, google.ServiceAccount, pubsubScope)
	default:
		creds, err = google.FindDefaultCredentials(ctx, pubsubScope)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: gcppubsub: loading credentials: %v", ErrInvalidOptions, err)
	}
	s.tokens = creds.TokenSource
	return s.tokens, nil
}

// checkServiceAccountKey applies the endpoint policy to the token endpoint named by a
// service account key (golang.org/x/oauth2/google honours its token_uri): https only, and
// the Network.ValidateURL hook. An absent token_uri means Google's default endpoint, which
// the validating token client still checks when it is contacted.
func (s *gcpPubSubService) checkServiceAccountKey(data []byte) error {
	var key struct {
		Type     string `json:"type"`
		TokenURI string `json:"token_uri"`
	}
	if err := json.Unmarshal(data, &key); err != nil {
		return fmt.Errorf("%w: gcppubsub: key is not valid JSON", ErrInvalidOptions)
	}
	if key.Type != string(google.ServiceAccount) {
		return fmt.Errorf("%w: gcppubsub: only %q keys are supported, got %q", ErrInvalidOptions, google.ServiceAccount, key.Type)
	}
	if strings.TrimSpace(key.TokenURI) == "" {
		return nil
	}
	u, err := parseEndpoint(key.TokenURI)
	if err != nil {
		return fmt.Errorf("gcppubsub: key token_uri: %w", err)
	}
	if err := validateEndpoint(u, []string{"https"}, s.http.validate); err != nil {
		return fmt.Errorf("gcppubsub: key token_uri: %w", err)
	}
	return nil
}

// tokenHTTPClient returns the client used for OAuth2 token exchange: the service's guarded
// client whose transport also validates the URL of every request (the client's redirect
// check alone does not cover the first request).
func (s *gcpPubSubService) tokenHTTPClient() *http.Client {
	c := *s.http.client
	c.Transport = validatingTransport{base: s.http.client.Transport, check: s.http.checkURL}
	return &c
}

// validatingTransport refuses requests whose URL fails check.
type validatingTransport struct {
	base  http.RoundTripper
	check func(*url.URL) error
}

func (t validatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.check(req.URL); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator configuration
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := readLimited(f, limit+1)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func (s *gcpPubSubService) Send(ctx context.Context, n Notification, dest Destination) error {
	topic := strings.TrimSpace(firstNonEmpty(dest.Recipient, s.opts.Topic))
	if topic == "" {
		return fmt.Errorf("%w: gcppubsub: topic is not configured", ErrInvalidNotification)
	}
	ts, err := s.tokenSource()
	if err != nil {
		return err
	}
	tok, err := ts.Token()
	if err != nil {
		return fmt.Errorf("%w: gcppubsub: obtaining access token: %v", ErrDeliveryFailed, err)
	}
	local := s.http.redactor.With(tok.AccessToken)
	msg := map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(n.Message))}
	if n.GCPPubSub != nil && len(n.GCPPubSub.Attributes) > 0 {
		msg["attributes"] = n.GCPPubSub.Attributes
	}
	endpoint := joinURL(s.opts.Endpoint, "/v1/projects/"+url.PathEscape(s.opts.Project)+"/topics/"+url.PathEscape(topic)+":publish")
	header := http.Header{}
	header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, map[string]any{"messages": []any{msg}})
	if err != nil {
		return redactErr(local, err)
	}
	if err := s.http.expect2xx(http.MethodPost, endpoint, resp); err != nil {
		return redactErr(local, err)
	}
	var out struct {
		MessageIDs []string `json:"messageIds"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil || len(out.MessageIDs) == 0 {
		return fmt.Errorf("%w: gcppubsub: publish returned no message id", ErrDeliveryFailed)
	}
	return nil
}
