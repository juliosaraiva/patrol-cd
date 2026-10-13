package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------
// Mattermost

// MattermostOptions configure a mattermost service.
type MattermostOptions struct {
	APIURL string `json:"apiURL"`
	Token  string `json:"token"`
	TransportOptions
}

type mattermostService struct {
	opts MattermostOptions
	http *httpClient
}

func newMattermostService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts MattermostOptions
	if err := decodeOptions(TypeMattermost, raw, &opts); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.APIURL) == "" || strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("%w: mattermost: apiURL and token are required", ErrInvalidOptions)
	}
	r.Add(opts.Token)
	hc, err := newHTTPClient(TypeMattermost, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &mattermostService{opts: opts, http: hc}, nil
}

func (s *mattermostService) Send(ctx context.Context, n Notification, dest Destination) error {
	if dest.Recipient == "" {
		return fmt.Errorf("%w: mattermost: recipient channel id is empty", ErrInvalidNotification)
	}
	payload := map[string]any{"channel_id": dest.Recipient, "message": n.Message}
	if n.Mattermost != nil && strings.TrimSpace(n.Mattermost.Attachments) != "" {
		var attachments []any
		if err := json.Unmarshal([]byte(n.Mattermost.Attachments), &attachments); err != nil {
			return fmt.Errorf("%w: mattermost: attachments is not a JSON array: %v", ErrInvalidNotification, err)
		}
		payload["props"] = map[string]any{"attachments": attachments}
	}
	endpoint := joinURL(s.opts.APIURL, "/api/v4/posts")
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.opts.Token)
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, payload)
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, endpoint, resp)
}

// ---------------------------------------------------------------------------------------
// Rocket.Chat

// RocketChatOptions configure a rocketchat service.
type RocketChatOptions struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	Alias     string `json:"alias"`
	Icon      string `json:"icon"`
	Avatar    string `json:"avatar"`
	ServerURL string `json:"serverUrl"`
	Scheme    string `json:"scheme"`
	TransportOptions
}

type rocketChatService struct {
	opts    RocketChatOptions
	baseURL string
	http    *httpClient
}

func newRocketChatService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts RocketChatOptions
	if err := decodeOptions(TypeRocketChat, raw, &opts); err != nil {
		return nil, err
	}
	if opts.Email == "" || opts.Password == "" {
		return nil, fmt.Errorf("%w: rocketchat: email and password are required", ErrInvalidOptions)
	}
	r.Add(opts.Password)
	base := strings.TrimSpace(opts.ServerURL)
	if base == "" {
		base = "open.rocket.chat"
	}
	if !strings.Contains(base, "://") {
		scheme := opts.Scheme
		if scheme == "" {
			scheme = "https"
		}
		base = scheme + "://" + base
	}
	hc, err := newHTTPClient(TypeRocketChat, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &rocketChatService{opts: opts, baseURL: base, http: hc}, nil
}

