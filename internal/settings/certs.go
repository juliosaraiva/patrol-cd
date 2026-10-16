package settings

import (
	"bufio"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Limits of certificate and known_hosts parsing (upstream util/cert).
const (
	// CertificateMaxLines is the maximum number of lines of one PEM certificate.
	CertificateMaxLines = 128
	// CertificateMaxEntriesPerStream is the maximum number of certificates per host or
	// known_hosts entries.
	CertificateMaxEntriesPerStream = 256
)

const (
	pemBeginMarker = "-----BEGIN CERTIFICATE-----"
	pemEndMarker   = "-----END CERTIFICATE-----"
)

// validHostNameRE is upstream's hostname check for TLS certificate entries.
var validHostNameRE = regexp.MustCompile(`^([a-zA-Z0-9]|[a-zA-Z0-9_][a-zA-Z0-9-_]{0,61}[a-zA-Z0-9_])(\.([a-zA-Z0-9]|[a-zA-Z0-9_][a-zA-Z0-9-_]{0,61}[a-zA-Z0-9]))*(\.){0,1}$`)

// IsValidHostname reports whether hostname is acceptable as a TLS certificate key.
func IsValidHostname(hostname string) bool { return validHostNameRE.MatchString(hostname) }

// ParseTLSCertificates splits PEM data into certificates the way upstream does: text
// between BEGIN/END CERTIFICATE markers, at most CertificateMaxLines lines per certificate and
// CertificateMaxEntriesPerStream certificates. Text outside the markers is ignored.
func ParseTLSCertificates(data string) ([]string, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		in    bool
		lines int
		cur   strings.Builder
		out   []string
	)
	for scanner.Scan() {
		line := scanner.Text()
		if !in {
			if strings.HasPrefix(line, pemBeginMarker) {
				in, lines = true, 1
				cur.Reset()
				cur.WriteString(line + "\n")
			}
			continue
		}
		lines++
		cur.WriteString(line + "\n")
		if strings.HasPrefix(line, pemEndMarker) {
			in = false
			out = append(out, cur.String())
			if len(out) > CertificateMaxEntriesPerStream {
				return nil, errors.New("limit exceeded")
			}
		}
		if lines > CertificateMaxLines {
			return nil, errors.New("maximum number of lines exceeded during certificate parsing")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// decodePEMCertificate parses one PEM certificate (upstream wording).
func decodePEMCertificate(pemData string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("could not decode PEM data from input")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("could not parse X509 data from input")
	}
	return cert, nil
}

// parseTLSCerts reads patrol-tls-certs-cm: hostname → PEM certificates. Invalid hostnames,
// invalid certificates and entries without certificates are dropped and reported.
func (p *parser) parseTLSCerts() {
	src := SourceTLSCertsConfigMap
	data := p.cmData(src)
	for _, host := range slices.Sorted(maps.Keys(data)) {
		if !IsValidHostname(host) {
			p.errorf(src, host, "invalid hostname; the entry is ignored")
			continue
		}
		certs, err := ParseTLSCertificates(data[host])
		if err != nil {
			p.errorf(src, host, "%v; the entry is ignored", err)
			continue
		}
		var valid []string
		for i, c := range certs {
			if _, err := decodePEMCertificate(c); err != nil {
				p.errorf(src, host, "certificate %d: %v; it is ignored", i+1, err)
				continue
			}
			valid = append(valid, c)
		}
		if len(valid) == 0 {
			p.errorf(src, host, "no certificates found in existing file; the entry is ignored")
			continue
		}
		p.s.tlsCerts[host] = valid
		p.accept(src, host, data[host])
	}
}

// SSHKnownHost is one valid known_hosts entry.
type SSHKnownHost struct {
	// Line is the entry as written (trimmed).
	Line string
	// Marker is "@cert-authority", "@revoked" or "".
	Marker string
	// Hosts are the host patterns of the entry.
	Hosts []string
	// KeyType is the SSH key algorithm (for example "ssh-ed25519").
	KeyType string
	// FingerprintSHA256 is the unpadded base64 SHA-256 fingerprint of the key.
	FingerprintSHA256 string
}

// SSHFingerprintSHA256 returns the unpadded base64 SHA-256 fingerprint of key.
func SSHFingerprintSHA256(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// ParseSSHKnownHosts parses known_hosts data. Blank lines and comments are skipped; every
// other line must have at least three fields (host, key type, key) and a parseable key.
// Invalid lines are returned as errors (one per line) and skipped. Entries beyond
// CertificateMaxEntriesPerStream are dropped with an error.
func ParseSSHKnownHosts(data string) ([]SSHKnownHost, []error) {
	var (
		out  []SSHKnownHost
		errs []error
	)
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		if len(strings.SplitN(line, " ", 3)) != 3 {
			errs = append(errs, lineError(lineNo, errors.New("expected <host> <keytype> <key>")))
			continue
		}
		marker, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			errs = append(errs, lineError(lineNo, err))
			continue
		}
		if len(out) >= CertificateMaxEntriesPerStream {
			errs = append(errs, lineError(lineNo, errors.New("limit exceeded: more than 256 entries; the remaining entries are ignored")))
			break
		}
		h := SSHKnownHost{Line: line, Hosts: hosts, KeyType: key.Type(), FingerprintSHA256: SSHFingerprintSHA256(key)}
		if marker != "" {
			h.Marker = "@" + marker
		}
		out = append(out, h)
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, err)
	}
	return out, errs
}

type lineErr struct {
	line int
	err  error
}

func (e lineErr) Error() string { return "line " + strconv.Itoa(e.line) + ": " + e.err.Error() }
func (e lineErr) Unwrap() error { return e.err }

func lineError(n int, err error) error { return lineErr{line: n, err: err} }

// parseSSHKnownHosts reads patrol-ssh-known-hosts-cm.
func (p *parser) parseSSHKnownHosts() {
	src := SourceSSHKnownHostsConfigMap
	data := p.cmData(src)
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if k != SSHKnownHostsKey {
			p.unknownKey(src, k)
		}
	}
	v, ok := data[SSHKnownHostsKey]
	if !ok {
		return
	}
	hosts, errs := ParseSSHKnownHosts(v)
	for _, err := range errs {
		p.errorf(src, SSHKnownHostsKey, "%v; the entry is ignored", err)
	}
	for _, h := range hosts {
		for _, host := range h.Hosts {
			if strings.HasPrefix(host, "|1|") {
				p.warn(src, SSHKnownHostsKey, "hashed host names are not supported by the certificate API and cannot be listed")
				break
			}
		}
	}
	p.s.sshKnownHosts = hosts
	p.accept(src, SSHKnownHostsKey, v)
}
