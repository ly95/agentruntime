package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type preparedOperation struct {
	call                ToolCall
	callData            json.RawMessage
	operation           Operation
	normalizedArguments json.RawMessage
	executionID         string
	modelOutput         []ModelOutputItem
	responseID          string
	batchSize           int
	resumed             bool
}

type operationEnvelope struct {
	runID               string
	sessionID           string
	executionID         string
	input               Input
	operation           Operation
	call                ToolCall
	normalizedArguments json.RawMessage
}

func newOperationEnvelope(run *RunRecord, input Input, operation preparedOperation) (operationEnvelope, error) {
	clonedInput, err := cloneOperationInput(input)
	if err != nil {
		return operationEnvelope{}, err
	}
	call := operation.call
	call.Input = append(json.RawMessage(nil), operation.call.Input...)
	return operationEnvelope{
		runID: run.ID, sessionID: run.SessionID, executionID: operation.executionID,
		input: clonedInput, operation: cloneOperation(operation.operation), call: call,
		normalizedArguments: append(json.RawMessage(nil), operation.normalizedArguments...),
	}, nil
}

func (envelope operationEnvelope) Request(registry *OperationRegistry, attemptID string, lease SessionLeaseFence) (OperationRequest, error) {
	input, err := cloneOperationInput(envelope.input)
	if err != nil {
		return OperationRequest{}, err
	}
	arguments, err := registry.DecodeInput(envelope.call.Name, envelope.normalizedArguments)
	if err != nil {
		return OperationRequest{}, err
	}
	call := envelope.call
	call.Input = append(json.RawMessage(nil), envelope.call.Input...)
	return OperationRequest{
		RunID: envelope.runID, SessionID: envelope.sessionID, ExecutionID: envelope.executionID, AttemptID: attemptID,
		SessionLease: lease, Input: input, Operation: operationSummary(envelope.operation),
		Call: call, Arguments: arguments,
	}, nil
}

