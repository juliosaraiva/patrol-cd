package cmdutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseOpts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    Opts
		wantErr string
	}{
		{"", Opts{}, ""},
		{"--server cd.example.com --plaintext", Opts{"server": "cd.example.com", "plaintext": "true"}, ""},
		{"--plaintext --server=cd.example.com:443", Opts{"plaintext": "true", "server": "cd.example.com:443"}, ""},
		{`--header "X-A: 1" --header 'X-B: 2'`, Opts{"header": "X-B: 2"}, ""},
		{`--grpc-web-root-path /patrol\ cd`, Opts{"grpc-web-root-path": "/patrol cd"}, ""},
		{"--insecure --plaintext", Opts{"insecure": "true", "plaintext": "true"}, ""},
		{"server x", nil, "PATROL_OPTS invalid at 'server'"},
		{`--server "x`, nil, "unterminated double-quoted string"},
		{`--server 'x`, nil, "unterminated single-quoted string"},
		{strings.Repeat("a", maxOptsBytes+1), nil, "exceeds"},
	}
	for _, tc := range cases {
		got, err := ParseOpts(EnvOpts, tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%q: err %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("%q: %v %v, want %v", tc.in, got, err, tc.want)
		}
	}
	o := Opts{"n": "3", "bad": "x", "list": `a,"b,c"`, "empty": ""}
	if n, err := o.Int("n", 0); n != 3 || err != nil {
		t.Fatal("Int")
	}
	if _, err := o.Int("bad", 0); err == nil {
		t.Fatal("Int accepted x")
	}
	if l, err := o.StringSlice("list", nil); err != nil || len(l) != 2 || l[1] != "b,c" {
		t.Fatalf("StringSlice: %v %v", l, err)
	}
	if l, _ := o.StringSlice("empty", []string{"d"}); len(l) != 0 {
		t.Fatal("empty StringSlice")
	}
	if !(Opts{"plaintext": "true"}).Bool("plaintext") || (Opts{}).Bool("plaintext") {
		t.Fatal("Bool")
	}
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err   error
		code  int
		print bool
	}{
		{nil, 0, false},
		{errors.New("x"), ExitGeneric, true},
		{Fatalf("boom"), ExitFatal, true},
		{Silent(7), 7, false},
		{Usagef("bad"), ExitUsage, true},
		{fmt.Errorf("wrapped: %w", Silent(3)), 3, false},
	} {
		code, p := ExitCodeOf(tc.err)
		if code != tc.code || p != tc.print {
			t.Fatalf("%v: (%d,%v), want (%d,%v)", tc.err, code, p, tc.code, tc.print)
		}
	}
}

func TestPrompter(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := NewPrompter(strings.NewReader("maybe\nY\nn\na\n\nname\n"), &out)
	if ok, err := p.Confirm("go? "); !ok || err != nil {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	if strings.Count(out.String(), "go? ") != 2 {
		t.Fatalf("invalid answers must re-ask: %q", out.String())
	}
	if ok, _ := p.Confirm("again? "); ok {
		t.Fatal("n accepted as yes")
	}
	if yes, all, _ := p.ConfirmAll("all? "); !yes || !all {
		t.Fatal("a not all")
	}
	if v, err := p.Input("Name"); v != "name" || err != nil {
		t.Fatalf("input: %q %v", v, err)
	}
	if _, err := p.Input("More"); !errors.Is(err, ErrNoInput) {
		t.Fatalf("EOF: %v", err)
	}
	// Optional prompts answer yes when disabled.
	if ok, err := (OptionalPrompt{Enabled: false, P: NewPrompter(strings.NewReader(""), io.Discard)}).Confirm("x"); !ok || err != nil {
		t.Fatal("disabled optional prompt")
	}
	var npOut bytes.Buffer
	np := NewPrompter(strings.NewReader("a1\nb2\nc3\nc3\n"), &npOut)
	if v, err := np.NewPassword("bob"); v != "c3" || err != nil || !strings.Contains(npOut.String(), "Passwords do not match") {
		t.Fatalf("new password: %q %v %q", v, err, npOut.String())
	}
	if _, err := NewPrompter(strings.NewReader(strings.Repeat("x\ny\n", 5)), io.Discard).NewPassword("bob"); err == nil || err.Error() != "passwords do not match" {
		t.Fatalf("five mismatches: %v", err)
	}
}

func TestDebugTransportRedacts(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	hc := withDebugLog(&http.Client{Timeout: 5 * time.Second}, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/repositories/https:%2F%2Fuser:s3cret@git.example.com%2Frepo?token=t0ken", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bearer-secret")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	l := logs.String()
	for _, s := range []string{"s3cret", "t0ken", "bearer-secret"} {
		if strings.Contains(l, s) {
			t.Fatalf("debug log leaks %q: %s", s, l)
		}
	}
	for _, s := range []string{`"status":418`, `"requestId":"req-1"`, `"method":"GET"`, "redacted@git.example.com"} {
		if !strings.Contains(l, s) {
			t.Fatalf("debug log lacks %q: %s", s, l)
		}
	}
}
