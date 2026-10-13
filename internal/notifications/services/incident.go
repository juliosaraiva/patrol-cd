package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------------------
// Opsgenie

// OpsgenieOptions configure an opsgenie service. APIKeys maps a recipient (team id) to the
// team's API integration key.
type OpsgenieOptions struct {
	APIURL  string            `json:"apiUrl"`
	APIKeys map[string]string `json:"apiKeys"`
	TransportOptions
}

const (
	opsgenieMaxMessage     = 130
	opsgenieMaxAlias       = 512
	opsgenieMaxDescription = 15000
)

type opsgenieService struct {
	opts    OpsgenieOptions
	baseURL string
	http    *httpClient
	deps    Deps
}

func newOpsgenieService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts OpsgenieOptions
	if err := decodeOptions(TypeOpsgenie, raw, &opts); err != nil {
		return nil, err
	}
	for _, k := range opts.APIKeys {
		r.Add(k)
	}
	base := ensureScheme(opts.APIURL)
	if base == "" {
		base = "https://api.opsgenie.com"
	}
	hc, err := newHTTPClient(TypeOpsgenie, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &opsgenieService{opts: opts, baseURL: base, http: hc, deps: deps}, nil
}

func (s *opsgenieService) Send(ctx context.Context, n Notification, dest Destination) error {
	key, ok := s.opts.APIKeys[dest.Recipient]
	if !ok || key == "" {
		return fmt.Errorf("%w: opsgenie: no API key configured for recipient %q", ErrRecipientNotConfigured, dest.Recipient)
	}
	if strings.TrimSpace(n.Message) == "" {
		return fmt.Errorf("%w: opsgenie: message is required", ErrInvalidNotification)
	}
	alert := map[string]any{
		"message":    truncateRunes(n.Message, opsgenieMaxMessage),
		"source":     s.deps.product(),
		"responders": []any{map[string]string{"type": "team", "id": dest.Recipient}},
	}
	if o := n.Opsgenie; o != nil {
		setIf(alert, "description", truncateRunes(o.Description, opsgenieMaxDescription))
		setIf(alert, "priority", o.Priority)
		setIf(alert, "alias", truncateRunes(o.Alias, opsgenieMaxAlias))
		setIf(alert, "note", o.Note)
		setIf(alert, "entity", o.Entity)
		setIf(alert, "user", o.User)
		if len(o.Actions) > 0 {
			alert["actions"] = o.Actions
		}
		if len(o.Tags) > 0 {
			alert["tags"] = o.Tags
		}
		if len(o.Details) > 0 {
			alert["details"] = o.Details
		}
		if len(o.VisibleTo) > 0 {
			vis := make([]OpsgenieResponder, 0, len(o.VisibleTo))
			for _, v := range o.VisibleTo {
				v.Type = strings.ToLower(v.Type)
				if v.Type != "team" && v.Type != "user" {
					return fmt.Errorf("%w: opsgenie: visibleTo type must be team or user, got %q", ErrInvalidNotification, v.Type)
				}
				vis = append(vis, v)
			}
			alert["visibleTo"] = vis
		}
		if p := o.Priority; p != "" {
			switch p {
			case "P1", "P2", "P3", "P4", "P5":
			default:
				return fmt.Errorf("%w: opsgenie: priority must be one of P1..P5, got %q", ErrInvalidNotification, p)
			}
		}
	}
	endpoint := joinURL(s.baseURL, "/v2/alerts")
	header := http.Header{}
	header.Set("Authorization", "GenieKey "+key)
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, alert)
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, endpoint, resp)
}

