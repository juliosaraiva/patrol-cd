package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// Issuer is the "iss" claim of every Patrol-issued token.
	Issuer = "patrol"
	// DefaultLeeway is the clock-skew allowance applied to exp, nbf and iat.
	DefaultLeeway = 10 * time.Second
	// DefaultRenewWithin is how close to expiry a login session must be before
	// RenewIfExpiring issues a replacement (upstream: 5 minutes).
	DefaultRenewWithin = 5 * time.Minute
	// MaxTokenLength bounds accepted token strings (bytes). It matches the capacity of the
	// split session cookie (MaxCookieChunks chunks).
	MaxTokenLength = 80 * 1024
	// MaxGroups bounds the number of groups carried in a token.
	MaxGroups = 1000
	// ProjectSubjectPrefix prefixes project-role token subjects ("proj:<project>:<role>").
	ProjectSubjectPrefix = "proj:"

	claimKind       = "kind"
	maxExtraClaims  = 32
	maxSubjectBytes = 1024
	maxTokenIDBytes = 256
)

// TokenKind distinguishes the kinds of Patrol-issued tokens. It is carried in the "kind"
// claim and must agree with the subject format, so one kind can never be replayed as
// another (e.g. an SSO subject that looks like "admin:login").
type TokenKind string

const (
	// KindLogin is an interactive session for a local account (sub "<account>:login").
	KindLogin TokenKind = "login"
	// KindAPIKey is an account API token (sub "<account>:apiKey").
	KindAPIKey TokenKind = "apiKey"
	// KindProject is a project-role token (sub "proj:<project>:<role>").
	KindProject TokenKind = "project"
	// KindSSO is a Patrol session created after an OIDC login (sub = IdP user identifier).
	KindSSO TokenKind = "sso"
	// KindExternal marks principals verified by an ExternalVerifier (IdP-issued tokens).
	KindExternal TokenKind = "external"
)

var reservedClaims = []string{"iss", "sub", "aud", "exp", "nbf", "iat", "jti", claimKind, "groups", "email", "name"}

// Principal is a verified identity.
type Principal struct {
	// Subject is the RBAC subject: the account name for login/apiKey tokens,
	// "proj:<project>:<role>" for project tokens, the IdP user identifier otherwise.
	Subject   string
	Kind      TokenKind
	Issuer    string
	TokenID   string
	IssuedAt  time.Time
	ExpiresAt time.Time // zero when the token never expires
	Groups    []string
	Email     string
	Name      string
	// Project and Role are set for KindProject.
	Project string
	Role    string
	// Claims holds all token claims (with "sub" normalized to Subject for account
	// tokens) for RBAC scope evaluation.
	Claims map[string]any
}

// Username returns a human-readable user name: the subject for local identities and the
// email (falling back to the subject) for SSO identities.
func (p *Principal) Username() string {
	if (p.Kind == KindSSO || p.Kind == KindExternal) && p.Email != "" {
		return p.Email
	}
	return p.Subject
}

// Issued is a freshly signed token.
type Issued struct {
	Token     string
	ID        string
	IssuedAt  time.Time
	ExpiresAt time.Time // zero when the token never expires
}

// SSOIdentity describes a user authenticated by an external IdP, used to mint a Patrol
// session (KindSSO).
type SSOIdentity struct {
	Subject string
	Email   string
	Name    string
	Groups  []string
	// Extra carries additional claims needed for RBAC scopes (e.g. "roles"). Reserved
	// claim names are rejected.
	Extra map[string]any
}

// ExternalVerifier verifies tokens not issued by Patrol (iss != "patrol"), e.g. OIDC ID
// tokens presented by the CLI. Token problems must be reported wrapping ErrInvalidToken.
type ExternalVerifier interface {
	VerifyExternal(ctx context.Context, raw string) (*Principal, error)
}

// TokenServiceConfig configures a TokenService.
type TokenServiceConfig struct {
	Keys        KeySet
	Accounts    AccountLookup   // required for login and apiKey tokens
	Revocations RevocationStore // required
	Projects    TokenRegistry   // optional; nil rejects project tokens
	External    ExternalVerifier
	Leeway      time.Duration // <= 0 selects DefaultLeeway
	RenewWithin time.Duration // <= 0 selects DefaultRenewWithin
	Now         func() time.Time
}

