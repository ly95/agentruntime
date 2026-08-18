package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

type operationAttempt struct {
	prepared           preparedOperation
	envelope           operationEnvelope
	attemptID          string
	result             OperationResult
	expectedProjection []TerminalSessionProjection
	replayed           bool
	executionStatus    OperationExecutionStatus
	journalExecution   OperationExecutionRecord
}

type operationExecutionRef struct {
	executionID          string
	attemptID            string
	runID                string
	callID               string
	verificationRequired bool
	record               OperationExecutionRecord
}

func (attempt *operationAttempt) executionRef() operationExecutionRef {
	return operationExecutionRef{
		executionID: attempt.prepared.executionID, attemptID: attempt.attemptID,
		runID: attempt.executionRunID(), callID: attempt.executionCallID(),
		verificationRequired: attempt.prepared.operation.Confirmation.Mode == ConfirmationRequired,
		record:               detachedOperationExecutionRecord(attempt.journalExecution),
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
		cause = validateUTF8Error("runtime dependency", cause)
		if !errors.Is(cause, ErrApprovalPending) {
			r.emit(Event{Type: EventOperationFailed, RunID: run.ID, SessionID: run.SessionID, Operation: call.Name, CallID: call.ID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID, ErrorCode: errorCode(cause), Error: cause.Error()})
		}
		return nil, correlateOperationError(call.ID, operation.executionID, attempt.attemptID, cause)
	}

	if !operation.resumed {
		itemID, err := r.nextGeneratedID(ctx, "operation call item id")
		if err != nil {
			return fail(err)
		}
		r.emit(Event{Type: EventOperationRequested, RunID: run.ID, SessionID: run.SessionID, Operation: call.Name, CallID: call.ID, ExecutionID: operation.executionID, Data: operation.callData})
		if err := r.appendItem(ctx, ItemRecord{ID: itemID, RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationCall, CallID: call.ID, ExecutionID: operation.executionID, Name: call.Name, Data: operation.callData, CreatedAt: r.now()}); err != nil {
			return fail(err)
		}
	}
	envelope, err := newOperationEnvelope(run, input, operation)
	if err != nil {
		return fail(err)
	}
	attempt.envelope = envelope
	if err := r.enforceOperationDecision(ctx, run, state, attempt); err != nil {
		if errors.Is(err, ErrApprovalDenied) {
			return r.persistDeniedOperation(ctx, run, operation, err.Error())
		}
		return fail(err)
	}
	if err := r.preflightTerminalWriteProjection(run, attempt); err != nil {
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

func (r *Runtime) preflightTerminalWriteProjection(run *RunRecord, attempt *operationAttempt) error {
	operation := attempt.prepared
	if !operation.operation.Terminal || operation.operation.Effect != OperationEffectWrite {
		return nil
	}
	if err := validateTerminalWriteFunctionCall(operation); err != nil {
		return err
	}
	projections := cloneTerminalSessionProjections(operation.terminalProjection)
	if !operation.terminalProjectionReady {
		arguments, err := r.operations.DecodeInput(operation.call.Name, operation.normalizedArguments)
		if err != nil {
			return err
		}
		projections, err = r.operations.BuildTerminalSessionProjection(operation.call.Name, arguments)
		if err != nil {
			return err
		}
	}
	prospective := append(terminalArtifactProjections(run.Artifacts), projections...)
	if err := validateTerminalSessionProjections(prospective); err != nil {
		return fmt.Errorf("agent: terminal operation %q accumulated session projection: %w", operation.call.Name, err)
	}
	attempt.expectedProjection = cloneTerminalSessionProjections(projections)
	return nil
}

func (r *Runtime) preflightTerminalWriteBatch(run *RunRecord, operations []preparedOperation) error {
	prospective := terminalArtifactProjections(run.Artifacts)
	for index := range operations {
		operation := operations[index]
		if !operation.operation.Terminal || operation.operation.Effect != OperationEffectWrite {
			continue
		}
		if err := validateTerminalWriteFunctionCall(operation); err != nil {
			return err
		}
		arguments, err := r.operations.DecodeInput(operation.call.Name, operation.normalizedArguments)
		if err != nil {
			return err
		}
		projections, err := r.operations.BuildTerminalSessionProjection(operation.call.Name, arguments)
		if err != nil {
			return err
		}
		operations[index].terminalProjection = cloneTerminalSessionProjections(projections)
		operations[index].terminalProjectionReady = true
		prospective = append(prospective, projections...)
	}
	if err := validateTerminalSessionProjections(prospective); err != nil {
		return fmt.Errorf("agent: terminal operation batch accumulated session projection: %w", err)
	}
	return nil
}

func validateTerminalWriteFunctionCall(operation preparedOperation) error {
	if !operation.operation.Terminal || operation.operation.Effect != OperationEffectWrite {
		return nil
	}
	matches := 0
	for _, item := range operation.modelOutput {
		if item.Type != ModelOutputFunctionCall || item.Call == nil || item.Call.ID != operation.call.ID {
			continue
		}
		matches++
		if err := validateProjectedFunctionCall(ModelInputItem{Raw: item.Raw}, operation.call.ID); err != nil {
			return fmt.Errorf("%w: terminal operation %q function call cannot be projected before execution: %v", ErrInvalidModelOutput, operation.call.Name, err)
		}
	}
	if matches != 1 {
		return fmt.Errorf("%w: terminal operation %q has %d matching function-call envelopes", ErrInvalidModelOutput, operation.call.Name, matches)
	}
	return nil
}

func (r *Runtime) enforceOperationDecision(ctx context.Context, run *RunRecord, state *agentState, attempt *operationAttempt) error {
	approvalReq, err := attempt.envelope.Request(r.operations, "", state.lease.Fence())
	if err != nil {
		return err
	}
	return r.enforceDecision(
		ctx, run, state, approvalReq, attempt.prepared.policyDecision,
		attempt.prepared.responseID, attempt.prepared.modelOutput, attempt.prepared.approvalCheckpoint,
	)
}

func (r *Runtime) acquireOperationExecution(ctx context.Context, attempt *operationAttempt) error {
	operation := attempt.prepared
	if operation.operation.Effect != OperationEffectWrite {
		return nil
	}
	attemptID, err := r.nextGeneratedID(ctx, "operation attempt id")
	if err != nil {
		return err
	}
	attempt.attemptID = attemptID
	transitionID, err := r.nextGeneratedID(ctx, "operation acquisition transition id")
	if err != nil {
		return err
	}
	now := r.now()
	contractID := operationSummary(operation.operation).ContractID
	verificationRequired := operation.operation.Confirmation.Mode == ConfirmationRequired
	canonicalArguments, err := canonicalJSONIdentity(operation.normalizedArguments)
	if err != nil {
		return fmt.Errorf("agent: canonicalize operation execution arguments for %q: %w", operation.call.Name, err)
	}
	expectedExecution := OperationExecutionRecord{
		ID: operation.executionID, IdempotencyKey: attempt.envelope.input.IdempotencyKey,
		IdempotencyScope: attempt.envelope.input.IdempotencyScope,
		RunID:            attempt.envelope.runID, SessionID: attempt.envelope.sessionID,
		CallID: operation.call.ID, AttemptID: attempt.attemptID, Name: operation.call.Name,
		ContractID: contractID, VerificationRequired: verificationRequired,
		Arguments: append(json.RawMessage(nil), canonicalArguments...),
		Status:    OperationExecutionStarted, CreatedAt: now, UpdatedAt: now,
	}
	acquireRequest := AcquireExecutionRequest{
		Execution: detachedOperationExecutionRecord(expectedExecution),
		Transition: OperationExecutionTransition{
			ID: transitionID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.envelope.runID, CallID: operation.call.ID,
			Actor: "runtime", Message: "execution acquired", To: OperationExecutionStarted, CreatedAt: now,
			VerificationRequired: verificationRequired,
		},
	}
	acquired, err := r.executions.AcquireExecution(ctx, detachedAcquireExecutionRequest(acquireRequest))
	if err != nil {
		return fmt.Errorf("agent: acquire operation execution %q: %w", operation.call.Name, validateUTF8Error("execution store", err))
	}
	if err := validateUTF8Boundary("execution acquisition", acquired); err != nil {
		return err
	}
	acquired.Execution = detachedOperationExecutionRecord(acquired.Execution)
	if err := validateAcquiredExecutionRecord(expectedExecution, acquired.Execution, acquired.Disposition); err != nil {
		return err
	}
	if err := r.validateOperationExecutionRecordContract(acquired.Execution, operation.operation); err != nil {
		return err
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

func detachedAcquireExecutionRequest(request AcquireExecutionRequest) AcquireExecutionRequest {
	out := request
	out.Execution = detachedOperationExecutionRecord(request.Execution)
	out.Transition.Result = detachedOperationResult(request.Transition.Result)
	if request.Transition.Verification != nil {
		out.Transition.Verification = verificationPointer(*request.Transition.Verification)
	}
	out.Transition.Evidence = append(json.RawMessage(nil), request.Transition.Evidence...)
	return out
}

func detachedOperationExecutionRecord(record OperationExecutionRecord) OperationExecutionRecord {
	out := record
	out.Arguments = append(json.RawMessage(nil), record.Arguments...)
	out.Result = detachedOperationResult(record.Result)
	if record.Verification != nil {
		out.Verification = verificationPointer(*record.Verification)
	}
	return out
}

func detachedOperationResult(result OperationResult) OperationResult {
	out := result
	out.Output = append(json.RawMessage(nil), result.Output...)
	out.Receipt = append(json.RawMessage(nil), result.Receipt...)
	out.Artifacts = make([]ResultArtifact, len(result.Artifacts))
	for index := range result.Artifacts {
		out.Artifacts[index] = result.Artifacts[index]
		out.Artifacts[index].Data = append(json.RawMessage(nil), result.Artifacts[index].Data...)
		out.Artifacts[index].InternalData = append(json.RawMessage(nil), result.Artifacts[index].InternalData...)
		out.Artifacts[index].SessionSummary = append(json.RawMessage(nil), result.Artifacts[index].SessionSummary...)
	}
	return out
}

func validateAcquiredExecutionRecord(expected, actual OperationExecutionRecord, disposition ExecutionAcquireDisposition) error {
	if err := validateOperationExecutionRecord(actual); err != nil {
		return err
	}
	if actual.ID != expected.ID || actual.IdempotencyKey != expected.IdempotencyKey ||
		actual.IdempotencyScope != expected.IdempotencyScope || actual.SessionID != expected.SessionID ||
		actual.Name != expected.Name || actual.ContractID != expected.ContractID ||
		actual.VerificationRequired != expected.VerificationRequired ||
		!jsonSemanticallyEqual(actual.Arguments, expected.Arguments) {
		return fmt.Errorf("%w: execution %s stable identity changed during acquisition", ErrOperationPlanChanged, expected.ID)
	}
	if actual.CreatedAt.IsZero() || actual.UpdatedAt.IsZero() || actual.UpdatedAt.Before(actual.CreatedAt) {
		return fmt.Errorf("%w: execution %s has invalid durable timestamps", ErrInvalidExecutionTransition, expected.ID)
	}
	if actual.CreatedAt.After(expected.UpdatedAt) || actual.UpdatedAt.After(expected.UpdatedAt) {
		return fmt.Errorf("%w: execution %s acquisition timestamps do not match the requested event", ErrInvalidExecutionTransition, expected.ID)
	}
	if disposition == ExecutionAcquired &&
		(actual.RunID != expected.RunID || actual.CallID != expected.CallID || actual.AttemptID != expected.AttemptID) {
		return fmt.Errorf("%w: newly acquired execution %s changed its run, call, or attempt identity", ErrOperationAttemptLost, expected.ID)
	}
	switch disposition {
	case ExecutionAcquired:
		if actual.Status != OperationExecutionStarted || !actual.UpdatedAt.Equal(expected.UpdatedAt) {
			return fmt.Errorf("%w: newly acquired execution %s has an invalid started acknowledgement", ErrInvalidExecutionTransition, expected.ID)
		}
	case ExecutionReplay:
		if actual.Status != OperationExecutionExecuted && actual.Status != OperationExecutionCompleted {
			return fmt.Errorf("%w: execution %s replay has status %q", ErrOperationOutcomeUnknown, expected.ID, actual.Status)
		}
	case ExecutionBlocked:
		if actual.Status == OperationExecutionExecuted || actual.Status == OperationExecutionCompleted {
			return fmt.Errorf("%w: execution %s blocked disposition conflicts with replayable status %q", ErrInvalidExecutionTransition, expected.ID, actual.Status)
		}
	default:
		return fmt.Errorf("%w: execution store returned invalid acquisition disposition %q", ErrInvalidExecutionTransition, disposition)
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
				return nil, r.handleExecutionAttemptValidationFailure(ctx, attempt, err)
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
			err = validateUTF8Error("operation executor", err)
			if operation.operation.Effect == OperationEffectWrite && errors.Is(err, ErrOperationNotApplied) {
				return nil, r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionRetryable, err)
			}
			return nil, r.unknownOperationFailure(ctx, attempt, err)
		}
		if err := validateUTF8Boundary("operation result", attempt.result); err != nil {
			return nil, r.unknownOperationFailure(ctx, attempt, err)
		}
		attempt.result = cloneOperationResult(attempt.result)
	}

	result := attempt.result
	if err := validateUTF8Boundary("operation result", result); err != nil {
		return nil, r.unknownOperationFailure(ctx, attempt, err)
	}
	// Keep protocol-invalid writes recoverable: once a result is journaled as
	// completed, replay must treat it as immutable.
	if err := validateOperationResultProtocol(operation.operation, result); err != nil {
		return nil, r.unknownOperationFailure(ctx, attempt, err)
	}
	if len(result.Output) == 0 {
		return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: operation %q returned empty output", operation.call.Name))
	}
	output, err := r.operations.DecodeOutput(operation.call.Name, result.Output)
	if err != nil {
		return nil, r.unknownOperationFailure(ctx, attempt, err)
	}
	if len(result.Receipt) > 0 {
		if _, err := decodeExactJSON(result.Receipt); err != nil {
			return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: operation %q returned ambiguous or invalid receipt JSON: %w", operation.call.Name, err))
		}
	}
	if err := validateResultArtifacts(result.Artifacts); err != nil {
		return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: operation %q returned invalid artifacts: %w", operation.call.Name, err))
	}
	if operation.operation.Terminal && operation.operation.Effect == OperationEffectWrite {
		actualProjection, err := completeTerminalArtifactProjections(result.Artifacts)
		if err != nil {
			return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: terminal operation %q returned an incomplete session projection: %w", operation.call.Name, err))
		}
		if !equalTerminalSessionProjections(attempt.expectedProjection, actualProjection) {
			return nil, r.unknownOperationFailure(ctx, attempt, fmt.Errorf("agent: terminal operation %q returned a session projection that differs from its preflight declaration", operation.call.Name))
		}
	}
	if operation.operation.Effect == OperationEffectWrite && !attempt.replayed {
		// FALLBACK: justified because if the executed transition fails, the side
		// effect has already returned successfully; preserve that persistence
		// error while fencing the unresolved outcome for reconciliation.
		message := "operation executed"
		if operation.operation.Confirmation.Mode == ConfirmationRequired {
			message = "operation executed; verification pending"
		}
		transitionID, err := r.nextGeneratedID(ctx, "operation executed transition id")
		if err != nil {
			cause := fmt.Errorf("agent: persist executed operation %q: %w", operation.call.Name, err)
			return nil, r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionUnknown, cause)
		}
		journalExecution, err := r.transitionExecution(ctx, OperationExecutionTransition{
			ID: transitionID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.executionRunID(), CallID: attempt.executionCallID(), Actor: "runtime",
			Message: message,
			From:    OperationExecutionStarted, To: OperationExecutionExecuted,
			VerificationRequired: operation.operation.Confirmation.Mode == ConfirmationRequired,
			Result:               cloneOperationResult(result), CreatedAt: r.now(),
		}, attempt.journalExecution)
		if err != nil {
			cause := fmt.Errorf("agent: persist executed operation %q: %w", operation.call.Name, err)
			return nil, r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionUnknown, cause)
		}
		attempt.journalExecution = journalExecution
		attempt.executionStatus = OperationExecutionExecuted
	}
	return output, nil
}

