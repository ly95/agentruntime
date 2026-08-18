package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

func operationRequestID(input Input) string {
	digest := sha256.New()
	if input.SessionID == "" {
		writeHashField(digest, []byte(input.IdempotencyScope))
	}
	writeHashField(digest, []byte(input.SessionID))
	writeHashField(digest, []byte(input.IdempotencyKey))
	return "req_" + hex.EncodeToString(digest.Sum(nil))
}

func operationExecutionID(requestID string, batchIndex, stepIndex uint64, operation, contractID string, arguments json.RawMessage) (string, error) {
	canonicalArguments, err := canonicalJSONIdentity(arguments)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	writeHashField(digest, []byte(requestID))
	writeHashField(digest, []byte(strconv.FormatUint(batchIndex, 10)))
	writeHashField(digest, []byte(strconv.FormatUint(stepIndex, 10)))
	writeHashField(digest, []byte(operation))
	writeHashField(digest, []byte(contractID))
	writeHashField(digest, canonicalArguments)
	return "op_" + hex.EncodeToString(digest.Sum(nil)), nil
}

func terminalOperationExecutionID(runID, callID, operation, contractID string, arguments json.RawMessage) (string, error) {
	canonicalArguments, err := canonicalJSONIdentity(arguments)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	writeHashField(digest, []byte(runID))
	writeHashField(digest, []byte(callID))
	writeHashField(digest, []byte(operation))
	writeHashField(digest, []byte(contractID))
	writeHashField(digest, canonicalArguments)
	return "terminal_op_" + hex.EncodeToString(digest.Sum(nil)), nil
}

func (r *Runtime) transitionOperationFailure(ctx context.Context, ref operationExecutionRef, target OperationExecutionStatus, cause error) error {
	if cause == nil {
		cause = errors.New("agent: operation failure has no cause")
	}
	cause = validateUTF8Error("runtime dependency", cause)
	if ref.executionID == "" || r.executions == nil {
		return cause
	}
	label := "mark operation outcome unknown"
	result := cause
	if target == OperationExecutionRetryable {
		label = "mark operation retryable before execution"
	} else if target == OperationExecutionUnknown {
		result = errors.Join(ErrOperationOutcomeUnknown, cause)
	} else {
		return errors.Join(cause, fmt.Errorf("agent: invalid failure transition target %q", target))
	}
	transitionID, idErr := r.nextGeneratedID(ctx, "operation failure transition id")
	if idErr != nil {
		return errors.Join(result, idErr)
	}
	transition := OperationExecutionTransition{
		ID: transitionID, ExecutionID: ref.executionID, AttemptID: ref.attemptID,
		RunID: ref.runID, CallID: ref.callID, Actor: "runtime", Message: cause.Error(),
		From: OperationExecutionStarted, To: target, VerificationRequired: ref.verificationRequired, CreatedAt: r.now(),
	}
	cleanupCtx, cancel := r.detachedCleanupContext(ctx)
	_, err := r.transitionExecution(cleanupCtx, transition, ref.record)
	cancel()
	if err != nil {
		transitionErr := fmt.Errorf("agent: %s: %w", label, err)
		inspectCtx, inspectCancel := r.detachedCleanupContext(ctx)
		defer inspectCancel()
		fenced, inspectErr := r.inspectFailureTransition(inspectCtx, ref, transition)
		if fenced {
			return errors.Join(result, transitionErr)
		}
		if target == OperationExecutionRetryable {
			inspectErr = errors.Join(inspectErr, fmt.Errorf(
				"agent: pre-effect release for execution %s was not proved; after proving the executor did not begin, use reconciliation action %q with evidence",
				ref.executionID,
				OperationReconciliationAbandon,
			))
		}
		return errors.Join(result, transitionErr, inspectErr)
	}
	return result
}

func (r *Runtime) inspectFailureTransition(
	ctx context.Context,
	ref operationExecutionRef,
	transition OperationExecutionTransition,
) (bool, error) {
	current, err := r.executions.GetExecution(ctx, ref.executionID)
	if err != nil {
		return false, fmt.Errorf("agent: inspect failed operation transition: %w", validateUTF8Error("execution store", err))
	}
	if err := validateUTF8Boundary("execution store record", current); err != nil {
		return false, err
	}
	current = detachedOperationExecutionRecord(current)
	if err := validateOperationExecutionRecord(current); err != nil {
		return false, err
	}
	registeredOperation, ok := r.operations.Get(current.Name)
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrOperationNotFound, current.Name)
	}
	if err := r.validateOperationExecutionRecordContract(current, registeredOperation); err != nil {
		return false, err
	}
	prior := detachedOperationExecutionRecord(ref.record)
	if current.ID != prior.ID || current.IdempotencyKey != prior.IdempotencyKey ||
		current.IdempotencyScope != prior.IdempotencyScope || current.SessionID != prior.SessionID ||
		current.Name != prior.Name || current.ContractID != prior.ContractID ||
		current.VerificationRequired != prior.VerificationRequired ||
		!jsonSemanticallyEqual(current.Arguments, prior.Arguments) || !current.CreatedAt.Equal(prior.CreatedAt) {
		return false, fmt.Errorf("%w: failed operation transition returned a different durable identity", ErrOperationPlanChanged)
	}
	if err := validateTransitionExecutionRecord(transition, current, prior); err == nil {
		return true, nil
	}
	if current.Status != OperationExecutionStarted || current.RunID != prior.RunID ||
		current.CallID != prior.CallID || current.AttemptID != prior.AttemptID {
		return true, nil
	}
	if !current.UpdatedAt.Equal(prior.UpdatedAt) {
		return false, fmt.Errorf("%w: failed operation transition changed the owned started timestamp", ErrInvalidExecutionTransition)
	}
	return false, fmt.Errorf("agent: execution %s remains on its owned started attempt", current.ID)
}

