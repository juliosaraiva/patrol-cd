package settings

import (
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

// minServerSecretKeyLen is the minimum signing key length accepted by internal/auth.
const minServerSecretKeyLen = 32

// Errors returned by ResolveSecretRef.
var (
	// ErrSecretNotFound means a "$<secret>:<key>" reference names a Secret that does not
	// exist or is not labelled app.kubernetes.io/part-of=patrol-cd.
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretKeyNotFound means the referenced key does not exist in the Secret.
	ErrSecretKeyNotFound = errors.New("secret key does not exist in secret")
)

// IsSecretRef reports whether v is a secret reference ("$<key>" or "$<secret>:<key>").
func IsSecretRef(v string) bool { return len(v) > 1 && v[0] == '$' }

// ResolveSecretRef resolves a settings value. A value that does not start with "$" is
// returned unchanged. "$<key>" reads key from patrol-secret; "$<secret>:<key>" reads key from
// the Secret <secret>, which must be labelled app.kubernetes.io/part-of=patrol-cd (TEN-131).
// Resolved values are whitespace-trimmed. On failure the error wraps ErrSecretNotFound or
// ErrSecretKeyNotFound and the returned value is v unchanged (upstream leaves unresolved
// references as-is). Errors never contain secret values.
func (s *Settings) ResolveSecretRef(v string) (string, error) {
	return resolveSecretRef(v, s.secretData, s.refSecrets, s.names.Secret, nil)
}

func resolveSecretRef(v string, secretData map[string]string, refSecrets map[string]map[string]string, secretName string, unlabelled map[string]bool) (string, error) {
	if !IsSecretRef(v) {
		return v, nil
	}
	// Messages never quote the reference itself: an unresolvable "reference" may be a secret
	// value that happens to start with "$".
	ref := v[1:]
	if name, key, ok := strings.Cut(ref, ":"); ok {
		data, found := refSecrets[name]
		if !found {
			if unlabelled[name] {
				return v, fmt.Errorf("%w: Secret %q is not labelled %s=%s", ErrSecretNotFound, name, LabelKeyPartOf, LabelValuePartOf)
			}
			return v, fmt.Errorf("%w: the referenced Secret does not exist or is not labelled %s=%s", ErrSecretNotFound, LabelKeyPartOf, LabelValuePartOf)
		}
		val, found := data[key]
		if !found {
			return v, fmt.Errorf("%w %q (key %q)", ErrSecretKeyNotFound, name, key)
		}
		return strings.TrimSpace(val), nil
	}
	val, found := secretData[ref]
	if !found {
		return v, fmt.Errorf("%w %q", ErrSecretKeyNotFound, secretName)
	}
	return strings.TrimSpace(val), nil
}

// resolveRef resolves v for key of src, reporting failures as warnings.
func (p *parser) resolveRef(src Source, key, v string) string {
	out, err := resolveSecretRef(v, p.s.secretData, p.s.refSecrets, p.names.Secret, p.refSecretsUnlabelled)
	if err != nil {
		p.warn(src, key, "unresolved secret reference: %v; the value is used as is", err)
	}
	return out
}

// replaceMapSecrets resolves every string value (not map keys) of a decoded YAML document.
func (p *parser) replaceMapSecrets(src Source, key string, obj any) any {
	switch val := obj.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for _, k := range slices.Sorted(maps.Keys(val)) {
			out[k] = p.replaceMapSecrets(src, key, val[k])
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, v := range val {
			out[i] = p.replaceMapSecrets(src, key, v)
		}
		return out
	case string:
		return p.resolveRef(src, key, val)
	default:
		return val
	}
}

// parseSecrets reads patrol-secret: the server key, the API server certificate, the webhook
// secrets and the data used to resolve "$<key>" references.
func (p *parser) parseSecrets() {
	src := SourceSecret
	sec := p.secrets[src]
	s := p.s
	if sec == nil {
		p.errorf(src, SecretKeyServerSecretKey, "server.secretkey is missing: Secret %q not found", p.names.Secret)
		p.keepPreviousServerKey()
		return
	}
	data := secretStringData(sec)
	s.secretData = data
	s.webhook.SecretsLoaded = true

	switch key, ok := data[SecretKeyServerSecretKey]; {
	case !ok || key == "":
		p.errorf(src, SecretKeyServerSecretKey, "server.secretkey is missing")
		p.keepPreviousServerKey()
	case len(key) < minServerSecretKeyLen:
		p.errorf(src, SecretKeyServerSecretKey, "server.secretkey is shorter than %d bytes", minServerSecretKeyLen)
		p.keepPreviousServerKey()
	default:
		s.serverSecretKey = NewSensitive(key)
	}
	if prev, ok := data[SecretKeyServerSecretKeyPrevious]; ok && prev != "" {
		if len(prev) < minServerSecretKeyLen {
			p.errorf(src, SecretKeyServerSecretKeyPrevious, "server.secretkey.previous is shorter than %d bytes and is ignored", minServerSecretKeyLen)
		} else {
			s.serverSecretKeyPrev = NewSensitive(prev)
		}
	}

	certPEM, hasCert := data[SecretKeyTLSCert]
	keyPEM, hasKey := data[SecretKeyTLSKey]
	switch {
	case hasCert && hasKey:
		cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
		if err != nil {
			// The error text of X509KeyPair describes structure, never key material.
			if p.prev != nil && p.prev.serverCertificate != nil {
				s.serverCertificate = p.prev.serverCertificate
				p.errorf(src, SecretKeyTLSCert, "invalid TLS certificate or key: %v; keeping the last valid certificate", err)
			} else {
				p.errorf(src, SecretKeyTLSCert, "invalid TLS certificate or key: %v", err)
			}
		} else {
			s.serverCertificate = &cert
		}
	case hasCert != hasKey:
		p.warn(src, SecretKeyTLSCert, "tls.crt and tls.key must be set together; the certificate is ignored")
	}

	for _, w := range []struct {
		key string
		dst *Sensitive
	}{
		{SecretKeyWebhookGitHub, &s.webhook.GitHubSecret},
		{SecretKeyWebhookGitLab, &s.webhook.GitLabSecret},
		{SecretKeyWebhookBitbucketUUID, &s.webhook.BitbucketUUID},
		{SecretKeyWebhookBitbucketServer, &s.webhook.BitbucketServerSecret},
		{SecretKeyWebhookGogs, &s.webhook.GogsSecret},
		{SecretKeyWebhookAzureDevOpsUser, &s.webhook.AzureDevOpsUsername},
		{SecretKeyWebhookAzureDevOpsPass, &s.webhook.AzureDevOpsPassword},
	} {
		if v, ok := data[w.key]; ok {
			*w.dst = NewSensitive(p.resolveRef(src, w.key, v))
		}
	}
}

func (p *parser) keepPreviousServerKey() {
	if p.prev != nil && !p.prev.serverSecretKey.IsZero() {
		p.s.serverSecretKey = p.prev.serverSecretKey
		p.warn(SourceSecret, SecretKeyServerSecretKey, "keeping the last valid server.secretkey until the Secret is fixed")
	}
}

func defaultAccounts() auth.Accounts {
	a, _ := auth.AccountsFromMaps(nil, nil)
	return a
}

// parseAccounts interprets accounts.* and admin.* from patrol-cm and patrol-secret with
// auth.AccountsFromMaps (fail-closed per account).
//
// While patrol-cm or patrol-secret is not loaded (retained or absent) every account is
// disabled: their current settings are unknown, so a disabled account must not come back
// and a revoked password or token must not keep working (upstream GetAccounts fails).
func (p *parser) parseAccounts() {
	cm := p.cmData(SourceConfigMap)
	accounts, err := auth.AccountsFromMaps(cm, p.s.secretData)
	if p.s.SourceState(SourceConfigMap) != SourceStateLoaded || p.s.SourceState(SourceSecret) != SourceStateLoaded {
		for name, a := range accounts {
			a.Enabled = false
			accounts[name] = a
		}
	}
	p.s.accounts = accounts
	if err == nil {
		return
	}
	var errs []error
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	} else {
		errs = []error{err}
	}
	for _, e := range errs {
		var ace *auth.AccountConfigError
		if !errors.As(e, &ace) {
			p.errorf(SourceConfigMap, "", "accounts: %v", e)
			continue
		}
		src := SourceConfigMap
		if _, inCM := cm[ace.Key]; !inCM {
			src = SourceSecret
		}
		// AccountConfigError texts name keys and parse errors; token and password values
		// are not included by auth for Secret keys, but be conservative for Secret sources.
		msg := e.Error()
		if src == SourceSecret {
			msg = fmt.Sprintf("account %q: invalid value; the account is interpreted fail-closed", ace.Account)
		}
		p.errorf(src, ace.Key, "%s", msg)
	}
}