// TokenService issues and verifies Patrol tokens (JWT, HS256, iss "patrol").
type TokenService struct {
	keys        atomic.Pointer[KeySet]
	accounts    AccountLookup
	revocations RevocationStore
	projects    TokenRegistry
	external    ExternalVerifier
	leeway      time.Duration
	renewWithin time.Duration
	now         func() time.Time
}

// NewTokenService validates cfg and returns a TokenService.
func NewTokenService(cfg TokenServiceConfig) (*TokenService, error) {
	if cfg.Keys.Current.id == "" {
		return nil, ErrNoSigningKey
	}
	if cfg.Accounts == nil {
		return nil, errors.New("token service: account lookup is required")
	}
	if cfg.Revocations == nil {
		return nil, errors.New("token service: revocation store is required")
	}
	s := &TokenService{
		accounts:    cfg.Accounts,
		revocations: cfg.Revocations,
		projects:    cfg.Projects,
		external:    cfg.External,
		leeway:      cfg.Leeway,
		renewWithin: cfg.RenewWithin,
		now:         cfg.Now,
	}
	if s.leeway <= 0 {
		s.leeway = DefaultLeeway
	}
	if s.renewWithin <= 0 {
		s.renewWithin = DefaultRenewWithin
	}
	if s.now == nil {
		s.now = time.Now
	}
	ks := cfg.Keys
	s.keys.Store(&ks)
	return s, nil
}

// SetKeys atomically replaces the key set (e.g. after the settings Secret changed).
func (s *TokenService) SetKeys(ks KeySet) error {
	if ks.Current.id == "" {
		return ErrNoSigningKey
	}
	s.keys.Store(&ks)
	return nil
}

// IssueLoginSession creates an interactive session for acct, which must be enabled and
// have the login capability. ttl <= 0 selects DefaultSessionDuration.
func (s *TokenService) IssueLoginSession(_ context.Context, acct Account, ttl time.Duration) (Issued, error) {
	if err := checkAccount(acct, CapabilityLogin); err != nil {
		return Issued{}, err
	}
	if ttl <= 0 {
		ttl = DefaultSessionDuration
	}
	id, err := newTokenID()
	if err != nil {
		return Issued{}, err
	}
	return s.sign(KindLogin, acct.Name+":"+string(CapabilityLogin), id, ttl, nil)
}

// IssueAccountToken mints an API token for acct, which must be enabled and have the
// apiKey capability. An empty id generates a random one; expiresIn == 0 creates a token
// without expiry. The returned TokenMeta must be persisted with AddToken; until it is,
// the token does not verify.
func (s *TokenService) IssueAccountToken(_ context.Context, acct Account, id string, expiresIn time.Duration) (Issued, TokenMeta, error) {
	if err := checkAccount(acct, CapabilityAPIKey); err != nil {
		return Issued{}, TokenMeta{}, err
	}
	if expiresIn < 0 {
		return Issued{}, TokenMeta{}, errors.New("expiresIn must not be negative")
	}
	id, err := tokenIDOrNew(id)
	if err != nil {
		return Issued{}, TokenMeta{}, err
	}
	if acct.TokenIndex(id) >= 0 {
		return Issued{}, TokenMeta{}, fmt.Errorf("%w: %q", ErrTokenExists, id)
	}
	iss, err := s.sign(KindAPIKey, acct.Name+":"+string(CapabilityAPIKey), id, expiresIn, nil)
	if err != nil {
		return Issued{}, TokenMeta{}, err
	}
	meta := TokenMeta{ID: id, IssuedAt: iss.IssuedAt.Unix()}
	if !iss.ExpiresAt.IsZero() {
		meta.ExpiresAt = iss.ExpiresAt.Unix()
	}
	return iss, meta, nil
}

// IssueProjectToken mints a project-role token with subject "proj:<project>:<role>".
// The caller records the returned ID/IssuedAt/ExpiresAt in project status; the
// TokenRegistry consults that record during verification.
func (s *TokenService) IssueProjectToken(_ context.Context, project, role, id string, expiresIn time.Duration) (Issued, error) {
	sub, err := ProjectSubject(project, role)
	if err != nil {
		return Issued{}, err
	}
	if expiresIn < 0 {
		return Issued{}, errors.New("expiresIn must not be negative")
	}
	if id, err = tokenIDOrNew(id); err != nil {
		return Issued{}, err
	}
	return s.sign(KindProject, sub, id, expiresIn, nil)
}

