// Package notifications implements the Patrol CD notifications engine (ADR-0012): it parses
// the Argo CD compatible notifications configuration (service.*, template.*, trigger.*,
// defaultTriggers[.*], subscriptions, context and $secret references), evaluates triggers
// with expr, renders templates with text/template + sprig, resolves subscriptions from
// annotations and default subscriptions, deduplicates deliveries through a delivery-state
// annotation, and sends notifications through the services in the services subpackage.
//
// The engine operates on applications as map[string]any (the unstructured JSON form of the
// Application resource) so it is independent of the Go CRD types: callers convert their
// typed objects with runtime.DefaultUnstructuredConverter or encoding/json.
//
// Typical controller usage:
//
//	cfg, err := notifications.ParseConfig(cm.Data, secret.Data, notifications.WithDefaultCatalog())
//	eng, err := notifications.NewEngine(cfg, notifications.WithProjectGetter(projects), ...)
//	if ok, _ := notifications.SyncStatusRefreshed(app); !ok { return }
//	state := notifications.DeliveryStateFromAnnotations(annotations, eng.AnnotationPrefix())
//	newState, results, err := eng.Process(ctx, app, state)
//	// persist newState with ApplyDeliveryState and emit Events for failed results.
package notifications
