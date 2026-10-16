package clusters

import (
	"errors"
	"fmt"
)

var (
	// ErrClusterNotFound matches errors for servers or names with no registered cluster,
	// including the disabled in-cluster cluster.
	ErrClusterNotFound = errors.New("cluster not found")
	// ErrInClusterDisabled matches errors caused by cluster.inClusterEnabled=false.
	ErrInClusterDisabled = errors.New("in-cluster has been disabled")
	// ErrAmbiguousName matches name lookups that resolve to more than one server.
	ErrAmbiguousName = errors.New("ambiguous cluster name")
	// ErrSecretNotFound matches errors for a credentials Secret that is not in the registry.
	ErrSecretNotFound = errors.New("cluster credentials secret not found")
	// ErrInvalidConfig matches configuration errors detected while building a rest.Config.
	ErrInvalidConfig = errors.New("invalid cluster configuration")
	// ErrNotSynced is returned by Registry lookups before the informers synced.
	ErrNotSynced = errors.New("cluster registry not synced")
	// ErrNotLegacyToken is returned by RotateManagerToken for tokens that are not legacy
	// service-account token Secrets.
	ErrNotLegacyToken = errors.New("token is not a legacy service account token")
	// ErrNoBearerToken matches clusters that do not authenticate with a bearer token.
	ErrNoBearerToken = errors.New("cluster does not use bearer token authentication")
)

// NotFoundError reports a server with no registered cluster. Its message is upstream's
// (`cluster "<server>" not found`, or `cluster "<server>" is disabled` for the disabled
// in-cluster cluster).
type NotFoundError struct {
	Server   string
	Disabled bool
}

func (e *NotFoundError) Error() string {
	if e.Disabled {
		return fmt.Sprintf("cluster %q is disabled", e.Server)
	}
	return fmt.Sprintf("cluster %q not found", e.Server)
}

// Is matches ErrClusterNotFound, and ErrInClusterDisabled when Disabled is set.
func (e *NotFoundError) Is(target error) bool {
	return target == ErrClusterNotFound || (e.Disabled && target == ErrInClusterDisabled)
}

// NameError reports a name that matches no cluster or several clusters. Messages are upstream's
// GetDestinationServer messages.
type NameError struct {
	Name    string
	Servers []string
}

func (e *NameError) Error() string {
	if len(e.Servers) == 0 {
		return "there are no clusters with this name: " + e.Name
	}
	return fmt.Sprintf("there are %d clusters with the same name: %v", len(e.Servers), e.Servers)
}

// Is matches ErrClusterNotFound (no match) or ErrAmbiguousName (several matches).
func (e *NameError) Is(target error) bool {
	if len(e.Servers) == 0 {
		return target == ErrClusterNotFound
	}
	return target == ErrAmbiguousName
}

// SecretError reports a missing or unusable credentials Secret.
type SecretError struct {
	Cluster   string // Cluster object name
	Namespace string
	Secret    string
	Reason    string
}

func (e *SecretError) Error() string {
	return fmt.Sprintf("credentials secret %s/%s of cluster %q: %s", e.Namespace, e.Secret, e.Cluster, e.Reason)
}

// Is matches ErrSecretNotFound.
func (e *SecretError) Is(target error) bool { return target == ErrSecretNotFound }
