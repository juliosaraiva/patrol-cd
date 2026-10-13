package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// TeamsOptions configure the teams and teams-workflows services: a map from recipient
// (channel name) to incoming webhook URL. Every URL is treated as a secret.
type TeamsOptions struct {
	RecipientURLs map[string]string `json:"recipientUrls"`
	TransportOptions
}

type teamsService struct {
	typ       string
	opts      TeamsOptions
	http      *httpClient
	workflows bool // teams-workflows: always Adaptive Cards
}

func newTeamsService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	return newTeamsLike(TypeTeams, raw, deps, r, false)
}

func newTeamsWorkflowsService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	return newTeamsLike(TypeTeamsWorkflows, raw, deps, r, true)
}

func newTeamsLike(typ string, raw []byte, deps Deps, r *Redactor, workflows bool) (Service, error) {
	var opts TeamsOptions
	if err := decodeOptions(typ, raw, &opts); err != nil {
		return nil, err
	}
	for _, u := range opts.RecipientURLs {
		r.Add(u)
	}
	hc, err := newHTTPClient(typ, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &teamsService{typ: typ, opts: opts, http: hc, workflows: workflows}, nil
}

// isWorkflowsURL detects Power Automate / Teams Workflows webhook endpoints.
func isWorkflowsURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "api.powerautomate.com", host == "api.powerplatform.com", host == "flow.microsoft.com":
		return true
	case strings.HasSuffix(host, ".powerautomate.com"), strings.HasSuffix(host, ".powerplatform.com"),
		strings.HasSuffix(host, ".logic.azure.com"):
		return true
	case strings.Contains(u.Path, "/powerautomate/"):
		return true
	}
	return false
}

func (s *teamsService) Send(ctx context.Context, n Notification, dest Destination) error {
	webhook, ok := s.opts.RecipientURLs[dest.Recipient]
	if !ok || strings.TrimSpace(webhook) == "" {
		return fmt.Errorf("%w: %s: no webhook URL configured for recipient %q", ErrRecipientNotConfigured, s.typ, dest.Recipient)
	}
	workflows := s.workflows || isWorkflowsURL(webhook)

	var body []byte
	var err error
	if workflows {
		var src *TeamsWorkflowsNotification
		if n.TeamsWorkflows != nil {
			src = n.TeamsWorkflows
		} else if n.Teams != nil {
			src = teamsAsWorkflows(n.Teams)
		}
		body, err = buildWorkflowsPayload(n.Message, src)
	} else {
		body, err = buildMessageCard(n.Message, n.Teams)
	}
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	resp, err := s.http.do(ctx, http.MethodPost, webhook, header, body)
	if err != nil {
		return err
	}
	if err := s.http.expect2xx(http.MethodPost, webhook, resp); err != nil {
		return err
	}
	if !workflows && strings.TrimSpace(string(resp.Body)) != "1" {
		// Office 365 connectors acknowledge with a literal "1"; anything else is an error
		// message (often with HTTP 200).
		return fmt.Errorf("%w: %s: webhook rejected the message: %s", ErrDeliveryFailed, s.typ, s.http.redactor.Redact(truncateBytes(resp.Body, 512)))
	}
	return nil
}

func teamsAsWorkflows(t *TeamsNotification) *TeamsWorkflowsNotification {
	return &TeamsWorkflowsNotification{
		Template:        t.Template,
		Title:           t.Title,
		Summary:         t.Summary,
		Text:            t.Text,
		ThemeColor:      t.ThemeColor,
		Facts:           t.Facts,
		Sections:        t.Sections,
		PotentialAction: t.PotentialAction,
	}
}

func parseJSONArray(service, field, raw string) ([]any, error) {
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%w: %s: %s is not a JSON array: %v", ErrInvalidNotification, service, field, err)
	}
	return out, nil
}