// IssueSSOSession creates a Patrol session for an identity verified by an external IdP.
// ttl <= 0 selects DefaultSessionDuration.
func (s *TokenService) IssueSSOSession(_ context.Context, ident SSOIdentity, ttl time.Duration) (Issued, error) {
	if err := validateSubject(ident.Subject); err != nil {
		return Issued{}, err
	}
	if len(ident.Groups) > MaxGroups {
		return Issued{}, fmt.Errorf("too many groups: %d > %d", len(ident.Groups), MaxGroups)
	}
	if len(ident.Extra) > maxExtraClaims {
		return Issued{}, fmt.Errorf("too many extra claims: %d > %d", len(ident.Extra), maxExtraClaims)
	}
	extra := make(map[string]any, len(ident.Extra)+3)
	for k, v := range ident.Extra {
		if k == "" || slices.Contains(reservedClaims, k) {
			return Issued{}, fmt.Errorf("extra claim %q is reserved", k)
		}
		extra[k] = v
	}
	if len(ident.Groups) > 0 {
		extra["groups"] = slices.Clone(ident.Groups)
	}
	if ident.Email != "" {
		extra["email"] = ident.Email
	}
	if ident.Name != "" {
		extra["name"] = ident.Name
	}
	if ttl <= 0 {
		ttl = DefaultSessionDuration
	}
	id, err := newTokenID()
	if err != nil {
		return Issued{}, err
	}
	return s.sign(KindSSO, ident.Subject, id, ttl, extra)
}

func (s *TokenService) sign(kind TokenKind, sub, jti string, ttl time.Duration, extra map[string]any) (Issued, error) {
	ks := s.keys.Load()
	now := s.now().UTC().Truncate(time.Second)
	claims := jwt.MapClaims{}
	for k, v := range extra {
		claims[k] = v
	}
	claims["iss"] = Issuer
	claims["sub"] = sub
	claims["jti"] = jti
	claims["iat"] = now.Unix()
	claims["nbf"] = now.Unix()
	claims[claimKind] = string(kind)
	out := Issued{ID: jti, IssuedAt: now}
	if ttl > 0 {
		out.ExpiresAt = now.Add(ttl)
		claims["exp"] = out.ExpiresAt.Unix()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = ks.Current.id
	signed, err := tok.SignedString(ks.Current.secret)
	if err != nil {
		return Issued{}, fmt.Errorf("sign token: %w", err)
	}
	if len(signed) > MaxTokenLength {
		return Issued{}, fmt.Errorf("%w: %d bytes", ErrTokenTooLarge, len(signed))
	}
	out.Token = signed
	return out, nil
}

// Verify verifies raw and returns the authenticated principal. Patrol-issued tokens are
// checked for HS256 signature with a known key ID, issuer, expiry/not-before/issued-at
// (with leeway), revocation, and kind-specific state (account enabled, capability, API
// token still listed, password not changed since issue, project token still recorded).
// Other issuers are delegated to the ExternalVerifier, if any.
//
// Errors caused by the token wrap ErrInvalidToken; other errors are infrastructure
// failures.
func (s *TokenService) Verify(ctx context.Context, raw string) (*Principal, error) {
	if raw == "" {
		return nil, invalid(ErrNoToken)
	}
	if len(raw) > MaxTokenLength {
		return nil, invalid(ErrTokenTooLarge)
	}
	unverified := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(raw, unverified); err != nil {
		return nil, invalid(err)
	}
	if iss, _ := unverified["iss"].(string); iss != Issuer {
		if s.external == nil {
			return nil, invalid(fmt.Errorf("untrusted issuer %q", truncate(iss, 128)))
		}
		p, err := s.external.VerifyExternal(ctx, raw)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, errors.New("external verifier returned no principal")
		}
		if p.TokenID != "" {
			if err := s.checkRevoked(ctx, p); err != nil {
				return nil, err
			}
		}
		return p, nil
	}
	return s.verifyLocal(ctx, raw)
}

