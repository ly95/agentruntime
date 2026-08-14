package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type operationAttempt struct {
	prepared         preparedOperation
	envelope         operationEnvelope
	attemptID        string
	result           OperationResult
	replayed         bool
	executionStatus  OperationExecutionStatus
	journalExecution OperationExecutionRecord
}

type operationExecutionRef struct {
	executionID string
	attemptID   string
	runID       string
	callID      string
}

func (attempt *operationAttempt) executionRef() operationExecutionRef {
	return operationExecutionRef{
		executionID: attempt.prepared.executionID, attemptID: attempt.attemptID,
		runID: attempt.executionRunID(), callID: attempt.executionCallID(),
	}
}

func (attempt *operationAttempt) executionRunID() string {
	if attempt.journalExecution.RunID != "" {
		return attempt.journalExecution.RunID
	}
	return attempt.envelope.runID
}

func (attempt *operationAttempt) executionCallID() string {
	if attempt.journalExecution.CallID != "" {
		return attempt.journalExecution.CallID
	}
	return attempt.prepared.call.ID
}

func (attempt *operationAttempt) phase() string {
	if !attempt.replayed {
		return "execute"
	}
	if attempt.executionStatus == OperationExecutionExecuted {
		return "journal_replay_pending_verification"
	}
	return "journal_replay"
}

func (r *Runtime) executeOperation(ctx context.Context, run *RunRecord, input Input, state *agentState, operation preparedOperation) (json.RawMessage, error) {
	attempt := &operationAttempt{prepared: operation}
	call := operation.call
	fail := func(cause error) (json.RawMessage, error) {
		if !errors.Is(cause, ErrApprovalPending) {
			r.emit(Event{Type: EventOperationFailed, RunID: run.ID, SessionID: run.SessionID, Operation: call.Name, CallID: call.ID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID, ErrorCode: errorCode(cause), Error: cause.Error()})
		}
		return nil, correlateOperationError(call.ID, operation.executionID, attempt.attemptID, cause)
	}

	if !operation.resumed {
		r.emit(Event{Type: EventOperationRequested, RunID: run.ID, SessionID: run.SessionID, Operation: call.Name, CallID: call.ID, ExecutionID: operation.executionID, Data: operation.callData})
		if err := r.appendItem(ctx, ItemRecord{ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationCall, CallID: call.ID, ExecutionID: operation.executionID, Name: call.Name, Data: operation.callData, CreatedAt: r.now()}); err != nil {
			return fail(err)
		}
	}
	envelope, err := newOperationEnvelope(run, input, operation)
	if err != nil {
		return fail(err)
	}
	attempt.envelope = envelope
	if err := r.authorizeOperation(ctx, run, state, attempt); err != nil {
		if errors.Is(err, ErrApprovalDenied) {
			return r.persistDeniedOperation(ctx, run, operation, err.Error())
		}
		return fail(err)
	}
	if err := r.acquireOperationExecution(ctx, attempt); err != nil {
		return fail(err)
	}
	output, err := r.executeAndValidateOperation(ctx, run, state, attempt)
	if err != nil {
		return fail(err)
	}
	verification, err := r.verifyAndCompleteOperation(ctx, run, state, attempt, output)
	if err != nil {
		return fail(err)
	}
	toolResult, err := marshalOperationToolResult(attempt, verification)
	if err != nil {
		return fail(err)
	}
	return r.persistOperationResult(ctx, run, attempt, toolResult)
}

func (r *Runtime) authorizeOperation(ctx context.Context, run *RunRecord, state *agentState, attempt *operationAttempt) error {
	policyReq, err := attempt.envelope.Request(r.operations, "", state.lease.Fence())
	if err != nil {
		return err
	}
	decision, err := r.policy.Evaluate(ctx, policyReq)
	if err != nil {
		return fmt.Errorf("agent: evaluate operation policy for %q: %w", attempt.prepared.call.Name, err)
	}
	if decision.Action == PolicyRequireApproval && attempt.prepared.batchSize != 1 {
		return fmt.Errorf("%w: an approval-gated operation must be the only operation in a model turn", ErrInvalidModelOutput)
	}
	approvalReq, err := attempt.envelope.Request(r.operations, "", state.lease.Fence())
	if err != nil {
		return err
	}
	return r.enforceDecision(ctx, run, state, approvalReq, decision, attempt.prepared.responseID, attempt.prepared.modelOutput)
}