func (r *Runtime) transitionExecution(ctx context.Context, transition OperationExecutionTransition, expected ...OperationExecutionRecord) (OperationExecutionRecord, error) {
	if err := transition.Validate(); err != nil {
		return OperationExecutionRecord{}, err
	}
	if len(expected) != 1 {
		return OperationExecutionRecord{}, fmt.Errorf("%w: transition requires exactly one expected execution record", ErrInvalidExecutionTransition)
	}
	expectedTransition := detachedOperationExecutionTransition(transition)
	expected[0] = detachedOperationExecutionRecord(expected[0])
	if err := validateOperationExecutionRecord(expected[0]); err != nil {
		return OperationExecutionRecord{}, fmt.Errorf("%w: expected prior record is invalid: %v", ErrInvalidExecutionTransition, err)
	}
	if expectedTransition.ExecutionID != expected[0].ID || expectedTransition.AttemptID != expected[0].AttemptID ||
		expectedTransition.RunID != expected[0].RunID || expectedTransition.CallID != expected[0].CallID ||
		expectedTransition.From != expected[0].Status ||
		expectedTransition.VerificationRequired != expected[0].VerificationRequired {
		return OperationExecutionRecord{}, fmt.Errorf("%w: transition does not match the expected prior execution record", ErrInvalidExecutionTransition)
	}
	if expectedTransition.CreatedAt.Before(expected[0].UpdatedAt) {
		return OperationExecutionRecord{}, fmt.Errorf("%w: transition timestamp precedes the expected prior execution record", ErrInvalidExecutionTransition)
	}
	if expectedTransition.From == OperationExecutionExecuted && expectedTransition.To == OperationExecutionCompleted &&
		!equalOperationResult(expectedTransition.Result, expected[0].Result) {
		return OperationExecutionRecord{}, fmt.Errorf("%w: completion transition rewrites the durable executed result", ErrInvalidExecutionTransition)
	}
	record, err := r.executions.TransitionExecution(ctx, detachedOperationExecutionTransition(transition))
	if err != nil {
		return OperationExecutionRecord{}, validateUTF8Error("execution store", err)
	}
	if err := validateUTF8Boundary("execution store record", record); err != nil {
		return OperationExecutionRecord{}, err
	}
	record = detachedOperationExecutionRecord(record)
	if err := validateTransitionExecutionRecord(expectedTransition, record, expected...); err != nil {
		return OperationExecutionRecord{}, err
	}
	return record, nil
}

func detachedOperationExecutionTransition(transition OperationExecutionTransition) OperationExecutionTransition {
	out := transition
	out.Result = detachedOperationResult(transition.Result)
	if transition.Verification != nil {
		out.Verification = verificationPointer(*transition.Verification)
	}
	out.Evidence = append(json.RawMessage(nil), transition.Evidence...)
	return out
}

func validateTransitionExecutionRecord(transition OperationExecutionTransition, record OperationExecutionRecord, expected ...OperationExecutionRecord) error {
	if len(expected) != 1 {
		return fmt.Errorf("%w: transition acknowledgement requires one expected prior record", ErrInvalidExecutionTransition)
	}
	if err := validateOperationExecutionRecord(record); err != nil {
		return err
	}
	if record.ID != transition.ExecutionID || record.AttemptID != transition.AttemptID ||
		record.RunID != transition.RunID || record.CallID != transition.CallID {
		return fmt.Errorf("%w: execution store returned a record for a different transition identity", ErrInvalidExecutionTransition)
	}
	if record.Status != transition.To {
		return fmt.Errorf("%w: execution %s returned status %q after transition to %q", ErrInvalidExecutionTransition, transition.ExecutionID, record.Status, transition.To)
	}
	if record.VerificationRequired != transition.VerificationRequired {
		return fmt.Errorf("%w: execution %s returned a different verification requirement", ErrInvalidExecutionTransition, transition.ExecutionID)
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return fmt.Errorf("%w: execution %s returned invalid timestamps", ErrInvalidExecutionTransition, transition.ExecutionID)
	}
	want := expected[0]
	if record.ID != want.ID || record.IdempotencyKey != want.IdempotencyKey ||
		record.IdempotencyScope != want.IdempotencyScope || record.RunID != want.RunID ||
		record.SessionID != want.SessionID || record.CallID != want.CallID ||
		record.AttemptID != want.AttemptID || record.Name != want.Name ||
		record.ContractID != want.ContractID || record.VerificationRequired != want.VerificationRequired ||
		!jsonSemanticallyEqual(record.Arguments, want.Arguments) || !record.CreatedAt.Equal(want.CreatedAt) {
		return fmt.Errorf("%w: execution %s immutable identity changed during transition", ErrOperationPlanChanged, transition.ExecutionID)
	}
	if record.UpdatedAt.Before(want.UpdatedAt) || !record.UpdatedAt.Equal(transition.CreatedAt) {
		return fmt.Errorf("%w: execution %s returned an unexpected transition timestamp", ErrInvalidExecutionTransition, transition.ExecutionID)
	}
	switch transition.To {
	case OperationExecutionExecuted, OperationExecutionCompleted:
		if !equalOperationResult(record.Result, transition.Result) {
			return fmt.Errorf("%w: execution %s returned a different transition result", ErrInvalidExecutionTransition, transition.ExecutionID)
		}
	case OperationExecutionUnknown, OperationExecutionRetryable:
		if hasOperationResult(record.Result) {
			return fmt.Errorf("%w: execution %s returned an unexpected transition result", ErrInvalidExecutionTransition, transition.ExecutionID)
		}
	case OperationExecutionRecoveryFailed:
		if !equalOptionalOperationResult(record.Result, want.Result) {
			return fmt.Errorf("%w: execution %s changed its durable result during recovery failure", ErrInvalidExecutionTransition, transition.ExecutionID)
		}
	}
	wantError := ""
	if transition.To == OperationExecutionUnknown || transition.To == OperationExecutionRetryable || transition.To == OperationExecutionRecoveryFailed {
		wantError = transition.Message
	}
	if record.Error != wantError {
		return fmt.Errorf("%w: execution %s returned an unexpected durable error", ErrInvalidExecutionTransition, transition.ExecutionID)
	}
	if transition.To == OperationExecutionCompleted {
		if transition.VerificationRequired {
			if record.Verification == nil || transition.Verification == nil {
				return fmt.Errorf("%w: execution %s completion lacks durable verification", ErrInvalidExecutionTransition, transition.ExecutionID)
			}
			want, err := normalizePositiveVerificationResult(*transition.Verification)
			if err != nil {
				return fmt.Errorf("%w: transition verification is invalid: %v", ErrInvalidExecutionTransition, err)
			}
			got, err := normalizePositiveVerificationResult(*record.Verification)
			if err != nil {
				return fmt.Errorf("%w: durable verification is invalid: %v", ErrInvalidExecutionTransition, err)
			}
			if got.Confirmed != want.Confirmed || got.Message != want.Message || !jsonSemanticallyEqual(got.Evidence, want.Evidence) {
				return fmt.Errorf("%w: execution %s returned different durable verification", ErrInvalidExecutionTransition, transition.ExecutionID)
			}
		} else if record.Verification != nil {
			return fmt.Errorf("%w: direct execution %s returned unexpected verification", ErrInvalidExecutionTransition, transition.ExecutionID)
		}
	} else if record.Verification != nil {
		return fmt.Errorf("%w: non-completed execution %s returned verification", ErrInvalidExecutionTransition, transition.ExecutionID)
	}
	return nil
}