func setIf(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

// ---------------------------------------------------------------------------------------
// PagerDuty (REST API v2 incidents)

// PagerDutyOptions configure a pagerduty service. APIURL is a Patrol extension (default
// https://api.pagerduty.com). PagerdutyToken is accepted as an alias of Token.
type PagerDutyOptions struct {
	Token          string `json:"token"`
	PagerdutyToken string `json:"pagerdutyToken"`
	From           string `json:"from"`
	ServiceID      string `json:"serviceID"`
	APIURL         string `json:"apiURL"`
	TransportOptions
}

type pagerDutyService struct {
	opts PagerDutyOptions
	http *httpClient
}

func newPagerDutyService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts PagerDutyOptions
	if err := decodeOptions(TypePagerDuty, raw, &opts); err != nil {
		return nil, err
	}
	opts.Token = firstNonEmpty(opts.Token, opts.PagerdutyToken)
	if opts.Token == "" {
		return nil, fmt.Errorf("%w: pagerduty: token is required", ErrInvalidOptions)
	}
	r.Add(opts.Token)
	if opts.APIURL == "" {
		opts.APIURL = "https://api.pagerduty.com"
	}
	hc, err := newHTTPClient(TypePagerDuty, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &pagerDutyService{opts: opts, http: hc}, nil
}

func (s *pagerDutyService) Send(ctx context.Context, n Notification, dest Destination) error {
	var pd PagerDutyNotification
	if n.PagerDuty != nil {
		pd = *n.PagerDuty
	}
	title := firstNonEmpty(pd.Title, n.Message)
	if title == "" {
		return fmt.Errorf("%w: pagerduty: title is required", ErrInvalidNotification)
	}
	serviceID := firstNonEmpty(dest.Recipient, s.opts.ServiceID)
	if serviceID == "" {
		return fmt.Errorf("%w: pagerduty: service id (recipient) is required", ErrInvalidNotification)
	}
	incident := map[string]any{
		"type":    "incident",
		"title":   title,
		"service": map[string]string{"id": serviceID, "type": "service_reference"},
	}
	if pd.Urgency != "" {
		incident["urgency"] = pd.Urgency
	}
	if pd.Body != "" {
		incident["body"] = map[string]string{"type": "incident_body", "details": pd.Body}
	}
	if pd.PriorityID != "" {
		incident["priority"] = map[string]string{"id": pd.PriorityID, "type": "priority_reference"}
	}
	endpoint := joinURL(s.opts.APIURL, "/incidents")
	header := http.Header{}
	header.Set("Authorization", "Token token="+s.opts.Token)
	header.Set("Accept", "application/vnd.pagerduty+json;version=2")
	if s.opts.From != "" {
		header.Set("From", s.opts.From)
	}
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, map[string]any{"incident": incident})
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, endpoint, resp)
}

// ---------------------------------------------------------------------------------------
// PagerDuty Events API v2

// PagerDutyV2Options configure a pagerdutyv2 service. ServiceKeys maps a recipient to an
// Events API v2 integration (routing) key. APIURL is a Patrol extension (default
// https://events.pagerduty.com).
type PagerDutyV2Options struct {
	ServiceKeys map[string]string `json:"serviceKeys"`
	APIURL      string            `json:"apiURL"`
	TransportOptions
}

type pagerDutyV2Service struct {
	opts PagerDutyV2Options
	http *httpClient
	deps Deps
}

func newPagerDutyV2Service(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts PagerDutyV2Options
	if err := decodeOptions(TypePagerDutyV2, raw, &opts); err != nil {
		return nil, err
	}
	for _, k := range opts.ServiceKeys {
		r.Add(k)
	}
	if opts.APIURL == "" {
		opts.APIURL = "https://events.pagerduty.com"
	}
	hc, err := newHTTPClient(TypePagerDutyV2, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &pagerDutyV2Service{opts: opts, http: hc, deps: deps}, nil
}

func (s *pagerDutyV2Service) Send(ctx context.Context, n Notification, dest Destination) error {
	key, ok := s.opts.ServiceKeys[dest.Recipient]
	if !ok || key == "" {
		return fmt.Errorf("%w: pagerdutyv2: no routing key configured for recipient %q", ErrRecipientNotConfigured, dest.Recipient)
	}
	if n.PagerDutyV2 == nil {
		return fmt.Errorf("%w: pagerdutyv2: template has no pagerdutyv2 section", ErrInvalidNotification)
	}
	pd := *n.PagerDutyV2
	if pd.Summary == "" || pd.Severity == "" || pd.Source == "" {
		return fmt.Errorf("%w: pagerdutyv2: summary, severity and source are required", ErrInvalidNotification)
	}
	switch pd.Severity {
	case "critical", "error", "warning", "info":
	default:
		return fmt.Errorf("%w: pagerdutyv2: severity must be critical, error, warning or info, got %q", ErrInvalidNotification, pd.Severity)
	}
	payload := map[string]any{"summary": pd.Summary, "severity": pd.Severity, "source": pd.Source}
	setIf(payload, "component", pd.Component)
	setIf(payload, "group", pd.Group)
	setIf(payload, "class", pd.Class)
	event := map[string]any{
		"routing_key":  key,
		"event_action": "trigger",
		"payload":      payload,
		"client":       s.deps.product(),
	}
	setIf(event, "dedup_key", pd.DedupKey)
	setIf(event, "client_url", pd.URL)
	endpoint := joinURL(s.opts.APIURL, "/v2/enqueue")
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, nil, event)
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, endpoint, resp)
}

