package clusters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// AKS authentication settings (upstream argocd-k8s-auth azure, which delegates to kubelogin).
const (
	// AzureDefaultServerApplicationID is the AKS AAD server application (the token audience).
	AzureDefaultServerApplicationID = "6dae42f8-4368-4678-94ff-3960e28e3630"

	EnvAzureServerApplicationID = "AAD_SERVER_APPLICATION_ID"
	EnvAzureEnvironmentName     = "AAD_ENVIRONMENT_NAME"
	EnvAzureLoginMethod         = "AAD_LOGIN_METHOD"
	EnvAzureClientID            = "AZURE_CLIENT_ID"
	EnvAzureTenantID            = "AZURE_TENANT_ID"
	EnvAzureClientSecret        = "AZURE_CLIENT_SECRET"
	EnvAzureClientCertPath      = "AZURE_CLIENT_CERTIFICATE_PATH"
	EnvAzureClientCertPassword  = "AZURE_CLIENT_CERTIFICATE_PASSWORD"
	EnvAzureAuthorityHost       = "AZURE_AUTHORITY_HOST"
	EnvAzureFederatedTokenFile  = "AZURE_FEDERATED_TOKEN_FILE"
	EnvAzureUsername            = "AAD_USER_PRINCIPAL_NAME"
	EnvAzurePassword            = "AAD_USER_PRINCIPAL_PASSWORD"
	// kubelogin's older service principal variables, accepted as fallbacks.
	envAzureSPClientID     = "AAD_SERVICE_PRINCIPAL_CLIENT_ID"
	envAzureSPClientSecret = "AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"
)

// Azure login methods (kubelogin names).
const (
	AzureLoginWorkloadIdentity = "workloadidentity"
	AzureLoginServicePrincipal = "spn"
	AzureLoginManagedIdentity  = "msi"
	AzureLoginAzureCLI         = "azurecli"
	AzureLoginROPC             = "ropc"
)

// AzureOptions configures AzureTokenSource.
type AzureOptions struct {
	// Lookup resolves the environment variables above; nil uses the process environment.
	Lookup func(string) (string, bool)
	// ClientOptions are passed to the azidentity credential (transport, retries). The cloud
	// is derived from AAD_ENVIRONMENT_NAME and AZURE_AUTHORITY_HOST unless set here.
	ClientOptions azcore.ClientOptions
	// DisableInstanceDiscovery skips Entra instance metadata discovery (private clouds, tests).
	DisableInstanceDiscovery bool
}

