package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// NewCertCommand returns `patrol cert` (API-074, REPO-062).
func NewCertCommand(f *cmdutil.Factory) *cobra.Command {
	c := &cobra.Command{
		Use:   "cert",
		Short: "Manage repository certificates and SSH known hosts entries",
		RunE:  func(c *cobra.Command, args []string) error { return cmdutil.HelpExit(c, args) },
		Example: `  # Add a TLS certificate for cd.example.com to Patrol CD cert store from a file
  patrol cert add-tls --from ~/mycert.pem cd.example.com

  # Add a TLS certificate for cd.example.com to Patrol CD via stdin
  cat ~/mycert.pem | patrol cert add-tls cd.example.com

  # Add SSH known host entries for cd.example.com to Patrol CD by scanning host
  ssh-keyscan cd.example.com | patrol cert add-ssh --batch

  # List all known TLS certificates
  patrol cert list --cert-type https

  # Remove all TLS certificates for cd.example.com
  patrol cert rm --cert-type https cd.example.com

  # Remove all certificates and SSH known host entries for cd.example.com
  patrol cert rm cd.example.com
`,
	}
	c.AddCommand(newCertAddSSHCommand(f), newCertAddTLSCommand(f), newCertListCommand(f), newCertRemoveCommand(f))
	return c
}

func newCertAddTLSCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		fromFile string
		upsert   bool
	)
	c := &cobra.Command{
		Use:   "add-tls SERVERNAME",
		Short: "Add TLS certificate data for connecting to repository server SERVERNAME",
		Args:  cmdutil.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var data []byte
			var err error
			if fromFile != "" {
				output.Printf(f.Out(), "Reading TLS certificate data in PEM format from '%s'\n", fromFile)
				data, err = readInputFile(f, fromFile)
			} else {
				output.Println(f.Out(), "Enter TLS certificate data in PEM format. Press CTRL-D when finished.")
				data, err = readInputFile(f, "-")
			}
			if err != nil {
				return err
			}
			certs, err := parsePEMCertificates(data)
			if err != nil {
				return err
			}
			unique := dedupeCertificates(f, certs)
			if len(unique) == 0 {
				output.Println(f.Out(), "No valid certificates have been detected in the stream.")
				return nil
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			_, err = client.CreateCertificates(c.Context(), &apiclient.RepositoryCertificateCreateRequest{
				Upsert: upsert,
				Body: apiclient.RepositoryCertificateList{Items: []apiclient.RepositoryCertificate{{
					ServerName: args[0], CertType: apiclient.CertificateTypeHTTPS, CertData: strings.Join(unique, "\n"),
				}}},
			})
			if err != nil {
				return err
			}
			output.Printf(f.Out(), "Created/updated TLS certificate entry for repository server %s with %d unique PEM certificates\n", args[0], len(unique))
			return nil
		},
	}
	c.Flags().StringVar(&fromFile, "from", "", "Read TLS certificate data from file (default is to read from stdin)")
	c.Flags().BoolVar(&upsert, "upsert", false, "Replace existing TLS certificate if certificate is different in input")
	return c
}

type pemCert struct {
	pem  string
	cert *x509.Certificate
}

// parsePEMCertificates extracts the CERTIFICATE blocks of data; other block types
// (private keys in particular) are refused without echoing their content.
func parsePEMCertificates(data []byte) ([]pemCert, error) {
	var out []pemCert
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("invalid PEM block of type %q: only certificates are accepted", b.Type)
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid certificate: %w", err)
		}
		out = append(out, pemCert{pem: strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes}))), cert: c})
	}
	if len(bytes.TrimSpace(rest)) > 0 && len(out) == 0 && bytes.Contains(rest, []byte("-----BEGIN")) {
		return nil, errors.New("invalid PEM data")
	}
	return out, nil
}

// dedupeCertificates removes certificates with the same SHA-256 fingerprint.
func dedupeCertificates(f *cmdutil.Factory, certs []pemCert) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range certs {
		sum := sha256.Sum256(c.cert.Raw)
		fp := hex.EncodeToString(sum[:])
		if seen[fp] {
			output.Printf(f.Out(), "WARNING: Duplicate certificate detected (SHA256 fingerprint %s, subject '%s'), skipping.\n", fp, c.cert.Subject.String())
			continue
		}
		seen[fp] = true
		out = append(out, c.pem)
	}
	return out
}

func newCertAddSSHCommand(f *cmdutil.Factory) *cobra.Command {
	var (
		fromFile string
		batch    bool
		upsert   bool
	)
	c := &cobra.Command{
		Use:   "add-ssh --batch",
		Short: "Add SSH known host entries for repository servers",
		Args:  cmdutil.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !batch {
				return errors.New("you need to specify --batch or specify --help for usage instructions")
			}
			var data []byte
			var err error
			if fromFile != "" {
				output.Printf(f.Out(), "Reading SSH known hosts entries from file '%s'\n", fromFile)
				data, err = readInputFile(f, fromFile)
			} else {
				output.Println(f.Out(), "Enter SSH known hosts entries, one per line. Press CTRL-D when finished.")
				data, err = readInputFile(f, "-")
			}
			if err != nil {
				return err
			}
			certs, err := parseKnownHosts(data)
			if err != nil {
				return err
			}
			if len(certs) == 0 {
				return errors.New("No valid SSH known hosts data found.") //nolint:staticcheck // upstream wording
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			res, err := client.CreateCertificates(c.Context(), &apiclient.RepositoryCertificateCreateRequest{
				Upsert: upsert, Body: apiclient.RepositoryCertificateList{Items: certs},
			})
			if err != nil {
				return err
			}
			output.Printf(f.Out(), "Successfully created %d SSH known host entries\n", len(res.Items))
			return nil
		},
	}
	c.Flags().StringVar(&fromFile, "from", "", "Read SSH known hosts data from file (default is to read from stdin)")
	c.Flags().BoolVar(&batch, "batch", false, "Perform batch processing by reading in SSH known hosts data (mandatory flag)")
	c.Flags().BoolVar(&upsert, "upsert", false, "Replace existing SSH server public host keys if key is different in input")
	return c
}

