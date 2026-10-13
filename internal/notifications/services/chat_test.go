package services

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestSlack(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, c captured) {
		switch c.Path {
		case "/api/chat.postMessage", "/api/chat.update":
			writeJSON(w, 200, map[string]any{"ok": true, "channel": "C123", "ts": "1700000000.000100"})
		default:
			writeJSON(w, 404, map[string]any{"ok": false, "error": "unknown_method"})
		}
	})
	svc := mustNew(t, TypeSlack, map[string]any{
		"token": "xoxb-secret-token", "apiURL": srv.URL + "/api", "username": "patrol", "icon": ":robot_face:", "disableUnfurl": true,
	}, testDeps())

	n := Notification{Message: "hello", Slack: &SlackNotification{
		Attachments: `[{"title":"guestbook","color":"#18be52"}]`,
		Blocks:      `[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`,
		GroupingKey: "rev1",
	}}
	mustSend(t, svc, n, Destination{Service: "slack", Recipient: "my-channel"})

	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/api/chat.postMessage" {
		t.Fatalf("got %s %s", r.Method, r.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer xoxb-secret-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := r.json(t)
	if str(t, body, "channel") != "my-channel" || str(t, body, "text") != "hello" || str(t, body, "username") != "patrol" || str(t, body, "icon_emoji") != ":robot_face:" {
		t.Fatalf("unexpected body: %v", body)
	}
	if body["unfurl_links"] != false || body["unfurl_media"] != false {
		t.Fatalf("unfurl flags missing: %v", body)
	}
	if att, ok := body["attachments"].([]any); !ok || len(att) != 1 {
		t.Fatalf("attachments = %v", body["attachments"])
	}
	if blocks, ok := body["blocks"].([]any); !ok || len(blocks) != 1 {
		t.Fatalf("blocks = %v", body["blocks"])
	}
	if _, ok := body["thread_ts"]; ok {
		t.Fatalf("first message of a group must not be threaded")
	}

	// Second message with the same grouping key replies in the thread and broadcasts.
	n2 := Notification{Message: "second", Slack: &SlackNotification{GroupingKey: "rev1", NotifyBroadcast: true, Icon: "https://example.com/i.png"}}
	mustSend(t, svc, n2, Destination{Service: "slack", Recipient: "my-channel"})
	body = rec.all()[1].json(t)
	if str(t, body, "thread_ts") != "1700000000.000100" || body["reply_broadcast"] != true || str(t, body, "icon_url") != "https://example.com/i.png" {
		t.Fatalf("threaded reply body: %v", body)
	}

	// PostAndUpdate edits the first message of the group.
	n3 := Notification{Message: "updated", Slack: &SlackNotification{GroupingKey: "rev1", DeliveryPolicy: SlackPolicyPostAndUpdate}}
	mustSend(t, svc, n3, Destination{Service: "slack", Recipient: "my-channel"})
	r3 := rec.all()[2]
	body = r3.json(t)
	if r3.Path != "/api/chat.update" || str(t, body, "channel") != "C123" || str(t, body, "ts") != "1700000000.000100" || str(t, body, "text") != "updated" {
		t.Fatalf("update request: %s %v", r3.Path, body)
	}

	// Update with no known message for the key sends nothing.
	n4 := Notification{Message: "x", Slack: &SlackNotification{GroupingKey: "unknown", DeliveryPolicy: SlackPolicyUpdate}}
	mustSend(t, svc, n4, Destination{Service: "slack", Recipient: "my-channel"})
	if len(rec.all()) != 3 {
		t.Fatalf("Update policy without a message must not send")
	}

	// Invalid JSON in attachments.
	err := send(t, svc, Notification{Slack: &SlackNotification{Attachments: "[{"}}, Destination{Recipient: "c"})
	if !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("want ErrInvalidNotification, got %v", err)
	}
	// PostAndUpdate without grouping key.
	err = send(t, svc, Notification{Slack: &SlackNotification{DeliveryPolicy: SlackPolicyPostAndUpdate}}, Destination{Recipient: "c"})
	if !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("want ErrInvalidNotification, got %v", err)
	}
}

