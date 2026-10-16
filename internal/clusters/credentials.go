package clusters

import (
	"fmt"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Credentials are the secret connection settings of a Cluster, read from the Secret named by
// spec.config.credentialsRef (keys v1alpha1.ClusterSecretKey*). The String method never prints
// secret values.
type Credentials struct {
	BearerToken string
	Username    string
	Password    string
	CAData      []byte
	CertData    []byte
	KeyData     []byte
}

// CredentialsFromSecretData extracts Credentials from Secret data. Unknown keys are ignored and
// a nil map yields empty credentials.
func CredentialsFromSecretData(data map[string][]byte) Credentials {
	clone := func(b []byte) []byte {
		if len(b) == 0 {
			return nil
		}
		return append([]byte(nil), b...)
	}
	return Credentials{
		BearerToken: string(data[v1alpha1.ClusterSecretKeyBearerToken]),
		Username:    string(data[v1alpha1.ClusterSecretKeyUsername]),
		Password:    string(data[v1alpha1.ClusterSecretKeyPassword]),
		CAData:      clone(data[v1alpha1.ClusterSecretKeyCAData]),
		CertData:    clone(data[v1alpha1.ClusterSecretKeyCertData]),
		KeyData:     clone(data[v1alpha1.ClusterSecretKeyKeyData]),
	}
}

// SecretData returns the Secret data that stores c. Empty fields are omitted.
func (c Credentials) SecretData() map[string][]byte {
	out := map[string][]byte{}
	put := func(k string, v []byte) {
		if len(v) > 0 {
			out[k] = append([]byte(nil), v...)
		}
	}
	put(v1alpha1.ClusterSecretKeyBearerToken, []byte(c.BearerToken))
	put(v1alpha1.ClusterSecretKeyUsername, []byte(c.Username))
	put(v1alpha1.ClusterSecretKeyPassword, []byte(c.Password))
	put(v1alpha1.ClusterSecretKeyCAData, c.CAData)
	put(v1alpha1.ClusterSecretKeyCertData, c.CertData)
	put(v1alpha1.ClusterSecretKeyKeyData, c.KeyData)
	return out
}

// HasBasicOrToken reports whether a username, password or bearer token is set (the condition
// upstream uses to override the in-cluster service-account identity).
func (c Credentials) HasBasicOrToken() bool {
	return c.Username != "" || c.Password != "" || c.BearerToken != ""
}

// HasCertAuth reports whether both a client certificate and key are set (client-go's
// transport.Config.HasCertAuth). Exec plugins are not run for such configurations.
func (c Credentials) HasCertAuth() bool {
	return len(c.CertData) > 0 && len(c.KeyData) > 0
}

// String describes which credentials are present without revealing them.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{bearerToken:%t username:%t password:%t caData:%t certData:%t keyData:%t}",
		c.BearerToken != "", c.Username != "", c.Password != "", len(c.CAData) > 0, len(c.CertData) > 0, len(c.KeyData) > 0)
}

// GoString implements fmt.GoStringer so %#v does not print secrets either.
func (c Credentials) GoString() string { return c.String() }
