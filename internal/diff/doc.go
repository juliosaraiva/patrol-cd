// Package diff compares desired (config) Kubernetes objects with their live counterparts
// and decides whether they are in sync.
//
// It implements the three strategies of the Argo CD v3.5.5 baseline with the same
// observable semantics:
//
//   - Legacy diff (default): a strategic-merge three-way diff that uses the
//     kubectl.kubernetes.io/last-applied-configuration annotation of the live object as the
//     "original" state, so fields the user never set (defaults, controller-populated fields)
//     do not count as drift. Without the annotation the config itself is used as the
//     original (two-way diff).
//   - Structured-merge diff: when the application syncs with server-side apply, the
//     predicted live state is computed locally with sigs.k8s.io/structured-merge-diff,
//     honoring managedFields ownership of the configured field manager.
//   - Server-side diff: the predicted live state is obtained from a server-side apply dry
//     run performed by a caller-supplied DryRunner; mutating-webhook changes are removed
//     unless IncludeMutationWebhook is requested.
//
// Normalization (ignore rules with JSON pointers, jq path expressions and managedFields
// managers; known Kubernetes types embedded in CRDs; status and aggregated ClusterRole
// handling) and Secret masking are provided as reusable building blocks.
//
// The package deliberately does not import the Patrol CRD types. Callers translate
// Application/settings structures into the neutral types declared here (IgnoreRule,
// KnownTypeField, CompareOptions); see docs/architecture/components/diff.md.
package diff