func validateOperationExecutionRecord(record OperationExecutionRecord) error {
	if err := validateUTF8Boundary("operation execution record", record); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidExecutionTransition, err)
	}
	if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.IdempotencyKey) == "" ||
		strings.TrimSpace(record.RunID) == "" || strings.TrimSpace(record.CallID) == "" ||
		strings.TrimSpace(record.AttemptID) == "" || strings.TrimSpace(record.Name) == "" ||
		strings.TrimSpace(record.ContractID) == "" ||
		(strings.TrimSpace(record.SessionID) == "" && strings.TrimSpace(record.IdempotencyScope) == "") {
		return fmt.Errorf("%w: execution %s has an incomplete durable identity", ErrInvalidExecutionTransition, record.ID)
	}
	if len(record.Arguments) == 0 {
		return fmt.Errorf("%w: execution %s has empty durable arguments", ErrInvalidExecutionTransition, record.ID)
	}
	if _, err := decodeExactJSON(record.Arguments); err != nil {
		return fmt.Errorf("%w: execution %s has ambiguous or invalid durable arguments: %v", ErrInvalidExecutionTransition, record.ID, err)
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return fmt.Errorf("%w: execution %s has invalid durable timestamps", ErrInvalidExecutionTransition, record.ID)
	}
	validateResult := func(required bool) error {
		if !hasOperationResult(record.Result) {
			if required {
				return fmt.Errorf("%w: execution %s status %q requires a durable result", ErrInvalidExecutionTransition, record.ID, record.Status)
			}
			return nil
		}
		if len(record.Result.Output) == 0 {
			return fmt.Errorf("%w: execution %s durable result has empty output", ErrInvalidExecutionTransition, record.ID)
		}
		if _, err := decodeExactJSON(record.Result.Output); err != nil {
			return fmt.Errorf("%w: execution %s durable output is ambiguous or invalid: %v", ErrInvalidExecutionTransition, record.ID, err)
		}
		if len(record.Result.Receipt) > 0 {
			if _, err := decodeExactJSON(record.Result.Receipt); err != nil {
				return fmt.Errorf("%w: execution %s durable receipt is ambiguous or invalid: %v", ErrInvalidExecutionTransition, record.ID, err)
			}
		}
		if err := validateResultArtifacts(record.Result.Artifacts); err != nil {
			return fmt.Errorf("%w: execution %s durable artifacts are invalid: %v", ErrInvalidExecutionTransition, record.ID, err)
		}
		return nil
	}
	switch record.Status {
	case OperationExecutionStarted:
		if hasOperationResult(record.Result) || record.Verification != nil || record.Error != "" {
			return fmt.Errorf("%w: started execution %s contains result, verification, or error", ErrInvalidExecutionTransition, record.ID)
		}
	case OperationExecutionExecuted:
		if err := validateResult(true); err != nil {
			return err
		}
		if record.Verification != nil || record.Error != "" {
			return fmt.Errorf("%w: executed execution %s contains verification or error", ErrInvalidExecutionTransition, record.ID)
		}
	case OperationExecutionCompleted:
		if err := validateResult(true); err != nil {
			return err
		}
		if record.Error != "" {
			return fmt.Errorf("%w: completed execution %s contains an error", ErrInvalidExecutionTransition, record.ID)
		}
		if record.VerificationRequired {
			if record.Verification == nil {
				return fmt.Errorf("%w: %w: completed execution %s lacks durable verification", ErrInvalidExecutionTransition, ErrVerificationFailed, record.ID)
			}
			if _, err := normalizePositiveVerificationResult(*record.Verification); err != nil {
				return fmt.Errorf("%w: %w: completed execution %s has invalid durable verification: %v", ErrInvalidExecutionTransition, ErrVerificationFailed, record.ID, err)
			}
		} else if record.Verification != nil {
			return fmt.Errorf("%w: direct completed execution %s contains unexpected verification", ErrInvalidExecutionTransition, record.ID)
		}
	case OperationExecutionUnknown, OperationExecutionRetryable:
		if hasOperationResult(record.Result) || record.Verification != nil || strings.TrimSpace(record.Error) == "" {
			return fmt.Errorf("%w: execution %s status %q has an invalid result, verification, or error", ErrInvalidExecutionTransition, record.ID, record.Status)
		}
	case OperationExecutionRecoveryFailed:
		if err := validateResult(false); err != nil {
			return err
		}
		if record.Verification != nil || strings.TrimSpace(record.Error) == "" {
			return fmt.Errorf("%w: recovery-failed execution %s has an invalid verification or error", ErrInvalidExecutionTransition, record.ID)
		}
	default:
		return fmt.Errorf("%w: execution %s has unsupported status %q", ErrInvalidExecutionTransition, record.ID, record.Status)
	}
	return nil
}

