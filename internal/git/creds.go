package git

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DefaultHTTPSUsername is used when HTTPSCreds has a password or token but no
// username (upstream's GitHub access-token convention).
const DefaultHTTPSUsername = "x-access-token"

// Environment variables understood by the askpass helper.
const (
	askpassFileEnv   = "PATROL_GIT_ASKPASS_FILE"
	askpassOriginEnv = "PATROL_GIT_ASKPASS_ORIGIN"
)

// Creds produces the environment that lets git authenticate to a repository.
//
// Environ is called once per credentialed git command with workdir, a private
// (0700) directory created for that command and removed after it. Implementations
// write any secret material as 0600 files inside workdir and return environment
// entries referring to them; secrets must never be returned as command arguments.
// cleanup is always non-nil when err is nil and removes everything Environ created.
//
// Git configuration may be returned through GIT_CONFIG_COUNT / GIT_CONFIG_KEY_<n> /
// GIT_CONFIG_VALUE_<n>; the client merges those entries with its own.
type Creds interface {
	Environ(workdir string) (env []string, cleanup func(), err error)
}

// redactor is implemented by the package's credential types to list the secret
// strings that must be scrubbed from error messages.
type redactor interface {
	redactions() []string
}

// fingerprinter is implemented by the package's credential types so the ref cache
// can key listings by credential identity without storing secrets.
type fingerprinter interface {
	fingerprint(h hash.Hash)
}

// NopCreds performs anonymous access.
type NopCreds struct{}

var (
	_ Creds         = NopCreds{}
	_ fingerprinter = NopCreds{}
)

// Environ returns no environment.
func (NopCreds) Environ(string) ([]string, func(), error) { return nil, func() {}, nil }

func (NopCreds) fingerprint(h hash.Hash) { h.Write([]byte("nop\x00")) }

// HTTPSCreds authenticates over HTTP(S).
//
// Precedence follows upstream: when ForceBasicAuth is set and Password is non-empty,
// an "Authorization: Basic" header is sent preemptively; otherwise a non-empty
// BearerToken is sent as "Authorization: Bearer". Username/password are always
// offered to git through the askpass helper (Username defaults to
// DefaultHTTPSUsername). A client certificate is used only when both ClientCertData
// and ClientCertKey are set (PEM; the key must not be encrypted).
type HTTPSCreds struct {
	Username       string
	Password       string
	BearerToken    string
	ClientCertData string
	ClientCertKey  string
	// CAData is a PEM bundle used as the only trust roots for the server
	// (GIT_SSL_CAINFO). Ignored when Insecure is set.
	CAData []byte
	// Insecure disables TLS verification (GIT_SSL_NO_VERIFY).
	Insecure       bool
	ForceBasicAuth bool
}

var (
	_ Creds         = HTTPSCreds{}
	_ redactor      = HTTPSCreds{}
	_ fingerprinter = HTTPSCreds{}
)

func (c HTTPSCreds) username() string {
	if c.Username == "" {
		return DefaultHTTPSUsername
	}
	return c.Username
}

// HasClientCert reports whether both certificate and key are present.
func (c HTTPSCreds) HasClientCert() bool { return c.ClientCertData != "" && c.ClientCertKey != "" }

func (c HTTPSCreds) basicHeader() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(c.username()+":"+c.Password))
}

// authHeader returns the preemptive Authorization header, or "".
func (c HTTPSCreds) authHeader() string {
	switch {
	case c.Password != "" && c.ForceBasicAuth:
		return c.basicHeader()
	case c.BearerToken != "":
		return "Authorization: Bearer " + c.BearerToken
	default:
		return ""
	}
}

