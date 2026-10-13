# Component: auth (`internal/auth`)

Status: implemented (identity primitives; HTTP handlers live in `internal/apiserver`).
Decision record: [ADR-0009](../adr/0009-authn-authz.md). Parity inventory:
[`docs/parity/areas/07-projects-rbac-auth.md`](../../parity/areas/07-projects-rbac-auth.md) §B, §G, §H.

| Package | Contents | Dependencies |
|---------|----------|--------------|
| `internal/auth` | accounts, passwords, signing keys, JWT issue/verify, revocation interfaces, login throttling, redirect validation, cookie/bearer helpers | `golang-jwt/jwt/v5`, `x/crypto/bcrypt`, `google/uuid` |
| `internal/auth/oidc` | OIDC relying party: lazy discovery, auth-code + PKCE flow, encrypted state cookie, ID token/audience verification, groups/userinfo, Dex helpers | `coreos/go-oidc/v3`, `x/oauth2`, `x/sync/singleflight` |
| `internal/auth/revocation` | `SecretStore`: `auth.RevocationStore` on Secret `patrol-session-revocations` | `client-go` (typed client) |

None of the packages import `api/v1alpha1`; CRD-backed state is reached through the small
interfaces below.

## Responsibilities

1. Parse local accounts from settings (`patrol-cm`, `patrol-secret`) and produce Secret
   mutations for password changes and API-token creation/deletion.
2. Hash and verify passwords (bcrypt, cost 10) and enforce `passwordPattern`.
3. Issue and verify Patrol JWTs (HS256, `iss=patrol`, `kid` header) for login sessions, SSO
   sessions, account API tokens and project-role tokens, with key rotation and revocation.
4. Throttle local logins (per-user failure window/lockout, bounded cache, global
   concurrency limit, timing equalization).
5. Run the OIDC authorization-code flow (state + nonce + PKCE) and verify IdP ID tokens
   (including bearer ID tokens presented by the CLI).
6. Share logout state across replicas through a Kubernetes Secret (no Redis, ADR-0004).
7. Extract tokens from requests and write/clear session cookies; validate post-login
   redirect targets.

## Public API

### `internal/auth`

| Area | API |
|------|-----|
| Accounts | `Account{Name, Enabled, Capabilities, PasswordHash, PasswordMtime, Tokens []TokenMeta}`, `TokenMeta{ID, IssuedAt, ExpiresAt}`, `Capability` (`CapabilityLogin`, `CapabilityAPIKey`), `Accounts` (map; implements `AccountLookup`), `AccountsFromMaps(cm, secret map[string]string) (Accounts, error)`, `ValidateAccountName`, `SecretData(map[string][]byte)` |
| Secret mutations (pure; return a new map) | `ChangePassword(secret, acct, newPassword, policy, now)`, `AddToken(secret, acct, meta)`, `DeleteToken(secret, acct, id)`, `SecretWithPassword`, `SecretWithTokens` |
| Passwords | `HashPassword`, `HashPasswordWithCost`, `VerifyPassword`, `PasswordNeedsRehash`, `NewPasswordPolicy`, `PasswordPolicyFromSettings`, `(*PasswordPolicy).Validate`, `GenerateInitialPassword` |
| Keys/settings | `NewSigningKey`, `NewKeySet(current, previous...)`, `KeySetFromSecret(secret)`, `ParseSessionDuration` |
| Tokens | `NewTokenService(TokenServiceConfig{Keys, Accounts, Revocations, Projects, External, Leeway, RenewWithin, Now})`; `IssueLoginSession`, `IssueSSOSession(SSOIdentity)`, `IssueAccountToken`, `IssueProjectToken`, `Verify(ctx, raw) (*Principal, error)`, `RenewIfExpiring`, `Revoke`, `SetKeys`; `ProjectSubject`, `ParseProjectSubject` |
| Extension interfaces | `AccountLookup`, `TokenRegistry{IsTokenKnown(ctx, sub, jti, iat)}`, `RevocationStore{Revoke(ctx, jti, exp); IsRevoked(ctx, jti)}`, `ExternalVerifier{VerifyExternal(ctx, raw)}`; adapters `AccountLookupFunc`, `TokenRegistryFunc`; `MemoryRevocationStore` (tests/dev) |
| Login | `DefaultLoginConfig`, `LoginConfigFromEnv(os.LookupEnv)`, `NewAuthenticator(accounts, cfg, opts...)`, `(*Authenticator).Authenticate(ctx, user, pass) (Account, error)`, `LoginError{Reason}` |
| HTTP | `TokenFromRequest(r)`, `SetSessionCookie(w, r, token, CookieOptions{Path, Secure, Expires})`, `ClearSessionCookie`, `CookiePath`, `ValidateRedirectURL(url, allowedBases)`, `IsValidRedirectURL` |

