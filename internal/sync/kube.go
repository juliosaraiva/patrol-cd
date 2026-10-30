package sync

import (
	"context"
	"errors"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

// Kube is the cluster access the engine needs. [KubeClient] implements it; tests use fakes.
//
// Every mutating method honors its DryRun option with a server-side dry run
// (dryRun=All): the API server validates and admits the request without persisting it.
// Errors are returned unmodified (or wrapped with %w) so apierrors predicates work.
type Kube interface {
	// Resource returns the API resource serving gvk. With an empty gvk.Version any served
	// version of the group/kind matches (the preferred one first). It returns an error for
	// which apierrors.IsNotFound is true when the kind is not served.
	Resource(ctx context.Context, gvk schema.GroupVersionKind) (*metav1.APIResource, error)
	// Get returns the live object (apierrors NotFound when it does not exist).
	Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error)
	// ClientSideApply applies obj like `kubectl apply`: create when missing, otherwise a
	// three-way patch from the last-applied-configuration annotation, which it updates. With
	// Force a patch rejected as invalid or conflicting is replaced by delete and re-create.
	// It returns a kubectl-style message such as "configmap/x configured".
	ClientSideApply(ctx context.Context, obj *unstructured.Unstructured, opts ApplyOptions) (string, error)
	// ServerSideApply applies obj with server-side apply, forcing field conflicts.
	ServerSideApply(ctx context.Context, obj *unstructured.Unstructured, opts ApplyOptions) (string, error)
	// MigrateClientSideApply moves the fields owned by csaManager (operation Update) of the
	// live object to the server-side apply field manager, so a following server-side apply
	// can remove fields dropped from the manifest. It decides on a fresh read of the object
	// and does nothing when no migration is needed: the engine also calls it when the live
	// state carries no managedFields (internal/cache strips them by default).
	MigrateClientSideApply(ctx context.Context, live *unstructured.Unstructured, csaManager string) error
	// Replace replaces an existing object (PUT); with Force it deletes and re-creates it.
	Replace(ctx context.Context, obj *unstructured.Unstructured, opts ReplaceOptions) (string, error)
	// Create creates obj.
	Create(ctx context.Context, obj *unstructured.Unstructured, opts CreateOptions) (string, error)
	// Update updates obj (PUT, obj carries the resourceVersion to update).
	Update(ctx context.Context, obj *unstructured.Unstructured, opts UpdateOptions) (*unstructured.Unstructured, error)
	// Delete deletes an object.
	Delete(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string, opts DeleteOptions) error
	// WaitForCRDEstablished waits until the CustomResourceDefinition reports Established or
	// the timeout elapses, then refreshes discovery for its group.
	WaitForCRDEstablished(ctx context.Context, name string, timeout time.Duration) error
}

// ApplyOptions configure ClientSideApply and ServerSideApply.
type ApplyOptions struct {
	// DryRun performs a server-side dry run.
	DryRun bool
	// Force deletes and re-creates the object when a client-side apply patch is rejected
	// as invalid or conflicting (kubectl apply --force). Server-side apply always forces
	// field-manager conflicts.
	Force bool
	// Validate selects field validation Strict; otherwise Ignore.
	Validate bool
}

// ReplaceOptions configure Replace.
type ReplaceOptions struct {
	DryRun bool
	// Force deletes and re-creates the object instead of updating it.
	Force bool
}

// CreateOptions configure Create.
type CreateOptions struct {
	DryRun   bool
	Validate bool
}

// UpdateOptions configure Update.
type UpdateOptions struct {
	DryRun bool
}

// DeleteOptions configure Delete.
type DeleteOptions struct {
	DryRun bool
	// PropagationPolicy of the deletion; empty means the server default.
	PropagationPolicy metav1.DeletionPropagation
}

// ErrTransient marks errors that Step returns when a Kubernetes API call failed
// transiently (timeouts, throttling, unavailable server, connection errors). Nothing was
// recorded as failed: the caller should requeue without counting a retry attempt (the
// Operation reconciler requeues after at most its poll interval, or through the workqueue's
// rate limiter when the status write fails too; docs/architecture/sync-state-machine.md,
// "Retry semantics").
var ErrTransient = errors.New("sync: transient Kubernetes API error")

// ErrInvalidInput is returned for inputs the engine cannot work with.
var ErrInvalidInput = errors.New("sync: invalid input")

// IsTransient reports whether err is a transient API error: server or client timeouts,
// 429, 503, connection refused/reset and similar network errors. Conflicts are retried
// inside the Kube implementation and internal (500) errors, such as failing admission
// webhooks, are treated as task failures like upstream.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTransient) {
		return true
	}
	if apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) {
		return true
	}
	if utilnet.IsConnectionRefused(err) || utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err) {
		return true
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