// Environ writes the askpass credentials file (and TLS material) into workdir.
func (c HTTPSCreds) Environ(workdir string) ([]string, func(), error) {
	files := &tempFiles{}
	var env []string
	if c.Insecure {
		env = append(env, "GIT_SSL_NO_VERIFY=true")
	} else if len(c.CAData) > 0 {
		p, err := files.write(workdir, "ca-*.pem", c.CAData, 0o600)
		if err != nil {
			return nil, nil, err
		}
		env = append(env, "GIT_SSL_CAINFO="+p)
	}
	if c.HasClientCert() {
		cert, err := files.write(workdir, "cert-*.pem", []byte(c.ClientCertData), 0o600)
		if err != nil {
			files.remove()
			return nil, nil, err
		}
		key, err := files.write(workdir, "key-*.pem", []byte(c.ClientCertKey), 0o600)
		if err != nil {
			files.remove()
			return nil, nil, err
		}
		env = append(env, "GIT_SSL_CERT="+cert, "GIT_SSL_KEY="+key)
	}
	if h := c.authHeader(); h != "" {
		env = appendGitConfig(env, gitConfig{"http.extraHeader", h})
	}
	askpass, err := askpassExecutable()
	if err != nil {
		files.remove()
		return nil, nil, fmt.Errorf("%w: resolve askpass helper: %w", ErrInvalidCredentials, err)
	}
	payload, err := json.Marshal(askpassPayload{Username: c.username(), Password: c.Password})
	if err != nil {
		files.remove()
		return nil, nil, fmt.Errorf("encode askpass payload: %w", err)
	}
	p, err := files.write(workdir, "askpass-*", payload, 0o600)
	if err != nil {
		files.remove()
		return nil, nil, err
	}
	env = append(env, "GIT_ASKPASS="+askpass, askpassFileEnv+"="+p)
	return env, files.remove, nil
}

func (c HTTPSCreds) redactions() []string {
	var out []string
	for _, s := range []string{c.Password, c.BearerToken, c.ClientCertKey} {
		if s != "" {
			out = append(out, s)
		}
	}
	if c.Password != "" {
		out = append(out, strings.TrimPrefix(c.basicHeader(), "Authorization: Basic "))
	}
	return out
}

func (c HTTPSCreds) fingerprint(h hash.Hash) {
	writeFields(h, "https", c.Username, c.Password, c.BearerToken, c.ClientCertData, c.ClientCertKey,
		string(c.CAData), strconv.FormatBool(c.Insecure), strconv.FormatBool(c.ForceBasicAuth))
}

// SSHCreds authenticates over SSH with a private key.
type SSHCreds struct {
	// PrivateKey is the unencrypted private key (OpenSSH or PEM format).
	PrivateKey string
	// InsecureIgnoreHostKey disables host key verification
	// (StrictHostKeyChecking=no, UserKnownHostsFile=/dev/null).
	InsecureIgnoreHostKey bool
	// KnownHosts is the known_hosts data used for strict host key checking. When
	// empty (and not insecure) every host is unknown and connections fail.
	KnownHosts []byte
	// Proxy is an optional socks5:// or socks5h:// proxy (host and port required),
	// reached through connect-proxy as upstream. Credentials in the URL are passed
	// as SOCKS5_USER / SOCKS5_PASSWD.
	Proxy string
}

var (
	_ Creds         = SSHCreds{}
	_ redactor      = SSHCreds{}
	_ fingerprinter = SSHCreds{}
)

