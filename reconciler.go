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

func NewOperationReconciler(
	operations *OperationRegistry,
	executions ExecutionStore,
) (*OperationReconciler, error) {
	if operations == nil || isNilDependency(executions) {
		return nil, errors.New("agent: operation registry and execution store are required for reconciliation")
	}
	if err := operations.Freeze(); err != nil {
		return nil, err
	}
	return &OperationReconciler{runtime: &Runtime{
		operations:     operations,
		executions:     executions,
		newID:          randomID,
		now:            time.Now,
		cleanupTimeout: defaultDetachedCleanupTimeout,
	}}, nil
}

func (r *OperationReconciler) ReconcileOperation(ctx context.Context, request ReconcileOperationRequest) error {
	if r == nil || r.runtime == nil {
		return ErrExecutionStoreRequired
	}
	return r.runtime.ReconcileOperation(ctx, request)
}
