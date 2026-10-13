package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	defaultSlackAPIURL = "https://slack.com/api/"
	// slackThreadCacheSize bounds the in-memory groupingKey -> message map.
	slackThreadCacheSize = 10000
)

// SlackOptions configure a slack service.
type SlackOptions struct {
	Token         string   `json:"token"`
	Username      string   `json:"username"`
	Icon          string   `json:"icon"`
	APIURL        string   `json:"apiURL"`
	Channels      []string `json:"channels"`
	SigningSecret string   `json:"signingSecret"`
	DisableUnfurl flexBool `json:"disableUnfurl"`
	TransportOptions
}

type slackThread struct {
	channelID string
	ts        string
}

type slackService struct {
	opts    SlackOptions
	apiURL  string
	http    *httpClient
	threads *lru.Cache[string, slackThread]
	deps    Deps
}

func newSlackService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts SlackOptions
	if err := decodeOptions(TypeSlack, raw, &opts); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("%w: slack: token is required", ErrInvalidOptions)
	}
	r.Add(opts.Token, opts.SigningSecret)
	api := strings.TrimSpace(opts.APIURL)
	if api == "" {
		api = defaultSlackAPIURL
	}
	if !strings.HasSuffix(api, "/") {
		api += "/"
	}
	hc, err := newHTTPClient(TypeSlack, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	cache, err := lru.New[string, slackThread](slackThreadCacheSize)
	if err != nil {
		return nil, fmt.Errorf("slack: thread cache: %w", err)
	}
	return &slackService{opts: opts, apiURL: api, http: hc, threads: cache, deps: deps}, nil
}

type slackAPIResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

func (s *slackService) Send(ctx context.Context, n Notification, dest Destination) error {
	channel := strings.TrimSpace(dest.Recipient)
	if channel == "" {
		return fmt.Errorf("%w: slack: recipient channel is empty", ErrInvalidNotification)
	}
	payload := map[string]any{"channel": channel, "text": n.Message}
	var sn SlackNotification
	if n.Slack != nil {
		sn = *n.Slack
	}
	if strings.TrimSpace(sn.Attachments) != "" {
		var attachments []any
		if err := json.Unmarshal([]byte(sn.Attachments), &attachments); err != nil {
			return fmt.Errorf("%w: slack: attachments is not a JSON array: %v", ErrInvalidNotification, err)
		}
		payload["attachments"] = attachments
	}
	if strings.TrimSpace(sn.Blocks) != "" {
		var blocks []any
		if err := json.Unmarshal([]byte(sn.Blocks), &blocks); err != nil {
			return fmt.Errorf("%w: slack: blocks is not a JSON array: %v", ErrInvalidNotification, err)
		}
		payload["blocks"] = blocks
	}
	username := firstNonEmpty(sn.Username, s.opts.Username)
	if username != "" {
		payload["username"] = username
	}
	icon := strings.TrimSpace(firstNonEmpty(sn.Icon, s.opts.Icon))
	switch {
	case icon == "":
	case strings.HasPrefix(icon, ":") && strings.HasSuffix(icon, ":"):
		payload["icon_emoji"] = icon
	default:
		if u, err := url.Parse(icon); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			payload["icon_url"] = icon
		} else {
			s.deps.logger().Warn("slack: ignoring icon that is neither an emoji nor an http(s) URL")
		}
	}
	if s.opts.DisableUnfurl {
		payload["unfurl_links"] = false
		payload["unfurl_media"] = false
	}

	policy := sn.DeliveryPolicy
	switch policy {
	case "", SlackPolicyPost:
		policy = SlackPolicyPost
	case SlackPolicyPostAndUpdate, SlackPolicyUpdate:
	default:
		return fmt.Errorf("%w: slack: unknown deliveryPolicy %q (want Post, PostAndUpdate or Update)", ErrInvalidNotification, sn.DeliveryPolicy)
	}
	if policy != SlackPolicyPost && sn.GroupingKey == "" {
		return fmt.Errorf("%w: slack: deliveryPolicy %s requires groupingKey", ErrInvalidNotification, policy)
	}

	cacheKey := channel + "\x00" + sn.GroupingKey
	var thread slackThread
	found := false
	if sn.GroupingKey != "" {
		thread, found = s.threads.Get(cacheKey)
	}

	switch {
	case found && policy != SlackPolicyPost:
		payload["channel"] = thread.channelID
		payload["ts"] = thread.ts
		_, err := s.call(ctx, "chat.update", payload)
		return err
	case !found && policy == SlackPolicyUpdate:
		s.deps.logger().Debug("slack: no message to update for grouping key; skipping", "channel", channel)
		return nil
	}
	if found {
		payload["thread_ts"] = thread.ts
		if sn.NotifyBroadcast {
			payload["reply_broadcast"] = true
		}
	}
	resp, err := s.call(ctx, "chat.postMessage", payload)
	if err != nil {
		return err
	}
	if sn.GroupingKey != "" && !found && resp.TS != "" {
		s.threads.Add(cacheKey, slackThread{channelID: firstNonEmpty(resp.Channel, channel), ts: resp.TS})
	}
	return nil
}

func (s *slackService) call(ctx context.Context, method string, payload map[string]any) (*slackAPIResponse, error) {
	endpoint := s.apiURL + method
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.opts.Token)
	header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.http.doJSON(ctx, http.MethodPost, endpoint, header, payload)
	if err != nil {
		return nil, err
	}
	if err := s.http.expect2xx(http.MethodPost, endpoint, resp); err != nil {
		return nil, err
	}
	var out slackAPIResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("%w: slack: %s: invalid response: %v", ErrDeliveryFailed, method, err)
	}
	if !out.OK {
		return nil, fmt.Errorf("%w: slack: %s: %s", ErrDeliveryFailed, method, firstNonEmpty(out.Error, "unknown error"))
	}
	return &out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
