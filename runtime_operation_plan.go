package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

type preparedOperation struct {
	call                    ToolCall
	callData                json.RawMessage
	operation               Operation
	normalizedArguments     json.RawMessage
	executionID             string
	modelOutput             []ModelOutputItem
	responseID              string
	batchSize               int
	resumed                 bool
	policyDecision          PolicyDecision
	approvalCheckpoint      *ApprovalCheckpoint
	terminalProjection      []TerminalSessionProjection
	terminalProjectionReady bool
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
	boundary := input
	boundary.ImageAttachmentResolver = nil
	if err := validateUTF8Boundary("operation input", boundary); err != nil {
		return Input{}, err
	}
	out := input
	out.Attachments = cloneModelInputAttachments(input.Attachments)
	if input.Metadata == nil {
		return out, nil
	}
	normalized, err := normalizeExactJSONHostValue("operation input metadata", input.Metadata)
	if err != nil {
		return Input{}, err
	}
	metadata, ok := normalized.(map[string]any)
	if !ok {
		return Input{}, fmt.Errorf("agent: operation input metadata encoded as %T instead of a JSON object", normalized)
	}
	out.Metadata = metadata
	return out, nil
}

func clonePersistentOperationInput(input Input) (Input, error) {
	out, err := cloneOperationInput(input)
	if err != nil {
		return Input{}, err
	}
	out.ImageAttachmentResolver = nil
	out.TrustedContext = ""
	for index := range out.Attachments {
		out.Attachments[index].URL = ""
		out.Attachments[index].CurrentRun = false
	}
	return out, nil
}

func persistentOperationInputDigest(input Input) (string, error) {
	trustedContextDigest := ""
	if input.TrustedContext != "" {
		canonicalTrustedContext, err := canonicalJSONIdentity(json.RawMessage(input.TrustedContext))
		if err != nil {
			return "", fmt.Errorf("agent: canonicalize trusted context identity: %w", err)
		}
		digest := sha256.Sum256(canonicalTrustedContext)
		trustedContextDigest = hex.EncodeToString(digest[:])
	}
	persistent, err := clonePersistentOperationInput(input)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		RunID            string                 `json:"run_id"`
		User             string                 `json:"user"`
		SessionID        string                 `json:"session_id,omitempty"`
		IdempotencyKey   string                 `json:"idempotency_key,omitempty"`
		IdempotencyScope string                 `json:"idempotency_scope,omitempty"`
		Attachments      []ModelInputAttachment `json:"attachments,omitempty"`
		Metadata         map[string]any         `json:"metadata,omitempty"`
		TrustedContext   string                 `json:"trusted_context_digest,omitempty"`
	}{
		RunID: persistent.RunID, User: persistent.User, SessionID: persistent.SessionID,
		IdempotencyKey: persistent.IdempotencyKey, IdempotencyScope: persistent.IdempotencyScope,
		Attachments: persistent.Attachments, Metadata: persistent.Metadata,
		TrustedContext: trustedContextDigest,
	})
	if err != nil {
		return "", fmt.Errorf("agent: marshal persistent operation input digest: %w", err)
	}
	canonicalPayload, err := canonicalJSONIdentity(payload)
	if err != nil {
		return "", fmt.Errorf("agent: canonicalize persistent operation input digest: %w", err)
	}
	digest := sha256.Sum256(canonicalPayload)
	return "input_" + hex.EncodeToString(digest[:]), nil
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
	arguments, err = normalizeExactJSONHostValue("normalized operation arguments", arguments)
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
	if err := r.assignOperationExecutionIDs(run, input, state, operations); err != nil {
		return err
	}
	steps := make([]OperationPlanStep, 0, len(operations))
	requestID := operationRequestID(input)
	batchIndex := state.operationBatchCount
	firstWrite := -1
	for i := range operations {
		if operations[i].operation.Effect != OperationEffectWrite {
			continue
		}
		if firstWrite == -1 {
			firstWrite = i
		}
		contractID := operationSummary(operations[i].operation).ContractID
		steps = append(steps, OperationPlanStep{
			ExecutionID: operations[i].executionID,
			Name:        operations[i].call.Name,
			ContractID:  contractID,
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
	expectedBatch := cloneOperationPlanBatch(batch)
	reservation, err := r.executions.ReservePlanBatch(ctx, cloneOperationPlanBatch(batch))
	if err != nil {
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], expectedBatch, fmt.Errorf("agent: reserve operation plan batch %d: %w", batchIndex, validateUTF8Error("execution store", err)))
	}
	reservation = PlanBatchReservation{Batch: cloneOperationPlanBatch(reservation.Batch), Created: reservation.Created}
	if err := validateUTF8Boundary("operation plan reservation", reservation); err != nil {
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], expectedBatch, err)
	}
	if !equalOperationPlanBatch(reservation.Batch, expectedBatch) {
		cause := fmt.Errorf("%w: request %s batch %d", ErrOperationPlanChanged, requestID, batchIndex)
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], expectedBatch, cause)
	}
	data, err := json.Marshal(struct {
		Batch   OperationPlanBatch `json:"batch"`
		Created bool               `json:"created"`
	}{Batch: reservation.Batch, Created: reservation.Created})
	if err != nil {
		return correlateOperationError(state.planCallID, state.planExecutionID, "", err)
	}
	itemID, err := r.nextGeneratedID(ctx, "operation plan item id")
	if err != nil {
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], expectedBatch, err)
	}
	if err := r.appendItem(ctx, ItemRecord{
		ID: itemID, RunID: run.ID, SessionID: input.SessionID,
		Type: ItemTypeOperationPlan, RequestID: requestID, PlanBatch: batchIndex,
		CallID: state.planCallID, ExecutionID: state.planExecutionID, Data: data, CreatedAt: r.now(),
	}); err != nil {
		return r.rejectOperationPlan(ctx, run, operations[firstWrite], expectedBatch, err)
	}
	r.emit(Event{
		Type: EventOperationPlanReserved, RunID: run.ID, SessionID: input.SessionID,
		RequestID: requestID, PlanBatch: batchIndex, CallID: state.planCallID,
		ExecutionID: state.planExecutionID, Text: planReservationText(reservation.Created), Data: data,
	})
	state.operationBatchCount++
	return nil
}