// parseKnownHosts turns known_hosts lines into one ssh entry per host name; comments and
// blank lines are skipped, markers (@cert-authority, @revoked) refused.
func parseKnownHosts(data []byte) ([]apiclient.RepositoryCertificate, error) {
	var out []apiclient.RepositoryCertificate
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		marker, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			if errors.Is(err, io.EOF) {
				continue
			}
			return nil, fmt.Errorf("invalid SSH known hosts entry on line %d: %w", n, err)
		}
		if marker != "" {
			return nil, fmt.Errorf("invalid SSH known hosts entry on line %d: marker %q is not supported", n, marker)
		}
		data := base64.StdEncoding.EncodeToString(key.Marshal())
		for _, h := range hosts {
			out = append(out, apiclient.RepositoryCertificate{ServerName: h, CertType: apiclient.CertificateTypeSSH, CertSubType: key.Type(), CertData: data})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read SSH known hosts data: %w", err)
	}
	return out, nil
}

func newCertRemoveCommand(f *cmdutil.Factory) *cobra.Command {
	var certType, certSubType string
	c := &cobra.Command{
		Use:   "rm REPOSERVER",
		Short: "Remove certificate of TYPE for REPOSERVER",
		Args:  cmdutil.MinimumArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			pattern := args[0]
			if pattern == "*" {
				return errors.New("A single wildcard is not allowed as REPOSERVER name.") //nolint:staticcheck // upstream wording
			}
			ok, err := f.OptionalPrompt().Confirm(fmt.Sprintf("Are you sure you want to remove all certificates for '%s'? [y/n]", pattern))
			if err != nil {
				return err
			}
			if !ok {
				output.Printf(f.Out(), "The command to remove all certificates for '%s' was cancelled.\n", pattern)
				return nil
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			removed, err := client.DeleteCertificates(c.Context(), &apiclient.RepositoryCertificateQuery{HostNamePattern: pattern, CertType: certType, CertSubType: certSubType})
			if err != nil {
				return err
			}
			if len(removed.Items) == 0 {
				output.Println(f.Out(), "No certificates were removed (none matched the given pattern)")
				return nil
			}
			for _, cert := range removed.Items {
				output.Printf(f.Out(), "Removed cert for '%s' of type '%s' (subtype '%s')\n", cert.ServerName, cert.CertType, cert.CertSubType)
			}
			return nil
		},
	}
	c.Flags().StringVar(&certType, "cert-type", "", "Only remove certs of given type (ssh, https)")
	c.Flags().StringVar(&certSubType, "cert-sub-type", "", "Only remove certs of given sub-type (only for ssh)")
	return c
}

func newCertListCommand(f *cmdutil.Factory) *cobra.Command {
	var certType, hostNamePattern, sortOrder, format string
	c := &cobra.Command{
		Use:   "list",
		Short: "List configured certificates",
		RunE: func(c *cobra.Command, _ []string) error {
			if certType != "" && certType != "ssh" && certType != "https" {
				output.Println(f.Out(), "cert-type must be either ssh or https")
				return cmdutil.Silent(cmdutil.ExitFatal)
			}
			if err := output.CheckFormat(format, output.JSON, output.YAML, output.Wide, ""); err != nil {
				return output.UnknownFormat(format)
			}
			client, err := f.NewClient(c.Context())
			if err != nil {
				return err
			}
			l, err := client.ListCertificates(c.Context(), &apiclient.RepositoryCertificateQuery{HostNamePattern: hostNamePattern, CertType: certType})
			if err != nil {
				return err
			}
			switch format {
			case output.JSON, output.YAML:
				return output.PrintResourceList(f.Out(), l.Items, format, false)
			}
			items := l.Items
			switch sortOrder {
			case "hostname", "":
				sort.SliceStable(items, func(i, j int) bool { return items[i].ServerName < items[j].ServerName })
			case "type":
				sort.SliceStable(items, func(i, j int) bool { return items[i].CertType < items[j].CertType })
			}
			t := output.NewTable(f.Out())
			t.Row("HOSTNAME", "TYPE", "SUBTYPE", "INFO")
			for _, ce := range items {
				t.Row(ce.ServerName, ce.CertType, ce.CertSubType, ce.CertInfo)
			}
			return t.Flush()
		},
	}
	c.Flags().StringVarP(&format, "output", "o", output.Wide, "Output format. One of: json|yaml|wide")
	c.Flags().StringVar(&sortOrder, "sort", "", "Set display sort order for output format wide. One of: hostname|type")
	c.Flags().StringVar(&certType, "cert-type", "", "Only list certificates of given type, valid: 'ssh','https'")
	c.Flags().StringVar(&hostNamePattern, "hostname-pattern", "", "Only list certificates for hosts matching given glob-pattern")
	return c
}