### `internal/auth/oidc`

`Config` (issuer, clientID/secret, cliClientID, requestedScopes, requestedIDTokenClaims,
rootCA, enablePKCE, logoutURL, userInfoPath/BaseURL, enableUserInfoGroups,
userInfoCacheExpiration, insecureSkipVerify, domainHint, audiences,
skipAudienceCheckWhenTokenHasNoAudience, groupsClaim); `NewProvider(cfg, opts...)`;
`(*Provider).BuildAuthURL(ctx, state, nonce, pkceVerifier, redirectURL)`,
`StartLogin(ctx, codec, LoginRequest)`, `HandleCallback(ctx, Callback, stateCookie, codec)
(*Result, error)`, `VerifyIDToken`, `VerifyExternal` (implements `auth.ExternalVerifier`),
`FetchUserInfo`; `NewStateCodec(now, current, previous...)`, `NewLoginState`,
`StateCookie`, `ClearStateCookie`, `StateCookieValue`, `CallbackFromRequest`;
`Claims{Issuer, Sub, Email, Name, Groups, Expiry, Raw}` with `UserID()` and
`SSOIdentity(extraClaims...)`; `ExtractGroups`; `DexIssuerURL`, `DexClientSecret`,
`DexConfig`; `(Config).BuildLogoutURL`.

### `internal/auth/revocation`

`NewSecretStore(kubernetes.Interface, namespace, opts...)` with `WithSecretName`,
`WithCacheTTL` (default 5 s), `WithMaxStale` (30 s), `WithMaxEntries` (15000),
`WithBackoff`, `WithClock`, `WithLogger`; methods `Revoke`, `IsRevoked`, `Prune`;
`EncodeKey`.

## Settings keys

| Key | Store | Meaning |
|-----|-------|---------|
| `accounts.<name>` | patrol-cm | `login`, `apiKey` (comma-separated) |
| `accounts.<name>.enabled`, `admin.enabled` | patrol-cm | default `true` |
| `accounts.<name>.password` / `.passwordMtime` / `.tokens` | patrol-secret | bcrypt hash, RFC 3339, JSON `[{"id","iat","exp"}]` |
| `admin.password` / `admin.passwordMtime` / `admin.tokens` | patrol-secret | same for `admin` |
| `server.secretkey` | patrol-secret | current HS256 key (>= 32 bytes); also derives the OIDC state key and the Dex client secret |
| `server.secretkey.previous` | patrol-secret | optional previous key, verify-only |
| `passwordPattern` | patrol-cm | default `^.{8,32}$` |
| `users.session.duration` | patrol-cm | default `24h`; accepts `<n>[smhd]` and Go durations |
| `PATROL_SESSION_FAILURE_MAX_FAIL_COUNT` / `_WINDOW_SECONDS` / `PATROL_SESSION_MAX_CACHE_SIZE` / `PATROL_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT` | env | 5 / 300 / 1000 / 50 |

## Token format

All Patrol tokens: header `{"alg":"HS256","kid":<16-char id derived by HMAC from the key>}`;
claims `iss=patrol`, `sub`, `jti`, `iat`, `nbf`, `kind`, optional `exp`, and for SSO
sessions `groups`, `email`, `name` plus caller-selected extra claims.

| `kind` | `sub` | `exp` | Extra verification |
|--------|-------|-------|--------------------|
| `login` | `<account>:login` | required | account exists, enabled, has `login`; `iat >= passwordMtime` |
| `apiKey` | `<account>:apiKey` | optional | as above with `apiKey`; `jti` listed in the account's tokens |
| `project` | `proj:<project>:<role>` | optional | `TokenRegistry.IsTokenKnown(sub, jti, iat)` |
| `sso` | IdP user id (`federated_claims.user_id` or `sub`) | required | none beyond revocation |

Every kind is checked against the revocation store. The `kind` claim must agree with the
subject format, so an SSO subject such as `admin:login` stays an SSO identity.

## Invariants

- Only HS256 with a known `kid` verifies Patrol tokens; `none`, HS384/512, RS*/ES* and
  unknown or missing `kid` are rejected before claims are trusted. IdP tokens are verified
  by go-oidc with the algorithms the IdP advertises (RS256 default), never HMAC.
- Tokens are always signed with `KeySet.Current`; `Previous` keys only verify.
- Every verification failure caused by the token wraps `auth.ErrInvalidToken` (plus a
  specific sentinel: `ErrTokenExpired`, `ErrTokenRevoked`, `ErrSessionInvalidated`,
  `ErrAccountDisabled`, `ErrMissingCapability`, `ErrAccountNotFound`). Infrastructure
  failures (revocation store, account lookup, project registry) do not, so callers return
  401 vs 5xx correctly and never accept a token when a check could not run.