func (r *Runtime) validateOperationExecutionRecordContract(record OperationExecutionRecord, operation Operation) error {
	if err := validateOperationExecutionRecord(record); err != nil {
		return err
	}
	summary := operationSummary(operation)
	if record.Name != operation.Name || record.ContractID != summary.ContractID ||
		record.VerificationRequired != (operation.Confirmation.Mode == ConfirmationRequired) {
		return fmt.Errorf("%w: operation %s durable contract changed", ErrOperationPlanChanged, record.Name)
	}
	arguments, err := r.operations.DecodeInput(record.Name, record.Arguments)
	if err != nil {
		return fmt.Errorf("%w: execution %s durable arguments violate the operation contract: %v", ErrInvalidExecutionTransition, record.ID, err)
	}
	if !hasOperationResult(record.Result) {
		return nil
	}
	if err := r.operations.ValidateOutput(record.Name, record.Result.Output); err != nil {
		return fmt.Errorf("%w: execution %s durable result violates the operation contract: %v", ErrInvalidExecutionTransition, record.ID, err)
	}
	if err := validateOperationResultProtocol(operation, record.Result); err != nil {
		return fmt.Errorf("%w: execution %s durable result violates the operation protocol: %v", ErrInvalidExecutionTransition, record.ID, err)
	}
	if operation.Terminal && operation.Effect == OperationEffectWrite {
		declared, err := r.operations.BuildTerminalSessionProjection(record.Name, arguments)
		if err != nil {
			return fmt.Errorf("%w: execution %s terminal projection cannot be rebuilt: %v", ErrInvalidExecutionTransition, record.ID, err)
		}
		actual, err := completeTerminalArtifactProjections(record.Result.Artifacts)
		if err != nil {
			return fmt.Errorf("%w: execution %s terminal result projection is incomplete: %v", ErrInvalidExecutionTransition, record.ID, err)
		}
		if !equalTerminalSessionProjections(declared, actual) {
			return fmt.Errorf("%w: execution %s terminal result projection changed", ErrInvalidExecutionTransition, record.ID)
		}
	}
	return nil
}

func equalOptionalOperationResult(left, right OperationResult) bool {
	if !hasOperationResult(left) || !hasOperationResult(right) {
		return !hasOperationResult(left) && !hasOperationResult(right)
	}
	return equalOperationResult(left, right)
}

