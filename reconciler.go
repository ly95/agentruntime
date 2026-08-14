package agentruntime

import (
	"context"
	"errors"
	"time"
)

// OperationReconciler settles persisted operation executions without creating
// a model adapter or starting an Agent Run. Hosts use it before credentials,
// attachments, runtime-version checks, and terminal Run handling so an
// unresolved write cannot be stranded by unrelated conversation state.
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
	return &OperationReconciler{runtime: &Runtime{
		operations: operations,
		executions: executions,
		newID:      randomID,
		now:        time.Now,
	}}, nil
}

func (r *OperationReconciler) ReconcileOperation(ctx context.Context, request ReconcileOperationRequest) error {
	if r == nil || r.runtime == nil {
		return ErrExecutionStoreRequired
	}
	return r.runtime.ReconcileOperation(ctx, request)
}
