package clusters

import (
	"errors"
	"strings"

	"k8s.io/client-go/rest"
)

// ServiceAccountUserPrefix starts the Kubernetes user name of a service account.
const ServiceAccountUserPrefix = "system:serviceaccount:"

// ServiceAccountUsername returns "system:serviceaccount:<namespace>:<name>".
func ServiceAccountUsername(namespace, name string) string {
	return ServiceAccountUserPrefix + namespace + ":" + name
}

// ImpersonatingConfig returns a copy of base that impersonates user. base is not modified. The
// user comes from the project's destinationServiceAccounts (internal/project
// SelectServiceAccount), normally "system:serviceaccount:<ns>:<name>". Apply it to the config
// returned by RestConfig: client-go then adds the Impersonate-User header to every request,
// and the credentials of base must be allowed to impersonate the user.
func ImpersonatingConfig(base *rest.Config, user string) (*rest.Config, error) {
	if base == nil {
		return nil, errors.New("impersonation: nil base config")
	}
	if strings.TrimSpace(user) == "" {
		return nil, errors.New("impersonation: empty user name")
	}
	cfg := rest.CopyConfig(base)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	return cfg, nil
}
