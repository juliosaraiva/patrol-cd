package repositories

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// bracketedHostRE matches the known_hosts "[host]:port" form.
var bracketedHostRE = regexp.MustCompile(`^\[(.*)\]:\d{1,5}$`)

// knownHostLine is one line of patrol-ssh-known-hosts-cm. Comments, blank and invalid
// lines are kept verbatim when the data is rewritten.
type knownHostLine struct {
	raw         string
	ok          bool
	host        string // first field (after an optional marker)
	keyType     string
	keyData     string
	fingerprint string
}

func parseKnownHostLines(data string) []knownHostLine {
	if data == "" {
		return nil
	}
	raw := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	out := make([]knownHostLine, 0, len(raw))
	for _, line := range raw {
		l := knownHostLine{raw: line}
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed[0] != '#' {
			hosts, errs := settings.ParseSSHKnownHosts(trimmed)
			fields := strings.Fields(trimmed)
			i := 0
			if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
				i = 1
			}
			if len(errs) == 0 && len(hosts) == 1 && len(fields) >= i+3 {
				l.ok = true
				l.host, l.keyType, l.keyData = fields[i], fields[i+1], fields[i+2]
				l.fingerprint = "SHA256:" + hosts[0].FingerprintSHA256
			}
		}
		out = append(out, l)
	}
	return out
}

func renderKnownHostLines(lines []knownHostLine) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// matchHostName is upstream MatchHostName: a file glob in which brackets are literal;
// an empty pattern matches everything.
func matchHostName(host, pattern string) bool {
	if pattern == "" {
		return true
	}
	ok, err := filepath.Match(nonBracketedPattern(pattern), host)
	return err == nil && ok
}

func nonBracketedPattern(p string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(p)
}

// certSelector is a validated RepositoryCertificateQuery.
type certSelector struct {
	pattern, subType string
	ssh, tls         bool
}

func newCertSelector(q *apiclient.RepositoryCertificateQuery) (certSelector, error) {
	sel := certSelector{pattern: q.HostNamePattern, subType: q.CertSubType}
	switch q.CertType {
	case "", "*":
		sel.ssh, sel.tls = true, true
	case apiclient.CertificateTypeSSH:
		sel.ssh = true
	case apiclient.CertificateTypeHTTPS, "tls":
		sel.tls = true
	default:
		return sel, rest.InvalidArgument("unknown certificate type: %s", q.CertType)
	}
	if _, err := filepath.Match(nonBracketedPattern(q.HostNamePattern), ""); err != nil {
		return sel, rest.InvalidArgument("invalid hostNamePattern: %v", err)
	}
	if err := checkText("hostNamePattern", q.HostNamePattern, 1024, false); err != nil {
		return sel, err
	}
	return sel, nil
}

func (sel certSelector) matchSSH(l knownHostLine) bool {
	return l.ok && matchHostName(l.host, sel.pattern) && (sel.subType == "" || sel.subType == "*" || sel.subType == l.keyType)
}

// tlsItems describes the certificates of one TLS entry (one item per PEM certificate; a
// single "invalid" item when the data cannot be parsed).
func tlsItems(host, data string, withData bool) []apiclient.RepositoryCertificate {
	pems, err := settings.ParseTLSCertificates(data)
	if err != nil || len(pems) == 0 {
		msg := "no certificates found"
		if err != nil {
			msg = err.Error()
		}
		return []apiclient.RepositoryCertificate{{ServerName: host, CertType: apiclient.CertificateTypeHTTPS, CertSubType: "invalid", CertInfo: msg}}
	}
	out := make([]apiclient.RepositoryCertificate, 0, len(pems))
	for _, p := range pems {
		item := apiclient.RepositoryCertificate{ServerName: host, CertType: apiclient.CertificateTypeHTTPS}
		if cert, err := decodeCertificate(p); err != nil {
			item.CertSubType, item.CertInfo = "invalid", err.Error()
		} else {
			item.CertSubType = strings.ToLower(cert.PublicKeyAlgorithm.String())
			item.CertInfo = cert.Subject.String()
		}
		if withData {
			item.CertData = p
		}
		out = append(out, item)
	}
	return out
}