func cloneOperationInput(input Input) (Input, error) {
	out := input
	out.Attachments = cloneModelInputAttachments(input.Attachments)
	if input.Metadata == nil {
		return out, nil
	}
	data, err := json.Marshal(input.Metadata)
	if err != nil {
		return Input{}, fmt.Errorf("agent: clone operation input metadata: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	out.Metadata = nil
	if err := decoder.Decode(&out.Metadata); err != nil {
		return Input{}, fmt.Errorf("agent: clone operation input metadata: %w", err)
	}
	return out, nil
}

func sessionLeaseFence(handle RunHandle) SessionLeaseFence {
	if handle.SessionID == "" {
		return SessionLeaseFence{}
	}
	return SessionLeaseFence{
		RunID: handle.RunID, SessionID: handle.SessionID, LeaseID: handle.LeaseID,
		Generation: handle.LeaseGeneration, Deadline: handle.LeaseDeadline,
		SessionRevision: handle.SessionRevision,
	}
}

func (r *Runtime) prepareOperation(input Input, call ToolCall) (preparedOperation, error) {
	callData, err := json.Marshal(call)
	if err != nil {
		return preparedOperation{}, err
	}
	op, ok := r.operations.Get(call.Name)
	if !ok {
		return preparedOperation{}, fmt.Errorf("%w: %s", ErrOperationNotFound, call.Name)
	}
	arguments, err := r.operations.DecodeInput(call.Name, call.Input)
	if err != nil {
		return preparedOperation{}, err
	}
	arguments, err = r.operations.NormalizeInput(call.Name, arguments)
	if err != nil {
		return preparedOperation{}, err
	}
	normalizedArguments, err := json.Marshal(arguments)
	if err != nil {
		return preparedOperation{}, fmt.Errorf("agent: marshal normalized operation arguments: %w", err)
	}
	if op.Effect == OperationEffectWrite && input.IdempotencyKey == "" {
		return preparedOperation{}, fmt.Errorf("%w: operation %q", ErrIdempotencyKeyRequired, call.Name)
	}
	if op.Effect == OperationEffectWrite && input.SessionID == "" && input.IdempotencyScope == "" {
		return preparedOperation{}, fmt.Errorf("%w: operation %q", ErrIdempotencyScopeRequired, call.Name)
	}
	return preparedOperation{
		call: call, callData: callData, operation: op,
		normalizedArguments: normalizedArguments,
	}, nil
}

func (r *Runtime) failOperationPreparation(run *RunRecord, call ToolCall, cause error) error {
	callData, marshalErr := json.Marshal(call)
	if marshalErr != nil {
		cause = errors.Join(cause, marshalErr)
	}
	r.emit(Event{Type: EventOperationRequested, RunID: run.ID, SessionID: run.SessionID, Operation: call.Name, CallID: call.ID, Data: callData})
	r.emit(Event{Type: EventOperationFailed, RunID: run.ID, SessionID: run.SessionID, Operation: call.Name, CallID: call.ID, ErrorCode: errorCode(cause), Error: cause.Error()})
	return correlateOperationError(call.ID, "", "", cause)
}

func (r *Runtime) reserveOperationPlan(ctx context.Context, run *RunRecord, input Input, state *agentState, operations []preparedOperation) error {
	steps := make([]OperationPlanStep, 0, len(operations))
	requestID := operationRequestID(input)
	batchIndex := state.operationBatchCount
	firstWrite := -1
	for i := range operations {
		if operations[i].operation.Effect != OperationEffectWrite {
			if operations[i].operation.Terminal {
				operations[i].executionID = terminalOperationExecutionID(
					run.ID,
					operations[i].call.ID,
					operations[i].call.Name,
					operations[i].normalizedArguments,
				)
			}
			continue
		}
		if firstWrite == -1 {
			firstWrite = i
		}
		stepIndex := uint64(len(steps))
		operations[i].executionID = operationExecutionID(requestID, batchIndex, stepIndex, operations[i].call.Name, operations[i].normalizedArguments)
		steps = append(steps, OperationPlanStep{
			ExecutionID: operations[i].executionID,
			Name:        operations[i].call.Name,
			Arguments:   append(json.RawMessage(nil), operations[i].normalizedArguments...),
		})
	}
	if len(steps) == 0 {
		return nil
	}
	state.planCallID = operations[firstWrite].call.ID
	state.planExecutionID = operations[firstWrite].executionID
	batch := OperationPlanBatch{
		RequestID: requestID, SessionID: input.SessionID,
		IdempotencyKey: input.IdempotencyKey, IdempotencyScope: input.IdempotencyScope,
		Index: batchIndex, Steps: steps, CreatedAt: r.now(),
	}
	reservation, err := r.executions.ReservePlanBatch(ctx, batch)
	if err != nil {
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], batch, fmt.Errorf("agent: reserve operation plan batch %d: %w", batchIndex, err))
	}
	if !equalOperationPlanBatch(reservation.Batch, batch) {
		cause := fmt.Errorf("%w: request %s batch %d", ErrOperationPlanChanged, requestID, batchIndex)
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], batch, cause)
	}
	data, err := json.Marshal(struct {
		Batch   OperationPlanBatch `json:"batch"`
		Created bool               `json:"created"`
	}{Batch: reservation.Batch, Created: reservation.Created})
	if err != nil {
		return correlateOperationError(state.planCallID, state.planExecutionID, "", err)
	}
	if err := r.appendItem(ctx, ItemRecord{
		ID: r.newID(), RunID: run.ID, SessionID: input.SessionID,
		Type: ItemTypeOperationPlan, RequestID: requestID, PlanBatch: batchIndex,
		CallID: state.planCallID, ExecutionID: state.planExecutionID, Data: data, CreatedAt: r.now(),
	}); err != nil {
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], batch, err)
	}
	r.emit(Event{
		Type: EventOperationPlanReserved, RunID: run.ID, SessionID: input.SessionID,
		RequestID: requestID, PlanBatch: batchIndex, CallID: state.planCallID,
		ExecutionID: state.planExecutionID, Text: planReservationText(reservation.Created), Data: data,
	})
	state.operationBatchCount++
	return nil
}

