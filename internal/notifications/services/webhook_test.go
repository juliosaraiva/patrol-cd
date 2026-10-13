package services

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookRequestShape(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { w.WriteHeader(201) })
	svc := mustNew(t, TypeWebhook, map[string]any{
		"url":       srv.URL + "/base/",
		"headers":   []map[string]string{{"name": "Content-Type", "value": "application/json"}, {"name": "Authorization", "value": "token gh-secret"}},
		"basicAuth": nil,
	}, testDeps())
	n := Notification{Webhook: WebhookNotifications{
		"github": {Method: "post", Path: "/repos/example/guestbook/statuses/abc", Body: `{"state":"success"}`},
		"other":  {Method: "PUT", Path: "/x", Body: "ignored"},
	}}
	mustSend(t, svc, n, Destination{Service: "github"})
	r := rec.all()[0]
	if r.Method != http.MethodPost || r.Path != "/base/repos/example/guestbook/statuses/abc" {
		t.Fatalf("got %s %s", r.Method, r.Path)
	}
	if r.Header.Get("Authorization") != "token gh-secret" || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", r.Header)
	}
	if string(r.Body) != `{"state":"success"}` {
		t.Fatalf("body %q", r.Body)
	}
	// No entry for this webhook name: GET the base URL without body.
	mustSend(t, svc, Notification{}, Destination{Service: "jenkins"})
	r2 := rec.all()[1]
	if r2.Method != http.MethodGet || r2.Path != "/base/" || len(r2.Body) != 0 {
		t.Fatalf("default request %s %s %q", r2.Method, r2.Path, r2.Body)
	}
}

func TestAppendWebhookPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		base, path, want string
	}{
		{"https://h/base", "/repos/a", "https://h/base/repos/a"},
		{"https://h/base/", "/repos/a", "https://h/base/repos/a"},
		{"https://h/base/", "repos/a", "https://h/base/repos/a"},
		{"https://h/apps/", "guestbook", "https://h/apps/guestbook"},
		{"https://h/hook?app=", "guestbook", "https://h/hook?app=guestbook"},
		{"https://h/hook", "?app=guestbook", "https://h/hook?app=guestbook"},
		{"https://h", "/x", "https://h/x"},
	}
	for _, tc := range tests {
		if got := appendWebhookPath(tc.base, tc.path); got != tc.want {
			t.Errorf("appendWebhookPath(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestWebhookPathContinuesQuery(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { w.WriteHeader(204) })
	svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL + "/hook?app="}, testDeps())
	mustSend(t, svc, Notification{Webhook: WebhookNotifications{"ci": {Method: "POST", Path: "guestbook"}}}, Destination{Service: "ci"})
	if r := rec.all()[0]; r.Path != "/hook" || r.RawQuery != "app=guestbook" || r.Method != http.MethodPost {
		t.Fatalf("got %s %s?%s", r.Method, r.Path, r.RawQuery)
	}
}

func TestWebhookBasicAuth(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { w.WriteHeader(200) })
	svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL + "/job/build?token=abc", "basicAuth": map[string]string{"username": "u", "password": "p@ss"}}, testDeps())
	mustSend(t, svc, Notification{}, Destination{Service: "jenkins"})
	r := rec.all()[0]
	if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("u:p@ss")) || r.RawQuery != "token=abc" {
		t.Fatalf("auth %q query %q", r.Header.Get("Authorization"), r.RawQuery)
	}
}

func TestWebhookRetries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		statuses     []int
		retryMax     *int
		wantAttempts int32
		wantErr      bool
		wantStatus   int
	}{
		{name: "5xx then success", statuses: []int{503, 502, 200}, wantAttempts: 3},
		{name: "429 then success", statuses: []int{429, 200}, wantAttempts: 2},
		{name: "exhausted", statuses: []int{500, 500, 500, 500}, wantAttempts: 3, wantErr: true, wantStatus: 500},
		{name: "4xx not retried", statuses: []int{400, 200}, wantAttempts: 1, wantErr: true, wantStatus: 400},
		{name: "501 not retried", statuses: []int{501, 200}, wantAttempts: 1, wantErr: true, wantStatus: 501},
		{name: "retryMax 0", statuses: []int{503, 200}, retryMax: new(0), wantAttempts: 1, wantErr: true, wantStatus: 503},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var n atomic.Int32
			srv, _ := newServer(t, func(w http.ResponseWriter, _ captured) {
				i := int(n.Add(1)) - 1
				status := tc.statuses[min(i, len(tc.statuses)-1)]
				if status == 429 {
					w.Header().Set("Retry-After", "0")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte("status body"))
			})
			opts := map[string]any{"url": srv.URL, "retryWaitMin": "1ms", "retryWaitMax": "2ms"}
			if tc.retryMax != nil {
				opts["retryMax"] = *tc.retryMax
			}
			svc := mustNew(t, TypeWebhook, opts, testDeps())
			err := send(t, svc, Notification{}, Destination{Service: "hook"})
			if got := n.Load(); got != tc.wantAttempts {
				t.Fatalf("attempts = %d, want %d", got, tc.wantAttempts)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				var se *StatusError
				if !errors.As(err, &se) || se.StatusCode != tc.wantStatus || !errors.Is(err, ErrDeliveryFailed) {
					t.Fatalf("want StatusError %d, got %v", tc.wantStatus, err)
				}
				if !strings.Contains(err.Error(), "status body") {
					t.Fatalf("error should include the response body: %v", err)
				}
			}
		})
	}
}

