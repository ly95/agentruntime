package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

func applyTerminalOperationResult(run *RunRecord, raw json.RawMessage) (string, error) {
	if _, err := decodeExactJSON(raw); err != nil {
		return "", fmt.Errorf("terminal operation result must be unambiguous valid JSON: %w", err)
	}
	var terminal struct {
		FinalResponse string           `json:"final_response"`
		Artifacts     []ResultArtifact `json:"artifacts,omitempty"`
	}
	if err := json.Unmarshal(raw, &terminal); err != nil {
		return "", err
	}
	for index, artifact := range terminal.Artifacts {
		if strings.TrimSpace(artifact.Type) == "" || len(artifact.Data) == 0 {
			return "", fmt.Errorf("terminal result artifact %d is invalid", index)
		}
		if _, err := decodeExactJSON(artifact.Data); err != nil {
			return "", fmt.Errorf("terminal result artifact %d is ambiguous or invalid: %w", index, err)
		}
	}
	if len(run.Artifacts) == 0 {
		run.Artifacts = cloneResultArtifacts(terminal.Artifacts)
	}
	return terminal.FinalResponse, nil
}

func terminalOperationContinues(raw json.RawMessage) (bool, error) {
	if _, err := decodeExactJSON(raw); err != nil {
		return false, fmt.Errorf("terminal operation result must be unambiguous valid JSON: %w", err)
	}
	var result struct {
		Continue bool `json:"continue"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return false, err
	}
	return result.Continue, nil
}

func cloneVerificationResult(result VerificationResult) VerificationResult {
	result.Evidence = append(json.RawMessage(nil), result.Evidence...)
	return result
}

func verificationPointer(result VerificationResult) *VerificationResult {
	cloned := cloneVerificationResult(result)
	return &cloned
}

func validateNonNullExactJSON(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("JSON value is required")
	}
	value, err := decodeExactJSON(raw)
	if err != nil {
		return fmt.Errorf("JSON value must be unambiguous and valid: %w", err)
	}
	if value == nil {
		return errors.New("JSON value must not be null")
	}
	return nil
}

func normalizePositiveVerificationResult(result VerificationResult) (VerificationResult, error) {
	result = cloneVerificationResult(result)
	if !utf8.ValidString(result.Message) {
		return result, errors.New("verification message must be valid UTF-8")
	}
	if err := validateUTF8Boundary("verification result", result); err != nil {
		return result, err
	}
	result.Message = strings.TrimSpace(result.Message)
	if !result.Confirmed {
		return result, errors.New("verification is not confirmed")
	}
	if len(bytes.TrimSpace(result.Evidence)) == 0 {
		return result, errors.New("verification evidence is required")
	}
	if err := validateNonNullExactJSON(result.Evidence); err != nil {
		return result, fmt.Errorf("verification evidence is invalid: %w", err)
	}
	return result, nil
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
		return fail(fmt.Errorf("%w: operation %q: %w", ErrVerificationFailed, req.Operation.Name, validateUTF8Error("result verifier", err)), nil)
	}
	verification, validationErr := normalizePositiveVerificationResult(verification)
	if len(verification.Evidence) > 0 {
		if _, err := decodeExactJSON(verification.Evidence); err != nil {
			return fail(fmt.Errorf("%w: operation %q verifier returned invalid evidence JSON: %v", ErrVerificationFailed, req.Operation.Name, err), nil)
		}
	}
	data, err := json.Marshal(verification)
	if err != nil {
		return fail(fmt.Errorf("agent: marshal verification result for %q: %w", req.Operation.Name, err), nil)
	}
	itemID, err := r.nextGeneratedID(ctx, "verification item id")
	if err != nil {
		return fail(err, data)
	}
	item := ItemRecord{ID: itemID, RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeVerification, CallID: req.Call.ID, ExecutionID: req.ExecutionID, AttemptID: req.AttemptID, Name: req.Operation.Name, Data: data, CreatedAt: r.now()}
	if validationErr != nil {
		item.Error = verification.Message
	}
	if err := r.appendItem(ctx, item); err != nil {
		return fail(fmt.Errorf("agent: append verification audit for %q: %w", req.Operation.Name, err), data)
	}
	if validationErr != nil {
		return fail(fmt.Errorf("%w: operation %q: %v", ErrVerificationFailed, req.Operation.Name, validationErr), data)
	}
	r.emit(Event{Type: EventVerificationCompleted, RunID: run.ID, SessionID: run.SessionID, Operation: req.Operation.Name, CallID: req.Call.ID, ExecutionID: req.ExecutionID, AttemptID: req.AttemptID, Data: data})
	return verification, nil
}

func (r *Runtime) enforceDecision(ctx context.Context, run *RunRecord, state *agentState, req OperationRequest, decision PolicyDecision, responseID string, modelOutput []ModelOutputItem, checkpoint *ApprovalCheckpoint) error {
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
			Checkpoint: cloneApprovalCheckpoint(checkpoint, false),
		}
		approverRequest, err := r.cloneApprovalRequest(approvalRequest)
		if err != nil {
			return fail(fmt.Errorf("agent: clone approval request for %q: %w", req.Operation.Name, err))
		}
		approval, err := r.approver.RequestApproval(ctx, approverRequest)
		if err != nil {
			return fail(fmt.Errorf("agent: request approval for %q: %w", req.Operation.Name, validateUTF8Error("approver", err)))
		}
		if err := validateUTF8Boundary("approval decision", approval); err != nil {
			return fail(err)
		}
		approval.ID = strings.TrimSpace(approval.ID)
		approval.Reason = strings.TrimSpace(approval.Reason)
		if approval.Pending && approval.ID == "" {
			return fail(fmt.Errorf("agent: approver returned an empty approval id for %q", req.Operation.Name))
		}
		if approval.Pending && r.runStore == nil {
			return fail(fmt.Errorf("%w: pending approval %s for %q requires a durable run store", ErrApprovalRequired, approval.ID, req.Operation.Name))
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
		itemID, err := r.nextGeneratedID(ctx, "approval item id")
		if err != nil {
			return fail(err)
		}
		audit := ItemRecord{ID: itemID, RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeApproval, CallID: req.Call.ID, ExecutionID: req.ExecutionID, Name: req.Operation.Name, Data: data, CreatedAt: r.now()}
		if approval.Pending && r.runStore != nil {
			if state.pendingApproval != nil {
				return fail(fmt.Errorf("agent: run %s already has an uncommitted approval", run.ID))
			}
			persistentRequest, err := r.clonePersistentApprovalRequest(approvalRequest)
			if err != nil {
				return fail(fmt.Errorf("agent: sanitize pending approval for %q: %w", req.Operation.Name, err))
			}
			pending := PendingApprovalCommit{
				AuthorityVersion: pendingApprovalAuthorityVersion,
				Request:          persistentRequest, Decision: approval, Audit: audit,
			}
			digest, err := pendingApprovalAuthorityDigest(pending)
			if err != nil {
				return fail(err)
			}
			pending.Digest = digest
			state.pendingApproval = &pending
			state.pendingApprovalDigest = digest
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

func (r *Runtime) completeModel(ctx context.Context, runID, sessionID string, state *agentState, req ModelRequest) (*ModelResponse, string, error) {
	modelCallID, err := r.nextGeneratedID(ctx, "model call id")
	if err != nil {
		return nil, "", err
	}
	if err := validateModelInputItemsForReplay(req.Input); err != nil {
		return nil, modelCallID, correlateModelCallError(modelCallID, fmt.Errorf("%w: model request transcript is invalid: %v", ErrInvalidModelOutput, err))
	}
	if err := validateContextTranscriptToolSequences(req.Input); err != nil {
		return nil, modelCallID, correlateModelCallError(modelCallID, fmt.Errorf("%w: model request transcript sequence is invalid: %v", ErrInvalidModelOutput, err))
	}
	auditRequest := cloneModelRequest(req)
	auditRequest.Input = clonePersistentModelInputItems(req.Input)
	persistentInstructions, err := persistentModelRequestInstructions(state, req.Instructions)
	if err != nil {
		return nil, "", err
	}
	auditRequest.Instructions = persistentInstructions
	reqData, err := json.Marshal(auditRequest)
	if err != nil {
		return nil, "", fmt.Errorf("agent: marshal model request: %w", err)
	}
	if err := r.appendItem(ctx, ItemRecord{ID: modelCallID, RunID: runID, SessionID: sessionID, Type: ItemTypeModelRequest, ModelCallID: modelCallID, Data: reqData, CreatedAt: r.now()}); err != nil {
		return nil, "", err
	}
	r.emit(Event{Type: EventModelStarted, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID})
	var streamMu sync.Mutex
	var streamValidationErr error
	if r.eventSink != nil {
		req.StreamSink = func(streamEvent ModelStreamEvent) {
			streamMu.Lock()
			defer streamMu.Unlock()
			if streamValidationErr != nil {
				return
			}
			chunk := streamEvent
			chunk.ModelCallID = modelCallID
			if err := validateUTF8Boundary("model stream event", chunk); err != nil {
				streamValidationErr = err
				return
			}
			r.emit(Event{Type: EventModelStreamChunk, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, CallID: chunk.CallID, ResponseID: chunk.ResponseID, Chunk: &chunk})
		}
	}
	req.ModelCallID = modelCallID
	resp, err := r.model.Complete(ctx, req)
	streamMu.Lock()
	streamErr := streamValidationErr
	streamMu.Unlock()
	if streamErr != nil {
		if err != nil {
			streamErr = errors.Join(streamErr, validateUTF8Error("model", err))
		}
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ErrorCode: errorCode(streamErr), Error: streamErr.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, streamErr)
	}
	if err != nil {
		err = validateUTF8Error("model", err)
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	if resp == nil {
		err := errors.New("agent: model returned nil response")
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	if err := validateUTF8Boundary("model response", resp); err != nil {
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	identity, err := validateModelResponseBeforeAudit(resp, req.Input, state, r.maxCallsPerTurn)
	if err != nil {
		err = fmt.Errorf("%w: model response preflight failed: %v", ErrInvalidModelOutput, err)
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		err = fmt.Errorf("agent: marshal model response: %w", err)
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	itemID, err := r.nextGeneratedID(ctx, "model response item id")
	if err != nil {
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID, ErrorCode: errorCode(err), Error: err.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, err)
	}
	appendErr := r.appendItem(ctx, ItemRecord{ID: itemID, RunID: runID, SessionID: sessionID, Type: ItemTypeModelResponse, ModelCallID: modelCallID, ResponseID: resp.ID, Data: respData, CreatedAt: r.now()})
	if appendErr != nil {
		r.emit(Event{Type: EventModelFailed, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID, ErrorCode: errorCode(appendErr), Error: appendErr.Error()})
		return nil, modelCallID, correlateModelCallError(modelCallID, appendErr)
	}
	recordAuditedModelResponseIdentity(state, identity)
	r.emit(Event{Type: EventModelCompleted, RunID: runID, SessionID: sessionID, ModelCallID: modelCallID, ResponseID: resp.ID})
	return resp, modelCallID, nil
}

type modelResponseIdentity struct {
	responseID      string
	providerItemIDs []string
}

func validateModelResponseBeforeAudit(response *ModelResponse, retained []ModelInputItem, state *agentState, maximumCalls int) (modelResponseIdentity, error) {
	if err := validateModelResponseToolCallLimit(response.Items, maximumCalls); err != nil {
		return modelResponseIdentity{}, err
	}
	if err := validateModelResponseReplayIdentity(response); err != nil {
		return modelResponseIdentity{}, fmt.Errorf("structured identity is invalid: %w", err)
	}
	priorCallIDs := transcriptCallIDs(retained)
	if state != nil {
		for callID := range state.seenCallIDs {
			priorCallIDs[callID] = struct{}{}
		}
	}
	if _, err := responseToolCalls(response, priorCallIDs, maximumCalls); err != nil {
		return modelResponseIdentity{}, err
	}

	seenResponseIDs := make(map[string]struct{})
	seenProviderItemIDs := make(map[string]struct{})
	if state != nil {
		for responseID := range state.seenResponseIDs {
			seenResponseIDs[responseID] = struct{}{}
		}
		for itemID := range state.seenProviderItemIDs {
			seenProviderItemIDs[itemID] = struct{}{}
		}
	}
	for index, item := range retained {
		if item.Type != ModelInputAssistantOutput {
			continue
		}
		if item.ResponseID != "" {
			seenResponseIDs[item.ResponseID] = struct{}{}
		}
		itemID, err := replayProviderItemID(item.Raw)
		if err != nil {
			return modelResponseIdentity{}, fmt.Errorf("retained model input item %d: %w", index, err)
		}
		if itemID != "" {
			seenProviderItemIDs[itemID] = struct{}{}
		}
	}
	if _, exists := seenResponseIDs[response.ID]; exists {
		return modelResponseIdentity{}, fmt.Errorf("model response id %q is reused from retained response authority", response.ID)
	}
	identity := modelResponseIdentity{responseID: response.ID}
	for index, item := range response.Items {
		itemID, err := replayProviderItemID(item.Raw)
		if err != nil {
			return modelResponseIdentity{}, fmt.Errorf("model response output item %d: %w", index, err)
		}
		if item.ID != "" {
			itemID = item.ID
		}
		if itemID == "" {
			continue
		}
		if _, exists := seenProviderItemIDs[itemID]; exists {
			return modelResponseIdentity{}, fmt.Errorf("provider item id %q is reused from retained response authority", itemID)
		}
		identity.providerItemIDs = append(identity.providerItemIDs, itemID)
	}
	return identity, nil
}

func recordAuditedModelResponseIdentity(state *agentState, identity modelResponseIdentity) {
	if state.seenResponseIDs == nil {
		state.seenResponseIDs = make(map[string]struct{})
	}
	if state.seenProviderItemIDs == nil {
		state.seenProviderItemIDs = make(map[string]struct{})
	}
	state.seenResponseIDs[identity.responseID] = struct{}{}
	for _, itemID := range identity.providerItemIDs {
		state.seenProviderItemIDs[itemID] = struct{}{}
	}
}

func restoreModelResponseIdentityLedger(state *agentState, lastResponseID string, transcript []ModelInputItem) error {
	if lastResponseID != "" {
		if err := requireCanonicalIdentity(lastResponseID, "persisted last response id"); err != nil {
			return err
		}
		state.seenResponseIDs[lastResponseID] = struct{}{}
	}
	for index, item := range transcript {
		if item.Type != ModelInputAssistantOutput {
			continue
		}
		if item.ResponseID != "" {
			state.seenResponseIDs[item.ResponseID] = struct{}{}
		}
		itemID, err := replayProviderItemID(item.Raw)
		if err != nil {
			return fmt.Errorf("persisted model input item %d: %w", index, err)
		}
		if itemID != "" {
			state.seenProviderItemIDs[itemID] = struct{}{}
		}
	}
	return nil
}

func validateModelResponseToolCallLimit(items []ModelOutputItem, maximum int) error {
	count := 0
	for _, item := range items {
		if item.Type != ModelOutputFunctionCall {
			continue
		}
		count++
		if count > maximum {
			return fmt.Errorf("%w: model response exceeds the maximum of %d function calls", ErrInvalidModelOutput, maximum)
		}
	}
	return nil
}

func persistentModelRequestInstructions(state *agentState, instructions string) (string, error) {
	if state == nil {
		return "", errors.New("agent: model request has no runtime state")
	}
	suffix, ok := strings.CutPrefix(instructions, state.instructions)
	if !ok {
		return "", errors.New("agent: model request instructions do not match runtime state")
	}
	return state.persistentInstructions + suffix, nil
}

func (r *Runtime) appendUserItem(ctx context.Context, runID string, input Input) error {
	persistent, err := clonePersistentOperationInput(input)
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"text": persistent.User, "attachments": persistent.Attachments, "metadata": persistent.Metadata})
	if err != nil {
		return err
	}
	itemID, err := r.nextGeneratedID(ctx, "user message item id")
	if err != nil {
		return err
	}
	return r.appendItem(ctx, ItemRecord{ID: itemID, RunID: runID, SessionID: input.SessionID, Type: ItemTypeUserMessage, Data: data, CreatedAt: r.now()})
}

func (r *Runtime) sessionForRun(state *agentState, runID string, cause error) *SessionState {
	if state.sessionID == "" || !state.sessionReady {
		return nil
	}
	handle := state.lease.Handle()
	operationSetID := r.operationSetID
	if cause != nil && state.loadedOperationSetID == "" {
		operationSetID = ""
	}
	session := &SessionState{
		ID: state.sessionID, SkillSetID: r.skillSetID, OperationSetID: operationSetID, Revision: handle.SessionRevision + 1,
		Transcript:     clonePersistentModelInputItems(state.transcript),
		Checkpoint:     cloneContextCheckpoint(state.checkpoint),
		SeenCallIDs:    sortedCallIDs(transcriptCallIDs(state.transcript)),
		LastResponseID: state.lastResponseID, LastRunID: runID,
		CreatedAt: state.createdAt, UpdatedAt: r.now(),
	}
	if cause != nil {
		session.LastError = cause.Error()
	}
	return session
}

func (r *Runtime) appendItem(ctx context.Context, item ItemRecord) error {
	if err := validateUTF8Boundary("audit item", item); err != nil {
		return err
	}
	if len(item.Data) == 0 {
		return fmt.Errorf("agent: audit item %q data must be valid JSON", item.Type)
	}
	if _, err := decodeExactJSON(item.Data); err != nil {
		return fmt.Errorf("agent: audit item %q data must be valid JSON and unambiguous: %w", item.Type, err)
	}
	if r.runStore == nil {
		return nil
	}
	item.Data = append(json.RawMessage(nil), item.Data...)
	return validateUTF8Error("run store", r.runStore.AppendItem(ctx, item))
}

func (r *Runtime) completeRun(ctx context.Context, run RunRecord, state *agentState, output string) (*Result, error) {
	run.Status = RunStatusCompleted
	run.PendingApprovalDigest = ""
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
		storedRun, err := clonePersistentRunRecord(run)
		if err != nil {
			return nil, err
		}
		if err := r.runStore.FinishRun(ctx, FinishRunRequest{Handle: state.lease.Handle(), Run: storedRun, Session: session}); err != nil {
			return nil, err
		}
	}
	r.emit(Event{Type: EventRunCompleted, RunID: run.ID, SessionID: run.SessionID, ResponseID: state.lastResponseID, Text: output})
	return &Result{RunID: run.ID, SessionID: run.SessionID, Status: run.Status, LastResponseID: state.lastResponseID, Output: output}, nil
}

func (r *Runtime) waitRun(ctx context.Context, run RunRecord, state *agentState) (*Result, error) {
	run.Status = RunStatusWaitingUser
	if state.pendingApproval != nil {
		state.pendingApprovalDigest = state.pendingApproval.Digest
	}
	run.PendingApprovalDigest = state.pendingApprovalDigest
	run.Error = ""
	run.UpdatedAt = r.now()
	if r.runStore != nil {
		var session *SessionState
		// A fresh pending approval atomically commits the revision recorded in
		// its checkpoint. A pure pending poll must not advance that revision
		// while retaining the same approval authority.
		if state.pendingApproval != nil && r.commitsNewWaitingApprovalSession(state) {
			session = r.sessionForRun(state, run.ID, nil)
		}
		storedRun, err := clonePersistentRunRecord(run)
		if err != nil {
			return nil, err
		}
		if err := r.runStore.FinishRun(ctx, FinishRunRequest{
			Handle: state.lease.Handle(), Run: storedRun, Session: session,
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
	cause = validateUTF8Error("runtime dependency", cause)
	run.PendingApprovalDigest = ""
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
		itemID, idErr := r.nextGeneratedID(ctx, "error item id")
		if idErr != nil {
			cause = errors.Join(cause, idErr)
		} else {
			cleanupCtx, cancel := r.detachedCleanupContext(ctx)
			err := r.appendItem(cleanupCtx, ItemRecord{ID: itemID, RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeError, ModelCallID: modelCallID, CallID: callID, ExecutionID: executionID, AttemptID: attemptID, Data: data, Error: run.Error, CreatedAt: r.now()})
			cancel()
			if err != nil {
				cause = errors.Join(cause, err)
			}
		}
	}
	run.Error = cause.Error()
	if r.runStore != nil {
		session := r.sessionForRun(state, run.ID, cause)
		// A failed run must not turn a legacy empty operation binding into any
		// new session snapshot. Passing nil preserves the exact loaded session
		// while still allowing FinishRun to terminalize and release its lease.
		if state.sessionID != "" && r.operationSetID != "" && state.loadedOperationSetID == "" {
			session = nil
		}
		cleanupCtx, cancel := r.detachedCleanupContext(ctx)
		storedRun, cloneErr := clonePersistentRunRecord(run)
		if cloneErr != nil {
			cause = errors.Join(cause, cloneErr)
		}
		err := cloneErr
		if cloneErr == nil {
			err = r.runStore.FinishRun(cleanupCtx, FinishRunRequest{Handle: state.lease.Handle(), Run: storedRun, Session: session})
			err = validateUTF8Error("run store", err)
		}
		cancel()
		if err != nil {
			cause = errors.Join(cause, err)
		}
	}
	r.emit(Event{Type: eventType, RunID: run.ID, SessionID: run.SessionID, ModelCallID: modelCallID, CallID: callID, ExecutionID: executionID, AttemptID: attemptID, ErrorCode: run.ErrorCode, Error: cause.Error()})
	return cause
}

func (r *Runtime) detachedCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.cleanupTimeout
	if timeout <= 0 {
		timeout = defaultDetachedCleanupTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (r *Runtime) emit(event Event) {
	if r.eventSink != nil {
		event.Data = append(json.RawMessage(nil), event.Data...)
		event.ApprovalPreview = append(json.RawMessage(nil), event.ApprovalPreview...)
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

func clonePersistentRunRecord(run RunRecord) (RunRecord, error) {
	out := run
	input, err := clonePersistentOperationInput(run.Input)
	if err != nil {
		return RunRecord{}, err
	}
	out.Input = input
	out.Artifacts = cloneResultArtifacts(run.Artifacts)
	return out, nil
}
