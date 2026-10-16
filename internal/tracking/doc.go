// Package tracking implements Patrol CD's resource tracking: how rendered resources are
// marked with the Application that manages them and how that mark is read back from live
// objects. It re-implements the behavior of Argo CD v3.5.5 (util/argo/resource_tracking.go
// and the self-reference / shared-resource checks of controller/state.go) with Patrol keys
// from api/v1alpha1.
//
// # Methods
//
// Three tracking methods exist ([Method], the application.resourceTrackingMethod setting):
//
//   - annotation (default): the annotation cd.patrol.example/tracking-id holds
//     "<instance>:<group>/<kind>:<namespace>/<name>" and is authoritative for ownership.
//     The annotation cd.patrol.example/installation-id is written when an installation ID is
//     configured and removed otherwise.
//   - label: the instance label (application.instanceLabelKey, default
//     app.kubernetes.io/instance) holds the instance name, untruncated.
//   - annotation+label: both; the label is informational and truncated to 63 characters
//     with trailing non-alphanumeric characters stripped.
//
// The empty method and unknown values behave like annotation, as Argo CD's settings default
// does. Callers should validate the setting with [ValidateMethod].
//
// # Instance names
//
// The value that identifies an Application in tracking metadata is its instance name
// ([InstanceName]): the Application name when it lives in the control-plane namespace, and
// "<namespace>_<name>" otherwise (apps in any namespace).
//
// # Installation ID
//
// With annotation-based methods a resource is attributed to an Application only when its
// installation-id annotation equals the configured installation ID exactly (both empty also
// matches), so several installations sharing a cluster never adopt each other's resources,
// even with identically named Applications. Label tracking ignores the installation ID.
//
// # Self-reference
//
// A tracking id copied onto another object (for example by a tool replicating Secrets into
// other namespaces) does not make that object part of the Application: with annotation-based
// methods an object is self-referenced only if the group, kind, name and namespace in its
// tracking id match the object ([ResourceTracker.IsSelfReferencedObject]).
//
// # Managed roots
//
// [ResourceTracker.IsManagedBy] is the tracking-side precondition for pruning: the object is
// tracked by the Application, has no ownerReferences (owned children inheriting tracking
// metadata, such as Pods with a pod-template instance label, are excluded) and is
// self-referenced. It reads only the object's own metadata, which anyone who can write the
// object controls, so it is not an authorization check: AppProject restrictions must still be
// enforced before deleting.
//
// The package has no I/O and no global state; a [ResourceTracker] is immutable and safe for
// concurrent use.
package tracking