func decodeCertificate(p string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errInvalidPEM
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errInvalidX509
	}
	return cert, nil
}

var (
	errInvalidPEM  = rest.InvalidArgument("could not decode PEM data from input")
	errInvalidX509 = rest.InvalidArgument("could not parse X509 data from input")
)

// listCertificateItems lists the entries of the two ConfigMaps matching sel: SSH entries
// in file order, then TLS entries by host name. certData is never listed.
func listCertificateItems(sshData string, tlsData map[string]string, sel certSelector) []apiclient.RepositoryCertificate {
	out := []apiclient.RepositoryCertificate{}
	if sel.ssh {
		for _, l := range parseKnownHostLines(sshData) {
			if sel.matchSSH(l) {
				out = append(out, apiclient.RepositoryCertificate{ServerName: l.host, CertType: apiclient.CertificateTypeSSH, CertSubType: l.keyType, CertInfo: l.fingerprint})
			}
		}
	}
	if sel.tls {
		for _, host := range slices.Sorted(maps.Keys(tlsData)) {
			if matchHostName(host, sel.pattern) {
				out = append(out, tlsItems(host, tlsData[host], false)...)
			}
		}
	}
	return out
}

// certChange is the outcome of applying a create or delete to the ConfigMap data.
type certChange struct {
	ssh        string
	tls        map[string]string
	sshChanged bool
	tlsChanged bool
	items      []apiclient.RepositoryCertificate
}

// applyCertificates adds items to the data (upstream CreateRepoCertificate). Every item is
// validated before anything is saved; the first error fails the whole request.
func applyCertificates(sshData string, tlsData map[string]string, items []apiclient.RepositoryCertificate, upsert bool) (certChange, error) {
	ch := certChange{tls: maps.Clone(tlsData), items: []apiclient.RepositoryCertificate{}}
	if ch.tls == nil {
		ch.tls = map[string]string{}
	}
	lines := parseKnownHostLines(sshData)
	for _, c := range items {
		switch c.CertType {
		case apiclient.CertificateTypeSSH:
			item, changed, err := addKnownHost(&lines, c, upsert)
			if err != nil {
				return ch, err
			}
			if changed {
				ch.sshChanged = true
				ch.items = append(ch.items, item)
			}
		case apiclient.CertificateTypeHTTPS:
			// The host name is a ConfigMap key (no IPv6 literals, no ports).
			if !validHost(c.ServerName, settings.IsValidHostname) || len(validation.IsConfigMapKey(c.ServerName)) > 0 {
				return ch, rest.InvalidArgument("invalid hostname in request: %s", c.ServerName)
			}
			if strings.Contains(c.CertData, "PRIVATE KEY") {
				return ch, rest.InvalidArgument("certData for %s contains private key material; only certificates may be submitted", c.ServerName)
			}
			pems, err := settings.ParseTLSCertificates(c.CertData)
			if err != nil {
				return ch, rest.InvalidArgument("certData for %s: %v", c.ServerName, err)
			}
			if len(pems) == 0 {
				return ch, rest.InvalidArgument("no valid PEM data received")
			}
			for _, p := range pems {
				if _, err := decodeCertificate(p); err != nil {
					return ch, err
				}
			}
			// Only the certificate blocks are stored, never surrounding text.
			normalized := strings.Join(pems, "")
			if existing, ok := ch.tls[c.ServerName]; ok {
				if existing == normalized {
					continue
				}
				if !upsert {
					return ch, rest.AlreadyExists("TLS certificate for server '%s' already exists, and upsert was not specified", c.ServerName)
				}
			}
			ch.tls[c.ServerName] = normalized
			ch.tlsChanged = true
			ch.items = append(ch.items, tlsItems(c.ServerName, normalized, true)...)
		default:
			return ch, rest.InvalidArgument("unknown certificate type: %s", c.CertType)
		}
	}
	ch.ssh = renderKnownHostLines(lines)
	return ch, nil
}