func (s *rocketChatService) Send(ctx context.Context, n Notification, dest Destination) error {
	if dest.Recipient == "" {
		return fmt.Errorf("%w: rocketchat: recipient is empty", ErrInvalidNotification)
	}
	// Log in for every delivery: tokens are never cached beyond one Send.
	loginURL := joinURL(s.baseURL, "/api/v1/login")
	resp, err := s.http.doJSON(ctx, http.MethodPost, loginURL, nil, map[string]string{"user": s.opts.Email, "password": s.opts.Password})
	if err != nil {
		return err
	}
	if err := s.http.expect2xx(http.MethodPost, loginURL, resp); err != nil {
		return err
	}
	var login struct {
		Status string `json:"status"`
		Data   struct {
			AuthToken string `json:"authToken"`
			UserID    string `json:"userId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &login); err != nil || login.Data.AuthToken == "" {
		return fmt.Errorf("%w: rocketchat: login did not return a token", ErrDeliveryFailed)
	}
	local := s.http.redactor.With(login.Data.AuthToken, login.Data.UserID)

	msg := map[string]any{"text": n.Message}
	if strings.HasPrefix(dest.Recipient, "#") || strings.HasPrefix(dest.Recipient, "@") {
		msg["channel"] = dest.Recipient
	} else {
		msg["roomId"] = dest.Recipient
	}
	if s.opts.Alias != "" {
		msg["alias"] = s.opts.Alias
	}
	if s.opts.Icon != "" {
		msg["emoji"] = s.opts.Icon
	}
	if s.opts.Avatar != "" {
		msg["avatar"] = s.opts.Avatar
	}
	if n.RocketChat != nil && strings.TrimSpace(n.RocketChat.Attachments) != "" {
		var attachments []any
		if err := json.Unmarshal([]byte(n.RocketChat.Attachments), &attachments); err != nil {
			return fmt.Errorf("%w: rocketchat: attachments is not a JSON array: %v", ErrInvalidNotification, err)
		}
		msg["attachments"] = attachments
	}
	postURL := joinURL(s.baseURL, "/api/v1/chat.postMessage")
	header := http.Header{}
	header.Set("X-Auth-Token", login.Data.AuthToken)
	header.Set("X-User-Id", login.Data.UserID)
	resp, err = s.http.doJSON(ctx, http.MethodPost, postURL, header, msg)
	if err != nil {
		return redactErr(local, err)
	}
	if err := s.http.expect2xx(http.MethodPost, postURL, resp); err != nil {
		return redactErr(local, err)
	}
	var out struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return fmt.Errorf("%w: rocketchat: invalid response: %v", ErrDeliveryFailed, err)
	}
	if !out.Success {
		return redactErr(local, fmt.Errorf("%w: rocketchat: %s", ErrDeliveryFailed, firstNonEmpty(out.Error, "message was not accepted")))
	}
	return nil
}

// ---------------------------------------------------------------------------------------
// Telegram

// TelegramOptions configure a telegram service. APIURL is a Patrol extension (default
// https://api.telegram.org) for proxies and tests.
type TelegramOptions struct {
	Token  string `json:"token"`
	APIURL string `json:"apiURL"`
	TransportOptions
}

type telegramService struct {
	opts TelegramOptions
	http *httpClient
}

func newTelegramService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts TelegramOptions
	if err := decodeOptions(TypeTelegram, raw, &opts); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("%w: telegram: token is required", ErrInvalidOptions)
	}
	r.Add(opts.Token)
	if opts.APIURL == "" {
		opts.APIURL = "https://api.telegram.org"
	}
	hc, err := newHTTPClient(TypeTelegram, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &telegramService{opts: opts, http: hc}, nil
}

// telegramChat parses a recipient: "-100123" (chat id), "-100123|42" (chat id + topic
// thread), a positive numeric chat id, or a public channel username (with or without @).
func telegramChat(recipient string) (chatID any, threadID int64, err error) {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return nil, 0, fmt.Errorf("%w: telegram: recipient is empty", ErrInvalidNotification)
	}
	parts := strings.SplitN(recipient, "|", 2)
	if id, perr := strconv.ParseInt(parts[0], 10, 64); perr == nil {
		if len(parts) == 2 {
			threadID, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return nil, 0, fmt.Errorf("%w: telegram: invalid thread id %q", ErrInvalidNotification, parts[1])
			}
		}
		return id, threadID, nil
	}
	if strings.HasPrefix(recipient, "-") {
		return nil, 0, fmt.Errorf("%w: telegram: invalid chat id %q", ErrInvalidNotification, parts[0])
	}
	return "@" + strings.TrimPrefix(recipient, "@"), 0, nil
}

func (s *telegramService) Send(ctx context.Context, n Notification, dest Destination) error {
	chatID, threadID, err := telegramChat(dest.Recipient)
	if err != nil {
		return err
	}
	payload := map[string]any{"chat_id": chatID, "text": n.Message, "parse_mode": "Markdown"}
	if threadID != 0 {
		payload["message_thread_id"] = threadID
	}
	endpoint := joinURL(s.opts.APIURL, "/bot"+s.opts.Token+"/sendMessage")
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, nil, payload)
	if err != nil {
		return err
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(resp.Body, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !out.OK {
		if out.Description != "" {
			return fmt.Errorf("%w: telegram: HTTP %d: %s", ErrDeliveryFailed, resp.StatusCode, s.http.redactor.Redact(out.Description))
		}
		return s.http.statusError(http.MethodPost, endpoint, resp)
	}
	return nil
}

// ---------------------------------------------------------------------------------------
// Webex

// WebexOptions configure a webex service. APIURL is a Patrol extension (default
// https://webexapis.com).
type WebexOptions struct {
	Token  string `json:"token"`
	APIURL string `json:"apiURL"`
	TransportOptions
}

type webexService struct {
	opts WebexOptions
	http *httpClient
}

func newWebexService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts WebexOptions
	if err := decodeOptions(TypeWebex, raw, &opts); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("%w: webex: token is required", ErrInvalidOptions)
	}
	r.Add(opts.Token)
	if opts.APIURL == "" {
		opts.APIURL = "https://webexapis.com"
	}
	hc, err := newHTTPClient(TypeWebex, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &webexService{opts: opts, http: hc}, nil
}

func (s *webexService) Send(ctx context.Context, n Notification, dest Destination) error {
	recipient := strings.TrimSpace(dest.Recipient)
	if recipient == "" {
		return fmt.Errorf("%w: webex: recipient is empty", ErrInvalidNotification)
	}
	payload := map[string]any{"markdown": n.Message}
	if addr, err := mail.ParseAddress(recipient); err == nil && addr.Address == recipient {
		payload["toPersonEmail"] = recipient
	} else {
		payload["roomId"] = recipient
	}
	endpoint := joinURL(s.opts.APIURL, "/v1/messages")
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.opts.Token)
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, payload)
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, endpoint, resp)
}

// ---------------------------------------------------------------------------------------
// Pushover

// PushoverOptions configure a pushover service. APIURL is a Patrol extension (default
// https://api.pushover.net).
type PushoverOptions struct {
	Token  string `json:"token"`
	APIURL string `json:"apiURL"`
	TransportOptions
}

type pushoverService struct {
	opts PushoverOptions
	http *httpClient
}

func newPushoverService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts PushoverOptions
	if err := decodeOptions(TypePushover, raw, &opts); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("%w: pushover: token is required", ErrInvalidOptions)
	}
	r.Add(opts.Token)
	if opts.APIURL == "" {
		opts.APIURL = "https://api.pushover.net"
	}
	hc, err := newHTTPClient(TypePushover, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &pushoverService{opts: opts, http: hc}, nil
}

func (s *pushoverService) Send(ctx context.Context, n Notification, dest Destination) error {
	if dest.Recipient == "" {
		return fmt.Errorf("%w: pushover: recipient user key is empty", ErrInvalidNotification)
	}
	r := s.http.redactor
	endpoint := joinURL(s.opts.APIURL, "/1/messages.json")
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, nil, map[string]string{
		"token":   s.opts.Token,
		"user":    dest.Recipient,
		"message": n.Message,
	})
	if err != nil {
		return err
	}
	var out struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(resp.Body, &out)
	if resp.StatusCode != http.StatusOK || out.Status != 1 {
		if len(out.Errors) > 0 {
			return fmt.Errorf("%w: pushover: HTTP %d: %s", ErrDeliveryFailed, resp.StatusCode, r.Redact(strings.Join(out.Errors, "; ")))
		}
		return s.http.statusError(http.MethodPost, endpoint, resp)
	}
	return nil
}

// ---------------------------------------------------------------------------------------
// Grafana

// GrafanaOptions configure a grafana service.
type GrafanaOptions struct {
	APIURL string `json:"apiUrl"`
	APIKey string `json:"apiKey"`
	TransportOptions
}

type grafanaService struct {
	opts GrafanaOptions
	http *httpClient
	deps Deps
}

func newGrafanaService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts GrafanaOptions
	if err := decodeOptions(TypeGrafana, raw, &opts); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.APIURL) == "" {
		return nil, fmt.Errorf("%w: grafana: apiUrl is required", ErrInvalidOptions)
	}
	r.Add(opts.APIKey)
	hc, err := newHTTPClient(TypeGrafana, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &grafanaService{opts: opts, http: hc, deps: deps}, nil
}

func (s *grafanaService) Send(ctx context.Context, n Notification, dest Destination) error {
	if n.Message == "" {
		s.deps.logger().Warn("grafana: annotation text is empty; set message in the template")
	}
	tags := []string{}
	for _, t := range strings.Split(dest.Recipient, "|") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	payload := map[string]any{
		"time":     s.deps.now().UnixMilli(),
		"isRegion": false,
		"tags":     tags,
		"text":     n.Message,
	}
	endpoint := joinURL(s.opts.APIURL, "/annotations")
	header := http.Header{}
	if s.opts.APIKey != "" {
		header.Set("Authorization", "Bearer "+s.opts.APIKey)
	}
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, payload)
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, endpoint, resp)
}