// buildMessageCard renders the legacy Office 365 connector MessageCard payload.
func buildMessageCard(message string, t *TeamsNotification) ([]byte, error) {
	card := map[string]any{
		"@type":    "MessageCard",
		"@context": "https://schema.org/extensions",
		"text":     message,
	}
	if t != nil {
		if strings.TrimSpace(t.Template) != "" {
			if !json.Valid([]byte(t.Template)) {
				return nil, fmt.Errorf("%w: teams: template is not valid JSON", ErrInvalidNotification)
			}
			return []byte(t.Template), nil
		}
		if t.Title != "" {
			card["title"] = t.Title
		}
		if t.Text != "" {
			card["text"] = t.Text
		}
		if t.Summary != "" {
			card["summary"] = t.Summary
		}
		if t.ThemeColor != "" {
			card["themeColor"] = t.ThemeColor
		}
		var sections []any
		if strings.TrimSpace(t.Sections) != "" {
			s, err := parseJSONArray(TypeTeams, "sections", t.Sections)
			if err != nil {
				return nil, err
			}
			sections = s
		}
		if strings.TrimSpace(t.Facts) != "" {
			facts, err := parseJSONArray(TypeTeams, "facts", t.Facts)
			if err != nil {
				return nil, err
			}
			sections = append(sections, map[string]any{"facts": facts})
		}
		if sections != nil {
			card["sections"] = sections
		}
		if strings.TrimSpace(t.PotentialAction) != "" {
			actions, err := parseJSONArray(TypeTeams, "potentialAction", t.PotentialAction)
			if err != nil {
				return nil, err
			}
			card["potentialAction"] = actions
		}
	}
	return json.Marshal(card)
}

const adaptiveCardContentType = "application/vnd.microsoft.card.adaptive"

func wrapAdaptiveCard(card any) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": adaptiveCardContentType,
			"contentUrl":  nil,
			"content":     card,
		}},
	})
}

// buildWorkflowsPayload renders the Teams Workflows (Adaptive Card) payload.
func buildWorkflowsPayload(message string, t *TeamsWorkflowsNotification) ([]byte, error) {
	var tw TeamsWorkflowsNotification
	if t != nil {
		tw = *t
	}
	if strings.TrimSpace(tw.Template) != "" {
		if !json.Valid([]byte(tw.Template)) {
			return nil, fmt.Errorf("%w: teams-workflows: template is not valid JSON", ErrInvalidNotification)
		}
		return []byte(tw.Template), nil
	}
	if strings.TrimSpace(tw.AdaptiveCard) != "" {
		var card map[string]any
		if err := json.Unmarshal([]byte(tw.AdaptiveCard), &card); err != nil {
			return nil, fmt.Errorf("%w: teams-workflows: adaptiveCard is not a JSON object: %v", ErrInvalidNotification, err)
		}
		return wrapAdaptiveCard(card)
	}
	body := []any{}
	if tw.Title != "" {
		block := map[string]any{"type": "TextBlock", "text": tw.Title, "size": "Large", "weight": "Bolder", "wrap": true}
		if c := adaptiveColor(tw.ThemeColor); c != "" {
			block["color"] = c
		}
		body = append(body, block)
	}
	if text := firstNonEmpty(tw.Text, message); text != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": text, "wrap": true})
	}
	if strings.TrimSpace(tw.Facts) != "" {
		facts, err := parseJSONArray(TypeTeamsWorkflows, "facts", tw.Facts)
		if err != nil {
			return nil, err
		}
		if fs := factSet(facts); fs != nil {
			body = append(body, fs)
		}
	}
	if strings.TrimSpace(tw.Sections) != "" {
		sections, err := parseJSONArray(TypeTeamsWorkflows, "sections", tw.Sections)
		if err != nil {
			return nil, err
		}
		for _, sec := range sections {
			m, ok := sec.(map[string]any)
			if !ok {
				continue
			}
			if facts, ok := m["facts"].([]any); ok {
				if fs := factSet(facts); fs != nil {
					body = append(body, fs)
				}
			}
		}
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if strings.TrimSpace(tw.PotentialAction) != "" {
		actions, err := parseJSONArray(TypeTeamsWorkflows, "potentialAction", tw.PotentialAction)
		if err != nil {
			return nil, err
		}
		var out []any
		for _, a := range actions {
			m, ok := a.(map[string]any)
			if !ok || !strings.EqualFold(fmt.Sprint(m["@type"]), "OpenUri") {
				continue
			}
			targets, _ := m["targets"].([]any)
			for _, tg := range targets {
				tm, ok := tg.(map[string]any)
				if !ok {
					continue
				}
				if uri, ok := tm["uri"].(string); ok && uri != "" {
					out = append(out, map[string]any{"type": "Action.OpenUrl", "title": fmt.Sprint(m["name"]), "url": uri})
					break
				}
			}
		}
		if len(out) > 0 {
			card["actions"] = out
		}
	}
	return wrapAdaptiveCard(card)
}