// addKnownHost adds or (with upsert) replaces the entry for (host, key type).
func addKnownHost(lines *[]knownHostLine, c apiclient.RepositoryCertificate, upsert bool) (apiclient.RepositoryCertificate, bool, error) {
	host := c.ServerName
	if m := bracketedHostRE.FindStringSubmatch(host); m != nil {
		host = m[1]
	}
	if !validHost(host, settings.IsValidHostname) {
		return c, false, rest.InvalidArgument("invalid hostname in request: %s", host)
	}
	if c.CertSubType == "" || strings.ContainsFunc(c.CertSubType, func(r rune) bool { return r <= ' ' }) {
		return c, false, rest.InvalidArgument("certSubType (the SSH key type) is required for %s", c.ServerName)
	}
	data := strings.TrimSpace(c.CertData)
	if data == "" || strings.ContainsAny(data, " \t\r\n") || strings.Contains(data, "PRIVATE") {
		return c, false, rest.InvalidArgument("certData for %s must be the base64 public key", c.ServerName)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return c, false, rest.InvalidArgument("certData for %s must be the base64 public key", c.ServerName)
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return c, false, rest.InvalidArgument("certData for %s is not a valid SSH public key", c.ServerName)
	}
	if key.Type() != c.CertSubType {
		return c, false, rest.InvalidArgument("the key of %s is not of type %s", c.ServerName, c.CertSubType)
	}
	line := c.ServerName + " " + c.CertSubType + " " + data
	parsed := parseKnownHostLines(line)
	if len(parsed) != 1 || !parsed[0].ok {
		return c, false, rest.InvalidArgument("invalid SSH known host entry for %s", c.ServerName)
	}
	item := apiclient.RepositoryCertificate{ServerName: c.ServerName, CertType: apiclient.CertificateTypeSSH, CertSubType: c.CertSubType, CertData: data, CertInfo: parsed[0].fingerprint}
	for i, l := range *lines {
		if !l.ok || l.host != c.ServerName || l.keyType != c.CertSubType {
			continue
		}
		if l.keyData == data {
			return item, false, nil
		}
		if !upsert {
			return item, false, rest.AlreadyExists("key for '%s' (subtype: '%s') already exists, and upsert was not specified", c.ServerName, c.CertSubType)
		}
		(*lines)[i] = parsed[0]
		return item, true, nil
	}
	*lines = append(*lines, parsed[0])
	return item, true, nil
}

// removeCertificates removes the entries matching sel (upstream RemoveRepoCertificates;
// TLS entries whose data cannot be parsed are removed too and reported as "invalid").
func removeCertificates(sshData string, tlsData map[string]string, sel certSelector) certChange {
	ch := certChange{tls: maps.Clone(tlsData), items: []apiclient.RepositoryCertificate{}}
	if ch.tls == nil {
		ch.tls = map[string]string{}
	}
	lines := parseKnownHostLines(sshData)
	if sel.ssh {
		kept := lines[:0:0]
		for _, l := range lines {
			if sel.matchSSH(l) {
				ch.items = append(ch.items, apiclient.RepositoryCertificate{ServerName: l.host, CertType: apiclient.CertificateTypeSSH, CertSubType: l.keyType, CertData: l.keyData, CertInfo: l.fingerprint})
				ch.sshChanged = true
				continue
			}
			kept = append(kept, l)
		}
		lines = kept
	}
	if sel.tls {
		for _, host := range slices.Sorted(maps.Keys(ch.tls)) {
			if !matchHostName(host, sel.pattern) {
				continue
			}
			ch.items = append(ch.items, tlsItems(host, ch.tls[host], true)...)
			delete(ch.tls, host)
			ch.tlsChanged = true
		}
	}
	ch.ssh = renderKnownHostLines(lines)
	return ch
}

