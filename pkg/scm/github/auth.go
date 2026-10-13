package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

const (
	// tokenRefreshBefore is how long before expiry an installation token is replaced.
	tokenRefreshBefore = 5 * time.Minute
	// installationIDTTL mirrors upstream's 60-minute discovery cache.
	installationIDTTL = 60 * time.Minute
	// defaultTokenLifetime is assumed when GitHub omits expires_at.
	defaultTokenLifetime = time.Hour
)

// Process-wide caches shared by provider instances, keyed by a hash of the API URL, the
// App ID and the private key, so providers rebuilt per reconcile reuse tokens (upstream
// caches installation transports the same way) and different keys never share entries.
var (
	installationTokens = newLRUCache[scm.Token](512)
	installationIDs    = newLRUCache[cachedInstallation](1024)
)

type cachedInstallation struct {
	inst    scm.Installation
	expires time.Time
}

// appAuth implements GitHub App authentication.
type appAuth struct {
	appID          int64
	installationID int64
	owner          string
	keyHash        string
	client         *gh.Client // authenticated with the App JWT
	now            func() time.Time
}

func newAppAuth(appID, installationID int64, owner, baseURL string, privateKey []byte, client *gh.Client, now func() time.Time) *appAuth {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00", baseURL, appID)
	h.Write(privateKey)
	return &appAuth{
		appID:          appID,
		installationID: installationID,
		owner:          owner,
		keyHash:        hex.EncodeToString(h.Sum(nil)),
		client:         client,
		now:            now,
	}
}

func (a *appAuth) tokenKey(installationID int64) string {
	return a.keyHash + ":" + strconv.FormatInt(installationID, 10)
}

// resolveInstallationID returns the configured ID or discovers it for the owner.
func (a *appAuth) resolveInstallationID(ctx context.Context) (int64, error) {
	if a.installationID > 0 {
		return a.installationID, nil
	}
	if strings.TrimSpace(a.owner) == "" {
		return 0, &scm.Error{Kind: scm.KindInvalid, Provider: Type, Op: "Token",
			Err: errors.New("installation ID is required for github: set the installation ID or the owner used for installation discovery")}
	}
	inst, err := a.find(ctx, a.owner)
	if err != nil {
		return 0, err
	}
	return inst.ID, nil
}

// cachedInstallationID returns a known installation ID without network I/O.
func (a *appAuth) cachedInstallationID() (int64, bool) {
	if a.installationID > 0 {
		return a.installationID, true
	}
	now := a.now()
	c, ok := installationIDs.get(a.keyHash+":"+strings.ToLower(a.owner), func(c cachedInstallation) bool { return now.Before(c.expires) })
	return c.inst.ID, ok
}

// find discovers the installation for owner (organization first, then user), caching
// the result for installationIDTTL. Owner matching is case-insensitive.
func (a *appAuth) find(ctx context.Context, owner string) (scm.Installation, error) {
	if err := scm.ValidateSegment("owner", owner); err != nil {
		return scm.Installation{}, err
	}
	lower := strings.ToLower(owner)
	key := a.keyHash + ":" + lower
	now := a.now()
	c, err := installationIDs.load(ctx, key, func(c cachedInstallation) bool { return a.now().Before(c.expires) },
		func(ctx context.Context) (cachedInstallation, error) {
			inst, resp, err := a.client.Apps.GetOrganizationInstallation(ctx, lower)
			if err != nil && isStatus(resp, err, http.StatusNotFound) {
				inst, resp, err = a.client.Apps.GetUserInstallation(ctx, lower)
			}
			if err != nil {
				if isStatus(resp, err, http.StatusNotFound) {
					return cachedInstallation{}, &scm.Error{Kind: scm.KindNotFound, Provider: Type, Op: "FindInstallation", StatusCode: http.StatusNotFound,
						Err: fmt.Errorf("installation not found for org: %s: ensure the GitHub App (ID: %d) is installed for this organization or user", lower, a.appID)}
				}
				return cachedInstallation{}, mapError("FindInstallation", resp, err, now)
			}
			return cachedInstallation{inst: toInstallation(inst), expires: a.now().Add(installationIDTTL)}, nil
		})
	if err != nil {
		return scm.Installation{}, err
	}
	return c.inst, nil
}

