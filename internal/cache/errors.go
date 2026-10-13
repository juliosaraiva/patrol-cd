package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

var (
	// ErrClusterUnreachable is returned (wrapped in *UnreachableError) when the API server
	// cannot be reached during a sync.
	ErrClusterUnreachable = errors.New("cluster unreachable")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("cluster cache closed")
	// ErrUnknownGroupKind is returned by IsNamespaced for kinds the cluster does not serve.
	ErrUnknownGroupKind = errors.New("unknown group kind")
	// ErrOutOfScope is returned by GetManagedLiveObjs for targets outside the namespaces (or
	// cluster-scoped resources) the cache is configured to manage.
	ErrOutOfScope = errors.New("resource out of cache scope")
	// ErrInvalidResourceKey is returned by ParseResourceKey.
	ErrInvalidResourceKey = errors.New("invalid resource key")

	// errSuperseded aborts a sync whose result is obsolete because Invalidate ran meanwhile.
	errSuperseded = errors.New("sync superseded by invalidation")
)

// OutOfScopeError reports a GetManagedLiveObjs target outside the cache's namespaces. Its
// message matches upstream Argo CD; it matches ErrOutOfScope with errors.Is.
type OutOfScopeError struct {
	Kind, Name, Namespace string
}

func (e *OutOfScopeError) Error() string {
	if e.Namespace == "" {
		return fmt.Sprintf("cluster level %s %q can not be managed when in namespaced mode", e.Kind, e.Name)
	}
	return fmt.Sprintf("namespace %q for %s %q is not managed", e.Namespace, e.Kind, e.Name)
}

// Is matches ErrOutOfScope.
func (e *OutOfScopeError) Is(target error) bool { return target == ErrOutOfScope }

// UnreachableError reports a connectivity failure to a cluster. It matches
// ErrClusterUnreachable with errors.Is.
type UnreachableError struct {
	Server string
	Err    error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("cluster %s unreachable: %v", e.Server, e.Err)
}

// Unwrap returns the underlying error.
func (e *UnreachableError) Unwrap() error { return e.Err }

// Is matches ErrClusterUnreachable.
func (e *UnreachableError) Is(target error) bool { return target == ErrClusterUnreachable }

// classifySyncError wraps connectivity failures in *UnreachableError.
func classifySyncError(server string, err error) error {
	if err == nil || errors.Is(err, ErrClusterUnreachable) {
		return err
	}
	if isUnreachable(err) {
		return &UnreachableError{Server: server, Err: err}
	}
	return err
}

// isUnreachable reports transport-level failures and API server unavailability, as opposed
// to errors returned by a reachable server (authorization, validation, not found).
func isUnreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		// A response was received. 503/504 are what proxies and load balancers in front of an
		// unreachable API server return.
		return apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err)
	}
	var urlErr *url.Error
	var netErr net.Error
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &urlErr), errors.As(err, &opErr), errors.As(err, &dnsErr), errors.As(err, &netErr):
		return true
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET), errors.Is(err, context.DeadlineExceeded):
		return true
	case utilnet.IsConnectionRefused(err), utilnet.IsConnectionReset(err), utilnet.IsProbableEOF(err), utilnet.IsTimeout(err):
		return true
	}
	return false
}

// isRetryableListError reports errors worth retrying during a list.
func isRetryableListError(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case apierrors.IsInternalError(err), apierrors.IsTooManyRequests(err), apierrors.IsServerTimeout(err),
		apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err), apierrors.IsUnexpectedServerError(err):
		return true
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) ||
		apierrors.IsResourceExpired(err) || apierrors.IsGone(err) || apierrors.IsBadRequest(err):
		return false
	}
	return isUnreachable(err)
}

// isAccessDenied reports 401/403 responses.
func isAccessDenied(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err)
}

// isExpired reports 410 Gone / Expired responses that require a relist.
func isExpired(err error) bool {
	return apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
}