func (r *Runtime) assignOperationExecutionIDs(run *RunRecord, input Input, state *agentState, operations []preparedOperation) error {
	requestID := operationRequestID(input)
	batchIndex := state.operationBatchCount
	writeIndex := uint64(0)
	for index := range operations {
		contractID := operationSummary(operations[index].operation).ContractID
		if operations[index].operation.Effect == OperationEffectWrite {
			executionID, err := operationExecutionID(
				requestID, batchIndex, writeIndex, operations[index].call.Name, contractID, operations[index].normalizedArguments,
			)
			if err != nil {
				return fmt.Errorf("agent: derive operation execution id for %q: %w", operations[index].call.Name, err)
			}
			operations[index].executionID = executionID
			writeIndex++
			continue
		}
		if operations[index].operation.Terminal {
			executionID, err := terminalOperationExecutionID(
				run.ID, operations[index].call.ID, operations[index].call.Name, contractID, operations[index].normalizedArguments,
			)
			if err != nil {
				return fmt.Errorf("agent: derive terminal operation execution id for %q: %w", operations[index].call.Name, err)
			}
			operations[index].executionID = executionID
		}
	}
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
		return r.rejectOperationPlanSeal(ctx, run, state, seal, fmt.Errorf("agent: seal operation plan: %w", validateUTF8Error("execution store", err)))
	}
	if err := validateUTF8Boundary("operation plan seal", result); err != nil {
		return r.rejectOperationPlanSeal(ctx, run, state, seal, err)
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
	itemID, err := r.nextGeneratedID(ctx, "operation plan seal item id")
	if err != nil {
		return r.rejectOperationPlanSeal(ctx, run, state, seal, err)
	}
	if err := r.appendItem(ctx, ItemRecord{
		ID: itemID, RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationPlan,
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
	cause = validateUTF8Error("execution store", cause)
	data, marshalErr := json.Marshal(batch)
	if marshalErr != nil {
		cause = errors.Join(cause, marshalErr)
	}
	itemID, idErr := r.nextGeneratedID(ctx, "rejected operation plan item id")
	if idErr != nil {
		cause = errors.Join(cause, idErr)
	} else {
		if err := r.appendItem(ctx, ItemRecord{
			ID: itemID, RunID: run.ID, SessionID: batch.SessionID, Type: ItemTypeOperationPlan,
			RequestID: batch.RequestID, PlanBatch: batch.Index, CallID: operation.call.ID,
			ExecutionID: operation.executionID, Data: data, Error: cause.Error(), CreatedAt: r.now(),
		}); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	r.emit(Event{
		Type: EventOperationPlanRejected, RunID: run.ID, SessionID: batch.SessionID,
		RequestID: batch.RequestID, PlanBatch: batch.Index, CallID: operation.call.ID,
		ExecutionID: operation.executionID, Data: data, ErrorCode: errorCode(cause), Error: cause.Error(),
	})
	return correlateOperationError(operation.call.ID, operation.executionID, "", cause)
}

func (r *Runtime) rejectOperationPlanSeal(ctx context.Context, run *RunRecord, state *agentState, seal OperationPlanSeal, cause error) error {
	cause = validateUTF8Error("execution store", cause)
	data, marshalErr := json.Marshal(seal)
	if marshalErr != nil {
		cause = errors.Join(cause, marshalErr)
	}
	itemID, idErr := r.nextGeneratedID(ctx, "rejected operation plan seal item id")
	if idErr != nil {
		cause = errors.Join(cause, idErr)
	} else {
		if err := r.appendItem(ctx, ItemRecord{
			ID: itemID, RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationPlan,
			RequestID: seal.RequestID, PlanBatch: seal.BatchCount, CallID: state.planCallID,
			ExecutionID: state.planExecutionID, Data: data, Error: cause.Error(), CreatedAt: r.now(),
		}); err != nil {
			cause = errors.Join(cause, err)
		}
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
		if left.Steps[i].ExecutionID != right.Steps[i].ExecutionID || left.Steps[i].Name != right.Steps[i].Name || left.Steps[i].ContractID != right.Steps[i].ContractID || !jsonSemanticallyEqual(left.Steps[i].Arguments, right.Steps[i].Arguments) {
			return false
		}
	}
	return true
}

func cloneOperationPlanBatch(batch OperationPlanBatch) OperationPlanBatch {
	out := batch
	out.Steps = append([]OperationPlanStep(nil), batch.Steps...)
	for index := range out.Steps {
		out.Steps[index].Arguments = append(json.RawMessage(nil), batch.Steps[index].Arguments...)
	}
	return out
}

func equalOperationPlanSeal(left, right OperationPlanSeal) bool {
	return left.RequestID == right.RequestID &&
		left.SessionID == right.SessionID &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.IdempotencyScope == right.IdempotencyScope &&
		left.BatchCount == right.BatchCount
}