// ReconcileOperation records a trusted reconciler decision for an unresolved write.
// Runtime validates completed output against the registered operation schema
// before the execution store atomically changes state and appends history. The
// abandon action is reserved for a trusted operator who has proved that a
// started attempt never entered its executor and supplies durable evidence.
// Complete may settle that exact started attempt only with durable evidence
// proving the executor committed and with the full validated completion payload.
func (r *Runtime) ReconcileOperation(ctx context.Context, request ReconcileOperationRequest) error {
	if r.executions == nil {
		return ErrExecutionStoreRequired
	}
	ctx, identityScope := r.beginIdentityScope(ctx)
	defer r.releaseIdentityScope(identityScope)
	if err := validateUTF8Boundary("reconciliation request", request); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
	}
	request.ExecutionID = strings.TrimSpace(request.ExecutionID)
	request.ExpectedAttemptID = strings.TrimSpace(request.ExpectedAttemptID)
	request.Actor = strings.TrimSpace(request.Actor)
	request.Message = strings.TrimSpace(request.Message)
	if request.ExecutionID == "" || request.ExpectedAttemptID == "" || request.Actor == "" || request.Message == "" {
		return fmt.Errorf("%w: execution_id, expected_attempt_id, actor, and message are required", ErrInvalidReconciliation)
	}
	if len(request.Evidence) > 0 {
		if err := validateNonNullExactJSON(request.Evidence); err != nil {
			return fmt.Errorf("%w: evidence must be non-null unambiguous valid JSON: %v", ErrInvalidReconciliation, err)
		}
	}
	execution, err := r.executions.GetExecution(ctx, request.ExecutionID)
	if err != nil {
		return validateUTF8Error("execution store", err)
	}
	if err := validateUTF8Boundary("persisted execution", execution); err != nil {
		return err
	}
	execution = detachedOperationExecutionRecord(execution)
	if err := validateOperationExecutionRecord(execution); err != nil {
		return fmt.Errorf("%w: persisted execution is invalid: %v", ErrInvalidReconciliation, err)
	}
	if execution.ID != request.ExecutionID {
		return fmt.Errorf("%w: execution store returned %q for requested execution %q", ErrInvalidReconciliation, execution.ID, request.ExecutionID)
	}
	if execution.AttemptID != request.ExpectedAttemptID {
		return fmt.Errorf("%w: execution %s current attempt is %q", ErrOperationAttemptLost, execution.ID, execution.AttemptID)
	}
	// A started attempt may still be inside its executor. Only evidence-bearing
	// proof can settle it: Abandon proves no side effect began, while Complete
	// proves the exact attempt committed despite missing post-effect journal state.
	if execution.Status == OperationExecutionStarted &&
		request.Action != OperationReconciliationAbandon && request.Action != OperationReconciliationComplete {
		return fmt.Errorf("%w: execution %s status %q requires evidence-bearing abandonment or completion", ErrInvalidReconciliation, execution.ID, execution.Status)
	}
	if execution.Status != OperationExecutionStarted && execution.Status != OperationExecutionExecuted && execution.Status != OperationExecutionUnknown {
		return fmt.Errorf("%w: execution %s status %q cannot be reconciled", ErrInvalidReconciliation, execution.ID, execution.Status)
	}
	switch request.Action {
	case OperationReconciliationRetry, OperationReconciliationComplete, OperationReconciliationFail, OperationReconciliationAbandon:
	default:
		return fmt.Errorf("%w: unsupported action %q", ErrInvalidReconciliation, request.Action)
	}
	if request.Action == OperationReconciliationAbandon && execution.Status != OperationExecutionStarted {
		return fmt.Errorf("%w: abandonment requires a started execution", ErrInvalidReconciliation)
	}
	registeredOperation, ok := r.operations.Get(execution.Name)
	if !ok {
		return fmt.Errorf("%w: %s", ErrOperationNotFound, execution.Name)
	}
	currentSummary := operationSummary(registeredOperation)
	if execution.ContractID == "" || execution.ContractID != currentSummary.ContractID ||
		execution.VerificationRequired != (registeredOperation.Confirmation.Mode == ConfirmationRequired) {
		return fmt.Errorf("%w: operation %s contract changed", ErrOperationPlanChanged, execution.Name)
	}
	if err := r.validateOperationExecutionRecordContract(execution, registeredOperation); err != nil {
		return fmt.Errorf("%w: persisted execution is invalid: %v", ErrInvalidReconciliation, err)
	}
	transition := OperationExecutionTransition{
		ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: request.Actor, Message: request.Message,
		From: execution.Status, VerificationRequired: execution.VerificationRequired,
		Evidence: append(json.RawMessage(nil), request.Evidence...), CreatedAt: r.now(),
	}
	switch request.Action {
	case OperationReconciliationAbandon:
		if len(request.Evidence) == 0 {
			return fmt.Errorf("%w: abandonment requires evidence that the executor did not begin", ErrInvalidReconciliation)
		}
		if hasOperationResult(request.Result) || request.Verification != nil {
			return fmt.Errorf("%w: abandonment cannot contain a result or verification", ErrInvalidReconciliation)
		}
		transition.To = OperationExecutionRetryable
	case OperationReconciliationRetry:
		if hasOperationResult(request.Result) || request.Verification != nil {
			return fmt.Errorf("%w: retry reconciliation cannot contain a result or verification", ErrInvalidReconciliation)
		}
		if execution.Status == OperationExecutionExecuted {
			return fmt.Errorf("%w: executed operation %s cannot be retried; complete its verification instead", ErrInvalidReconciliation, execution.ID)
		}
		transition.To = OperationExecutionRetryable
	case OperationReconciliationComplete:
		if execution.Status == OperationExecutionStarted && len(request.Evidence) == 0 {
			return fmt.Errorf("%w: started completion requires evidence that the exact attempt committed", ErrInvalidReconciliation)
		}
		if len(request.Result.Output) == 0 {
			return fmt.Errorf("%w: completed result output must be valid JSON", ErrInvalidReconciliation)
		}
		if len(request.Result.Receipt) > 0 {
			if _, err := decodeExactJSON(request.Result.Receipt); err != nil {
				return fmt.Errorf("%w: completed result receipt must be unambiguous valid JSON: %v", ErrInvalidReconciliation, err)
			}
		}
		if err := validateResultArtifacts(request.Result.Artifacts); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
		}
		if err := r.operations.ValidateOutput(execution.Name, request.Result.Output); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
		}
		if err := validateOperationResultProtocol(registeredOperation, request.Result); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
		}
		if registeredOperation.Terminal && registeredOperation.Effect == OperationEffectWrite {
			arguments, err := r.operations.DecodeInput(execution.Name, execution.Arguments)
			if err != nil {
				return fmt.Errorf("%w: persisted terminal arguments are invalid: %v", ErrInvalidReconciliation, err)
			}
			declared, err := r.operations.BuildTerminalSessionProjection(execution.Name, arguments)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
			}
			actual, err := completeTerminalArtifactProjections(request.Result.Artifacts)
			if err != nil {
				return fmt.Errorf("%w: terminal result has an incomplete session projection: %v", ErrInvalidReconciliation, err)
			}
			if !equalTerminalSessionProjections(declared, actual) {
				return fmt.Errorf("%w: terminal result projection differs from its preflight declaration", ErrInvalidReconciliation)
			}
		}
		if execution.Status == OperationExecutionExecuted && !equalOperationResult(execution.Result, request.Result) {
			return fmt.Errorf("%w: completed result does not match the durably executed result for %s", ErrInvalidReconciliation, execution.ID)
		}
		transition.To = OperationExecutionCompleted
		transition.Result = cloneOperationResult(request.Result)
		if execution.VerificationRequired {
			if request.Verification == nil {
				return fmt.Errorf("%w: confirmation-required reconciliation needs positive verification", ErrInvalidReconciliation)
			}
			verification, err := normalizePositiveVerificationResult(*request.Verification)
			if err != nil {
				return fmt.Errorf("%w: confirmation-required reconciliation needs positive verification: %v", ErrInvalidReconciliation, err)
			}
			transition.Verification = &verification
		} else if request.Verification != nil {
			return fmt.Errorf("%w: direct write reconciliation cannot contain verification", ErrInvalidReconciliation)
		}
	case OperationReconciliationFail:
		if hasOperationResult(request.Result) || request.Verification != nil {
			return fmt.Errorf("%w: failed reconciliation cannot contain a result", ErrInvalidReconciliation)
		}
		transition.To = OperationExecutionRecoveryFailed
	}
	transitionID, err := r.nextGeneratedID(ctx, "reconciliation transition id")
	if err != nil {
		return err
	}
	transition.ID = transitionID
	if _, err := r.transitionExecution(ctx, transition, execution); err != nil {
		if errors.Is(err, ErrInvalidExecutionTransition) || errors.Is(err, ErrOperationPlanChanged) {
			return fmt.Errorf("agent: reconcile operation execution: %w", err)
		}
		if proofErr := r.proveReconciliationTransitionCommitted(ctx, transition, execution); proofErr == nil {
			return nil
		} else {
			return errors.Join(fmt.Errorf("agent: reconcile operation execution: %w", err), proofErr)
		}
	}
	return nil
}