// AzureTokenSource returns AKS access tokens for the scope "<server application ID>/.default".
// The login method comes from AAD_LOGIN_METHOD and defaults to workload identity, as upstream.
// Supported methods: workloadidentity, spn (client secret or certificate), msi, azurecli and
// ropc. The interactive methods (devicecode, interactive) are rejected because the controller
// has no terminal.
func AzureTokenSource(opts AzureOptions) (TokenSource, error) {
	lookup := opts.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := lookup(k); ok && v != "" {
				return v
			}
		}
		return ""
	}
	serverID := AzureDefaultServerApplicationID
	if v, ok := lookup(EnvAzureServerApplicationID); ok {
		serverID = v
	}
	if strings.TrimSpace(serverID) == "" {
		return nil, errors.New("azure: empty server application ID")
	}
	co := opts.ClientOptions
	if co.Cloud.ActiveDirectoryAuthorityHost == "" {
		c, err := azureCloud(get(EnvAzureEnvironmentName))
		if err != nil {
			return nil, err
		}
		if host := get(EnvAzureAuthorityHost); host != "" {
			c.ActiveDirectoryAuthorityHost = host
		}
		co.Cloud = c
	}
	method := strings.ToLower(get(EnvAzureLoginMethod))
	if method == "" {
		method = AzureLoginWorkloadIdentity
	}
	clientID := get(EnvAzureClientID, envAzureSPClientID)
	tenantID := get(EnvAzureTenantID)

	switch method {
	case AzureLoginWorkloadIdentity, AzureLoginServicePrincipal, AzureLoginManagedIdentity, AzureLoginAzureCLI, AzureLoginROPC:
	default:
		return nil, fmt.Errorf("azure: unsupported login method %q", method)
	}
	build := func() (azcore.TokenCredential, error) {
		switch method {
		case AzureLoginWorkloadIdentity:
			return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
				ClientOptions:            co,
				ClientID:                 clientID,
				TenantID:                 tenantID,
				TokenFilePath:            get(EnvAzureFederatedTokenFile),
				DisableInstanceDiscovery: opts.DisableInstanceDiscovery,
			})
		case AzureLoginServicePrincipal:
			return azureServicePrincipal(tenantID, clientID, get, co, opts.DisableInstanceDiscovery)
		case AzureLoginManagedIdentity:
			mo := &azidentity.ManagedIdentityCredentialOptions{ClientOptions: co}
			if clientID != "" {
				mo.ID = azidentity.ClientID(clientID)
			}
			return azidentity.NewManagedIdentityCredential(mo)
		case AzureLoginAzureCLI:
			return azidentity.NewAzureCLICredential(&azidentity.AzureCLICredentialOptions{TenantID: tenantID})
		default: // AzureLoginROPC
			//nolint:staticcheck // kubelogin's ropc login method is username/password by definition.
			return azidentity.NewUsernamePasswordCredential(tenantID, clientID, get(EnvAzureUsername), get(EnvAzurePassword),
				&azidentity.UsernamePasswordCredentialOptions{ClientOptions: co, DisableInstanceDiscovery: opts.DisableInstanceDiscovery})
		}
	}
	// The credential is created on first use, like the exec plugin it replaces: an incomplete
	// environment fails requests to the cluster, not the construction of its configuration.
	var (
		mu   sync.Mutex
		cred azcore.TokenCredential
	)
	credential := func() (azcore.TokenCredential, error) {
		mu.Lock()
		defer mu.Unlock()
		if cred != nil {
			return cred, nil
		}
		c, err := build()
		if err != nil {
			return nil, fmt.Errorf("azure: create %s credential: %w", method, err)
		}
		cred = c
		return cred, nil
	}
	scope := serverID + "/.default"
	return TokenSourceFunc(func(ctx context.Context) (Token, error) {
		cred, err := credential()
		if err != nil {
			return Token{}, err
		}
		at, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
		if err != nil {
			return Token{}, fmt.Errorf("azure: get token: %w", err)
		}
		return Token{Value: at.Token, Expiry: at.ExpiresOn}, nil
	}), nil
}

func azureServicePrincipal(tenantID, clientID string, get func(...string) string, co azcore.ClientOptions, noDiscovery bool) (azcore.TokenCredential, error) {
	if secret := get(EnvAzureClientSecret, envAzureSPClientSecret); secret != "" {
		return azidentity.NewClientSecretCredential(tenantID, clientID, secret,
			&azidentity.ClientSecretCredentialOptions{ClientOptions: co, DisableInstanceDiscovery: noDiscovery})
	}
	path := get(EnvAzureClientCertPath)
	if path == "" {
		return nil, fmt.Errorf("%s or %s is required for the spn login method", EnvAzureClientSecret, EnvAzureClientCertPath)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is operator configuration, as for the exec plugin
	if err != nil {
		return nil, fmt.Errorf("read client certificate: %w", err)
	}
	var password []byte
	if p := get(EnvAzureClientCertPassword); p != "" {
		password = []byte(p)
	}
	certs, key, err := azidentity.ParseCertificates(data, password)
	if err != nil {
		return nil, fmt.Errorf("parse client certificate: %w", err)
	}
	return azidentity.NewClientCertificateCredential(tenantID, clientID, certs, key,
		&azidentity.ClientCertificateCredentialOptions{ClientOptions: co, DisableInstanceDiscovery: noDiscovery})
}

// azureCloud maps AAD_ENVIRONMENT_NAME (kubelogin/azure-sdk names) to a cloud configuration.
func azureCloud(name string) (cloud.Configuration, error) {
	switch strings.ToLower(name) {
	case "", "azurepubliccloud", "azurecloud", "public":
		return cloud.AzurePublic, nil
	case "azurechinacloud", "china":
		return cloud.AzureChina, nil
	case "azureusgovernmentcloud", "azureusgovernment", "usgovernment":
		return cloud.AzureGovernment, nil
	default:
		return cloud.Configuration{}, fmt.Errorf("azure: unknown environment %q", name)
	}
}