// token returns a cached installation token or mints a new one when the cached token
// expires within tokenRefreshBefore.
func (a *appAuth) token(ctx context.Context) (scm.Token, error) {
	id, err := a.resolveInstallationID(ctx)
	if err != nil {
		return scm.Token{}, err
	}
	valid := func(t scm.Token) bool { return a.now().Add(tokenRefreshBefore).Before(t.ExpiresAt) }
	return installationTokens.load(ctx, a.tokenKey(id), valid, func(ctx context.Context) (scm.Token, error) {
		tok, resp, err := a.client.Apps.CreateInstallationToken(ctx, id, nil)
		if err != nil {
			return scm.Token{}, mapError("CreateInstallationToken", resp, err, a.now())
		}
		if tok.GetToken() == "" {
			return scm.Token{}, &scm.Error{Kind: scm.KindUnavailable, Provider: Type, Op: "CreateInstallationToken", Err: errors.New("GitHub returned an empty installation token")}
		}
		exp := tok.GetExpiresAt().Time
		if exp.IsZero() {
			exp = a.now().Add(defaultTokenLifetime)
		}
		return scm.Token{Value: tok.GetToken(), ExpiresAt: exp, Username: GitUsername}, nil
	})
}

func (a *appAuth) tokenValue(ctx context.Context) (string, error) {
	t, err := a.token(ctx)
	return t.Value, err
}

// invalidate drops a cached token rejected by the API (revoked or expired early).
func (a *appAuth) invalidate(rejected string) {
	id, ok := a.cachedInstallationID()
	if !ok {
		return
	}
	installationTokens.removeIf(a.tokenKey(id), func(t scm.Token) bool { return t.Value == rejected })
}

// Token implements scm.CredentialSource. For GitHub Apps it returns the cached
// installation token (refreshed 5 minutes before expiry); for personal access tokens it
// returns the configured token without expiry. The user name is "x-access-token".
func (p *Provider) Token(ctx context.Context) (scm.Token, error) {
	switch p.mode {
	case authApp:
		return p.app.token(ctx)
	case authToken:
		v, err := p.token(ctx)
		if err != nil {
			return scm.Token{}, &scm.Error{Kind: scm.KindUnauthorized, Provider: Type, Op: "Token", Err: err}
		}
		if v == "" {
			return scm.Token{}, &scm.Error{Kind: scm.KindUnauthorized, Provider: Type, Op: "Token", Err: errors.New("empty token")}
		}
		return scm.Token{Value: v, Username: GitUsername}, nil
	}
	return scm.Token{}, &scm.UnsupportedError{Provider: Type, Name: p.info.Name, Capability: scm.CapabilityCredentials}
}

// BotIdentity returns the commit author identity of the GitHub App ("<slug>[bot]" and
// "<id>+<slug>[bot]@users.noreply.github.com"), as used by upstream's source hydrator.
func (p *Provider) BotIdentity(ctx context.Context) (name, email string, err error) {
	if p.app == nil {
		return "", "", &scm.UnsupportedError{Provider: Type, Name: p.info.Name, Capability: scm.CapabilityInstallations}
	}
	app, resp, err := p.app.client.Apps.Get(ctx, "")
	if err != nil {
		return "", "", p.mapErr("BotIdentity", resp, err)
	}
	login := app.GetSlug() + "[bot]"
	user, resp, err := p.client.Users.Get(ctx, login)
	if err != nil {
		return "", "", p.mapErr("BotIdentity", resp, err)
	}
	return user.GetLogin(), fmt.Sprintf("%d+%s@users.noreply.github.com", user.GetID(), user.GetLogin()), nil
}
