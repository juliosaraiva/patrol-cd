package auth

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashPasswordDefaultCost(t *testing.T) {
	t.Parallel()
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	cost, err := bcrypt.Cost([]byte(h))
	if err != nil {
		t.Fatal(err)
	}
	if cost != 10 {
		t.Fatalf("cost = %d, want 10", cost)
	}
	if PasswordNeedsRehash(h) {
		t.Fatal("default-cost hash should not need rehash")
	}
	if !PasswordNeedsRehash(mustHash(t, "x")) || !PasswordNeedsRehash("not-a-hash") {
		t.Fatal("low-cost or invalid hash should need rehash")
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	t.Parallel()
	h := mustHash(t, "s3cret-password")
	tests := []struct {
		name     string
		password string
		hash     string
		want     bool
	}{
		{"match", "s3cret-password", h, true},
		{"wrong", "s3cret-passwordX", h, false},
		{"blank password", "", h, false},
		{"empty hash", "s3cret-password", "", false},
		{"garbage hash", "s3cret-password", "$2a$garbage", false},
		{"plaintext stored", "s3cret-password", "s3cret-password", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := VerifyPassword(tc.password, tc.hash); got != tc.want {
				t.Fatalf("VerifyPassword = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHashPasswordRejects(t *testing.T) {
	t.Parallel()
	if _, err := HashPassword(""); !errors.Is(err, ErrBlankPassword) {
		t.Fatalf("blank: %v", err)
	}
	if _, err := HashPasswordWithCost(strings.Repeat("a", 73), bcrypt.MinCost); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("73 bytes: %v", err)
	}
	if _, err := HashPasswordWithCost("pw", bcrypt.MaxCost+1); err == nil {
		t.Fatal("cost above max accepted")
	}
	// Costs below the minimum are raised, not rejected.
	h, err := HashPasswordWithCost("pw", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := bcrypt.Cost([]byte(h)); c < bcrypt.MinCost {
		t.Fatalf("cost %d below minimum", c)
	}
}

func TestPasswordPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pattern  string
		password string
		wantErr  error
	}{
		{"default ok", "", "12345678", nil},
		{"default 32 ok", "", strings.Repeat("x", 32), nil},
		{"default too short", "", "1234567", ErrPasswordPolicy},
		{"default too long", "", strings.Repeat("x", 33), ErrPasswordPolicy},
		{"blank", "", "", ErrBlankPassword},
		{"custom ok", `^[a-z]+[0-9]+$`, "abc123", nil},
		{"custom fail", `^[a-z]+[0-9]+$`, "ABC123", ErrPasswordPolicy},
		{"bcrypt limit beats permissive pattern", `^.*$`, strings.Repeat("y", 73), ErrPasswordPolicy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := NewPasswordPolicy(tc.pattern)
			if err != nil {
				t.Fatal(err)
			}
			err = p.Validate(tc.password)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestPasswordPolicyErrorMessageAndSettings(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"(", `^(?=.*\d).{4,}$`} { // RE2: no lookahead
		if _, err := NewPasswordPolicy(bad); err == nil {
			t.Fatalf("pattern %q compiled", bad)
		}
	}
	p, err := PasswordPolicyFromSettings(map[string]string{KeyPasswordPattern: `^.{10,}$`})
	if err != nil {
		t.Fatal(err)
	}
	if p.Pattern() != `^.{10,}$` {
		t.Fatalf("pattern = %q", p.Pattern())
	}
	err = p.Validate("short")
	var pe *PasswordPolicyError
	if !errors.As(err, &pe) || pe.Pattern != `^.{10,}$` {
		t.Fatalf("err = %#v", err)
	}
	if got, want := err.Error(), "new password does not match the following expression: ^.{10,}$"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	d, err := PasswordPolicyFromSettings(nil)
	if err != nil || d.Pattern() != DefaultPasswordPattern {
		t.Fatalf("default policy: %v %q", err, d.Pattern())
	}
}

func TestGenerateInitialPassword(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	policy, _ := NewPasswordPolicy("")
	for range 50 {
		pw, err := GenerateInitialPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != InitialPasswordLength || strings.Trim(pw, initialPasswordAlphabet) != "" {
			t.Fatalf("password %q has wrong length or alphabet", pw)
		}
		if err := policy.Validate(pw); err != nil {
			t.Fatalf("generated password violates default policy: %v", err)
		}
		if seen[pw] {
			t.Fatalf("duplicate password %q", pw)
		}
		seen[pw] = true
	}
}