- Secret mutation helpers never modify their input and read the current token list from
  the Secret snapshot they are given, so a resourceVersion-guarded update is consistent.
- Login failures that could reveal account existence are all `*LoginError` with the same
  message; unknown users and accounts without a password still perform a bcrypt
  comparison, and every attempt is padded to a random 500-1000 ms.
- The OIDC state cookie is AES-256-GCM sealed (key = HKDF-SHA256(server secret)), expires
  after 5 minutes, and carries state, nonce, PKCE verifier, callback URL and return URL;
  the state parameter and the ID token nonce are compared in constant time.
- Return URLs are validated before any IdP request.
- Bounded resources: tokens <= 80 KiB, <= 20 cookie chunks of <= 4093 bytes, <= 1000
  groups, <= 1 MiB per IdP response, failure cache <= `MaxCacheSize`, userinfo cache
  <= 10000 entries, revocation Secret <= `MaxEntries`, state cookie <= 4 KiB.

## Failure modes

| Situation | Behavior |
|-----------|----------|
| `server.secretkey` missing or < 32 bytes | `ErrNoSigningKey` / `ErrWeakSigningKey`; no tokens can be issued |
| Malformed account settings | `AccountsFromMaps` returns usable accounts plus a joined `*AccountConfigError`; affected account fail-closed (invalid `enabled` or `passwordMtime` disables it, invalid token JSON drops its API tokens) |
| Revocation Secret unreadable | cached copy served for TTL + `MaxStale` (35 s), then `revocation.ErrUnavailable` (fail closed) |
| Concurrent revocations | resourceVersion conflicts and create races retried (`retry.OnError`); expired entries pruned on each write |
| Revocation Secret full | `auth.ErrRevocationStoreFull` (logout fails visibly rather than silently un-revoking) |
| IdP down at startup | no I/O until first use; discovery retried with backoff, failures cached for 5 s, a working provider is kept if a re-discovery fails |
| IdP key rotation | go-oidc refetches JWKS on unknown keys; a signature failure additionally triggers one re-discovery per minute |
| Userinfo groups enabled but no cached entry for a bearer ID token | rejected (`ErrUserInfoUnavailable`), as upstream |
| Login cache flooded with fake usernames | random non-admin entry evicted; admin's count preserved |

## Security notes

- Secrets are never logged or formatted: `SigningKey` implements `String`, `GoString`
  and `slog.LogValuer` with the key ID only; the raw ID token in `oidc.Result` must not
  be logged by callers; usernames of unknown accounts are not logged (they are often
  mistyped passwords).
- Cookies: session cookie `patrol.token` is `HttpOnly`, `SameSite=Strict`, `Secure`
  (configurable for plain-HTTP dev), path = root path; the state cookie is `SameSite=Lax`
  because it must survive the cross-site redirect from the IdP.
- `ValidateRedirectURL` rejects user info, opaque URLs, backslashes, whitespace,
  control characters (including encoded CR/LF in the path), scheme-relative and relative
  URLs, and paths that escape the base path after cleaning.
- `InsecureSkipVerify` exists only as an explicit operator opt-in; `RootCA`, when set,
  is the only trust anchor for the IdP.
- CSRF defense (`X-Requested-With: patrol`) and RBAC are enforced by the API server, not
  here.

## Caller integration

- Account lookups: a settings watcher calls `AccountsFromMaps` on every change and serves
  `AccountLookup` from the latest result (log the returned error).
- Project tokens: `TokenRegistry` is backed by the AppProject lister:
  `status.jwtTokensByRole[role]` matched by `jti`, falling back to `iat`, exactly as
  upstream (TEN-035). `IssueProjectToken` returns `ID`, `IssuedAt`, `ExpiresAt` to record.
- Account tokens: `IssueAccountToken` then `AddToken` and update patrol-secret with its
  resourceVersion; on conflict re-read and retry (minting a fresh token).
- Login handler: `Authenticator.Authenticate` -> `IssueLoginSession(acct,
  ParseSessionDuration(cm["users.session.duration"]))` -> `SetSessionCookie`. Divide
  `MaxConcurrent` by the replica count to reproduce upstream's per-replica split.
- Every authenticated request: `TokenFromRequest` -> `Verify` -> `RenewIfExpiring` (set
  the cookie when renewed).
- Logout: `Verify`, `Revoke` (ignore `ErrNotRevocable`), `ClearSessionCookie`, redirect
  to `Config.BuildLogoutURL` for SSO sessions when configured.