func (s *Service) listCertificates(ctx context.Context, q *apiclient.RepositoryCertificateQuery) (*apiclient.RepositoryCertificateList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	sel, err := newCertSelector(q)
	if err != nil {
		return nil, err
	}
	var sshData string
	var tlsData map[string]string
	if sel.ssh {
		t, err := s.readTrustConfigMap(ctx, kc, s.cfg.SSHKnownHostsConfigMap)
		if err != nil {
			return nil, err
		}
		sshData = t.data()[settings.SSHKnownHostsKey]
	}
	if sel.tls {
		t, err := s.readTrustConfigMap(ctx, kc, s.cfg.TLSCertsConfigMap)
		if err != nil {
			return nil, err
		}
		tlsData = t.data()
	}
	return &apiclient.RepositoryCertificateList{Items: listCertificateItems(sshData, tlsData, sel)}, nil
}

func (s *Service) createCertificates(ctx context.Context, req *apiclient.RepositoryCertificateCreateRequest) (*apiclient.RepositoryCertificateList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	if len(req.Body.Items) == 0 {
		return nil, rest.InvalidArgument("no certificates in request")
	}
	var ssh, tls bool
	for _, c := range req.Body.Items {
		ssh = ssh || c.CertType == apiclient.CertificateTypeSSH
		tls = tls || c.CertType == apiclient.CertificateTypeHTTPS
	}
	var out certChange
	err = retryOnConflict(func() error {
		sshCM, tlsCM, err := s.readCertConfigMaps(ctx, kc, ssh, tls)
		if err != nil {
			return err
		}
		out, err = applyCertificates(sshCM.data()[settings.SSHKnownHostsKey], tlsCM.data(), req.Body.Items, req.Upsert)
		if err != nil {
			return err
		}
		return s.saveCertificates(ctx, kc, sshCM, tlsCM, out)
	})
	if err != nil {
		return nil, err
	}
	rest.AuditFrom(ctx).SetMessage("created or updated %d certificate entries", len(out.items))
	return &apiclient.RepositoryCertificateList{Items: out.items}, nil
}

func (s *Service) deleteCertificates(ctx context.Context, q *apiclient.RepositoryCertificateQuery) (*apiclient.RepositoryCertificateList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	sel, err := newCertSelector(q)
	if err != nil {
		return nil, err
	}
	var out certChange
	err = retryOnConflict(func() error {
		sshCM, tlsCM, err := s.readCertConfigMaps(ctx, kc, sel.ssh, sel.tls)
		if err != nil {
			return err
		}
		out = removeCertificates(sshCM.data()[settings.SSHKnownHostsKey], tlsCM.data(), sel)
		return s.saveCertificates(ctx, kc, sshCM, tlsCM, out)
	})
	if err != nil {
		return nil, err
	}
	rest.AuditFrom(ctx).SetMessage("removed %d certificate entries", len(out.items))
	return &apiclient.RepositoryCertificateList{Items: out.items}, nil
}

// readCertConfigMaps reads the ConfigMaps a request needs (the others stay empty).
func (s *Service) readCertConfigMaps(ctx context.Context, kc KubeClient, ssh, tls bool) (sshCM, tlsCM trustConfigMap, err error) {
	if ssh {
		if sshCM, err = s.readTrustConfigMap(ctx, kc, s.cfg.SSHKnownHostsConfigMap); err != nil {
			return
		}
	}
	if tls {
		tlsCM, err = s.readTrustConfigMap(ctx, kc, s.cfg.TLSCertsConfigMap)
	}
	return
}

func (s *Service) saveCertificates(ctx context.Context, kc KubeClient, sshCM, tlsCM trustConfigMap, ch certChange) error {
	if ch.sshChanged {
		data := sshCM.data()
		data[settings.SSHKnownHostsKey] = ch.ssh
		if err := s.writeTrustConfigMap(ctx, kc, sshCM, data); err != nil {
			return err
		}
	}
	if ch.tlsChanged {
		if err := s.writeTrustConfigMap(ctx, kc, tlsCM, ch.tls); err != nil {
			return err
		}
	}
	return nil
}