func (r *Runtime) proveReconciliationTransitionCommitted(
	ctx context.Context,
	transition OperationExecutionTransition,
	prior OperationExecutionRecord,
) error {
	inspectCtx, cancel := r.detachedCleanupContext(ctx)
	defer cancel()
	record, err := r.executions.GetExecution(inspectCtx, transition.ExecutionID)
	if err != nil {
		return fmt.Errorf("agent: inspect reconciliation transition after error: %w", validateUTF8Error("execution store", err))
	}
	if err := validateUTF8Boundary("execution store record", record); err != nil {
		return err
	}
	record = detachedOperationExecutionRecord(record)
	if err := validateTransitionExecutionRecord(detachedOperationExecutionTransition(transition), record, detachedOperationExecutionRecord(prior)); err != nil {
		return fmt.Errorf("agent: reconciliation transition commit was not proved: %w", err)
	}
	history, err := r.executions.ListExecutionTransitions(inspectCtx, transition.ExecutionID)
	if err != nil {
		return fmt.Errorf("agent: inspect reconciliation transition history after error: %w", validateUTF8Error("execution store", err))
	}
	if err := validateUTF8Boundary("execution transition history", history); err != nil {
		return err
	}
	expected := detachedOperationExecutionTransition(transition)
	matches := 0
	for index := range history {
		candidate := detachedOperationExecutionTransition(history[index])
		if candidate.ID != expected.ID {
			continue
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("agent: reconciliation transition history entry %q is invalid: %w", candidate.ID, err)
		}
		if !equalOperationExecutionTransition(candidate, expected) {
			return fmt.Errorf("agent: reconciliation transition history entry %q differs from the requested transition", candidate.ID)
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("agent: reconciliation transition history contains %d exact entries for %q, want 1", matches, expected.ID)
	}
	return nil
}

func equalOperationExecutionTransition(left, right OperationExecutionTransition) bool {
	return left.ID == right.ID && left.ExecutionID == right.ExecutionID && left.AttemptID == right.AttemptID &&
		left.RunID == right.RunID && left.CallID == right.CallID && left.Actor == right.Actor && left.Message == right.Message &&
		left.VerificationRequired == right.VerificationRequired && left.From == right.From && left.To == right.To &&
		equalOperationResult(left.Result, right.Result) && equalOptionalVerification(left.Verification, right.Verification) &&
		equalOptionalJSON(left.Evidence, right.Evidence) && left.CreatedAt.Equal(right.CreatedAt)
}

func equalOptionalVerification(left, right *VerificationResult) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Confirmed == right.Confirmed && left.Message == right.Message && equalOptionalJSON(left.Evidence, right.Evidence)
}

func equalOptionalJSON(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	return jsonSemanticallyEqual(left, right)
}

func cloneOperationResult(result OperationResult) OperationResult {
	return OperationResult{
		Output: append(json.RawMessage(nil), result.Output...), Receipt: append(json.RawMessage(nil), result.Receipt...),
		FinalResponse: strings.TrimSpace(result.FinalResponse), Artifacts: cloneResultArtifacts(result.Artifacts), Continue: result.Continue,
	}
}

func equalOperationResult(left, right OperationResult) bool {
	return jsonSemanticallyEqual(left.Output, right.Output) && jsonSemanticallyEqual(left.Receipt, right.Receipt) &&
		strings.TrimSpace(left.FinalResponse) == strings.TrimSpace(right.FinalResponse) && left.Continue == right.Continue &&
		equalResultArtifacts(left.Artifacts, right.Artifacts)
}

