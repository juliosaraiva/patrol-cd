// Package settings is the single settings model shared by every Patrol CD component.
//
// It reads the settings ConfigMaps and Secrets of the control-plane namespace, whose key
// names follow Argo CD v3.5.5 (ADR-0003):
//
//	patrol-cm                    general settings (Argo CD argocd-cm)
//	patrol-rbac-cm               RBAC policy (argocd-rbac-cm)
//	patrol-cmd-params-cm         component parameters (argocd-cmd-params-cm)
//	patrol-tls-certs-cm          repository server TLS certificates (argocd-tls-certs-cm)
//	patrol-ssh-known-hosts-cm    SSH known hosts (argocd-ssh-known-hosts-cm)
//	patrol-gpg-keys-cm           GnuPG public keys (argocd-gpg-keys-cm)
//	patrol-notifications-cm      notifications catalog (argocd-notifications-cm)
//	patrol-secret                server key, accounts, webhook secrets (argocd-secret)
//	patrol-notifications-secret  notification service credentials (only referenced)
//
// The object names are configurable ([Names]). [Parse] turns the raw objects ([Input]) into
// an immutable [Settings] snapshot and a list of [Issue] values; it never fails. A key whose
// value cannot be parsed is reported and replaced by the value the previous snapshot accepted
// for that key (or by the default when there is none), so one bad key never disables the
// rest of the configuration. Likewise a deleted patrol-cm, patrol-rbac-cm or patrol-secret
// (or an unlabelled patrol-cm) keeps the data last loaded from it in force, with local
// accounts disabled meanwhile ([Settings.SourceState]). The exceptions, chosen to fail
// closed, are documented on [Parse].
//
// [Manager] keeps the current snapshot up to date from informers, debounces reloads,
// notifies subscribers, emits Kubernetes Events for invalid settings and reports metrics.
// [ParamsForComponent] and [ResolveParams] expose patrol-cmd-params-cm as typed component
// parameters with the same environment-variable semantics as upstream. [EnsureServerSecretKey]
// bootstraps the session signing key.
//
// Nothing in this package logs or formats secret values: secret material is held in
// [Sensitive] values, issue messages name keys only, and Events never carry values.
package settings
