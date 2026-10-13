package auth

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSigningKey(t *testing.T) {
	t.Parallel()
	if _, err := NewSigningKey(nil); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := NewSigningKey([]byte("too-short")); !errors.Is(err, ErrWeakSigningKey) {
		t.Fatalf("short: %v", err)
	}
	k1, err := NewSigningKey(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	k1b, _ := NewSigningKey(testSecret)
	k2, _ := NewSigningKey(testSecretPrev)
	if k1.ID() != k1b.ID() || k1.ID() == k2.ID() || len(k1.ID()) != 16 {
		t.Fatalf("key ids: %q %q %q", k1.ID(), k1b.ID(), k2.ID())
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("key", "k", k1)
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", k1, k1, k1, k1), buf.String()} {
		if strings.Contains(s, string(testSecret)) || strings.Contains(s, "0123456789abcdef") {
			t.Fatalf("secret leaked in %q", s)
		}
	}
	// The key copies its input.
	secret := bytes.Clone(testSecret)
	k3, _ := NewSigningKey(secret)
	secret[0] ^= 0xff
	if !bytes.Equal(k3.secret, testSecret) {
		t.Fatal("signing key aliases caller buffer")
	}
}

func TestKeySet(t *testing.T) {
	t.Parallel()
	if _, err := KeySetFromSecret(map[string]string{}); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := KeySetFromSecret(map[string]string{KeyServerSecretKey: string(testSecret), KeyServerSecretKeyPrevious: "short"}); !errors.Is(err, ErrWeakSigningKey) {
		t.Fatalf("weak previous: %v", err)
	}
	ks, err := KeySetFromSecret(map[string]string{KeyServerSecretKey: string(testSecret), KeyServerSecretKeyPrevious: string(testSecretPrev)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ks.Previous) != 1 {
		t.Fatalf("previous keys = %d", len(ks.Previous))
	}
	if _, ok := ks.lookup(ks.Previous[0].ID()); !ok {
		t.Fatal("previous key not found")
	}
	if _, ok := ks.lookup(""); ok {
		t.Fatal("empty kid matched")
	}
	same, err := NewKeySet(testSecret, testSecret, nil)
	if err != nil || len(same.Previous) != 0 {
		t.Fatalf("duplicate previous key kept: %v %d", err, len(same.Previous))
	}
}

func TestParseSessionDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultSessionDuration, false},
		{"24h", 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"30s", 30 * time.Second, false},
		{"1h30m", 90 * time.Minute, false},
		{" 2h ", 2 * time.Hour, false},
		{"0", DefaultSessionDuration, true},
		{"0h", DefaultSessionDuration, true},
		{"-1h", DefaultSessionDuration, true},
		{"10x", DefaultSessionDuration, true},
		{"abc", DefaultSessionDuration, true},
		{"999999999999999999d", DefaultSessionDuration, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSessionDuration(tc.in)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("ParseSessionDuration(%q) = %v, %v; want %v, err=%v", tc.in, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