// ---------------------------------------------------------------------------------------
// New Relic

// NewRelicOptions configure a newrelic service.
type NewRelicOptions struct {
	APIURL string `json:"apiURL"`
	APIKey string `json:"apiKey"`
	TransportOptions
}

type newRelicService struct {
	opts NewRelicOptions
	http *httpClient
}

func newNewRelicService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts NewRelicOptions
	if err := decodeOptions(TypeNewRelic, raw, &opts); err != nil {
		return nil, err
	}
	if opts.APIKey == "" {
		return nil, fmt.Errorf("%w: newrelic: apiKey is required", ErrInvalidOptions)
	}
	r.Add(opts.APIKey)
	if opts.APIURL == "" {
		opts.APIURL = "https://api.newrelic.com"
	}
	hc, err := newHTTPClient(TypeNewRelic, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &newRelicService{opts: opts, http: hc}, nil
}

func (s *newRelicService) Send(ctx context.Context, n Notification, dest Destination) error {
	appID := strings.TrimSpace(dest.Recipient)
	if appID == "" {
		return fmt.Errorf("%w: newrelic: recipient application id is empty", ErrInvalidNotification)
	}
	var nr NewRelicNotification
	if n.NewRelic != nil {
		nr = *n.NewRelic
	}
	if nr.Revision == "" {
		return fmt.Errorf("%w: newrelic: revision is required", ErrInvalidNotification)
	}
	deployment := map[string]string{"revision": nr.Revision}
	if d := firstNonEmpty(nr.Description, n.Message); d != "" {
		deployment["description"] = d
	}
	if nr.Changelog != "" {
		deployment["changelog"] = nr.Changelog
	}
	if nr.User != "" {
		deployment["user"] = nr.User
	}
	endpoint := joinURL(s.opts.APIURL, "/v2/applications/"+url.PathEscape(appID)+"/deployments.json")
	header := http.Header{}
	header.Set("X-Api-Key", s.opts.APIKey)
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, map[string]any{"deployment": deployment})
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, endpoint, resp)
}

// ---------------------------------------------------------------------------------------
// Alertmanager

// AlertmanagerBasicAuth holds basic authentication credentials.
type AlertmanagerBasicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// AlertmanagerOptions configure an alertmanager service.
type AlertmanagerOptions struct {
	Targets             []string               `json:"targets"`
	Scheme              string                 `json:"scheme"`
	APIPath             string                 `json:"apiPath"`
	InsecureSkipVerify  flexBool               `json:"insecureSkipVerify"`
	BasicAuth           *AlertmanagerBasicAuth `json:"basicAuth"`
	BearerToken         string                 `json:"bearerToken"`
	Timeout             flexString             `json:"timeout"`
	MaxIdleConns        flexString             `json:"maxIdleConns"`
	MaxIdleConnsPerHost flexString             `json:"maxIdleConnsPerHost"`
	MaxConnsPerHost     flexString             `json:"maxConnsPerHost"`
	IdleConnTimeout     flexString             `json:"idleConnTimeout"`
}

