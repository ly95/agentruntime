package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func applyTerminalOperationResult(run *RunRecord, raw json.RawMessage) (string, error) {
	var terminal struct {
		FinalResponse string           `json:"final_response"`
		Artifacts     []ResultArtifact `json:"artifacts,omitempty"`
	}
	if err := json.Unmarshal(raw, &terminal); err != nil {
		return "", err
	}
	for index, artifact := range terminal.Artifacts {
		if strings.TrimSpace(artifact.Type) == "" || len(artifact.Data) == 0 || !json.Valid(artifact.Data) {
			return "", fmt.Errorf("terminal result artifact %d is invalid", index)
		}
	}
	if len(run.Artifacts) == 0 {
		run.Artifacts = cloneResultArtifacts(terminal.Artifacts)
	}
	return strings.TrimSpace(terminal.FinalResponse), nil
}

func terminalOperationContinues(raw json.RawMessage) (bool, error) {
	var result struct {
		Continue bool `json:"continue"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return false, err
	}
	return result.Continue, nil
}

func jsonSemanticallyEqual(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	var leftValue any
	var rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftCanonical, leftErr := json.Marshal(leftValue)
	rightCanonical, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func cloneVerificationResult(result VerificationResult) VerificationResult {
	result.Evidence = append(json.RawMessage(nil), result.Evidence...)
	return result
}

func verificationPointer(result VerificationResult) *VerificationResult {
	cloned := cloneVerificationResult(result)
	return &cloned
}

func (r *Runtime) verifyOperation(ctx context.Context, run *RunRecord, req OperationRequest, result OperationResult, output any) (VerificationResult, error) {
	if r.verifier == nil {
		return VerificationResult{}, fmt.Errorf("%w: operation %q", ErrVerifierRequired, req.Operation.Name)
	}
	fail := func(cause error, data json.RawMessage) (VerificationResult, error) {
		r.emit(Event{
			Type: EventVerificationFailed, RunID: run.ID, SessionID: run.SessionID,
			Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID, AttemptID: req.AttemptID,
			Data: data, ErrorCode: errorCode(cause), Error: cause.Error(),
		})
		return VerificationResult{}, cause
	}
	r.emit(Event{
		Type: EventVerificationStarted, RunID: run.ID, SessionID: run.SessionID,
		Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID, AttemptID: req.AttemptID,
		Text: req.Operation.Confirmation.Description,
	})
	verification, err := r.verifier.Verify(ctx, VerificationRequest{Operation: req, Result: result, Output: output})
	if err != nil {
		return fail(fmt.Errorf("%w: operation %q: %w", ErrVerificationFailed, req.Operation.Name, err), nil)
	}
	verification = cloneVerificationResult(verification)
	verification.Message = strings.TrimSpace(verification.Message)
	if len(verification.Evidence) > 0 && !json.Valid(verification.Evidence) {
		return fail(fmt.Errorf("%w: operation %q verifier returned invalid evidence JSON", ErrVerificationFailed, req.Operation.Name), nil)
	}
	data, err := json.Marshal(verification)
	if err != nil {
		return fail(fmt.Errorf("agent: marshal verification result for %q: %w", req.Operation.Name, err), nil)
	}
	item := ItemRecord{ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeVerification, CallID: req.Call.ID, ExecutionID: req.ExecutionID, AttemptID: req.AttemptID, Name: req.Operation.Name, Data: data, CreatedAt: r.now()}
	if !verification.Confirmed {
		item.Error = verification.Message
	}
	if err := r.appendItem(ctx, item); err != nil {
		return fail(fmt.Errorf("agent: append verification audit for %q: %w", req.Operation.Name, err), data)
	}
	if !verification.Confirmed {
		return fail(fmt.Errorf("%w: operation %q: %s", ErrVerificationFailed, req.Operation.Name, verification.Message), data)
	}
	r.emit(Event{Type: EventVerificationCompleted, RunID: run.ID, SessionID: run.SessionID, Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID, AttemptID: req.AttemptID, Data: data})
	return verification, nil
}

func (r *Runtime) cloneApprovalRequest(request ApprovalRequest) (ApprovalRequest, error) {
	cloned := request
	cloned.ModelOutput = cloneModelOutputItems(request.ModelOutput)
	cloned.Preview = append(json.RawMessage(nil), request.Preview...)
	cloned.Operation = request.Operation
	input, err := cloneOperationInput(request.Operation.Input)
	if err != nil {
		return ApprovalRequest{}, err
	}
	cloned.Operation.Input = input
	cloned.Operation.Operation = cloneOperationSummaries([]OperationSummary{request.Operation.Operation})[0]
	cloned.Operation.Call.Input = append(json.RawMessage(nil), request.Operation.Call.Input...)
	arguments, err := json.Marshal(request.Operation.Arguments)
	if err != nil {
		return ApprovalRequest{}, err
	}
	cloned.Operation.Arguments, err = r.operations.DecodeInput(request.Operation.Call.Name, arguments)
	if err != nil {
		return ApprovalRequest{}, err
	}
	return cloned, nil
}

func (r *Runtime) enforceDecision(ctx context.Context, run *RunRecord, state *agentState, req OperationRequest, decision PolicyDecision, responseID string, modelOutput []ModelOutputItem) error {
	switch decision.Action {
	case PolicyAllow:
		return nil
	case PolicyDeny:
		return fmt.Errorf("%w: %s: %s", ErrOperationDenied, req.Operation.Name, decision.Reason)
	case PolicyRequireApproval:
		if r.approver == nil {
			return fmt.Errorf("%w: %s: %s", ErrApprovalRequired, req.Operation.Name, decision.Reason)
		}
		fail := func(cause error) error {
			r.emit(Event{
				Type: EventApprovalFailed, RunID: run.ID, SessionID: run.SessionID,
				Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID,
				ErrorCode: errorCode(cause), Error: cause.Error(),
			})
			return cause
		}
		preview, err := r.operations.BuildApprovalPreview(req.Operation.Name, req.Arguments)
		if err != nil {
			return fail(err)
		}
		r.emit(Event{
			Type: EventApprovalRequested, RunID: run.ID, SessionID: run.SessionID,
			Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID,
			Text: decision.Reason, ApprovalPreview: preview,
		})
		approvalRequest := ApprovalRequest{
			Operation: req, Reason: decision.Reason, ResponseID: responseID,
			ModelOutput: cloneModelOutputItems(modelOutput), Preview: preview,
		}
		approverRequest, err := r.cloneApprovalRequest(approvalRequest)
		if err != nil {
			return fail(fmt.Errorf("agent: clone approval request for %q: %w", req.Operation.Name, err))
		}
		approval, err := r.approver.RequestApproval(ctx, approverRequest)
		if err != nil {
			return fail(fmt.Errorf("agent: request approval for %q: %w", req.Operation.Name, err))
		}
		approval.ID = strings.TrimSpace(approval.ID)
		approval.Reason = strings.TrimSpace(approval.Reason)
		if approval.Pending && approval.ID == "" {
			return fail(fmt.Errorf("agent: approver returned an empty approval id for %q", req.Operation.Name))
		}
		if approval.Pending {
			r.emit(Event{
				Type: EventApprovalRequested, RunID: run.ID, SessionID: run.SessionID,
				Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID,
				ApprovalID: approval.ID, Text: decision.Reason, ApprovalPreview: preview,
			})
		}
		data, err := json.Marshal(approval)
		if err != nil {
			return fail(fmt.Errorf("agent: marshal approval for %q: %w", req.Operation.Name, err))
		}
		audit := ItemRecord{ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeApproval, CallID: req.Call.ID, ExecutionID: req.ExecutionID, Name: req.Operation.Name, Data: data, CreatedAt: r.now()}
		if approval.Pending && r.runStore != nil {
			if state.pendingApproval != nil {
				return fail(fmt.Errorf("agent: run %s already has an uncommitted approval", run.ID))
			}
			state.pendingApproval = &PendingApprovalCommit{
				Request: approvalRequest, Decision: approval, Audit: audit,
			}
		} else if err := r.appendItem(ctx, audit); err != nil {
			return fail(fmt.Errorf("agent: append approval audit for %q: %w", req.Operation.Name, err))
		}
		if approval.Pending {
			return fmt.Errorf("%w: %s", ErrApprovalPending, approval.ID)
		}
		r.emit(Event{Type: EventApprovalCompleted, RunID: run.ID, SessionID: run.SessionID, Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID, ApprovalID: approval.ID, Data: data})
		if !approval.Approved {
			return fmt.Errorf("%w: %s: %s", ErrApprovalDenied, req.Operation.Name, approval.Reason)
		}
		return nil
	default:
		return fmt.Errorf("agent: policy returned invalid action %q for %s", decision.Action, req.Operation.Name)
	}
}

func (r *Runtime) completeModel(ctx context.Context, runID, sessionID string, req ModelRequest) (*ModelResponse, string, error) {
	modelCallID := r.newID()
	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("agent: marshal model request: %w", err)
	}
	if err := r.appendItem(ctx, ItemRecord{ID: modelCallID, RunID: runID, SessionID: sessionID, Type: ItemTypeModelRequest, ModelCallID: modelCallID, Data: reqData, CreatedAt: r.now()}); err != nil {
		return nil, "", err
	}
	r.emit(Event{Type: EventModelStarted, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID})
	if r.eventSink != nil {
		req.StreamSink = func(streamEvent ModelStreamEvent) {
			chunk := streamEvent
			chunk.ModelCallID = modelCallID
			r.emit(Event{Type: EventModelStreamChunk, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, CallID: chunk.CallID, ResponseID: chunk.ResponseID, Chunk: &chunk})
		}
	}
	req.ModelCallID = modelCallID
	resp, err := r.model.Complete(ctx, req)
	if err != nil {
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	if resp == nil {
		err := errors.New("agent: model returned nil response")
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		err = fmt.Errorf("agent: marshal model response: %w", err)
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	appendErr := r.appendItem(ctx, ItemRecord{ID: r.newID(), RunID: runID, SessionID: sessionID, Type: ItemTypeModelResponse, ModelCallID: modelCallID, ResponseID: resp.ID, Data: respData, CreatedAt: r.now()})
	if appendErr != nil {
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID, ErrorCode: errorCode(appendErr), Error: appendErr.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, appendErr)
	}
	r.emit(Event{Type: EventModelCompleted, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID})
	return resp, modelCallID, nil
}

func (r *Runtime) appendUserItem(ctx context.Context, runID string, input Input) error {
	data, err := json.Marshal(map[string]any{"text": input.User, "attachments": input.Attachments, "metadata": input.Metadata})
	if err != nil {
		return err
	}
	return r.appendItem(ctx, ItemRecord{ID: r.newID(), RunID: runID, SessionID: input.SessionID, Type: ItemTypeUserMessage, Data: data, CreatedAt: r.now()})
}

func (r *Runtime) sessionForRun(state *agentState, runID string, cause error) *SessionState {
	if state.sessionID == "" || !state.sessionReady {
		return nil
	}
	handle := state.lease.Handle()
	session := &SessionState{
		ID: state.sessionID, SkillSetID: r.skillSetID, Revision: handle.SessionRevision + 1,
		Transcript:     clonePersistentModelInputItems(state.transcript),
		Checkpoint:     cloneContextCheckpoint(state.checkpoint),
		SeenCallIDs:    sortedCallIDs(state.seenCallIDs),
		LastResponseID: state.lastResponseID, LastRunID: runID,
		CreatedAt: state.createdAt, UpdatedAt: r.now(),
	}
	if cause != nil {
		session.LastError = cause.Error()
	}
	return session
}

func (r *Runtime) appendItem(ctx context.Context, item ItemRecord) error {
	if len(item.Data) == 0 || !json.Valid(item.Data) {
		return fmt.Errorf("agent: audit item %q data must be valid JSON", item.Type)
	}
	if r.runStore == nil {
		return nil
	}
	item.Data = append(json.RawMessage(nil), item.Data...)
	return r.runStore.AppendItem(ctx, item)
}

func (r *Runtime) completeRun(ctx context.Context, run RunRecord, state *agentState, output string) (*Result, error) {
	run.Status = RunStatusCompleted
	run.Result = output
	run.UpdatedAt = r.now()
	if r.runStore != nil {
		if state.sessionID != "" {
			projected, err := projectTerminalSessionTranscript(state.transcript, run.Artifacts)
			if err != nil {
				return nil, fmt.Errorf("agent: project terminal session history: %w", err)
			}
			state.transcript = projected
		}
		session := r.sessionForRun(state, run.ID, nil)
		if err := r.runStore.FinishRun(ctx, FinishRunRequest{Handle: state.lease.Handle(), Run: run, Session: session}); err != nil {
			return nil, err
		}
	}
	r.emit(Event{Type: EventRunCompleted, RunID: run.ID, SessionID: run.SessionID, ResponseID: state.lastResponseID, Text: output})
	return &Result{RunID: run.ID, SessionID: run.SessionID, Status: run.Status, LastResponseID: state.lastResponseID, Output: output}, nil
}

func (r *Runtime) waitRun(ctx context.Context, run RunRecord, state *agentState) (*Result, error) {
	run.Status = RunStatusWaitingUser
	run.Error = ""
	run.UpdatedAt = r.now()
	if r.runStore != nil {
		var session *SessionState
		if r.skillSetID != "" {
			session = r.sessionForRun(state, run.ID, nil)
		}
		if err := r.runStore.FinishRun(ctx, FinishRunRequest{
			Handle: state.lease.Handle(), Run: run, Session: session,
			PendingApproval: state.pendingApproval,
		}); err != nil {
			return nil, err
		}
		state.pendingApproval = nil
	}
	r.emit(Event{Type: EventRunWaitingUser, RunID: run.ID, SessionID: run.SessionID})
	return &Result{RunID: run.ID, SessionID: run.SessionID, Status: run.Status}, nil
}

func (r *Runtime) failRun(ctx context.Context, run RunRecord, state *agentState, cause error) error {
	eventType := EventRunFailed
	switch {
	case errors.Is(cause, ErrRunCancelled):
		run.Status = RunStatusCancelled
		eventType = EventRunCancelled
	case errors.Is(cause, ErrRunInterrupted), errors.Is(cause, ErrOperationOutcomeUnknown):
		run.Status = RunStatusInterrupted
		eventType = EventRunInterrupted
	default:
		run.Status = RunStatusFailed
	}
	run.Error = cause.Error()
	run.ErrorCode = errorCode(cause)
	run.UpdatedAt = r.now()
	modelCallID := ""
	callID := ""
	executionID := ""
	attemptID := ""
	if modelErr, ok := errors.AsType[*modelCallError](cause); ok {
		modelCallID = modelErr.modelCallID
	}
	if operationErr, ok := errors.AsType[*operationCallError](cause); ok {
		callID = operationErr.callID
		executionID = operationErr.executionID
		attemptID = operationErr.attemptID
	}
	data, marshalErr := json.Marshal(map[string]string{"error": cause.Error()})
	if marshalErr != nil {
		cause = errors.Join(cause, marshalErr)
	} else {
		cleanupCtx, cancel := r.detachedCleanupContext(ctx)
		err := r.appendItem(cleanupCtx, ItemRecord{ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeError, ModelCallID: modelCallID, CallID: callID, ExecutionID: executionID, AttemptID: attemptID, Data: data, Error: run.Error, CreatedAt: r.now()})
		cancel()
		if err != nil {
			cause = errors.Join(cause, err)
		}
	}
	run.Error = cause.Error()
	if r.runStore != nil {
		session := r.sessionForRun(state, run.ID, cause)
		cleanupCtx, cancel := r.detachedCleanupContext(ctx)
		err := r.runStore.FinishRun(cleanupCtx, FinishRunRequest{Handle: state.lease.Handle(), Run: run, Session: session})
		cancel()
		if err != nil {
			cause = errors.Join(cause, err)
		}
	}
	r.emit(Event{Type: eventType, RunID: run.ID, SessionID: run.SessionID, ModelCallID: modelCallID, CallID: callID, ExecutionID: executionID, AttemptID: attemptID, ErrorCode: run.ErrorCode, Error: cause.Error()})
	return cause
}

func (r *Runtime) detachedCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), r.cleanupTimeout)
}

func (r *Runtime) emit(event Event) {
	if r.eventSink != nil {
		event.Data = append(json.RawMessage(nil), event.Data...)
		if event.Chunk != nil {
			chunk := *event.Chunk
			if event.Chunk.SequenceNumber != nil {
				sequence := *event.Chunk.SequenceNumber
				chunk.SequenceNumber = &sequence
			}
			if event.Chunk.OutputIndex != nil {
				index := *event.Chunk.OutputIndex
				chunk.OutputIndex = &index
			}
			event.Chunk = &chunk
		}
		r.eventSink(event)
	}
}
