package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/golang-jwt/jwt/v5"

	"github.com/juliosaraiva/patrol-cd/internal/git"
)

const (
	// AzureDevOpsScope is the well-known Azure DevOps resource scope (upstream
	// azureDevopsEntraResourceId).
	AzureDevOpsScope = "499b84ac-1321-427f-aa17-267ca6975798/.default"
	// AzureEmptyGUID is the user name paired with Microsoft Entra tokens (upstream
	// workloadidentity.EmptyGuid).
	AzureEmptyGUID = "00000000-0000-0000-0000-000000000000"
	// DefaultAzureAuthorityHost is the default Microsoft Entra authority.
	DefaultAzureAuthorityHost = "https://login.microsoftonline.com"
	// DefaultAzureARMTokenResource is the ARM resource whose token is exchanged for an Azure
	// Container Registry refresh token (upstream AZURE_ARM_TOKEN_RESOURCE default).
	DefaultAzureARMTokenResource = "https://management.core.windows.net"
	// MaxAzureServicePrincipalCacheTTL caps how long a service principal token is reused
	// (upstream ARGOCD_AZURE_SERVICE_PRINCIPAL_CREDS_EXPIRATION_DURATION, capped at 59 minutes).
	MaxAzureServicePrincipalCacheTTL = 59 * time.Minute
)

const (
	azureRequestTimeout = 15 * time.Second
	acrRequestTimeout   = 10 * time.Second
	// maxRegistryResponse bounds registry challenge and exchange response bodies.
	maxRegistryResponse = 1 << 20
)

// AzureTokenProvider issues Microsoft Entra access tokens for the workload identity of the
// process. The default implementation uses azidentity's WorkloadIdentityCredential, which
// reads AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_FEDERATED_TOKEN_FILE and
// AZURE_AUTHORITY_HOST.
type AzureTokenProvider interface {
	GetToken(ctx context.Context, scope string) (token string, expiresOn time.Time, err error)
}

// workloadIdentityProvider is the default AzureTokenProvider. The credential is created on
// first use (and retried after a failure) so processes without Azure configuration pay
// nothing.
type workloadIdentityProvider struct {
	transport policy.Transporter
	mu        sync.Mutex
	cred      *azidentity.WorkloadIdentityCredential
}

func (p *workloadIdentityProvider) GetToken(ctx context.Context, scope string) (string, time.Time, error) {
	p.mu.Lock()
	cred := p.cred
	if cred == nil {
		c, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientOptions: azcore.ClientOptions{Transport: p.transport},
		})
		if err != nil {
			p.mu.Unlock()
			return "", time.Time{}, fmt.Errorf("failed to create Azure workload identity credential: %w", err)
		}
		p.cred, cred = c, c
	}
	p.mu.Unlock()
	tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
	if err != nil {
		return "", time.Time{}, err
	}
	return tok.Token, tok.ExpiresOn, nil
}

// azureWorkloadIdentityToken returns a cached Microsoft Entra token of the process workload
// identity for scope (upstream AzureWorkloadIdentityCreds, cached until five minutes before
// expiry).
func (r *Resolver) azureWorkloadIdentityToken(ctx context.Context, scope string) (mintedToken, error) {
	return r.tokens.get(ctx, tokenKindAzureWorkloadIdentity, cacheKey(tokenKindAzureWorkloadIdentity, scope), 0,
		func(ctx context.Context) (mintedToken, error) {
			tok, exp, err := r.azureWI.GetToken(ctx, scope)
			if err != nil {
				return mintedToken{}, tokenFailure("failed to get Azure access token", err)
			}
			return mintedToken{value: tok, username: AzureEmptyGUID, expiresAt: exp}, nil
		})
}

// checkAzureDevOpsURL allows the process workload identity's Azure DevOps token to be sent
// only to https repositories on an allowed Azure DevOps host (Options.AzureDevOpsHosts):
// the token belongs to the controller or API server, not to the repository object.
func (r *Resolver) checkAzureDevOpsURL(repoURL string) error {
	host := repoHost(repoURL)
	if urlScheme(repoURL) != "https" || !r.devOpsHosts.match(host) {
		return configError(ErrInvalidConfiguration, "Azure workload identity is only used for https repositories on an allowed Azure DevOps host, not %q", host)
	}
	return nil
}

