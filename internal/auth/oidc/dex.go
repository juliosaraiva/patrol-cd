package oidc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// Bundled Dex conventions.
const (
	// DexAPIPath is where patrol-server reverse-proxies Dex; the issuer is
	// "<server URL>/api/dex".
	DexAPIPath = "/api/dex"
	// DexCallbackPath is the connector callback registered with upstream IdPs.
	DexCallbackPath = "/api/dex/callback"
	// DexClientID is the static Dex client used by patrol-server.
	DexClientID = "patrol"
	// DexCLIClientID is the public Dex client used by the CLI.
	DexCLIClientID = "patrol-cli"
	// DexFederatedScope requests Dex's federated_claims (connector user id).
	DexFederatedScope = "federated:id"

	dexClientSecretLabel = "patrol dex client secret v1"
	dexClientSecretLen   = 40
)

// DexIssuerURL returns "<baseURL>/api/dex" for the server's external URL.
func DexIssuerURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%w: server URL must be an absolute http(s) URL", ErrInvalidConfig)
	}
	return strings.TrimRight(baseURL, "/") + DexAPIPath, nil
}

// DexClientSecret derives the static Dex client secret from the server secret
// ("server.secretkey") so that patrol-server and the Dex launcher compute the same value
// independently: base64url(HMAC-SHA256(serverSecret, label))[:40]. Upstream derives it as
// base64url(SHA-256(serverSecret))[:40]; the HMAC form adds domain separation and has the
// same shape.
func DexClientSecret(serverSecret []byte) (string, error) {
	if len(serverSecret) < minServerSecretLen {
		return "", fmt.Errorf("%w: server secret shorter than %d bytes", ErrInvalidConfig, minServerSecretLen)
	}
	m := hmac.New(sha256.New, serverSecret)
	m.Write([]byte(dexClientSecretLabel))
	return base64.URLEncoding.EncodeToString(m.Sum(nil))[:dexClientSecretLen], nil
}

// DexConfig returns the relying-party configuration for the bundled Dex reachable at
// baseURL + DexAPIPath: client "patrol" with the derived secret, CLI client
// "patrol-cli", default scopes plus "federated:id", and both clients as audiences.
func DexConfig(baseURL string, serverSecret []byte) (Config, error) {
	issuer, err := DexIssuerURL(baseURL)
	if err != nil {
		return Config{}, err
	}
	secret, err := DexClientSecret(serverSecret)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Issuer:          issuer,
		ClientID:        DexClientID,
		ClientSecret:    secret,
		CLIClientID:     DexCLIClientID,
		RequestedScopes: append(append([]string{}, DefaultScopes...), DexFederatedScope),
		Audiences:       []string{DexClientID, DexCLIClientID},
	}, nil
}
