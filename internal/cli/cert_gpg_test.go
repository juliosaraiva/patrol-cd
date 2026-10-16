package cli_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func knownHostsLine(t *testing.T, host string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
}

func TestCertCommands(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	dir := t.TempDir()

	ca, _, _ := clitest.NewCA(t, "git.example.com CA")
	ca2, _, _ := clitest.NewCA(t, "second CA")
	pemFile := writeFile(t, dir, "ca.pem", append(append([]byte{}, ca.CertPEM...), ca.CertPEM...))
	r := e.Run("cert", "add-tls", "git.example.com", "--from", pemFile).Succeeded(t)
	r.Contains(t, "Reading TLS certificate data in PEM format from", "WARNING: Duplicate certificate detected",
		"Created/updated TLS certificate entry for repository server git.example.com with 1 unique PEM certificates")
	cm, err := e.Clientset.CoreV1().ConfigMaps(e.Namespace).Get(e.Context(), settings.DefaultTLSCertsConfigMapName, metav1.GetOptions{})
	if err != nil || !strings.Contains(cm.Data["git.example.com"], "BEGIN CERTIFICATE") {
		t.Fatalf("TLS ConfigMap: %v %v", err, cm)
	}
	// stdin input; a different certificate needs --upsert.
	e.RunWith(clitest.RunOptions{Stdin: string(ca2.CertPEM)}, "cert", "add-tls", "git.example.com").Failed(t, 20)
	e.RunWith(clitest.RunOptions{Stdin: string(ca2.CertPEM)}, "cert", "add-tls", "git.example.com", "--upsert").Succeeded(t).
		Contains(t, "Enter TLS certificate data in PEM format. Press CTRL-D when finished.")
	// Private keys and garbage are refused.
	keyFile := writeFile(t, dir, "key.pem", ca.KeyPEM)
	e.Run("cert", "add-tls", "bad.example.com", "--from", keyFile).Failed(t, 20, `invalid PEM block of type "PRIVATE KEY"`)
	e.RunWith(clitest.RunOptions{Stdin: "not pem"}, "cert", "add-tls", "bad.example.com").Succeeded(t).
		Contains(t, "No valid certificates have been detected in the stream.")

	// SSH known hosts.
	hosts := knownHostsLine(t, "github.example.com") + "\n" + knownHostsLine(t, "gitlab.example.com,10.0.0.1") + "\n"
	hostsFile := writeFile(t, dir, "known_hosts", []byte("# comment\n"+hosts))
	e.Run("cert", "add-ssh", "--from", hostsFile).Failed(t, 20, "you need to specify --batch")
	e.Run("cert", "add-ssh", "--batch", "--from", hostsFile).Succeeded(t).Contains(t, "Successfully created 3 SSH known host entries")
	e.RunWith(clitest.RunOptions{Stdin: "@revoked " + knownHostsLine(t, "x.example.com")}, "cert", "add-ssh", "--batch").Failed(t, 20, "marker")
	e.RunWith(clitest.RunOptions{Stdin: "garbage line\n"}, "cert", "add-ssh", "--batch").Failed(t, 20)

	// list with filters and sorting.
	e.Run("cert", "list").Succeeded(t).Contains(t, "HOSTNAME", "TYPE", "SUBTYPE", "INFO", "git.example.com", "https", "github.example.com", "ssh", "ssh-ed25519")
	l := e.Run("cert", "list", "--cert-type", "ssh", "--sort", "hostname").Succeeded(t).Stdout
	if strings.Contains(l, "https") || strings.Index(l, "github.example.com") > strings.Index(l, "gitlab.example.com") {
		t.Fatalf("filtered/sorted list:\n%s", l)
	}
	e.Run("cert", "list", "--hostname-pattern", "gitlab*").Succeeded(t).Contains(t, "gitlab.example.com").NotContains(t, "github.example.com")
	e.Run("cert", "list", "-o", "json").Succeeded(t).Contains(t, `"serverName": "git.example.com"`)
	e.Run("cert", "list", "--cert-type", "ftp").Failed(t, 1, "")

	// rm.
	e.Run("cert", "rm", "*").Failed(t, 20, "A single wildcard is not allowed as REPOSERVER name.")
	e.Run("cert", "rm", "github.example.com", "--cert-type", "ssh").Succeeded(t).Contains(t, "Removed cert for 'github.example.com' of type 'ssh' (subtype 'ssh-ed25519')")
	e.Run("cert", "rm", "nothing.example.com").Succeeded(t).Contains(t, "No certificates were removed (none matched the given pattern)")
	e.Run("cert", "list").Succeeded(t).NotContains(t, "github.example.com")

	// Authorization: alice (read-only) can list but not add.
	alice := e.LoginToken("alice")
	e.RunAs(alice, "cert", "list").Succeeded(t).Contains(t, "gitlab.example.com")
	e.RunAs(alice, "cert", "add-tls", "x.example.com", "--from", pemFile).Failed(t, 20, "permission denied")
}