- SSO: `/auth/login` -> `StartLogin` (allowed bases = `url` + `additionalUrls`) and
  `StateCookie`; `/auth/callback` -> `HandleCallback` with `StateCookieValue(r)`, always
  `ClearStateCookie`, then `IssueSSOSession(res.Claims.SSOIdentity(<rbac scopes>...))`.
- Revocations: `revocation.NewSecretStore(clientset, namespace)`; run `Prune`
  periodically (e.g. hourly).

## Parity feature areas served

TEN-032/033/035 (project token issue and verification; storage stays in the project
API), TEN-090, TEN-091, TEN-092 (password generator only), TEN-093, TEN-094, TEN-095,
TEN-096 (hashing, policy and mtime; the RPC is the API server's), TEN-097, TEN-098,
TEN-099, TEN-100, TEN-101, TEN-102, TEN-105, TEN-108 (revocation and logout URL
helpers), TEN-109, TEN-116, TEN-117, TEN-118, TEN-119, TEN-120, TEN-121 (audience),
TEN-122, TEN-123, TEN-124, TEN-127, TEN-128, TEN-129, TEN-114 (issuer/client/secret
derivation only).

## Intentional divergences from upstream

| Upstream | Patrol | Reason |
|----------|--------|--------|
| After SSO the IdP ID token is the session cookie | Patrol mints its own `kind=sso` session (ADR-0009) | one verification path, revocable sessions, bounded cookie |
| Key rotation invalidates every token | optional `server.secretkey.previous`, `kid` header | zero-downtime rotation |
| Invalid `admin.enabled` ignored (admin stays enabled); invalid `accounts.x.enabled` fails parsing for all accounts | the affected account is disabled, others unaffected | fail closed without locking everyone out |
| `ARGOCD_SESSION_MAX_CACHE_SIZE` code default 10000 | 1000 (upstream documented value) | task specification |
| Cookie chunks `argocd.token-N`, `SameSite=Lax` | `patrol.token.N`, `SameSite=Strict` | ADR-0009 |
| Redirect path check is a raw prefix (`/argo` allows `/argocd-x`) | segment-boundary check, host compared case-insensitively | open-redirect hardening |
| Logout URL placeholders substituted literally | values are query-escaped | correct for the documented query usage |
| Dex secret `base64url(sha256(key))[:40]`, clients `argo-cd`/`argo-cd-cli` | `base64url(HMAC-SHA256(key, label))[:40]`, clients `patrol`/`patrol-cli` | domain separation; both sides use Patrol's helper |
| `rootCA` that fails to parse: warning, system roots used | configuration error | fail fast |
| No nonce in the auth-code flow; state 24 chars | nonce always sent and checked; state/nonce 256-bit | replay protection |
| Revocation checks `uti` but logout revokes `at_hash` for Entra tokens | one `jti` -> `uti` -> `at_hash` order for both | revocation actually effective |
| Userinfo only with `userInfoPath` | falls back to the discovered `userinfo_endpoint` | convenience |

## Known gaps

- Implicit grant (`response_type=id_token`) is not supported; IdPs must support `code`.
- Server-side OIDC refresh tokens (`refreshTokenThreshold`, TEN-125) are not
  implemented: SSO sessions are Patrol tokens with `users.session.duration` lifetime and
  are not renewed.
- Azure options (workload identity client assertions, groups overage via Graph,
  TEN-126) are not implemented.
- Userinfo groups for bearer ID tokens rely on the per-replica cache filled at web login;
  the CLI should exchange its ID token for a Patrol session instead (API design for
  `internal/apiserver`).
- Login failure state is per replica (as upstream); it is not shared through Kubernetes.
- Dex configuration generation (static clients, connector redirect URIs, `$secret`
  resolution), anonymous access, `GetUserInfo`, settings exposure and the CLI SSO flow
  belong to other components.

## Verification

Run on 2026-10-12, darwin/arm64, go1.27.2. Tests are hermetic: an in-process fake OIDC
provider on `httptest` (discovery, JWKS with per-test RSA keys, PKCE-enforcing token
endpoint, userinfo; plain and TLS) and `k8s.io/client-go/kubernetes/fake` with a reactor
that enforces resourceVersion conflicts.

| Command | Result |
|---------|--------|
| `go build ./...` | exit 0 |
| `go vet ./internal/auth/...` | exit 0 |
| `golangci-lint run ./internal/auth/...` | 0 issues |
| `go test -race -count=1 ./internal/auth/...` | pass: `internal/auth` 52 tests + 133 subtests, `internal/auth/oidc` 27 + 57, `internal/auth/revocation` 12; 0 failures |
| `go test -count=1 -cover ./internal/auth/...` | coverage 93.0% / 90.3% / 95.3% |

Not recorded in `docs/verification/log.md` (outside this component's file ownership);
the integrating agent should copy this table there.
