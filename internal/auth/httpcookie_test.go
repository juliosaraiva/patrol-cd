package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

const sampleJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"

func fakeJWT(n int) string {
	if n < 10 {
		n = 10
	}
	body := strings.Repeat("A", n-8)
	return "eyJh." + body + ".sig"
}

// requestWithCookies returns a request carrying the cookies set on rec.
func requestWithCookies(t *testing.T, rec *httptest.ResponseRecorder) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			continue
		}
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return r
}

func TestTokenFromRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		header  []string
		cookies map[string]string
		want    string
		wantErr error
	}{
		{name: "bearer", header: []string{"Bearer " + sampleJWT}, want: sampleJWT},
		{name: "bearer case-insensitive", header: []string{"bearer   " + sampleJWT}, want: sampleJWT},
		{name: "bearer preferred over cookie", header: []string{"Bearer " + sampleJWT}, cookies: map[string]string{SessionCookieName: "a.b.c"}, want: sampleJWT},
		{name: "basic ignored, cookie used", header: []string{"Basic dXNlcjpwYXNz"}, cookies: map[string]string{SessionCookieName: sampleJWT}, want: sampleJWT},
		{name: "non-JWT bearer falls back", header: []string{"Bearer opaque"}, cookies: map[string]string{SessionCookieName: sampleJWT}, want: sampleJWT},
		{name: "cookie", cookies: map[string]string{SessionCookieName: sampleJWT}, want: sampleJWT},
		{name: "chunked cookie", cookies: map[string]string{SessionCookieName: "3:eyJh.AA", "patrol.token.1": "AAAA", "patrol.token.2": "AA.sig"}, want: "eyJh.AAAAAAAA.sig"},
		{name: "nothing", wantErr: ErrNoToken},
		{name: "non-JWT bearer without cookie", header: []string{"Bearer opaque"}, wantErr: ErrNoToken},
		{name: "missing chunk", cookies: map[string]string{SessionCookieName: "3:eyJh.AA", "patrol.token.1": "AAAA"}, wantErr: ErrInvalidCookie},
		{name: "chunk count too large", cookies: map[string]string{SessionCookieName: "99:x"}, wantErr: ErrInvalidCookie},
		{name: "chunk count one", cookies: map[string]string{SessionCookieName: "1:x.y.z"}, wantErr: ErrInvalidCookie},
		{name: "chunk count padded", cookies: map[string]string{SessionCookieName: "02:a.b", "patrol.token.1": ".c"}, wantErr: ErrInvalidCookie},
		{name: "chunk count not a number", cookies: map[string]string{SessionCookieName: "x:a.b.c"}, wantErr: ErrInvalidCookie},
		{name: "cookie not a JWT", cookies: map[string]string{SessionCookieName: "opaque"}, wantErr: ErrInvalidCookie},
		{name: "empty cookie", cookies: map[string]string{SessionCookieName: ""}, wantErr: ErrInvalidCookie},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, h := range tc.header {
				r.Header.Add("Authorization", h)
			}
			for k, v := range tc.cookies {
				r.AddCookie(&http.Cookie{Name: k, Value: v})
			}
			got, err := TokenFromRequest(r)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestSetSessionCookieAttributes(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := SetSessionCookie(rec, nil, sampleJWT, CookieOptions{Path: "patrol/", Secure: true}); err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != SessionCookieName || c.Value != sampleJWT || c.Path != "/patrol" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || !c.Expires.IsZero() {
		t.Fatalf("cookie = %+v", c)
	}
	rec2 := httptest.NewRecorder()
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := SetSessionCookie(rec2, nil, sampleJWT, CookieOptions{Expires: exp}); err != nil {
		t.Fatal(err)
	}
	c2 := rec2.Result().Cookies()[0]
	if c2.Path != "/" || c2.Secure || !c2.Expires.Equal(exp) {
		t.Fatalf("cookie = %+v", c2)
	}
	for _, bad := range []string{"", "a b.c.d", "a;b.c.d", "a\"b.c.d", "a,b.c.d", "ä.b.c"} {
		if err := SetSessionCookie(httptest.NewRecorder(), nil, bad, CookieOptions{}); !errors.Is(err, ErrInvalidCookie) {
			t.Fatalf("token %q: %v", bad, err)
		}
	}
}

func TestSessionCookieChunkingRoundTrip(t *testing.T) {
	t.Parallel()
	for _, size := range []int{100, 3900, 4100, 20_000, 70_000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			tok := fakeJWT(size)
			rec := httptest.NewRecorder()
			opts := CookieOptions{Path: "/some/root/path", Secure: true, Expires: time.Now().Add(time.Hour)}
			if err := SetSessionCookie(rec, nil, tok, opts); err != nil {
				t.Fatal(err)
			}
			for _, h := range rec.Header().Values("Set-Cookie") {
				if len(h) > maxCookieLength {
					t.Fatalf("Set-Cookie of %d bytes exceeds %d", len(h), maxCookieLength)
				}
			}
			got, err := TokenFromRequest(requestWithCookies(t, rec))
			if err != nil || got != tok {
				t.Fatalf("round trip failed: len(got)=%d err=%v", len(got), err)
			}
		})
	}
	if err := SetSessionCookie(httptest.NewRecorder(), nil, fakeJWT(MaxCookieChunks*4096), CookieOptions{}); !errors.Is(err, ErrTokenTooLarge) {
		t.Fatalf("oversized token: %v", err)
	}
}