// Environ writes the key (and known_hosts / proxy script) into workdir and returns
// GIT_SSH_COMMAND.
func (c SSHCreds) Environ(workdir string) ([]string, func(), error) {
	var proxy *url.URL
	if c.Proxy != "" {
		if err := ValidateSSHProxyURL(c.Proxy); err != nil {
			return nil, nil, fmt.Errorf("%w: unsafe proxy URL rejected: %w", ErrInvalidCredentials, err)
		}
		proxy, _ = url.Parse(c.Proxy) // validated above
	}
	files := &tempFiles{}
	key := c.PrivateKey
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	keyPath, err := files.write(workdir, "ssh-key-*", []byte(key), 0o600)
	if err != nil {
		return nil, nil, err
	}
	args := []string{"ssh", "-i", shellQuote(keyPath), "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes"}
	if c.InsecureIgnoreHostKey {
		slog.Warn("git: temporarily disabling strict host key checking (StrictHostKeyChecking=no, UserKnownHostsFile=/dev/null); do not use in production")
		args = append(args, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
	} else {
		kh, err := files.write(workdir, "known-hosts-*", c.KnownHosts, 0o600)
		if err != nil {
			files.remove()
			return nil, nil, err
		}
		args = append(args, "-o", "StrictHostKeyChecking=yes", "-o", shellQuote("UserKnownHostsFile="+kh))
	}
	var env []string
	if proxy != nil {
		script := fmt.Sprintf("#!/bin/sh\nexec connect-proxy -5 -S %s \"$@\"\n",
			net.JoinHostPort(proxy.Hostname(), proxy.Port()))
		sp, err := files.write(workdir, "proxy-cmd-*", []byte(script), 0o700)
		if err != nil {
			files.remove()
			return nil, nil, err
		}
		if !safePathRe.MatchString(sp) {
			files.remove()
			return nil, nil, fmt.Errorf("%w: temp directory path is not usable in an SSH ProxyCommand", ErrInvalidCredentials)
		}
		args = append(args, "-o", shellQuote("ProxyCommand="+sp+" %h %p"))
		if proxy.User != nil {
			env = append(env, "SOCKS5_USER="+proxy.User.Username())
			if pw, ok := proxy.User.Password(); ok {
				env = append(env, "SOCKS5_PASSWD="+pw)
			}
		}
	}
	env = append(env, "GIT_SSH_COMMAND="+strings.Join(args, " "))
	return env, files.remove, nil
}

func (c SSHCreds) redactions() []string {
	var out []string
	if c.PrivateKey != "" {
		out = append(out, c.PrivateKey)
	}
	if c.Proxy != "" {
		if u, err := url.Parse(c.Proxy); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				out = append(out, pw)
			}
		}
	}
	return out
}

func (c SSHCreds) fingerprint(h hash.Hash) {
	writeFields(h, "ssh", c.PrivateKey, strconv.FormatBool(c.InsecureIgnoreHostKey), string(c.KnownHosts), c.Proxy)
}

var (
	proxyHostRe = regexp.MustCompile(`^[a-zA-Z0-9.\-]+$`)
	safePathRe  = regexp.MustCompile(`^[a-zA-Z0-9/._\-]+$`)
)

// ValidateSSHProxyURL checks an SSH proxy URL as upstream does: scheme socks5 or
// socks5h, a hostname made of [a-zA-Z0-9.-] or an IPv6 literal, and an explicit
// port in 1..65535. Callers that adapt Repository secrets can use it to drop an
// unsafe proxy at construction time (upstream logs and connects directly).
func ValidateSSHProxyURL(raw string) error {
	return validateProxyURL(raw, "socks5", "socks5h")
}

// ValidateHTTPProxyURL is ValidateSSHProxyURL for HTTP proxies (http, https,
// socks5, socks5h schemes).
func ValidateHTTPProxyURL(raw string) error {
	return validateProxyURL(raw, "http", "https", "socks5", "socks5h")
}

func validateProxyURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy URL: %w", err)
	}
	if !slices.Contains(schemes, u.Scheme) {
		return fmt.Errorf("invalid proxy scheme %q: only %v are supported", u.Scheme, schemes)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("proxy URL has empty hostname")
	}
	if strings.Contains(host, ":") {
		if net.ParseIP(host) == nil {
			return fmt.Errorf("invalid IPv6 address in proxy URL: %q", host)
		}
	} else if !proxyHostRe.MatchString(host) {
		return fmt.Errorf("proxy hostname contains invalid characters: %q", host)
	}
	port := u.Port()
	if port == "" {
		return errors.New("proxy URL has no port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid proxy port %q", port)
	}
	return nil
}

