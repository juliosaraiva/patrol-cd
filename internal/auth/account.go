package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Settings keys (patrol-cm / patrol-secret) owned by this package. Names follow the
// upstream argocd-cm / argocd-secret layout for migration familiarity.
const (
	// AdminUsername is the built-in administrator account.
	AdminUsername = "admin"

	KeyAdminPassword      = "admin.password"      // patrol-secret: bcrypt hash
	KeyAdminPasswordMtime = "admin.passwordMtime" // patrol-secret: RFC 3339
	KeyAdminTokens        = "admin.tokens"        // patrol-secret: JSON []TokenMeta
	KeyAdminEnabled       = "admin.enabled"       // patrol-cm: bool, default true

	accountsKeyPrefix   = "accounts."
	suffixPassword      = "password"
	suffixPasswordMtime = "passwordMtime"
	suffixTokens        = "tokens"
	suffixEnabled       = "enabled"

	// KeyServerSecretKey is the patrol-secret key holding the current JWT signing secret.
	KeyServerSecretKey = "server.secretkey"
	// KeyServerSecretKeyPrevious optionally holds the previous signing secret during a
	// rotation; tokens signed with it still verify until it is removed.
	KeyServerSecretKeyPrevious = "server.secretkey.previous"
	// KeyPasswordPattern is the patrol-cm key for the password policy regular expression.
	KeyPasswordPattern = "passwordPattern"
	// KeySessionDuration is the patrol-cm key for the login session lifetime.
	KeySessionDuration = "users.session.duration"

	// MaxUsernameLength bounds usernames accepted at login (upstream: 32 bytes), which also
	// bounds the memory used by login failure tracking.
	MaxUsernameLength = 32
	// maxAccountNameLength bounds account names parsed from settings.
	maxAccountNameLength = 253
)

// Capability is something an account is allowed to do.
type Capability string

const (
	// CapabilityLogin allows creating interactive sessions with username/password.
	CapabilityLogin Capability = "login"
	// CapabilityAPIKey allows minting API tokens for the account.
	CapabilityAPIKey Capability = "apiKey"
)

// TokenMeta is the persisted metadata of an account API token. The JSON layout matches
// the upstream "accounts.<name>.tokens" format.
type TokenMeta struct {
	ID        string `json:"id"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp,omitempty"`
}

// Account is a local account.
type Account struct {
	Name          string
	Enabled       bool
	Capabilities  []Capability
	PasswordHash  string
	PasswordMtime time.Time // zero when never changed
	Tokens        []TokenMeta
}

// HasCapability reports whether the account has capability c.
func (a Account) HasCapability(c Capability) bool { return slices.Contains(a.Capabilities, c) }

// TokenIndex returns the index of the token with the given id, or -1.
func (a Account) TokenIndex(id string) int {
	return slices.IndexFunc(a.Tokens, func(t TokenMeta) bool { return t.ID == id })
}

// AccountLookup resolves accounts by name. Implementations return an error wrapping
// ErrAccountNotFound for unknown accounts; any other error is treated as an
// infrastructure failure.
type AccountLookup interface {
	LookupAccount(ctx context.Context, name string) (Account, error)
}

// AccountLookupFunc adapts a function to AccountLookup.
type AccountLookupFunc func(ctx context.Context, name string) (Account, error)

// LookupAccount calls f.
func (f AccountLookupFunc) LookupAccount(ctx context.Context, name string) (Account, error) {
	return f(ctx, name)
}

// Accounts is the set of local accounts keyed by name. It implements AccountLookup.
type Accounts map[string]Account

// Get returns the named account or an error wrapping ErrAccountNotFound.
func (as Accounts) Get(name string) (Account, error) {
	a, ok := as[name]
	if !ok {
		return Account{}, fmt.Errorf("%w: %q", ErrAccountNotFound, name)
	}
	return a, nil
}

// LookupAccount implements AccountLookup.
func (as Accounts) LookupAccount(_ context.Context, name string) (Account, error) {
	return as.Get(name)
}

// Names returns the account names in lexical order.
func (as Accounts) Names() []string { return slices.Sorted(maps.Keys(as)) }

// AccountConfigError reports a malformed account setting. The affected account was
// interpreted fail-closed (see AccountsFromMaps).
type AccountConfigError struct {
	Account string
	Key     string
	Err     error
}

func (e *AccountConfigError) Error() string {
	if e.Account == "" {
		return fmt.Sprintf("settings key %q: %v", e.Key, e.Err)
	}
	return fmt.Sprintf("account %q: settings key %q: %v", e.Account, e.Key, e.Err)
}

func (e *AccountConfigError) Unwrap() error { return e.Err }