func (r *Runtime) sealOperationPlan(ctx context.Context, run *RunRecord, input Input, state *agentState) error {
	if r.executions == nil || input.IdempotencyKey == "" {
		return nil
	}
	seal := OperationPlanSeal{
		RequestID: operationRequestID(input), SessionID: input.SessionID,
		IdempotencyKey: input.IdempotencyKey, IdempotencyScope: input.IdempotencyScope,
		BatchCount: state.operationBatchCount, SealedAt: r.now(),
	}
	result, err := r.executions.SealPlan(ctx, seal)
	if err != nil {
		return r.rejectOperationPlanSeal(ctx, run, state, seal, fmt.Errorf("agent: seal operation plan: %w", err))
	}
	if !equalOperationPlanSeal(result.Seal, seal) {
		cause := fmt.Errorf("%w: request %s seal", ErrOperationPlanChanged, seal.RequestID)
		return r.rejectOperationPlanSeal(ctx, run, state, seal, cause)
	}
	data, err := json.Marshal(struct {
		Seal    OperationPlanSeal `json:"seal"`
		Created bool              `json:"created"`
	}{Seal: result.Seal, Created: result.Created})
	if err != nil {
		return correlateOperationError(state.planCallID, state.planExecutionID, "", err)
	}
	if err := r.appendItem(ctx, ItemRecord{
		ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationPlan,
		RequestID: seal.RequestID, PlanBatch: seal.BatchCount, CallID: state.planCallID,
		ExecutionID: state.planExecutionID, Data: data, CreatedAt: r.now(),
	}); err != nil {
		return correlateOperationError(state.planCallID, state.planExecutionID, "", err)
	}
	r.emit(Event{
		Type: EventOperationPlanSealed, RunID: run.ID, SessionID: run.SessionID,
		RequestID: seal.RequestID, PlanBatch: seal.BatchCount, CallID: state.planCallID,
		ExecutionID: state.planExecutionID, Text: planReservationText(result.Created), Data: data,
	})
	return nil
}

func (r *Runtime) rejectOperationPlan(ctx context.Context, run *RunRecord, operation preparedOperation, batch OperationPlanBatch, cause error) error {
	data, marshalErr := json.Marshal(batch)
	if marshalErr != nil {
		cause = errors.Join(cause, marshalErr)
	}
	if err := r.appendItem(ctx, ItemRecord{
		ID: r.newID(), RunID: run.ID, SessionID: batch.SessionID, Type: ItemTypeOperationPlan,
		RequestID: batch.RequestID, PlanBatch: batch.Index, CallID: operation.call.ID,
		ExecutionID: operation.executionID, Data: data, Error: cause.Error(), CreatedAt: r.now(),
	}); err != nil {
		cause = errors.Join(cause, err)
	}
	r.emit(Event{
		Type: EventOperationPlanRejected, RunID: run.ID, SessionID: batch.SessionID,
		RequestID: batch.RequestID, PlanBatch: batch.Index, CallID: operation.call.ID,
		ExecutionID: operation.executionID, Data: data, ErrorCode: errorCode(cause), Error: cause.Error(),
	})
	return correlateOperationError(operation.call.ID, operation.executionID, "", cause)
}

func (r *Runtime) rejectOperationPlanSeal(ctx context.Context, run *RunRecord, state *agentState, seal OperationPlanSeal, cause error) error {
	data, marshalErr := json.Marshal(seal)
	if marshalErr != nil {
		cause = errors.Join(cause, marshalErr)
	}
	if err := r.appendItem(ctx, ItemRecord{
		ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationPlan,
		RequestID: seal.RequestID, PlanBatch: seal.BatchCount, CallID: state.planCallID,
		ExecutionID: state.planExecutionID, Data: data, Error: cause.Error(), CreatedAt: r.now(),
	}); err != nil {
		cause = errors.Join(cause, err)
	}
	r.emit(Event{
		Type: EventOperationPlanRejected, RunID: run.ID, SessionID: run.SessionID,
		RequestID: seal.RequestID, PlanBatch: seal.BatchCount, CallID: state.planCallID,
		ExecutionID: state.planExecutionID, Data: data, ErrorCode: errorCode(cause), Error: cause.Error(),
	})
	return correlateOperationError(state.planCallID, state.planExecutionID, "", cause)
}

func planReservationText(created bool) string {
	if created {
		return "created"
	}
	return "reused"
}

func equalOperationPlanBatch(left, right OperationPlanBatch) bool {
	if left.RequestID != right.RequestID || left.SessionID != right.SessionID || left.IdempotencyKey != right.IdempotencyKey || left.IdempotencyScope != right.IdempotencyScope || left.Index != right.Index || len(left.Steps) != len(right.Steps) {
		return false
	}
	for i := range left.Steps {
		if left.Steps[i].ExecutionID != right.Steps[i].ExecutionID || left.Steps[i].Name != right.Steps[i].Name || !jsonSemanticallyEqual(left.Steps[i].Arguments, right.Steps[i].Arguments) {
			return false
		}
	}
	return true
}

func equalOperationPlanSeal(left, right OperationPlanSeal) bool {
	return left.RequestID == right.RequestID &&
		left.SessionID == right.SessionID &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.IdempotencyScope == right.IdempotencyScope &&
		left.BatchCount == right.BatchCount
}