func TestWebhookRetryHonorsContext(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t, func(w http.ResponseWriter, _ captured) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL}, testDeps())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := svc.Send(ctx, Notification{}, Destination{Service: "hook"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("retry wait ignored the context deadline")
	}
}

func TestWebhookSecretsRedacted(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t, func(w http.ResponseWriter, _ captured) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("bad credentials for token=supersecretvalue"))
	})
	deps := testDeps()
	deps.SensitiveValues = []string{"supersecretvalue"}
	svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL + "/hook?apikey=supersecretvalue"}, deps)
	err := send(t, svc, Notification{}, Destination{Service: "hook"})
	if err == nil || strings.Contains(err.Error(), "supersecretvalue") {
		t.Fatalf("secret leaked or no error: %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 403 || strings.Contains(se.Body, "supersecretvalue") || strings.Contains(se.URL, "supersecretvalue") {
		t.Fatalf("status error not redacted: %+v", se)
	}
}

func TestURLValidation(t *testing.T) {
	t.Parallel()
	srv, rec := newServer(t, func(w http.ResponseWriter, _ captured) { w.WriteHeader(200) })

	t.Run("scheme", func(t *testing.T) {
		t.Parallel()
		svc := mustNew(t, TypeWebhook, map[string]any{"url": "file:///etc/passwd"}, testDeps())
		if err := send(t, svc, Notification{}, Destination{Service: "h"}); !errors.Is(err, ErrURLNotAllowed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("hook", func(t *testing.T) {
		t.Parallel()
		deps := testDeps()
		deps.Network.ValidateURL = func(u *url.URL) error {
			if u.Hostname() == "127.0.0.1" {
				return errors.New("loopback is blocked")
			}
			return nil
		}
		svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL}, deps)
		if err := send(t, svc, Notification{}, Destination{Service: "h"}); !errors.Is(err, ErrURLNotAllowed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("guarded dialer", func(t *testing.T) {
		t.Parallel()
		deps := testDeps()
		deps.Network.DialContext = NewGuardedDialer(GuardOptions{})
		svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL}, deps)
		err := send(t, svc, Notification{}, Destination{Service: "h"})
		if !errors.Is(err, ErrURLNotAllowed) {
			t.Fatalf("guarded dialer must block loopback, got %v", err)
		}
	})
	t.Run("guarded dialer allow-list", func(t *testing.T) {
		t.Parallel()
		deps := testDeps()
		deps.Network.DialContext = NewGuardedDialer(GuardOptions{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
		svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL}, deps)
		mustSend(t, svc, Notification{}, Destination{Service: "h"})
	})
	t.Run("redirect revalidated", func(t *testing.T) {
		t.Parallel()
		redirector, _ := newServer(t, func(w http.ResponseWriter, _ captured) {
			w.Header().Set("Location", "ftp://example.com/x")
			w.WriteHeader(http.StatusFound)
		})
		svc := mustNew(t, TypeWebhook, map[string]any{"url": redirector.URL}, testDeps())
		if err := send(t, svc, Notification{}, Destination{Service: "h"}); !errors.Is(err, ErrURLNotAllowed) {
			t.Fatalf("got %v", err)
		}
	})
	_ = rec
}

func TestCustomRootCAs(t *testing.T) {
	t.Parallel()
	srv, rec := newTLSServer(t)
	cert := srv.Certificate()
	deps := testDeps()
	// Without the resolver the self-signed certificate is rejected (and not retried).
	svc := mustNew(t, TypeWebhook, map[string]any{"url": srv.URL}, deps)
	if err := send(t, svc, Notification{}, Destination{Service: "h"}); err == nil {
		t.Fatalf("expected TLS verification failure")
	}
	if len(rec.all()) != 0 {
		t.Fatalf("request reached the server despite TLS failure")
	}
	deps.Network.RootCAs = func(string) (*x509.CertPool, error) {
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		return pool, nil
	}
	svc = mustNew(t, TypeWebhook, map[string]any{"url": srv.URL}, deps)
	mustSend(t, svc, Notification{}, Destination{Service: "h"})
	// insecureSkipVerify also works.
	svc = mustNew(t, TypeWebhook, map[string]any{"url": srv.URL, "insecureSkipVerify": true}, testDeps())
	mustSend(t, svc, Notification{}, Destination{Service: "h"})
}