func TestSlackAPIError(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t, func(w http.ResponseWriter, _ captured) {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "channel_not_found"})
	})
	svc := mustNew(t, TypeSlack, map[string]any{"token": "xoxb-1234567", "apiURL": srv.URL + "/"}, testDeps())
	err := send(t, svc, Notification{Message: "m"}, Destination{Recipient: "nope"})
	if !errors.Is(err, ErrDeliveryFailed) || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "xoxb-1234567") {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestMattermost(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { writeJSON(w, 201, map[string]any{"id": "p1"}) })
	svc := mustNew(t, TypeMattermost, map[string]any{"apiURL": srv.URL, "token": "mm-token"}, testDeps())
	mustSend(t, svc, Notification{Message: "deployed", Mattermost: &MattermostNotification{Attachments: `[{"title":"t"}]`}}, Destination{Recipient: "chan-id"})
	r := rec.all()[0]
	if r.Path != "/api/v4/posts" || r.Header.Get("Authorization") != "Bearer mm-token" {
		t.Fatalf("request %s auth=%q", r.Path, r.Header.Get("Authorization"))
	}
	body := r.json(t)
	if str(t, body, "channel_id") != "chan-id" || str(t, body, "message") != "deployed" {
		t.Fatalf("body %v", body)
	}
	props, _ := body["props"].(map[string]any)
	if att, ok := props["attachments"].([]any); !ok || len(att) != 1 {
		t.Fatalf("props %v", body["props"])
	}
}

func TestRocketChat(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, c captured) {
		switch c.Path {
		case "/api/v1/login":
			writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"authToken": "session-token-xyz", "userId": "u1"}})
		case "/api/v1/chat.postMessage":
			writeJSON(w, 200, map[string]any{"success": true})
		}
	})
	svc := mustNew(t, TypeRocketChat, map[string]any{"email": "bot@example.com", "password": "pw-secret", "serverUrl": srv.URL, "alias": "Patrol", "icon": ":rocket:"}, testDeps())
	mustSend(t, svc, Notification{Message: "m", RocketChat: &RocketChatNotification{Attachments: `[{"title":"x"}]`}}, Destination{Recipient: "#general"})
	mustSend(t, svc, Notification{Message: "m"}, Destination{Recipient: "ROOMID"})
	reqs := rec.all()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4 (login+post twice)", len(reqs))
	}
	login := reqs[0].json(t)
	if str(t, login, "user") != "bot@example.com" || str(t, login, "password") != "pw-secret" {
		t.Fatalf("login body %v", login)
	}
	post := reqs[1]
	if post.Header.Get("X-Auth-Token") != "session-token-xyz" || post.Header.Get("X-User-Id") != "u1" {
		t.Fatalf("post headers %v", post.Header)
	}
	body := post.json(t)
	if str(t, body, "channel") != "#general" || str(t, body, "alias") != "Patrol" || str(t, body, "emoji") != ":rocket:" {
		t.Fatalf("post body %v", body)
	}
	if body2 := reqs[3].json(t); str(t, body2, "roomId") != "ROOMID" {
		t.Fatalf("room post body %v", body2)
	}
}

func TestTelegram(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, c captured) {
		if strings.Contains(string(c.Body), "bad-chat") {
			writeJSON(w, 400, map[string]any{"ok": false, "description": "Bad Request: chat not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	svc := mustNew(t, TypeTelegram, map[string]any{"token": "123456:SECRET-BOT-TOKEN", "apiURL": srv.URL}, testDeps())
	mustSend(t, svc, Notification{Message: "*hi*"}, Destination{Recipient: "-1001234567890|42"})
	mustSend(t, svc, Notification{Message: "hi"}, Destination{Recipient: "mychannel"})
	reqs := rec.all()
	if reqs[0].Path != "/bot123456:SECRET-BOT-TOKEN/sendMessage" {
		t.Fatalf("path = %q", reqs[0].Path)
	}
	b0 := reqs[0].json(t)
	if b0["chat_id"] != float64(-1001234567890) || b0["message_thread_id"] != float64(42) || str(t, b0, "parse_mode") != "Markdown" || str(t, b0, "text") != "*hi*" {
		t.Fatalf("body %v", b0)
	}
	if b1 := reqs[1].json(t); b1["chat_id"] != "@mychannel" {
		t.Fatalf("channel chat_id %v", b1["chat_id"])
	}
	err := send(t, svc, Notification{Message: "x"}, Destination{Recipient: "bad-chat"})
	if !errors.Is(err, ErrDeliveryFailed) || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("got %v", err)
	}
}

func TestTelegramTokenRedactedOnNetworkError(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t, func(http.ResponseWriter, captured) {})
	url := srv.URL
	srv.Close() // connection refused
	svc := mustNew(t, TypeTelegram, map[string]any{"token": "999:VERY-SECRET", "apiURL": url}, testDeps())
	err := send(t, svc, Notification{Message: "x"}, Destination{Recipient: "chan"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "VERY-SECRET") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestWebex(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { writeJSON(w, 200, map[string]any{"id": "m"}) })
	svc := mustNew(t, TypeWebex, map[string]any{"token": "webex-token", "apiURL": srv.URL}, testDeps())
	mustSend(t, svc, Notification{Message: "**md**"}, Destination{Recipient: "person@example.com"})
	mustSend(t, svc, Notification{Message: "m"}, Destination{Recipient: "Y2lzY29zcGFyazovL3VzL1JPT00v"})
	reqs := rec.all()
	if reqs[0].Path != "/v1/messages" || reqs[0].Header.Get("Authorization") != "Bearer webex-token" {
		t.Fatalf("request %s %v", reqs[0].Path, reqs[0].Header)
	}
	if b := reqs[0].json(t); str(t, b, "toPersonEmail") != "person@example.com" || str(t, b, "markdown") != "**md**" {
		t.Fatalf("body %v", b)
	}
	if b := reqs[1].json(t); str(t, b, "roomId") != "Y2lzY29zcGFyazovL3VzL1JPT00v" {
		t.Fatalf("body %v", b)
	}
}

func TestPushover(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, c captured) {
		if strings.Contains(string(c.Body), "baduser") {
			writeJSON(w, 400, map[string]any{"status": 0, "errors": []string{"user identifier is invalid"}})
			return
		}
		writeJSON(w, 200, map[string]any{"status": 1})
	})
	svc := mustNew(t, TypePushover, map[string]any{"token": "app-token-1", "apiURL": srv.URL}, testDeps())
	mustSend(t, svc, Notification{Message: "synced"}, Destination{Recipient: "uKey"})
	r := rec.all()[0]
	b := r.json(t)
	if r.Path != "/1/messages.json" || str(t, b, "token") != "app-token-1" || str(t, b, "user") != "uKey" || str(t, b, "message") != "synced" {
		t.Fatalf("request %s %v", r.Path, b)
	}
	err := send(t, svc, Notification{Message: "x"}, Destination{Recipient: "baduser"})
	if !errors.Is(err, ErrDeliveryFailed) || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("got %v", err)
	}
}