// shellQuote quotes s for the POSIX shell that git uses to run GIT_SSH_COMMAND.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tempFiles tracks files written by Environ so cleanup can remove them.
type tempFiles struct{ paths []string }

func (t *tempFiles) write(dir, pattern string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("create credential file: %w", err)
	}
	t.paths = append(t.paths, f.Name())
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("chmod credential file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write credential file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close credential file: %w", err)
	}
	return filepath.Clean(f.Name()), nil
}

func (t *tempFiles) remove() {
	for _, p := range t.paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("git: could not remove credential file", slog.String("file", filepath.Base(p)), slog.Any("error", err))
		}
	}
	t.paths = nil
}

// gitConfig is one configuration entry passed through the environment.
type gitConfig struct{ key, value string }

// appendGitConfig adds entries to the GIT_CONFIG_COUNT/KEY/VALUE triplets already
// present in env (later GIT_CONFIG_COUNT entries are replaced by one final count).
func appendGitConfig(env []string, entries ...gitConfig) []string {
	if len(entries) == 0 {
		return env
	}
	count := 0
	out := make([]string, 0, len(env)+2*len(entries)+1)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_COUNT" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				count = n
			}
			continue
		}
		out = append(out, kv)
	}
	for _, e := range entries {
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", count, e.key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", count, e.value))
		count++
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(count))
}

// scopeHeaders confines the http.extraHeader entries returned by Creds to the
// repository origin. Unscoped http.extraHeader entries are rewritten to
// http.<origin>/.extraHeader, so they are never sent to submodules on other
// hosts; without an http(s) origin (ssh, git or file repositories) they are
// dropped, since they could only reach other hosts. For every remaining header
// scope, redirects are disabled (http.<scope>.followRedirects=false): git's
// default (initial) rebases later requests onto the redirect target while still
// sending the headers loaded for the original URL. Other entries are kept.
func scopeHeaders(cfg []gitConfig, origin string) []gitConfig {
	out := make([]gitConfig, 0, len(cfg)+1)
	var guards []gitConfig
	guarded := map[string]bool{}
	for _, e := range cfg {
		scope, isHeader := extraHeaderScope(e.key)
		if !isHeader {
			out = append(out, e)
			continue
		}
		if scope == "" {
			if origin == "" {
				continue
			}
			scope = origin + "/"
			e.key = "http." + scope + ".extraHeader"
		}
		out = append(out, e)
		if lower := strings.ToLower(scope); !guarded[lower] {
			guarded[lower] = true
			guards = append(guards, gitConfig{"http." + scope + ".followRedirects", "false"})
		}
	}
	return append(out, guards...)
}

// extraHeaderScope reports whether key is an http.extraHeader configuration key
// (keys are case-insensitive) and returns its URL scope ("" when unscoped).
func extraHeaderScope(key string) (string, bool) {
	const prefix, suffix = "http.", ".extraheader"
	lower := strings.ToLower(key)
	if !strings.HasPrefix(lower, prefix) || !strings.HasSuffix(lower, suffix) {
		return "", false
	}
	if len(key) <= len(prefix)+len(suffix) {
		return "", true // "http.extraHeader" (or an empty "http..extraHeader" scope)
	}
	return key[len(prefix) : len(key)-len(suffix)], true
}

var fingerprintKey = func() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return b
}()

// credsFingerprint returns a process-local keyed digest of the credentials, or ""
// when the credential type cannot be fingerprinted (the ref cache is then bypassed).
func credsFingerprint(c Creds) string {
	f, ok := c.(fingerprinter)
	if !ok {
		return ""
	}
	h := hmac.New(sha256.New, fingerprintKey)
	f.fingerprint(h)
	return hex.EncodeToString(h.Sum(nil))
}

func writeFields(h hash.Hash, fields ...string) {
	for _, f := range fields {
		_, _ = h.Write([]byte(strconv.Itoa(len(f))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(f))
	}
}

func credsRedactions(c Creds) []string {
	if r, ok := c.(redactor); ok {
		return r.redactions()
	}
	return nil
}