type alertmanagerService struct {
	opts    AlertmanagerOptions
	http    *httpClient
	timeout time.Duration
}

func newAlertmanagerService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts AlertmanagerOptions
	if err := decodeOptions(TypeAlertmanager, raw, &opts); err != nil {
		return nil, err
	}
	if len(opts.Targets) == 0 {
		return nil, fmt.Errorf("%w: alertmanager: at least one target is required", ErrInvalidOptions)
	}
	if opts.Scheme == "" {
		opts.Scheme = "http"
	}
	if opts.APIPath == "" {
		opts.APIPath = "/api/v2/alerts"
	}
	timeout := 3 * time.Second
	if t := strings.TrimSpace(string(opts.Timeout)); t != "" {
		d, err := parseFlexDuration(t)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("%w: alertmanager: invalid timeout %q", ErrInvalidOptions, t)
		}
		timeout = d
	}
	if opts.BasicAuth != nil {
		r.Add(opts.BasicAuth.Password)
	}
	r.Add(opts.BearerToken)
	topts := TransportOptions{
		InsecureSkipVerify:  opts.InsecureSkipVerify,
		MaxIdleConns:        opts.MaxIdleConns,
		MaxIdleConnsPerHost: opts.MaxIdleConnsPerHost,
		MaxConnsPerHost:     opts.MaxConnsPerHost,
		IdleConnTimeout:     opts.IdleConnTimeout,
	}
	hc, err := newHTTPClient(TypeAlertmanager, deps, topts, r, nil)
	if err != nil {
		return nil, err
	}
	return &alertmanagerService{opts: opts, http: hc, timeout: timeout}, nil
}

func (s *alertmanagerService) Send(ctx context.Context, n Notification, _ Destination) error {
	if n.Alertmanager == nil {
		return fmt.Errorf("%w: alertmanager: template has no alertmanager section", ErrInvalidNotification)
	}
	am := *n.Alertmanager
	if len(am.Labels) == 0 {
		return fmt.Errorf("%w: alertmanager: at least one label pair is required", ErrInvalidNotification)
	}
	alert := map[string]any{"labels": am.Labels}
	if len(am.Annotations) > 0 {
		alert["annotations"] = am.Annotations
	}
	if g := strings.TrimSpace(am.GeneratorURL); g != "" {
		// Alertmanager rejects non-URI generator URLs (e.g. scp-like git URLs).
		if u, err := url.Parse(g); err == nil && u.Scheme != "" && u.Host != "" {
			alert["generatorURL"] = g
		}
	}
	payload := []any{alert}
	header := http.Header{}
	if s.opts.BasicAuth != nil && s.opts.BasicAuth.Username != "" {
		req := &http.Request{Header: http.Header{}}
		req.SetBasicAuth(s.opts.BasicAuth.Username, s.opts.BasicAuth.Password)
		header.Set("Authorization", req.Header.Get("Authorization"))
	} else if s.opts.BearerToken != "" {
		header.Set("Authorization", "Bearer "+s.opts.BearerToken)
	}

	targets := append([]string(nil), s.opts.Targets...)
	sort.Strings(targets)
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			tctx, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			endpoint := (&url.URL{Scheme: s.opts.Scheme, Host: strings.TrimSpace(target)}).String() + "/" + strings.TrimPrefix(s.opts.APIPath, "/")
			resp, err := s.http.doJSON(tctx, http.MethodPost, endpoint, header.Clone(), payload)
			if err == nil {
				err = s.http.expect2xx(http.MethodPost, endpoint, resp)
			}
			if err != nil {
				errs[i] = fmt.Errorf("target %s: %w", strconv.Quote(target), err)
			}
		}(i, target)
	}
	wg.Wait()
	var failed []error
	for _, e := range errs {
		if e != nil {
			failed = append(failed, e)
		}
	}
	if len(failed) == len(targets) {
		return fmt.Errorf("alertmanager: all targets failed: %w", errors.Join(failed...))
	}
	return nil
}