func armoredKey(t *testing.T, name string) (string, string, []byte) {
	t.Helper()
	ent, err := openpgp.NewEntity(name, "", strings.ToLower(name)+"@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var bin bytes.Buffer
	if err := ent.Serialize(&bin); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bin.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String(), ent.PrimaryKey.KeyIdString(), bin.Bytes()
}

func TestGPGCommands(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.LoginAs("admin")
	dir := t.TempDir()
	armored, id, _ := armoredKey(t, "Alice")
	_, id2, binary := armoredKey(t, "Bob")

	e.Run("gpg", "add").Failed(t, 20, "--from is mandatory")
	e.Run("gpg", "add", "--from", writeFile(t, dir, "a.asc", []byte(armored))).Succeeded(t).Contains(t, "Created 1 key(s) from input file.")
	e.Run("gpg", "add", "--from", writeFile(t, dir, "a2.asc", []byte(armored))).Succeeded(t).Contains(t, "1 key(s) were skipped because they exist already")
	// Binary key material is armored by the CLI.
	e.Run("gpg", "add", "-f", writeFile(t, dir, "b.gpg", binary)).Succeeded(t).Contains(t, "Created 1 key(s)")
	e.Run("gpg", "add", "-f", writeFile(t, dir, "empty", nil)).Failed(t, 20, "the key file is empty")

	e.Run("gpg", "list").Succeeded(t).Contains(t, "KEYID", "TYPE", "IDENTITY", id, id2, "alice@example.com")
	e.Run("gpg", "get", id).Succeeded(t).Contains(t, "Key ID:          "+id, "Key fingerprint:", "Key owner:       Alice <alice@example.com>")
	e.Run("gpg", "get", id, "-o", "yaml").Succeeded(t).Contains(t, "keyID: "+id, "keyData:")
	e.Run("gpg", "get").Failed(t, 20, "Missing KEYID argument")
	e.Run("gpg", "get", "0000000000000000").Failed(t, 20, "no such key", "HTTP 404")

	alice := e.LoginToken("alice")
	e.RunAs(alice, "gpg", "list").Succeeded(t).Contains(t, id)
	e.RunAs(alice, "gpg", "rm", id).Failed(t, 20, "permission denied")

	e.Run("gpg", "rm", id).Succeeded(t).Contains(t, "Deleted key with key ID "+id)
	e.Run("gpg", "list").Succeeded(t).NotContains(t, id+" ")
	cm, err := e.Clientset.CoreV1().ConfigMaps(e.Namespace).Get(e.Context(), settings.DefaultGPGKeysConfigMapName, metav1.GetOptions{})
	if err != nil || len(cm.Data) != 1 {
		t.Fatalf("GPG ConfigMap: %v %v", err, cm.Data)
	}
}
