package agentruntime

import (
	"context"
	"errors"
	"time"
)

// OperationReconciler settles persisted operation executions without creating
// a model adapter or starting an Agent Run. Hosts use it before credentials,
// attachments, runtime-version checks, and terminal Run handling so an
// unresolved write cannot be stranded by unrelated conversation state. The
// evidence-bearing abandon action is only for a started attempt whose executor
// is independently proved not to have begun; evidence-bearing completion can
// settle the exact started attempt when the executor is proved to have committed.
type OperationReconciler struct {
	runtime *Runtime
}

// OperationReconcilerConfig configures the model-independent reconciliation
// entry point. Operations and Executions are required. IDFactory, Now, and
// EventSink let hosts use the same identity and diagnostic boundaries as
// Runtime without constructing a model adapter.
type OperationReconcilerConfig struct {
	Operations *OperationRegistry
	Executions ExecutionStore
	IDFactory  IDFactory
	Now        func() time.Time
	EventSink  EventSink
}

func NewOperationReconciler(
	operations *OperationRegistry,
	executions ExecutionStore,
) (*OperationReconciler, error) {
	return NewOperationReconcilerWithConfig(OperationReconcilerConfig{
		Operations: operations,
		Executions: executions,
	})
}

// NewOperationReconcilerWithConfig constructs a reconciler with explicit
// identity, clock, and event dependencies.
func NewOperationReconcilerWithConfig(cfg OperationReconcilerConfig) (*OperationReconciler, error) {
	if cfg.Operations == nil || isNilDependency(cfg.Executions) {
		return nil, errors.New("agent: operation registry and execution store are required for reconciliation")
	}
	if err := cfg.Operations.Freeze(); err != nil {
		return nil, err
	}
	newID := cfg.IDFactory
	if newID == nil {
		newID = randomID
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &OperationReconciler{runtime: &Runtime{
		operations:         cfg.Operations,
		executions:         cfg.Executions,
		eventSink:          NewEventDispatcher(cfg.EventSink).EventSink(),
		newID:              newID,
		now:                now,
		cleanupTimeout:     defaultDetachedCleanupTimeout,
		assignedIdentities: make(map[string]runtimeIdentityClaim),
	}}, nil
}

func (r *OperationReconciler) ReconcileOperation(ctx context.Context, request ReconcileOperationRequest) error {
	if r == nil || r.runtime == nil {
		return ErrExecutionStoreRequired
	}
	return r.runtime.ReconcileOperation(ctx, request)
}
