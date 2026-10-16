// Package controller is the Patrol CD notifications controller (parity area 09): a
// controller-runtime reconciler that watches Applications (control-plane namespace, plus the
// application namespaces when apps-in-any-namespace is enabled) and AppProjects, evaluates the
// notifications configuration through the engine in internal/notifications, delivers
// notifications and records the delivery state in the notified.notifications.cd.patrol.example
// annotation with an optimistic-concurrency merge patch.
//
// Applications are handed to the engine in the Argo CD compatible map form (TemplateApplication):
// the stored JSON plus the parts Patrol keeps elsewhere (per-resource sync results and the
// requested operation live in the Operation named by status.operationState.operationName), so
// upstream catalog triggers and templates work unchanged.
//
// Configuration comes from patrol-notifications-cm / patrol-notifications-secret in the
// control-plane namespace and, with self-service enabled, from objects with the same names in
// each application's namespace. A namespace configuration only ever sees applications of its own
// namespace, resolves $secret references only against the Secret of its own namespace, never
// gets the .secrets template variable and delivers through its own SSRF-guarded egress policy
// (Config.TenantEgressAllowedPrefixes), never the control plane's.
//
// Failed deliveries are logged, counted in patrol_notifications_deliveries_total{succeeded="false"},
// reported as Warning Events on the Application and retried with backoff while their condition
// holds. An Event about an Application outside the failing configuration's namespace names only
// the trigger, service and failure class (no endpoint URL or response body). Like upstream there is no start-up suppression: conditions already true when the
// controller first sees an Application are notified unless the delivery state records them.
//
// Run starts the controller for a command; leader election is configured by the caller through
// Config. NewDryRun backs the "patrol admin notifications" CLI (trigger run, template notify and
// a dry evaluation of what the controller would send).
package controller