func (s *TokenService) verifyLocal(ctx context.Context, raw string) (*Principal, error) {
	ks := s.keys.Load()
	claims := jwt.MapClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(Issuer),
		jwt.WithLeeway(s.leeway),
		jwt.WithTimeFunc(s.now),
		jwt.WithIssuedAt(),
		jwt.WithStrictDecoding(),
	)
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		k, ok := ks.lookup(kid)
		if !ok {
			return nil, errors.New("unknown signing key id")
		}
		return k.secret, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, invalid(ErrTokenExpired)
		}
		return nil, invalid(err)
	}

	sub, _ := claims["sub"].(string)
	jti, _ := claims["jti"].(string)
	kind, _ := claims[claimKind].(string)
	iatClaim, err := claims.GetIssuedAt()
	if err != nil || iatClaim == nil {
		return nil, invalid(errors.New("missing iat claim"))
	}
	expClaim, err := claims.GetExpirationTime()
	if err != nil {
		return nil, invalid(err)
	}
	if jti == "" || len(jti) > maxTokenIDBytes {
		return nil, invalid(errors.New("missing or oversized jti claim"))
	}
	p := &Principal{
		Kind:     TokenKind(kind),
		Issuer:   Issuer,
		TokenID:  jti,
		IssuedAt: iatClaim.Time,
		Claims:   claims,
	}
	if expClaim != nil {
		p.ExpiresAt = expClaim.Time
	}
	p.Email, _ = claims["email"].(string)
	p.Name, _ = claims["name"].(string)
	p.Groups = stringList(claims["groups"])

	switch p.Kind {
	case KindLogin, KindAPIKey:
		name, ok := strings.CutSuffix(sub, ":"+string(p.Kind))
		if !ok || ValidateAccountName(name) != nil {
			return nil, invalid(ErrInvalidSubject)
		}
		p.Subject = name
		claims["sub"] = name
		if p.Kind == KindLogin && p.ExpiresAt.IsZero() {
			return nil, invalid(errors.New("session token without exp claim"))
		}
	case KindProject:
		proj, role, ok := ParseProjectSubject(sub)
		if !ok {
			return nil, invalid(ErrInvalidSubject)
		}
		p.Subject, p.Project, p.Role = sub, proj, role
	case KindSSO:
		if validateSubject(sub) != nil {
			return nil, invalid(ErrInvalidSubject)
		}
		if p.ExpiresAt.IsZero() {
			return nil, invalid(errors.New("session token without exp claim"))
		}
		p.Subject = sub
	default:
		return nil, invalid(fmt.Errorf("unknown token kind %q", truncate(kind, 32)))
	}

	if err := s.checkRevoked(ctx, p); err != nil {
		return nil, err
	}

	switch p.Kind {
	case KindLogin, KindAPIKey:
		if err := s.checkAccountToken(ctx, p); err != nil {
			return nil, err
		}
	case KindProject:
		if s.projects == nil {
			return nil, invalid(errors.New("project tokens are not accepted"))
		}
		known, err := s.projects.IsTokenKnown(ctx, sub, jti, p.IssuedAt.Unix())
		if err != nil {
			return nil, fmt.Errorf("check project token: %w", err)
		}
		if !known {
			return nil, invalid(ErrTokenRevoked)
		}
	}
	return p, nil
}

func (s *TokenService) checkAccountToken(ctx context.Context, p *Principal) error {
	acct, err := s.accounts.LookupAccount(ctx, p.Subject)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return invalid(err)
		}
		return fmt.Errorf("look up account: %w", err)
	}
	capability := CapabilityLogin
	if p.Kind == KindAPIKey {
		capability = CapabilityAPIKey
	}
	if err := checkAccount(acct, capability); err != nil {
		return invalid(err)
	}
	if p.Kind == KindAPIKey && acct.TokenIndex(p.TokenID) < 0 {
		return invalid(ErrTokenRevoked)
	}
	if !acct.PasswordMtime.IsZero() && p.IssuedAt.Before(acct.PasswordMtime) {
		return invalid(ErrSessionInvalidated)
	}
	return nil
}

func (s *TokenService) checkRevoked(ctx context.Context, p *Principal) error {
	revoked, err := s.revocations.IsRevoked(ctx, s.revocationRef(p))
	if err != nil {
		return fmt.Errorf("check token revocation: %w", err)
	}
	if revoked {
		return invalid(ErrTokenRevoked)
	}
	return nil
}