func TestGrafana(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { writeJSON(w, 200, map[string]any{"id": 1}) })
	svc := mustNew(t, TypeGrafana, map[string]any{"apiUrl": srv.URL + "/api", "apiKey": "glsa_key"}, testDeps())
	mustSend(t, svc, Notification{Message: "deployed v2"}, Destination{Recipient: "tag1|tag2"})
	r := rec.all()[0]
	if r.Path != "/api/annotations" || r.Header.Get("Authorization") != "Bearer glsa_key" {
		t.Fatalf("request %s %v", r.Path, r.Header)
	}
	b := r.json(t)
	if b["time"] != float64(testNow.UnixMilli()) || b["isRegion"] != false || str(t, b, "text") != "deployed v2" {
		t.Fatalf("body %v", b)
	}
	tags, _ := b["tags"].([]any)
	if len(tags) != 2 || tags[0] != "tag1" || tags[1] != "tag2" {
		t.Fatalf("tags %v", b["tags"])
	}
}

func TestTeamsMessageCard(t *testing.T) {
	t.Parallel()
	reply := "1"
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(reply))
	})
	svc := mustNew(t, TypeTeams, map[string]any{"recipientUrls": map[string]string{"ops": srv.URL + "/webhookb2/abc"}}, testDeps())
	n := Notification{Message: "fallback", Teams: &TeamsNotification{
		Title: "Synced", Summary: "sum", ThemeColor: "#000080",
		Facts:           `[{"name":"Sync Status","value":"Synced"}]`,
		Sections:        `[{"activityTitle":"x"}]`,
		PotentialAction: `[{"@type":"OpenUri","name":"Open","targets":[{"os":"default","uri":"https://cd.example.com"}]}]`,
	}}
	mustSend(t, svc, n, Destination{Recipient: "ops"})
	b := rec.all()[0].json(t)
	if str(t, b, "@type") != "MessageCard" || str(t, b, "title") != "Synced" || str(t, b, "text") != "fallback" || str(t, b, "themeColor") != "#000080" || str(t, b, "summary") != "sum" {
		t.Fatalf("card %v", b)
	}
	sections, _ := b["sections"].([]any)
	if len(sections) != 2 {
		t.Fatalf("sections (incl. facts) = %v", b["sections"])
	}
	if acts, _ := b["potentialAction"].([]any); len(acts) != 1 {
		t.Fatalf("potentialAction %v", b["potentialAction"])
	}
	reply = "Webhook message delivery failed with error: Microsoft Teams endpoint returned HTTP error 400"
	err := send(t, svc, n, Destination{Recipient: "ops"})
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("connector error must fail, got %v", err)
	}
	if strings.Contains(err.Error(), "/webhookb2/abc") {
		t.Fatalf("webhook URL leaked: %v", err)
	}
	if err := send(t, svc, n, Destination{Recipient: "unknown"}); !errors.Is(err, ErrRecipientNotConfigured) {
		t.Fatalf("got %v", err)
	}
}