// azureServicePrincipalToken returns an Azure DevOps token of a service principal (upstream
// AzureServicePrincipalCreds): a custom Active Directory endpoint disables instance
// discovery and must be an https URL on an allowed authority host; the repository proxy
// (when safe) and TLS client certificate apply to the token request; tokens are reused for
// at most AzureServicePrincipalCacheTTL.
func (r *Resolver) azureServicePrincipalToken(ctx context.Context, a *access) (mintedToken, error) {
	endpoint, disableDiscovery := DefaultAzureAuthorityHost, false
	if a.azureSPEndpoint != "" {
		endpoint, disableDiscovery = a.azureSPEndpoint, true
		if err := checkEndpointURL("azureServicePrincipal.activeDirectoryEndpoint", endpoint, r.authorityHosts); err != nil {
			return mintedToken{}, err
		}
	}
	key := cacheKey(tokenKindAzureServicePrincipal, a.azureSPTenantID, a.azureSPClientID, a.azureSPClientSecret, endpoint)
	return r.tokens.get(ctx, tokenKindAzureServicePrincipal, key, r.azureSPCacheTTL, func(ctx context.Context) (mintedToken, error) {
		client, err := r.httpClient(transportOptions{
			clientCertData: a.tlsClientCertData,
			clientCertKey:  a.tlsClientCertKey,
			proxy:          r.safeProxy(a.proxy, "azureServicePrincipal"),
			noProxy:        a.noProxy,
			what:           "Microsoft Entra ID",
			allow:          r.authorityHosts,
		}, azureRequestTimeout)
		if err != nil {
			return mintedToken{}, err
		}
		cred, err := azidentity.NewClientSecretCredential(a.azureSPTenantID, a.azureSPClientID, a.azureSPClientSecret,
			&azidentity.ClientSecretCredentialOptions{
				ClientOptions: azcore.ClientOptions{
					Cloud:     cloud.Configuration{ActiveDirectoryAuthorityHost: endpoint},
					Transport: client,
				},
				DisableInstanceDiscovery: disableDiscovery,
			})
		if err != nil {
			return mintedToken{}, fmt.Errorf("failed to create Azure client secret credential: %w: %w", ErrInvalidConfiguration, err)
		}
		tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{AzureDevOpsScope}})
		if err != nil {
			return mintedToken{}, tokenFailure("failed to get Azure access token", err)
		}
		return mintedToken{value: tok.Token, username: AzureEmptyGUID, expiresAt: tok.ExpiresOn}, nil
	})
}