func TestSetSessionCookieClearsStaleChunks(t *testing.T) {
	t.Parallel()
	big := httptest.NewRecorder()
	if err := SetSessionCookie(big, nil, fakeJWT(15_000), CookieOptions{}); err != nil {
		t.Fatal(err)
	}
	r := requestWithCookies(t, big)
	if len(r.Cookies()) < 4 {
		t.Fatalf("expected several chunks, got %d", len(r.Cookies()))
	}
	small := httptest.NewRecorder()
	if err := SetSessionCookie(small, r, sampleJWT, CookieOptions{}); err != nil {
		t.Fatal(err)
	}
	expired := map[string]bool{}
	for _, c := range small.Result().Cookies() {
		if c.MaxAge < 0 {
			expired[c.Name] = true
		}
	}
	for _, c := range r.Cookies() {
		if c.Name != SessionCookieName && !expired[c.Name] {
			t.Fatalf("stale chunk %s not cleared", c.Name)
		}
	}
	// Browser view after applying the response: just the small token.
	after := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range small.Result().Cookies() {
		if c.MaxAge >= 0 {
			after.AddCookie(c)
		}
	}
	if got, err := TokenFromRequest(after); err != nil || got != sampleJWT {
		t.Fatalf("after replace: %q %v", got, err)
	}
}

func TestClearSessionCookie(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "2:a"})
	r.AddCookie(&http.Cookie{Name: "patrol.token.1", Value: "b"})
	r.AddCookie(&http.Cookie{Name: "patrol.tokenX", Value: "unrelated"})
	r.AddCookie(&http.Cookie{Name: "other", Value: "x"})
	rec := httptest.NewRecorder()
	ClearSessionCookie(rec, r, CookieOptions{Path: "/p", Secure: true})
	got := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		got[c.Name] = c
	}
	if len(got) != 2 || got[SessionCookieName] == nil || got["patrol.token.1"] == nil {
		t.Fatalf("cleared = %v", got)
	}
	for _, c := range got {
		if c.MaxAge >= 0 || c.Path != "/p" || !c.HttpOnly || !c.Secure {
			t.Fatalf("clear cookie = %+v", c)
		}
	}
	rec2 := httptest.NewRecorder()
	ClearSessionCookie(rec2, nil, CookieOptions{})
	if n := len(rec2.Result().Cookies()); n != 1 {
		t.Fatalf("clear without request set %d cookies", n)
	}
}

func TestCookiePath(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "/", "/": "/", "patrol": "/patrol", "/patrol/": "/patrol", "//a/b//": "/a/b"} {
		if got := CookiePath(in); got != want {
			t.Errorf("CookiePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Property: any JWT-shaped token that fits in MaxCookieChunks cookies round-trips.
func TestCookieSplitJoinProperty(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		seg := func(label string, maxLen int) string {
			return rapid.StringMatching(`[A-Za-z0-9_-]{1,`+strconv.Itoa(maxLen)+`}`).Draw(rt, label)
		}
		n := rapid.IntRange(1, 60_000).Draw(rt, "payloadLen")
		tok := seg("header", 40) + "." + strings.Repeat("x", n) + seg("payloadTail", 20) + "." + seg("sig", 86)
		root := rapid.SampledFrom([]string{"", "/", "/patrol", "/a/very/long/root/path/for/patrol"}).Draw(rt, "root")
		rec := httptest.NewRecorder()
		if err := SetSessionCookie(rec, nil, tok, CookieOptions{Path: root, Secure: true}); err != nil {
			rt.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range rec.Result().Cookies() {
			r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
		got, err := TokenFromRequest(r)
		if err != nil || got != tok {
			rt.Fatalf("round trip: err=%v equal=%v", err, got == tok)
		}
	})
}