// AccountsFromMaps parses local accounts from the settings ConfigMap data (cm) and the
// settings Secret data (secret, values already decoded to strings).
//
// The admin account always exists: enabled unless "admin.enabled" (or
// "accounts.admin.enabled") is false, with the login capability. Other accounts are
// declared by "accounts.<name>: <capability>[, <capability>...]" or
// "accounts.<name>.enabled" in cm; their password hash, password mtime and tokens are
// read from "accounts.<name>.password|passwordMtime|tokens" in secret.
//
// The returned Accounts is always usable. A non-nil error (a join of
// *AccountConfigError) reports malformed settings; affected accounts are interpreted
// fail-closed: an unparsable "enabled" value or password mtime disables the account, an
// unparsable token list leaves it with no valid API tokens, and unknown capabilities or
// keys are ignored. Callers should log the error and continue with the result.
func AccountsFromMaps(cm, secret map[string]string) (Accounts, error) {
	var errs []error
	addErr := func(account, key string, err error) {
		errs = append(errs, &AccountConfigError{Account: account, Key: key, Err: err})
	}

	admin := Account{Name: AdminUsername, Enabled: true, Capabilities: []Capability{CapabilityLogin}}
	accounts := Accounts{AdminUsername: admin}

	for _, key := range slices.Sorted(maps.Keys(cm)) {
		val := cm[key]
		if key == KeyAdminEnabled {
			setEnabled(accounts, AdminUsername, key, val, addErr)
			continue
		}
		rest, ok := strings.CutPrefix(key, accountsKeyPrefix)
		if !ok {
			continue
		}
		name, suffix, _ := strings.Cut(rest, ".")
		if strings.Contains(suffix, ".") {
			addErr("", key, errors.New("unexpected key format"))
			continue
		}
		if err := ValidateAccountName(name); err != nil {
			addErr("", key, err)
			continue
		}
		acct, exists := accounts[name]
		if !exists {
			acct = Account{Name: name, Enabled: true}
			accounts[name] = acct
		}
		switch suffix {
		case "":
			for capStr := range strings.SplitSeq(val, ",") {
				capStr = strings.TrimSpace(capStr)
				switch Capability(capStr) {
				case "":
				case CapabilityLogin, CapabilityAPIKey:
					if !acct.HasCapability(Capability(capStr)) {
						acct.Capabilities = append(acct.Capabilities, Capability(capStr))
					}
				default:
					addErr(name, key, fmt.Errorf("unsupported capability %q", capStr))
				}
			}
			accounts[name] = acct
		case suffixEnabled:
			setEnabled(accounts, name, key, val, addErr)
		default:
			// Upstream also creates the account for unknown suffixes; it simply has no
			// capabilities. Keep that behavior but report the key.
			addErr(name, key, errors.New("unsupported account setting"))
		}
	}

	for _, name := range accounts.Names() {
		acct := accounts[name]
		if v, ok := secret[passwordKey(name)]; ok {
			acct.PasswordHash = v
		}
		if v, ok := secret[passwordMtimeKey(name)]; ok && v != "" {
			mt, err := time.Parse(time.RFC3339, v)
			if err != nil {
				acct.Enabled = false
				addErr(name, passwordMtimeKey(name), fmt.Errorf("invalid RFC 3339 time: %w", err))
			} else {
				acct.PasswordMtime = mt
			}
		}
		if v, ok := secret[tokensKey(name)]; ok && v != "" {
			tokens, err := parseTokens(v)
			if err != nil {
				addErr(name, tokensKey(name), err)
			} else {
				acct.Tokens = tokens
			}
		}
		accounts[name] = acct
	}
	return accounts, errors.Join(errs...)
}

func setEnabled(accounts Accounts, name, key, val string, addErr func(string, string, error)) {
	acct := accounts[name]
	enabled, err := strconv.ParseBool(strings.TrimSpace(val))
	if err != nil {
		acct.Enabled = false
		addErr(name, key, fmt.Errorf("invalid boolean %q", val))
	} else {
		// Any "false" wins (fail closed) when both admin.enabled and
		// accounts.admin.enabled are set.
		acct.Enabled = acct.Enabled && enabled
	}
	accounts[name] = acct
}

func parseTokens(v string) ([]TokenMeta, error) {
	var tokens []TokenMeta
	if err := json.Unmarshal([]byte(v), &tokens); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptTokenList, err)
	}
	for _, t := range tokens {
		if t.ID == "" {
			return nil, fmt.Errorf("%w: token without id", ErrCorruptTokenList)
		}
	}
	return tokens, nil
}

// ValidateAccountName rejects names that cannot be represented safely in settings keys
// and token subjects: empty, too long, containing '.', ':' (the subject separator),
// whitespace or control characters.
func ValidateAccountName(name string) error {
	if name == "" || len(name) > maxAccountNameLength {
		return fmt.Errorf("%w: length must be 1..%d", ErrInvalidAccountName, maxAccountNameLength)
	}
	for _, r := range name {
		if r == ':' || r == '.' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("%w: %q contains a forbidden character", ErrInvalidAccountName, name)
		}
	}
	return nil
}