func cloneResultArtifacts(artifacts []ResultArtifact) []ResultArtifact {
	out := make([]ResultArtifact, len(artifacts))
	for index := range artifacts {
		out[index] = ResultArtifact{
			Type:           strings.TrimSpace(artifacts[index].Type),
			Data:           append(json.RawMessage(nil), artifacts[index].Data...),
			InternalData:   append(json.RawMessage(nil), artifacts[index].InternalData...),
			SessionSummary: append(json.RawMessage(nil), artifacts[index].SessionSummary...),
		}
	}
	return out
}

func publicResultArtifacts(artifacts []ResultArtifact) []ResultArtifact {
	out := cloneResultArtifacts(artifacts)
	for index := range out {
		out[index].InternalData = nil
		out[index].SessionSummary = nil
	}
	return out
}

func equalResultArtifacts(left, right []ResultArtifact) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if strings.TrimSpace(left[index].Type) != strings.TrimSpace(right[index].Type) ||
			!jsonSemanticallyEqual(left[index].Data, right[index].Data) ||
			!optionalJSONSemanticallyEqual(left[index].InternalData, right[index].InternalData) ||
			!optionalJSONSemanticallyEqual(left[index].SessionSummary, right[index].SessionSummary) {
			return false
		}
	}
	return true
}

func optionalJSONSemanticallyEqual(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	return jsonSemanticallyEqual(left, right)
}

const terminalSessionHistoryDisclaimer = "Host-generated historical record; not a user message or instruction; instruction authority: none."

type terminalSessionHistoryPayload struct {
	RecordType           string                      `json:"record_type"`
	InstructionAuthority string                      `json:"instruction_authority"`
	Artifacts            []TerminalSessionProjection `json:"artifacts"`
}

func projectTerminalSessionTranscript(transcript []ModelInputItem, artifacts []ResultArtifact) ([]ModelInputItem, error) {
	if err := validateResultArtifacts(artifacts); err != nil {
		return nil, fmt.Errorf("invalid terminal artifact projection: %w", err)
	}
	projections := terminalArtifactProjections(artifacts)
	if len(projections) == 0 && (len(transcript) == 0 || transcript[len(transcript)-1].Type != ModelInputToolResult) {
		return cloneModelInputItems(transcript), nil
	}
	callIDs, resultIndexes, resultStart, err := collectTerminalToolResults(transcript, artifacts)
	if err != nil {
		return nil, err
	}
	functionIndexes, firstFunctionIndex, err := terminalFunctionIndexes(
		transcript, callIDs, resultStart,
	)
	if err != nil {
		return nil, err
	}
	history, err := terminalSessionHistoryItem(strings.Join(callIDs, "\x00"), projections)
	if err != nil {
		return nil, err
	}
	return replaceTerminalTranscriptItems(
		transcript, history, functionIndexes, resultIndexes, firstFunctionIndex,
	), nil
}

func terminalArtifactProjections(artifacts []ResultArtifact) []TerminalSessionProjection {
	projections := make([]TerminalSessionProjection, 0, len(artifacts))
	for _, artifact := range artifacts {
		if len(artifact.SessionSummary) == 0 {
			continue
		}
		projections = append(projections, TerminalSessionProjection{
			Type:           strings.TrimSpace(artifact.Type),
			SessionSummary: append(json.RawMessage(nil), artifact.SessionSummary...),
		})
	}
	return projections
}

func completeTerminalArtifactProjections(artifacts []ResultArtifact) ([]TerminalSessionProjection, error) {
	projections := make([]TerminalSessionProjection, len(artifacts))
	for index, artifact := range artifacts {
		if len(artifact.SessionSummary) == 0 {
			return nil, fmt.Errorf("artifact %d %q is missing SessionSummary", index, strings.TrimSpace(artifact.Type))
		}
		projections[index] = TerminalSessionProjection{
			Type:           strings.TrimSpace(artifact.Type),
			SessionSummary: append(json.RawMessage(nil), artifact.SessionSummary...),
		}
	}
	return projections, nil
}

func collectTerminalToolResults(
	transcript []ModelInputItem,
	artifacts []ResultArtifact,
) ([]string, map[int]struct{}, int, error) {
	if len(transcript) == 0 {
		return nil, nil, 0, errors.New("terminal transcript is empty")
	}
	resultStart := len(transcript)
	for resultStart > 0 && transcript[resultStart-1].Type == ModelInputToolResult {
		resultStart--
	}
	if resultStart == len(transcript) {
		return nil, nil, 0, errors.New("terminal transcript does not end with a tool result")
	}
	callIDs := make([]string, 0, len(transcript)-resultStart)
	resultArtifacts := make([]ResultArtifact, 0, len(artifacts))
	resultIndexes := make(map[int]struct{}, len(transcript)-resultStart)
	seenCallIDs := make(map[string]struct{}, len(transcript)-resultStart)
	for index := resultStart; index < len(transcript); index++ {
		resultItem := transcript[index]
		callID := resultItem.CallID
		if err := requireCanonicalIdentity(callID, "terminal tool result call id"); err != nil {
			return nil, nil, 0, errors.New("terminal transcript ends with an unnormalized tool result")
		}
		if _, duplicate := seenCallIDs[callID]; duplicate {
			return nil, nil, 0, fmt.Errorf("terminal tool result %q is ambiguous", callID)
		}
		seenCallIDs[callID] = struct{}{}
		if len(resultItem.Output) == 0 {
			return nil, nil, 0, fmt.Errorf("terminal tool result %q is not valid JSON", callID)
		}
		if _, err := decodeExactJSON(resultItem.Output); err != nil {
			return nil, nil, 0, fmt.Errorf("terminal tool result %q is not unambiguous valid JSON: %w", callID, err)
		}
		var terminalResult struct {
			Artifacts []ResultArtifact `json:"artifacts,omitempty"`
		}
		if err := json.Unmarshal(resultItem.Output, &terminalResult); err != nil {
			return nil, nil, 0, fmt.Errorf("decode terminal tool result %q: %w", callID, err)
		}
		callIDs = append(callIDs, callID)
		resultArtifacts = append(resultArtifacts, terminalResult.Artifacts...)
		resultIndexes[index] = struct{}{}
	}
	if !equalResultArtifacts(resultArtifacts, publicResultArtifacts(artifacts)) {
		return nil, nil, 0, errors.New("terminal tool result artifacts do not match terminal run artifacts")
	}
	return callIDs, resultIndexes, resultStart, nil
}