func (r *Runtime) acquireOperationExecution(ctx context.Context, attempt *operationAttempt) error {
	operation := attempt.prepared
	if operation.operation.Effect != OperationEffectWrite {
		return nil
	}
	attempt.attemptID = r.newID()
	now := r.now()
	acquired, err := r.executions.AcquireExecution(ctx, AcquireExecutionRequest{
		Execution: OperationExecutionRecord{
			ID: operation.executionID, IdempotencyKey: attempt.envelope.input.IdempotencyKey,
			IdempotencyScope: attempt.envelope.input.IdempotencyScope,
			RunID:            attempt.envelope.runID, SessionID: attempt.envelope.sessionID,
			CallID: operation.call.ID, AttemptID: attempt.attemptID, Name: operation.call.Name,
			Arguments: append(json.RawMessage(nil), operation.normalizedArguments...),
			Status:    OperationExecutionStarted, CreatedAt: now, UpdatedAt: now,
		},
		Transition: OperationExecutionTransition{
			ID: r.newID(), ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.envelope.runID, CallID: operation.call.ID,
			Actor: "runtime", Message: "execution acquired", To: OperationExecutionStarted, CreatedAt: now,
		},
	})
	if err != nil {
		return fmt.Errorf("agent: acquire operation execution %q: %w", operation.call.Name, err)
	}
	attempt.journalExecution = acquired.Execution
	attempt.executionStatus = acquired.Execution.Status
	switch acquired.Disposition {
	case ExecutionAcquired:
		if acquired.Execution.Status != OperationExecutionStarted || acquired.Execution.AttemptID != attempt.attemptID {
			return fmt.Errorf("%w: acquired execution %s with attempt %q and status %q", ErrOperationAttemptLost, operation.executionID, acquired.Execution.AttemptID, acquired.Execution.Status)
		}
	case ExecutionReplay:
		attempt.attemptID = acquired.Execution.AttemptID
		if acquired.Execution.Status != OperationExecutionExecuted && acquired.Execution.Status != OperationExecutionCompleted {
			return fmt.Errorf("%w: execution %s replay has status %q", ErrOperationOutcomeUnknown, operation.executionID, acquired.Execution.Status)
		}
		attempt.result = cloneOperationResult(acquired.Execution.Result)
		attempt.replayed = true
	case ExecutionBlocked:
		attempt.attemptID = acquired.Execution.AttemptID
		return fmt.Errorf("%w: operation %q execution %s is %s", ErrOperationOutcomeUnknown, operation.call.Name, operation.executionID, acquired.Execution.Status)
	default:
		return fmt.Errorf("agent: execution store returned invalid acquisition disposition %q", acquired.Disposition)
	}
	return nil
}