func passwordKey(name string) string {
	if name == AdminUsername {
		return KeyAdminPassword
	}
	return accountsKeyPrefix + name + "." + suffixPassword
}

func passwordMtimeKey(name string) string {
	if name == AdminUsername {
		return KeyAdminPasswordMtime
	}
	return accountsKeyPrefix + name + "." + suffixPasswordMtime
}

func tokensKey(name string) string {
	if name == AdminUsername {
		return KeyAdminTokens
	}
	return accountsKeyPrefix + name + "." + suffixTokens
}

// SecretWithPassword returns a copy of secret with the account's password hash and
// password mtime set. The mtime is stored with second precision (RFC 3339, UTC), which is
// also the precision of token "iat" claims.
func SecretWithPassword(secret map[string]string, account, hash string, mtime time.Time) (map[string]string, error) {
	if err := ValidateAccountName(account); err != nil {
		return nil, err
	}
	out := maps.Clone(secret)
	if out == nil {
		out = map[string]string{}
	}
	if hash == "" {
		delete(out, passwordKey(account))
		delete(out, passwordMtimeKey(account))
		return out, nil
	}
	out[passwordKey(account)] = hash
	out[passwordMtimeKey(account)] = mtime.UTC().Truncate(time.Second).Format(time.RFC3339)
	return out, nil
}

// SecretWithTokens returns a copy of secret with the account's token list replaced.
// An empty list removes the key.
func SecretWithTokens(secret map[string]string, account string, tokens []TokenMeta) (map[string]string, error) {
	if err := ValidateAccountName(account); err != nil {
		return nil, err
	}
	out := maps.Clone(secret)
	if out == nil {
		out = map[string]string{}
	}
	if len(tokens) == 0 {
		delete(out, tokensKey(account))
		return out, nil
	}
	b, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("encode tokens: %w", err)
	}
	out[tokensKey(account)] = string(b)
	return out, nil
}

// ChangePassword validates newPassword against policy, hashes it and returns a copy of
// secret with the new hash and a password mtime of now. Every session and API token of
// the account issued before now stops verifying.
//
// Authorization (current-password check, RBAC for changing other users' passwords) is
// the caller's responsibility.
func ChangePassword(secret map[string]string, acct Account, newPassword string, policy *PasswordPolicy, now time.Time) (map[string]string, error) {
	if policy == nil {
		var err error
		if policy, err = NewPasswordPolicy(""); err != nil {
			return nil, err
		}
	}
	if err := policy.Validate(newPassword); err != nil {
		return nil, err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return nil, err
	}
	return SecretWithPassword(secret, acct.Name, hash, now)
}

// AddToken returns a copy of secret with tok appended to the account's token list. The
// current list is read from secret itself (not from acct), so the result is consistent
// with the Secret version the caller will update. The account must be enabled and have
// the apiKey capability.
func AddToken(secret map[string]string, acct Account, tok TokenMeta) (map[string]string, error) {
	if !acct.Enabled {
		return nil, fmt.Errorf("%w: %q", ErrAccountDisabled, acct.Name)
	}
	if !acct.HasCapability(CapabilityAPIKey) {
		return nil, fmt.Errorf("%w: account %q does not have %s capability", ErrMissingCapability, acct.Name, CapabilityAPIKey)
	}
	if tok.ID == "" {
		return nil, errors.New("token id must not be empty")
	}
	tokens, err := tokensFromSecret(secret, acct.Name)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(tokens, func(t TokenMeta) bool { return t.ID == tok.ID }) {
		return nil, fmt.Errorf("%w: %q", ErrTokenExists, tok.ID)
	}
	return SecretWithTokens(secret, acct.Name, append(tokens, tok))
}

// DeleteToken returns a copy of secret without the token id. Deleting the metadata is
// what revokes an account API token: verification requires the token ID to be listed.
func DeleteToken(secret map[string]string, acct Account, id string) (map[string]string, error) {
	tokens, err := tokensFromSecret(secret, acct.Name)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(tokens, func(t TokenMeta) bool { return t.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("%w: %q", ErrTokenNotFound, id)
	}
	return SecretWithTokens(secret, acct.Name, slices.Delete(tokens, i, i+1))
}

func tokensFromSecret(secret map[string]string, account string) ([]TokenMeta, error) {
	if err := ValidateAccountName(account); err != nil {
		return nil, err
	}
	v := secret[tokensKey(account)]
	if v == "" {
		return nil, nil
	}
	return parseTokens(v)
}

// SecretData converts Kubernetes Secret data to the string map used by this package.
func SecretData(data map[string][]byte) map[string]string {
	out := make(map[string]string, len(data))
	for k, v := range data {
		out[k] = string(v)
	}
	return out
}