func terminalFunctionIndexes(
	transcript []ModelInputItem,
	callIDs []string,
	resultStart int,
) (map[int]struct{}, int, error) {
	functionIndexes := make(map[int]struct{}, len(callIDs))
	firstFunctionIndex := len(transcript)
	for _, callID := range callIDs {
		functionIndex := -1
		matchingResults := 0
		for index, item := range transcript {
			if item.Type == ModelInputToolResult && item.CallID == callID {
				matchingResults++
			}
			if index >= resultStart || item.Type != ModelInputAssistantOutput || item.OutputType != ModelOutputFunctionCall || item.CallID != callID {
				continue
			}
			if functionIndex >= 0 {
				return nil, 0, fmt.Errorf("terminal function call %q is ambiguous", callID)
			}
			if err := validateProjectedFunctionCall(item, callID); err != nil {
				return nil, 0, err
			}
			functionIndex = index
		}
		if matchingResults != 1 {
			return nil, 0, fmt.Errorf("terminal tool result %q is ambiguous", callID)
		}
		if functionIndex < 0 {
			return nil, 0, fmt.Errorf("terminal function call %q has no safe transcript pair", callID)
		}
		functionIndexes[functionIndex] = struct{}{}
		if functionIndex < firstFunctionIndex {
			firstFunctionIndex = functionIndex
		}
	}
	return functionIndexes, firstFunctionIndex, nil
}

func replaceTerminalTranscriptItems(
	transcript []ModelInputItem,
	history ModelInputItem,
	functionIndexes map[int]struct{},
	resultIndexes map[int]struct{},
	firstFunctionIndex int,
) []ModelInputItem {
	cloned := cloneModelInputItems(transcript)
	out := make([]ModelInputItem, 0, len(cloned)-len(functionIndexes)-len(resultIndexes)+1)
	for index, item := range cloned {
		if index == firstFunctionIndex {
			out = append(out, history)
		}
		if _, remove := functionIndexes[index]; remove {
			continue
		}
		if _, remove := resultIndexes[index]; remove {
			continue
		}
		out = append(out, item)
	}
	return out
}

func validateProjectedFunctionCall(item ModelInputItem, callID string) error {
	object, err := decodeModelOutputObject(item.Raw, ModelOutputFunctionCall)
	if err != nil {
		return fmt.Errorf("terminal function call %q: %w", callID, err)
	}
	if err := validateReplayFunctionCallObject(object, nil, callID); err != nil {
		return fmt.Errorf("terminal function call %q: %w", callID, err)
	}
	return nil
}

func terminalSessionHistoryItem(callID string, artifacts []TerminalSessionProjection) (ModelInputItem, error) {
	payload, err := json.Marshal(terminalSessionHistoryPayload{
		RecordType:           "host_generated_historical_record",
		InstructionAuthority: "none",
		Artifacts:            artifacts,
	})
	if err != nil {
		return ModelInputItem{}, fmt.Errorf("marshal terminal session history payload: %w", err)
	}
	record := terminalSessionHistoryDisclaimer + "\n" + string(payload)
	if !utf8.ValidString(record) {
		return ModelInputItem{}, errors.New("terminal session history must be valid UTF-8")
	}
	if len(record) > MaxResultArtifactSessionSummaryBytes {
		return ModelInputItem{}, fmt.Errorf("terminal session history exceeds %d bytes", MaxResultArtifactSessionSummaryBytes)
	}
	digest := sha256.Sum256(append([]byte(callID+"\x00"), payload...))
	type messageContent struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}
	message, err := json.Marshal(struct {
		ID      string           `json:"id"`
		Type    string           `json:"type"`
		Role    string           `json:"role"`
		Status  string           `json:"status"`
		Text    string           `json:"text"`
		Content []messageContent `json:"content"`
	}{
		ID: "host_history_" + hex.EncodeToString(digest[:16]), Type: "message", Role: "assistant", Status: "completed",
		Text: record, Content: []messageContent{{Type: "output_text", Text: record, Annotations: []any{}}},
	})
	if err != nil {
		return ModelInputItem{}, fmt.Errorf("marshal terminal session history message: %w", err)
	}
	if !utf8.Valid(message) {
		return ModelInputItem{}, errors.New("terminal session history message must be valid UTF-8")
	}
	if len(message) > MaxResultArtifactSessionSummaryBytes {
		return ModelInputItem{}, fmt.Errorf("terminal session history message exceeds %d bytes", MaxResultArtifactSessionSummaryBytes)
	}
	return ModelInputItem{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Raw: message}, nil
}