func (r *Runtime) handleExecutionAttemptValidationFailure(ctx context.Context, attempt *operationAttempt, validationErr error) error {
	validationErr = validateUTF8Error("execution store", validationErr)
	cause := fmt.Errorf("agent: validate operation execution attempt: %w", validationErr)
	return r.transitionOperationFailure(ctx, attempt.executionRef(), OperationExecutionRetryable, cause)
}

func validateOperationResultProtocol(operation Operation, result OperationResult) error {
	if !utf8.ValidString(result.FinalResponse) {
		return fmt.Errorf("agent: operation %q final response must be valid UTF-8", operation.Name)
	}
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
		transitionID, err := r.nextGeneratedID(ctx, "direct operation completion transition id")
		if err != nil {
			return nil, err
		}
		if _, err := r.transitionExecution(ctx, OperationExecutionTransition{
			ID: transitionID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.executionRunID(), CallID: attempt.executionCallID(), Actor: "runtime",
			Message: "direct operation completed",
			From:    OperationExecutionExecuted, To: OperationExecutionCompleted,
			VerificationRequired: false,
			Result:               cloneOperationResult(attempt.result), CreatedAt: r.now(),
		}, attempt.journalExecution); err != nil {
			return nil, fmt.Errorf("agent: complete direct operation execution %q: %w", operation.call.Name, err)
		}
		return nil, nil
	}
	if operation.operation.Effect == OperationEffectWrite && attempt.replayed && attempt.executionStatus == OperationExecutionCompleted {
		if attempt.journalExecution.Verification != nil {
			verification, err := normalizePositiveVerificationResult(*attempt.journalExecution.Verification)
			if err != nil {
				return nil, fmt.Errorf("%w: completed execution %s has invalid verification: %v", ErrVerificationFailed, operation.executionID, err)
			}
			return &verification, nil
		}
		return nil, fmt.Errorf("%w: completed execution %s has no verification", ErrVerificationFailed, operation.executionID)
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
		transitionID, err := r.nextGeneratedID(ctx, "verified operation completion transition id")
		if err != nil {
			return nil, err
		}
		if _, err := r.transitionExecution(ctx, OperationExecutionTransition{
			ID: transitionID, ExecutionID: operation.executionID, AttemptID: attempt.attemptID,
			RunID: attempt.executionRunID(), CallID: attempt.executionCallID(), Actor: "runtime",
			Message: "operation verification completed",
			From:    OperationExecutionExecuted, To: OperationExecutionCompleted,
			VerificationRequired: true,
			Result:               cloneOperationResult(attempt.result), Verification: verificationPointer(verification), CreatedAt: r.now(),
		}, attempt.journalExecution); err != nil {
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
	itemID, err := r.nextGeneratedID(ctx, "operation result item id")
	if err != nil {
		return nil, correlateOperationError(operation.call.ID, operation.executionID, attempt.attemptID, err)
	}
	appendErr := r.appendItem(ctx, ItemRecord{
		ID: itemID, RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationResult,
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