func (r *Runtime) executeAndValidateOperation(ctx context.Context, run *RunRecord, state *agentState, attempt *operationAttempt) (any, error) {
	operation := attempt.prepared
	r.emit(Event{Type: EventOperationStarted, RunID: run.ID, SessionID: run.SessionID, Operation: operation.call.Name, CallID: operation.call.ID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID, Text: attempt.phase()})
	if !attempt.replayed {
		lease := state.lease.Fence()
		if operation.operation.Effect == OperationEffectWrite {
			validated, err := state.lease.Validate(ctx)
			if err != nil {
				return nil, r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionRetryable, err)
			}
			lease = validated
			if err := r.executions.ValidateExecutionAttempt(ctx, operation.executionID, attempt.attemptID); err != nil {
				return nil, fmt.Errorf("agent: validate operation execution attempt: %w", err)
			}
		}
		executorReq, err := attempt.envelope.Request(r.operations, attempt.attemptID, lease)
		if err != nil {
			if operation.operation.Effect == OperationEffectWrite {
				err = r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionRetryable, err)
			}
			return nil, err
		}
		attempt.result, err = r.mcp.Execute(ctx, executorReq)
		if err != nil {
			if operation.operation.Effect == OperationEffectWrite && errors.Is(err, ErrOperationNotApplied) {
				return nil, r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionRetryable, err)
			}
			return nil, r.unknownOperationFailure(ctx, attempt, err)
		}
		attempt.result = cloneOperationResult(attempt.result)
	}

	result := attempt.result
	// Keep protocol-invalid writes recoverable: once a result is journaled as
	// completed, replay must treat it as immutable.
	if err := validateOperationResultProtocol(operation.operation, result); err != nil {
		return nil, r.unknownOperationFailure(ctx, attempt, err)
	}
	if len(result.Output) == 0 {
		return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: operation %q returned empty output", operation.call.Name))
	}
	if !json.Valid(result.Output) {
		return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: operation %q returned invalid output JSON", operation.call.Name))
	}
	output, err := r.operations.DecodeOutput(operation.call.Name, result.Output)
	if err != nil {
		return nil, r.unknownOperationFailure(ctx, attempt, err)
	}
	if len(result.Receipt) > 0 && !json.Valid(result.Receipt) {
		return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: operation %q returned invalid receipt JSON", operation.call.Name))
	}
	if err := validateResultArtifacts(result.Artifacts); err != nil {
		return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: operation %q returned invalid artifacts: %w", operation.call.Name, err))
	}
	if operation.operation.Effect == OperationEffectWrite && !attempt.replayed {
		// FALLBACK: justified because if the executed transition fails, the side
		// effect has already returned successfully; preserve that persistence
		// error while fencing the unresolved outcome for reconciliation.
		message := "operation executed"
		if operation.operation.Confirmation.Mode == ConfirmationRequired {
			message = "operation executed; verification pending"
		}
		if _, err := r.executions.TransitionExecution(ctx, OperationExecutionTransition{
			ID: r.newID(), ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.executionRunID(), CallID: attempt.executionCallID(), Actor: "runtime",
			Message: message,
			From:    OperationExecutionStarted, To: OperationExecutionExecuted,
			Result: cloneOperationResult(result), CreatedAt: r.now(),
		}); err != nil {
			cause := fmt.Errorf("agent: persist executed operation %q: %w", operation.call.Name, err)
			return nil, r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionUnknown, cause)
		}
		attempt.executionStatus = OperationExecutionExecuted
	}
	return output, nil
}

func validateOperationResultProtocol(operation Operation, result OperationResult) error {
	if result.Continue {
		if !operation.Terminal {
			return fmt.Errorf("agent: non-terminal operation %q requested terminal continuation", operation.Name)
		}
		if operation.Effect != OperationEffectRead {
			return fmt.Errorf("agent: terminal write operation %q requested continuation", operation.Name)
		}
		if strings.TrimSpace(result.FinalResponse) != "" || len(result.Receipt) != 0 || len(result.Artifacts) != 0 {
			return fmt.Errorf("agent: terminal operation %q continuation returned a final response, receipt, or artifacts", operation.Name)
		}
		return nil
	}
	if operation.Terminal && strings.TrimSpace(result.FinalResponse) == "" {
		return fmt.Errorf("%w: terminal operation %q returned no final response", ErrInvalidModelOutput, operation.Name)
	}
	return nil
}

func (r *Runtime) unknownOperationFailure(ctx context.Context, attempt *operationAttempt, cause error) error {
	if attempt.replayed {
		return cause
	}
	if attempt.prepared.operation.Effect != OperationEffectWrite {
		return cause
	}
	return r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionUnknown, cause)
}

