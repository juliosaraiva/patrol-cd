package settings

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func TestTLSCertificatesConfigMap(t *testing.T) {
	t.Parallel()
	c1, _ := selfSignedPEM(t)
	c2, _ := selfSignedPEM(t)
	s := Parse(in(map[string]string{}, newCM(DefaultTLSCertsConfigMapName, map[string]string{
		"git.example.com":     "leading text\n" + string(c1) + string(c2),
		"bad_host-.":          string(c1),
		"empty.example.com":   "no certificates here",
		"partial.example.com": string(c1) + "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	})))
	certs := s.TLSCertificates()
	require.Len(t, certs["git.example.com"], 2)
	assert.Equal(t, certs["git.example.com"], s.TLSCertificatesForServer("git.example.com:8443"), "port stripped on lookup (REPO-058)")
	assert.NotContains(t, certs, "empty.example.com")
	assert.Len(t, certs["partial.example.com"], 1, "invalid certificates are dropped")
	requireIssue(t, s, DefaultTLSCertsConfigMapName, "empty.example.com", SeverityError, "no certificates found in existing file")
	requireIssue(t, s, DefaultTLSCertsConfigMapName, "partial.example.com", SeverityError, "could not parse X509 data from input")
	requireIssue(t, s, DefaultTLSCertsConfigMapName, "bad_host-.", SeverityError, "invalid hostname")
}

func TestParseTLSCertificatesLimits(t *testing.T) {
	t.Parallel()
	long := pemBeginMarker + "\n" + strings.Repeat("AAAA\n", CertificateMaxLines+1) + pemEndMarker + "\n"
	_, err := ParseTLSCertificates(long)
	assert.EqualError(t, err, "maximum number of lines exceeded during certificate parsing")
	c, _ := selfSignedPEM(t)
	_, err = ParseTLSCertificates(strings.Repeat(string(c), CertificateMaxEntriesPerStream+1))
	assert.EqualError(t, err, "limit exceeded")
	got, err := ParseTLSCertificates(strings.Repeat(string(c), 3))
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func sshKeyLine(t *testing.T, host string) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), k
}

func TestSSHKnownHostsConfigMap(t *testing.T) {
	t.Parallel()
	gh, ghKey := sshKeyLine(t, "github.com")
	port, _ := sshKeyLine(t, "[git.example.com]:2222")
	ca, _ := sshKeyLine(t, "@cert-authority *.example.com")
	hashed, _ := sshKeyLine(t, "|1|F1E1KeoE/eEWhi10WpGv4OdiO6Y=|3988QV0VE8wmZL7suNrYQLITLCg=")
	data := strings.Join([]string{
		"# comment",
		"",
		gh,
		port,
		ca,
		hashed,
		"broken-line ssh-ed25519",
		"github.com ssh-ed25519 not-base64!!",
	}, "\n")
	s := Parse(in(map[string]string{}, newCM(DefaultSSHKnownHostsConfigMapName, map[string]string{SSHKnownHostsKey: data, "extra": "x"})))
	hosts := s.SSHKnownHosts()
	require.Len(t, hosts, 4)
	assert.Equal(t, []string{"github.com"}, hosts[0].Hosts)
	assert.Equal(t, "ssh-ed25519", hosts[0].KeyType)
	assert.Equal(t, SSHFingerprintSHA256(ghKey), hosts[0].FingerprintSHA256)
	assert.Equal(t, []string{"[git.example.com]:2222"}, hosts[1].Hosts)
	assert.Equal(t, "@cert-authority", hosts[2].Marker)
	assert.Equal(t, strings.Join([]string{gh, port, ca, hashed}, "\n")+"\n", s.SSHKnownHostsData())
	requireIssue(t, s, DefaultSSHKnownHostsConfigMapName, SSHKnownHostsKey, SeverityError, "line 7: expected <host> <keytype> <key>")
	requireIssue(t, s, DefaultSSHKnownHostsConfigMapName, SSHKnownHostsKey, SeverityError, "line 8:")
	requireIssue(t, s, DefaultSSHKnownHostsConfigMapName, SSHKnownHostsKey, SeverityWarning, "hashed host names")
	requireIssue(t, s, DefaultSSHKnownHostsConfigMapName, "extra", SeverityWarning, "unknown key")
}

func TestSSHKnownHostsLimit(t *testing.T) {
	t.Parallel()
	line, _ := sshKeyLine(t, "h")
	data := strings.Repeat(line+"\n", CertificateMaxEntriesPerStream+2)
	hosts, errs := ParseSSHKnownHosts(data)
	assert.Len(t, hosts, CertificateMaxEntriesPerStream)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "limit exceeded")
}

func newGPGKey(t *testing.T, name string) (armored, keyID, fingerprint string, e *openpgp.Entity) {
	t.Helper()
	e, err := openpgp.NewEntity(name, "test", name+"@example.com", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	require.NoError(t, err)
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, e.Serialize(w))
	require.NoError(t, w.Close())
	return buf.String(), e.PrimaryKey.KeyIdString(), strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)), e
}

