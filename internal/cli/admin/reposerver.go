package admin

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/pflag"

	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// repoServerOptions are the connection flags of commands that call the repo-server
// (upstream --argocd-repo-server, --argocd-repo-server-plaintext, --repo-server-ca-cert-path,
// --repo-server-client-cert-path, --repo-server-client-cert-key-path). The repo-server
// requires the shared internal token (Secret patrol-internal-token, ADR-0006).
type repoServerOptions struct {
	address        string
	plaintext      bool
	caCertPath     string
	clientCertPath string
	clientKeyPath  string
	tokenFile      string
}

func (o *repoServerOptions) bind(fs *pflag.FlagSet, addressFlag, defaultAddress string) {
	fs.StringVar(&o.address, addressFlag, defaultAddress, "Patrol CD repo server address (host:port)")
	fs.BoolVar(&o.plaintext, "repo-server-plaintext", false, "Use a plaintext client (non-TLS) to connect to repository server")
	fs.StringVar(&o.caCertPath, "repo-server-ca-cert-path", "", "Path to the CA certificate(s) verifying the repo server certificate (default: system roots)")
	fs.StringVar(&o.clientCertPath, "repo-server-client-cert-path", "", "Path to the client certificate for mutual TLS with the repo server")
	fs.StringVar(&o.clientKeyPath, "repo-server-client-cert-key-path", "", "Path to the client certificate key for mutual TLS with the repo server")
	fs.StringVar(&o.tokenFile, "repo-server-token-file", "", "Path to a file holding the internal token of the repo server (Secret patrol-internal-token)")
}

// client returns a repo-server client, or nil when no address is configured.
func (o *repoServerOptions) client() (apiclient.Client, error) {
	if o.address == "" {
		return nil, nil
	}
	if o.tokenFile == "" {
		return nil, errors.New("--repo-server-token-file is required to call the repo server")
	}
	tokenData, err := readFileLimited(o.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading repo server token: %w", err)
	}
	token := strings.TrimSpace(string(tokenData))
	if token == "" {
		return nil, errors.New("repo server token file is empty")
	}
	address := o.address
	var tlsConfig *tls.Config
	if o.plaintext {
		if !strings.Contains(address, "://") {
			address = "http://" + address
		}
	} else {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if o.caCertPath != "" {
			pem, err := readFileLimited(o.caCertPath)
			if err != nil {
				return nil, fmt.Errorf("reading repo server CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s contains no PEM certificate", o.caCertPath)
			}
			tlsConfig.RootCAs = pool
		}
		if (o.clientCertPath == "") != (o.clientKeyPath == "") {
			return nil, errors.New("--repo-server-client-cert-path and --repo-server-client-cert-key-path must be specified together")
		}
		if o.clientCertPath != "" {
			cert, err := tls.LoadX509KeyPair(o.clientCertPath, o.clientKeyPath)
			if err != nil {
				return nil, fmt.Errorf("loading repo server client certificate: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}
	c, err := apiclient.NewHTTPClient(address, token, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("creating repo server client: %w", err)
	}
	return c, nil
}