// acrRefreshToken returns an Azure Container Registry refresh token for the registry of a
// Helm OCI repository, obtained with the process workload identity (upstream helm
// AzureWorkloadIdentityCreds): challenge https://<registry>/v2/, exchange an ARM token at
// <realm host>/oauth2/exchange, cache until five minutes before the token's JWT expiry.
// The ARM token is the controller's or API server's own identity, so unlike upstream the
// registry must be on an allowed Azure Container Registry host
// (Options.AzureContainerRegistryHosts), TLS is always verified (the repository's insecure
// flag does not apply) and the realm must be on the registry host.
func (r *Resolver) acrRefreshToken(ctx context.Context, repoURL string) (mintedToken, error) {
	host := registryHost(repoURL)
	if host == "" {
		return mintedToken{}, configError(ErrInvalidConfiguration, "cannot determine the registry host of repository %q", git.SanitizeURL(repoURL))
	}
	if !r.acrHosts.match(hostOnly(host)) {
		return mintedToken{}, configError(ErrInvalidConfiguration, "Azure workload identity is only exchanged with allowed Azure Container Registry hosts, not %q", host)
	}
	return r.tokens.get(ctx, tokenKindACR, cacheKey(tokenKindACR, host), 0, func(ctx context.Context) (mintedToken, error) {
		client, err := r.httpClient(transportOptions{what: "Azure Container Registry", allow: r.acrHosts}, acrRequestTimeout)
		if err != nil {
			return mintedToken{}, err
		}
		params, err := challengeRegistry(ctx, client, host)
		if err != nil {
			return mintedToken{}, tokenFailure("failed to challenge Azure Container Registry", err)
		}
		realm, err := url.Parse(params["realm"])
		if err != nil || realm.Scheme != "https" || !strings.EqualFold(realm.Host, host) {
			return mintedToken{}, fmt.Errorf("failed to challenge Azure Container Registry: realm %q is not an https URL on the registry host %q: %w",
				params["realm"], host, ErrTokenUnavailable)
		}
		arm, err := r.azureWorkloadIdentityToken(ctx, r.armTokenResource+"/.default")
		if err != nil {
			return mintedToken{}, err
		}
		exchange := url.URL{Scheme: realm.Scheme, Host: realm.Host, Path: "/oauth2/exchange"}
		refresh, err := exchangeACRToken(ctx, client, exchange.String(), params["service"], arm.value)
		if err != nil {
			return mintedToken{}, tokenFailure("failed to get Azure access token after challenge", err)
		}
		exp, err := jwtExpiry(refresh)
		if err != nil {
			r.logger.Warn("failed to read the expiry of an Azure Container Registry token; it will not be cached", "registry", host, "error", err)
		}
		return mintedToken{value: refresh, username: AzureEmptyGUID, expiresAt: exp}, nil
	})
}

// challengeRegistry requests https://<host>/v2/ and returns the parameters of its Bearer
// challenge (realm and service are required), with upstream's error messages.
func challengeRegistry(ctx context.Context, client *http.Client, host string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v2/", http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to registry %q: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRegistryResponse))
	authenticate := resp.Header.Get("Www-Authenticate")
	if resp.StatusCode != http.StatusUnauthorized || authenticate == "" {
		return nil, fmt.Errorf("registry '%s' did not issue a challenge", host)
	}
	scheme, rest, _ := strings.Cut(strings.TrimSpace(authenticate), " ")
	if !strings.EqualFold(scheme, "bearer") {
		return nil, fmt.Errorf("registry does not allow 'Bearer' authentication, got '%s'", scheme)
	}
	params := parseChallengeParams(rest)
	if _, ok := params["realm"]; !ok {
		return nil, errors.New("realm parameter not found in challenge")
	}
	if _, ok := params["service"]; !ok {
		return nil, errors.New("service parameter not found in challenge")
	}
	return params, nil
}

// parseChallengeParams parses `key="value",key2=value2` (quoted values may contain commas).
func parseChallengeParams(s string) map[string]string {
	params := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; {
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				val, rest = rest[1:], ""
			} else {
				val, rest = rest[1:1+end], rest[2+end:]
			}
		} else {
			val, rest, _ = strings.Cut(rest, ",")
			rest = "," + rest
		}
		params[key] = strings.TrimSpace(val)
		rest = strings.TrimSpace(rest)
		rest = strings.TrimPrefix(rest, ",")
		s = strings.TrimSpace(rest)
	}
	return params
}

// exchangeACRToken posts the ARM token to the registry's exchange endpoint and returns the
// refresh token.
func exchangeACRToken(ctx context.Context, client *http.Client, exchangeURL, service, armToken string) (string, error) {
	form := url.Values{}
	form.Add("grant_type", "access_token")
	form.Add("service", service)
	form.Add("access_token", armToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, exchangeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to create request to get refresh token: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("unable to connect to registry: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryResponse))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get refresh token: %s", resp.Status)
	}
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}
	var res struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", errors.New("failed to unmarshal response body")
	}
	if res.RefreshToken == "" {
		return "", errors.New("the registry returned no refresh token")
	}
	return res.RefreshToken, nil
}

// jwtExpiry reads the exp claim of a JWT without verifying it (the token is opaque to us;
// only its lifetime matters for caching).
func jwtExpiry(token string) (time.Time, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return time.Time{}, errors.New("failed to parse JWT")
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		return time.Time{}, errors.New("'exp' claim not found or invalid in token")
	}
	return exp.Time, nil
}