func TestTeamsWorkflows(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { w.WriteHeader(202) })
	svc := mustNew(t, TypeTeamsWorkflows, map[string]any{"recipientUrls": map[string]string{"ops": srv.URL + "/workflows/1"}}, testDeps())
	n := Notification{Message: "body text", TeamsWorkflows: &TeamsWorkflowsNotification{
		Title: "Synced", ThemeColor: "Good",
		Facts:           `[{"name":"Sync Status","value":"Synced"}]`,
		Sections:        `[{"facts":[{"name":"Namespace","value":"guestbook"}]}]`,
		PotentialAction: `[{"@type":"OpenUri","name":"View","targets":[{"os":"default","uri":"https://cd.example.com/app"}]}]`,
	}}
	mustSend(t, svc, n, Destination{Recipient: "ops"})
	b := rec.all()[0].json(t)
	if str(t, b, "type") != "message" {
		t.Fatalf("envelope %v", b)
	}
	att := b["attachments"].([]any)[0].(map[string]any)
	if str(t, att, "contentType") != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("attachment %v", att)
	}
	card := att["content"].(map[string]any)
	body := card["body"].([]any)
	if len(body) != 4 {
		t.Fatalf("card body = %d elements, want 4 (title, text, 2 fact sets): %v", len(body), body)
	}
	title := body[0].(map[string]any)
	if title["text"] != "Synced" || title["color"] != "Good" || title["weight"] != "Bolder" {
		t.Fatalf("title block %v", title)
	}
	if body[1].(map[string]any)["text"] != "body text" {
		t.Fatalf("text block %v", body[1])
	}
	fs := body[2].(map[string]any)
	facts := fs["facts"].([]any)
	if fs["type"] != "FactSet" || facts[0].(map[string]any)["title"] != "Sync Status" {
		t.Fatalf("fact set %v", fs)
	}
	actions := card["actions"].([]any)
	if a := actions[0].(map[string]any); a["type"] != "Action.OpenUrl" || a["url"] != "https://cd.example.com/app" || a["title"] != "View" {
		t.Fatalf("actions %v", actions)
	}

	// adaptiveCard takes precedence and is wrapped as-is.
	n2 := Notification{TeamsWorkflows: &TeamsWorkflowsNotification{Title: "ignored", AdaptiveCard: `{"type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"custom"}]}`}}
	mustSend(t, svc, n2, Destination{Recipient: "ops"})
	b2 := rec.all()[1].json(t)
	card2 := b2["attachments"].([]any)[0].(map[string]any)["content"].(map[string]any)
	if card2["body"].([]any)[0].(map[string]any)["text"] != "custom" {
		t.Fatalf("adaptive card %v", card2)
	}
}

func TestTeamsDetectsWorkflowsURL(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { w.WriteHeader(202) })
	svc := mustNew(t, TypeTeams, map[string]any{"recipientUrls": map[string]string{"ops": srv.URL + "/powerautomate/automations/direct/workflows/x"}}, testDeps())
	mustSend(t, svc, Notification{Message: "m", Teams: &TeamsNotification{Title: "T", ThemeColor: "#FF0000"}}, Destination{Recipient: "ops"})
	b := rec.all()[0].json(t)
	card := b["attachments"].([]any)[0].(map[string]any)["content"].(map[string]any)
	title := card["body"].([]any)[0].(map[string]any)
	if title["text"] != "T" || title["color"] != "Attention" {
		t.Fatalf("converted title %v", title)
	}
}

func TestGoogleChat(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) {
		writeJSON(w, 200, map[string]any{"name": "spaces/x/messages/y"})
	})
	svc := mustNew(t, TypeGoogleChat, map[string]any{"webhooks": map[string]string{"space": srv.URL + "/v1/spaces/X/messages?key=K&token=T"}}, testDeps())
	n := Notification{Message: "synced", GoogleChat: &GoogleChatNotification{
		ThreadKey: "guestbook",
		CardsV2: `
- header:
    title: Patrol Bot
  sections:
    - widgets:
        - decoratedText:
            text: The app guestbook has synced
`,
	}}
	mustSend(t, svc, n, Destination{Recipient: "space"})
	r := rec.all()[0]
	if r.Path != "/v1/spaces/X/messages" {
		t.Fatalf("path %q", r.Path)
	}
	for _, want := range []string{"key=K", "token=T", "threadKey=guestbook", "messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"} {
		if !strings.Contains(r.RawQuery, want) {
			t.Fatalf("query %q missing %q", r.RawQuery, want)
		}
	}
	b := r.json(t)
	if str(t, b, "text") != "synced" {
		t.Fatalf("body %v", b)
	}
	cards := b["cardsV2"].([]any)
	c0 := cards[0].(map[string]any)
	if c0["cardId"] != "card-0" || c0["card"].(map[string]any)["header"].(map[string]any)["title"] != "Patrol Bot" {
		t.Fatalf("cardsV2 %v", cards)
	}
	if err := send(t, svc, n, Destination{Recipient: "other"}); !errors.Is(err, ErrRecipientNotConfigured) {
		t.Fatalf("got %v", err)
	}
}
