package sharding

import (
	"context"
	"errors"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// DestinationResolver resolves an Application destination to a server URL
// (*clusters.Registry implements it).
type DestinationResolver interface {
	ResolveDestination(dest v1alpha1.ApplicationDestination) (string, error)
}

// ShardOwner reports shard ownership (*LeaseManager implements it).
type ShardOwner interface {
	OwnsShard(shard int) bool
}

// ApplicationGetter returns an Application, with a NotFound API error when it does not exist
// (a controller-runtime cache reader is the usual source).
type ApplicationGetter func(ctx context.Context, namespace, name string) (*v1alpha1.Application, error)

// ClientApplicationGetter adapts a controller-runtime reader.
func ClientApplicationGetter(r client.Reader) ApplicationGetter {
	return func(ctx context.Context, namespace, name string) (*v1alpha1.Application, error) {
		var app v1alpha1.Application
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &app); err != nil {
			return nil, err
		}
		return &app, nil
	}
}

// Ownership decides which Applications, Operations and clusters this replica processes: the
// destination cluster's shard (Assigner) must be owned (ShardOwner).
type Ownership struct {
	assigner *Assigner
	owner    ShardOwner
	dest     DestinationResolver
	logger   *slog.Logger
}

// NewOwnership returns the ownership view.
func NewOwnership(assigner *Assigner, owner ShardOwner, dest DestinationResolver, logger *slog.Logger) (*Ownership, error) {
	if assigner == nil || owner == nil || dest == nil {
		return nil, errors.New("sharding: assigner, shard owner and destination resolver are required")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Ownership{assigner: assigner, owner: owner, dest: dest, logger: logger}, nil
}

// ShardForApplication returns the shard processing app. managed is false when the
// destination cluster has the skip-reconcile annotation (no shard processes it). A
// destination that does not resolve, or a server without a registered cluster, maps to shard
// 0 so that exactly one replica reports the error (upstream lets every shard process such
// Applications).
func (o *Ownership) ShardForApplication(app *v1alpha1.Application) (shard int, managed bool) {
	server, err := o.dest.ResolveDestination(app.Spec.Destination)
	if err != nil {
		return 0, true
	}
	return o.assigner.ShardForServer(server)
}

// OwnsApplication reports whether this replica processes app.
func (o *Ownership) OwnsApplication(app *v1alpha1.Application) bool {
	shard, managed := o.ShardForApplication(app)
	return managed && o.owner.OwnsShard(shard)
}

// OwnsCluster reports whether this replica processes the cluster serving server (and so
// keeps a live-state cache for it).
func (o *Ownership) OwnsCluster(server string) bool {
	shard, managed := o.assigner.ShardForServer(server)
	return managed && o.owner.OwnsShard(shard)
}

// OwnsClusterObject is OwnsCluster for a Cluster object, with the signature of the
// controller's cluster filter (core.Deps.OwnsCluster). A nil cluster stands for a destination
// that does not resolve and is served by shard 0.
func (o *Ownership) OwnsClusterObject(c *v1alpha1.Cluster) bool {
	if c == nil {
		return o.owner.OwnsShard(0)
	}
	return o.OwnsCluster(c.Spec.Server)
}

// OwnsOperation reports whether this replica executes op: the owner of its Application. An
// Operation whose Application does not exist belongs to shard 0 (which rejects it); when the
// Application cannot be read the answer is true, and the reconciler decides after reading it.
func (o *Ownership) OwnsOperation(ctx context.Context, op *v1alpha1.Operation, get ApplicationGetter) bool {
	app, err := get(ctx, op.Namespace, op.Spec.Application)
	switch {
	case apierrors.IsNotFound(err):
		return o.owner.OwnsShard(0)
	case err != nil:
		o.logger.Debug("cannot read the Application of an Operation; letting the event through",
			slog.String("namespace", op.Namespace), slog.String("operation", op.Name), slog.Any("error", err))
		return true
	}
	return o.OwnsApplication(app)
}

// ApplicationPredicate filters Application events to the ones this replica owns. Updates pass
// when either version is owned, so a replica sees an Application leave its shard. Ownership
// can change after the event: reconcilers must check OwnsApplication again.
func (o *Ownership) ApplicationPredicate() predicate.Predicate {
	owns := func(obj client.Object) bool {
		app, ok := obj.(*v1alpha1.Application)
		return ok && o.OwnsApplication(app)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return owns(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return owns(e.ObjectOld) || owns(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return owns(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return owns(e.Object) },
	}
}

// operationLookupTimeout bounds the Application lookup of OperationPredicate.
const operationLookupTimeout = 5 * time.Second

// OperationPredicate filters Operation events to the ones whose Application this replica
// owns (see OwnsOperation).
func (o *Ownership) OperationPredicate(get ApplicationGetter) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		op, ok := obj.(*v1alpha1.Operation)
		if !ok {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), operationLookupTimeout)
		defer cancel()
		return o.OwnsOperation(ctx, op, get)
	})
}
