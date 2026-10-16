package core

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// Event reasons recorded on Applications and Operations (APP-141, upstream argo.EventReason*).
const (
	EventReasonResourceUpdated    = "ResourceUpdated"
	EventReasonOperationStarted   = "OperationStarted"
	EventReasonOperationCompleted = "OperationCompleted"
	EventReasonStatusRefreshed    = "StatusRefreshed"
	EventReasonOperationRejected  = "OperationRejected"
	EventReasonOperationAdmitted  = "OperationAdmitted"
	EventReasonDeletionFailed     = "DeletionFailed"
	// EventReasonDeleteHookSkipped: a PreDelete/PostDelete hook was not created because an
	// object the Application does not track has its name (Patrol addition; upstream logs).
	EventReasonDeleteHookSkipped = "DeleteHookSkipped"
)

// Event actions (events.k8s.io/v1 action field).
const (
	ActionReconcile = "Reconcile"
	ActionSync      = "Sync"
	ActionDelete    = "Delete"
)

// EventRecorder is the subset of client-go's events.EventRecorder the controllers use; the
// controller-runtime manager's GetEventRecorder satisfies it.
type EventRecorder interface {
	Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...any)
}

// NopRecorder discards events.
type NopRecorder struct{}

// Eventf implements EventRecorder.
func (NopRecorder) Eventf(runtime.Object, runtime.Object, string, string, string, string, ...any) {}