func TestGPGKeysConfigMap(t *testing.T) {
	t.Parallel()
	k1, id1, fp1, _ := newGPGKey(t, "alice")
	k2, id2, fp2, _ := newGPGKey(t, "bob")
	k3, _, _, _ := newGPGKey(t, "carol")
	_, _, _, priv := newGPGKey(t, "dave")
	var privBuf bytes.Buffer
	w, err := armor.Encode(&privBuf, openpgp.PrivateKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, priv.SerializePrivate(w, nil))
	require.NoError(t, w.Close())
	privID := priv.PrimaryKey.KeyIdString()

	s := Parse(in(map[string]string{}, newCM(DefaultGPGKeysConfigMapName, map[string]string{
		id1:                  k1,
		fp2:                  k2, // long key ID (fingerprint form)
		fp1[:24] + id2:       k1, // long key ID of another key
		"NOTAKEYID":          k3,
		strings.ToLower(id2): k2,
		privID:               privBuf.String(),
		"0123456789ABCDEF":   "garbage",
		"FEDCBA9876543210":   k1 + "\n" + k2,
	})))
	keys := s.GPGKeys()
	require.Len(t, keys, 2)
	assert.Equal(t, GPGKey{KeyID: id1, Fingerprint: fp1, Owner: "alice (test) <alice@example.com>", SubType: "ed25519", KeyData: k1}, keys[id1])
	assert.Equal(t, fp2, keys[id2].Fingerprint, "a fingerprint key is stored under its short ID")
	requireIssue(t, s, DefaultGPGKeysConfigMapName, "NOTAKEYID", SeverityError, `error validating key ID "NOTAKEYID" in GPG keys configmap: 'NOTAKEYID' is not a valid GnuPG key ID`)
	requireIssue(t, s, DefaultGPGKeysConfigMapName, strings.ToLower(id2), SeverityError, fmt.Sprintf("key parsed for entry with key ID '%s' had different key ID '%s'", strings.ToLower(id2), id2))
	requireIssue(t, s, DefaultGPGKeysConfigMapName, fp1[:24]+id2, SeverityError, fmt.Sprintf("key parsed for entry with key ID '%s' had different key ID '%s'", id2, id1))
	requireIssue(t, s, DefaultGPGKeysConfigMapName, privID, SeverityError, "private key material must not be stored")
	requireIssue(t, s, DefaultGPGKeysConfigMapName, "0123456789ABCDEF", SeverityError, `error parsing GPG key for entry "0123456789ABCDEF"`)
	requireIssue(t, s, DefaultGPGKeysConfigMapName, "FEDCBA9876543210", SeverityError, "more than one key found in input data")
}

func TestGPGKeyID(t *testing.T) {
	t.Parallel()
	got, err := GPGKeyID("0123456789ABCDEF0123456789ABCDEF01234567")
	require.NoError(t, err)
	assert.Equal(t, "89ABCDEF01234567", got)
	got, err = GPGKeyID("0123456789abcdef")
	require.NoError(t, err)
	assert.Equal(t, "0123456789abcdef", got, "case preserved as upstream")
	for _, bad := range []string{"", "0123", "XYZ3456789ABCDEF", "0123456789ABCDEF0"} {
		_, err := GPGKeyID(bad)
		assert.Error(t, err, bad)
	}
}