func factSet(facts []any) map[string]any {
	var out []any
	for _, f := range facts {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		title := m["name"]
		if title == nil {
			title = m["title"]
		}
		out = append(out, map[string]any{"title": fmt.Sprint(valueOrEmpty(title)), "value": fmt.Sprint(valueOrEmpty(m["value"]))})
	}
	if len(out) == 0 {
		return nil
	}
	return map[string]any{"type": "FactSet", "facts": out}
}

func valueOrEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// adaptiveColor maps a theme color to an Adaptive Card TextBlock color. Semantic names
// pass through; hex colors map to the closest semantic color by dominant channel.
func adaptiveColor(theme string) string {
	theme = strings.TrimSpace(theme)
	if theme == "" {
		return ""
	}
	for _, c := range []string{"Default", "Dark", "Light", "Accent", "Good", "Warning", "Attention"} {
		if strings.EqualFold(theme, c) {
			return c
		}
	}
	hex := strings.TrimPrefix(theme, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) != 6 {
		return ""
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return ""
	}
	r, g, b := int(v>>16&0xff), int(v>>8&0xff), int(v&0xff)
	switch {
	case r > 150 && g > 150 && b < 100:
		return "Warning"
	case r >= g && r >= b:
		return "Attention"
	case g >= r && g >= b:
		return "Good"
	default:
		return "Accent"
	}
}

// ---------------------------------------------------------------------------------------
// Google Chat

// GoogleChatOptions configure a googlechat service: a map from recipient (space name) to
// incoming webhook URL. Every URL is treated as a secret.
type GoogleChatOptions struct {
	Webhooks map[string]string `json:"webhooks"`
	TransportOptions
}

type googleChatService struct {
	opts GoogleChatOptions
	http *httpClient
}

func newGoogleChatService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts GoogleChatOptions
	if err := decodeOptions(TypeGoogleChat, raw, &opts); err != nil {
		return nil, err
	}
	for _, u := range opts.Webhooks {
		r.Add(u)
	}
	hc, err := newHTTPClient(TypeGoogleChat, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &googleChatService{opts: opts, http: hc}, nil
}

func (s *googleChatService) Send(ctx context.Context, n Notification, dest Destination) error {
	webhook, ok := s.opts.Webhooks[dest.Recipient]
	if !ok || strings.TrimSpace(webhook) == "" {
		return fmt.Errorf("%w: googlechat: no webhook configured for recipient %q", ErrRecipientNotConfigured, dest.Recipient)
	}
	payload := map[string]any{}
	if n.Message != "" {
		payload["text"] = n.Message
	}
	threadKey := ""
	if g := n.GoogleChat; g != nil {
		threadKey = strings.TrimSpace(g.ThreadKey)
		if strings.TrimSpace(g.Cards) != "" {
			cards, err := parseYAMLList(TypeGoogleChat, "cards", g.Cards)
			if err != nil {
				return err
			}
			payload["cards"] = cards
		}
		if strings.TrimSpace(g.CardsV2) != "" {
			cards, err := parseYAMLList(TypeGoogleChat, "cardsV2", g.CardsV2)
			if err != nil {
				return err
			}
			wrapped := make([]any, 0, len(cards))
			for i, c := range cards {
				if m, ok := c.(map[string]any); ok {
					if _, has := m["card"]; has {
						wrapped = append(wrapped, m)
						continue
					}
				}
				wrapped = append(wrapped, map[string]any{"cardId": "card-" + strconv.Itoa(i), "card": c})
			}
			payload["cardsV2"] = wrapped
		}
	}
	if len(payload) == 0 {
		return fmt.Errorf("%w: googlechat: message and cards are empty", ErrInvalidNotification)
	}
	target := webhook
	if threadKey != "" {
		u, err := url.Parse(webhook)
		if err != nil {
			return fmt.Errorf("%w: googlechat: invalid webhook URL", ErrInvalidOptions)
		}
		q := u.Query()
		q.Set("threadKey", threadKey)
		q.Set("messageReplyOption", "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD")
		u.RawQuery = q.Encode()
		target = u.String()
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := s.http.doJSON(ctx, http.MethodPost, target, header, payload)
	if err != nil {
		return err
	}
	return s.http.expect2xx(http.MethodPost, target, resp)
}