// RenewIfExpiring returns a replacement login session when p is a login session within
// the renewal window of its expiry (upstream behavior: active users keep their session).
// The replacement has a new ID and the same lifetime as the original. The original stays
// valid until it expires.
func (s *TokenService) RenewIfExpiring(_ context.Context, p *Principal) (Issued, bool, error) {
	if p == nil || p.Kind != KindLogin || p.ExpiresAt.IsZero() {
		return Issued{}, false, nil
	}
	if p.ExpiresAt.Sub(s.now()) >= s.renewWithin {
		return Issued{}, false, nil
	}
	ttl := p.ExpiresAt.Sub(p.IssuedAt)
	if ttl <= 0 {
		ttl = DefaultSessionDuration
	}
	id, err := newTokenID()
	if err != nil {
		return Issued{}, false, err
	}
	iss, err := s.sign(KindLogin, p.Subject+":"+string(CapabilityLogin), id, ttl, nil)
	if err != nil {
		return Issued{}, false, err
	}
	return iss, true, nil
}

// Revoke records p's token in the revocation store until the token can no longer verify
// (its expiry plus the verification leeway); this is logout. Tokens without expiry
// return ErrNotRevocable: delete their metadata instead.
//
// Revocations are scoped to the principal (kind, issuer, subject): a principal that
// revokes more tokens than the store's per-subject limit has them collapsed into a
// watermark that also ends its own other sessions issued at or before the newest
// collapsed one, but it cannot make other principals' revocations fail.
func (s *TokenService) Revoke(ctx context.Context, p *Principal) error {
	if p == nil || p.TokenID == "" {
		return errors.New("revoke: token has no id")
	}
	if p.ExpiresAt.IsZero() {
		return ErrNotRevocable
	}
	if err := s.revocations.Revoke(ctx, s.revocationRef(p)); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

// revocationRef identifies p's token for the revocation store. The subject scope
// separates token kinds and issuers, so collapsing an account's login sessions never
// touches its API tokens and an SSO subject never shares a scope with a local account.
func (s *TokenService) revocationRef(p *Principal) TokenRef {
	ref := TokenRef{
		ID:       p.TokenID,
		Subject:  string(p.Kind) + "\x00" + p.Issuer + "\x00" + p.Subject,
		IssuedAt: p.IssuedAt,
	}
	if !p.ExpiresAt.IsZero() {
		ref.ExpiresAt = p.ExpiresAt.Add(s.leeway)
	}
	return ref
}

// ProjectSubject returns "proj:<project>:<role>" after validating both parts.
func ProjectSubject(project, role string) (string, error) {
	for _, part := range []string{project, role} {
		if part == "" || strings.ContainsAny(part, ": \t\r\n") || len(part) > 253 {
			return "", fmt.Errorf("%w: invalid project or role name %q", ErrInvalidSubject, part)
		}
	}
	return ProjectSubjectPrefix + project + ":" + role, nil
}

// ParseProjectSubject splits "proj:<project>:<role>".
func ParseProjectSubject(sub string) (project, role string, ok bool) {
	parts := strings.Split(sub, ":")
	if len(parts) != 3 || parts[0]+":" != ProjectSubjectPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func checkAccount(acct Account, c Capability) error {
	if !acct.Enabled {
		return fmt.Errorf("%w: %q", ErrAccountDisabled, acct.Name)
	}
	if !acct.HasCapability(c) {
		return fmt.Errorf("%w: account %q does not have %s capability", ErrMissingCapability, acct.Name, c)
	}
	return nil
}

func validateSubject(sub string) error {
	if sub == "" || len(sub) > maxSubjectBytes {
		return fmt.Errorf("%w: length must be 1..%d", ErrInvalidSubject, maxSubjectBytes)
	}
	if strings.IndexFunc(sub, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: control character", ErrInvalidSubject)
	}
	return nil
}

func newTokenID() (string, error) {
	u, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("generate token id: %w", err)
	}
	return u.String(), nil
}

func tokenIDOrNew(id string) (string, error) {
	if id == "" {
		return newTokenID()
	}
	if len(id) > maxTokenIDBytes || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return "", errors.New("invalid token id")
	}
	return id, nil
}

func invalid(err error) error { return fmt.Errorf("%w: %w", ErrInvalidToken, err) }

func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []string:
		return slices.Clone(t)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