func (r *Runtime) verifyAndCompleteOperation(ctx context.Context, run *RunRecord, state *agentState, attempt *operationAttempt, output any) (*VerificationResult, error) {
	operation := attempt.prepared
	if operation.operation.Confirmation.Mode != ConfirmationRequired {
		if operation.operation.Effect != OperationEffectWrite ||
			(attempt.replayed && attempt.executionStatus == OperationExecutionCompleted) {
			return nil, nil
		}
		if _, err := r.executions.TransitionExecution(ctx, OperationExecutionTransition{
			ID: r.newID(), ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.executionRunID(), CallID: attempt.executionCallID(), Actor: "runtime",
			Message: "direct operation completed",
			From:    OperationExecutionExecuted, To: OperationExecutionCompleted,
			Result: cloneOperationResult(attempt.result), CreatedAt: r.now(),
		}); err != nil {
			return nil, fmt.Errorf("agent: complete direct operation execution %q: %w", operation.call.Name, err)
		}
		return nil, nil
	}
	if operation.operation.Effect == OperationEffectWrite && attempt.replayed && attempt.executionStatus == OperationExecutionCompleted {
		if attempt.journalExecution.Verification != nil {
			verification := cloneVerificationResult(*attempt.journalExecution.Verification)
			return &verification, nil
		}
		return &VerificationResult{Confirmed: true, Message: "execution completed by reconciliation"}, nil
	}
	verificationReq, err := attempt.envelope.Request(r.operations, attempt.attemptID, state.lease.Fence())
	if err != nil {
		return nil, err
	}
	verification, err := r.verifyOperation(ctx, run, verificationReq, cloneOperationResult(attempt.result), output)
	if err != nil {
		return nil, err
	}
	if operation.operation.Effect == OperationEffectWrite {
		if _, err := r.executions.TransitionExecution(ctx, OperationExecutionTransition{
			ID: r.newID(), ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.executionRunID(), CallID: attempt.executionCallID(), Actor: "runtime",
			Message: "operation verification completed",
			From:    OperationExecutionExecuted, To: OperationExecutionCompleted,
			Result: cloneOperationResult(attempt.result), Verification: verificationPointer(verification), CreatedAt: r.now(),
		}); err != nil {
			return nil, fmt.Errorf("agent: complete verified operation execution %q: %w", operation.call.Name, err)
		}
	}
	return verificationPointer(verification), nil
}

func marshalOperationToolResult(attempt *operationAttempt, verification *VerificationResult) (json.RawMessage, error) {
	return json.Marshal(struct {
		Output        json.RawMessage     `json:"output"`
		Receipt       json.RawMessage     `json:"receipt,omitempty"`
		FinalResponse string              `json:"final_response,omitempty"`
		Artifacts     []ResultArtifact    `json:"artifacts,omitempty"`
		Continue      bool                `json:"continue,omitempty"`
		Confirmation  ConfirmationSpec    `json:"confirmation"`
		Verification  *VerificationResult `json:"verification,omitempty"`
	}{
		Output: attempt.result.Output, Receipt: attempt.result.Receipt, FinalResponse: attempt.result.FinalResponse,
		Artifacts: publicResultArtifacts(attempt.result.Artifacts), Continue: attempt.result.Continue,
		Confirmation: attempt.prepared.operation.Confirmation, Verification: verification,
	})
}

func (r *Runtime) persistOperationResult(ctx context.Context, run *RunRecord, attempt *operationAttempt, toolResult json.RawMessage) (json.RawMessage, error) {
	operation := attempt.prepared
	appendErr := r.appendItem(ctx, ItemRecord{
		ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationResult,
		CallID: operation.call.ID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
		Name: operation.call.Name, Data: toolResult, CreatedAt: r.now(),
	})
	if appendErr != nil {
		return nil, correlateOperationError(operation.call.ID, operation.executionID, attempt.attemptID, appendErr)
	}
	if operation.operation.Terminal && !attempt.result.Continue {
		run.Artifacts = append(run.Artifacts, cloneResultArtifacts(attempt.result.Artifacts)...)
	}
	r.emit(Event{Type: EventOperationCompleted, RunID: run.ID, SessionID: run.SessionID, Operation: operation.call.Name, CallID: operation.call.ID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID, Data: toolResult})
	return toolResult, nil
}
