// Package appset is the ApplicationSet controller: it generates Applications from the
// generators and template of each ApplicationSet (subpackages generators and template),
// validates them against their AppProject, creates, updates and deletes them according to the
// effective sync policy, maintains the ApplicationSet status (conditions, resources, health,
// progressive sync application status), drives progressive syncs (RollingSync) by creating
// Operation objects step by step, and implements ordered deletion.
//
// Behavior follows Argo CD v3.5.5 (applicationset/controllers, applicationset/progressivesync,
// applicationset/utils, applicationset/status, applicationset/metrics); the implementation is
// independent. Deliberate differences are listed in docs/architecture/components/appset.md:
// RollingSync triggers syncs by creating Operation objects (ADR-0011) instead of setting
// Application.operation, generated Applications are validated against the project's
// destinations and source namespaces, and the ApplicationSet finalizer is
// cd.patrol.example/applicationset-finalizer.
//
// Entry points:
//   - Reconciler (NewReconciler, SetupWithManager) and Run for the controller process;
//   - APIGenerator for the API server's dry-run rendering (POST /api/v1/applicationsets/generate);
//   - Evaluator, the webhook's generator evaluator for Matrix matching;
//   - CheckInvalidGenerators and ValidateForAPI for API admission of ApplicationSets.
package appset
