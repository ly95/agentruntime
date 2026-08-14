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
	writeHashField(digest, []byte(input.IdempotencyScope))
	writeHashField(digest, []byte(input.SessionID))
	writeHashField(digest, []byte(input.IdempotencyKey))
	return "req_" + hex.EncodeToString(digest.Sum(nil))
}

func operationExecutionID(requestID string, batchIndex, stepIndex uint64, operation string, arguments json.RawMessage) string {
	digest := sha256.New()
	writeHashField(digest, []byte(requestID))
	writeHashField(digest, []byte(strconv.FormatUint(batchIndex, 10)))
	writeHashField(digest, []byte(strconv.FormatUint(stepIndex, 10)))
	writeHashField(digest, []byte(operation))
	writeHashField(digest, arguments)
	return "op_" + hex.EncodeToString(digest.Sum(nil))
}

func terminalOperationExecutionID(runID, callID, operation string, arguments json.RawMessage) string {
	digest := sha256.New()
	writeHashField(digest, []byte(runID))
	writeHashField(digest, []byte(callID))
	writeHashField(digest, []byte(operation))
	writeHashField(digest, arguments)
	return "terminal_op_" + hex.EncodeToString(digest.Sum(nil))
}

func (r *Runtime) transitionOperationFailure(ctx context.Context, ref operationExecutionRef, target OperationExecutionStatus, cause error) error {
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
	cleanupCtx, cancel := r.detachedCleanupContext(ctx)
	defer cancel()
	_, err := r.executions.TransitionExecution(cleanupCtx, OperationExecutionTransition{
		ID: r.newID(), ExecutionID: ref.executionID, AttemptID: ref.attemptID,
		RunID: ref.runID, CallID: ref.callID, Actor: "runtime", Message: cause.Error(),
		From: OperationExecutionStarted, To: target, CreatedAt: r.now(),
	})
	if err != nil {
		return errors.Join(result, fmt.Errorf("agent: %s: %w", label, err))
	}
	return result
}

// ReconcileOperation records a trusted reconciler decision for an unresolved write.
// Runtime validates completed output against the registered operation schema
// before the execution store atomically changes state and appends history.
func (r *Runtime) ReconcileOperation(ctx context.Context, request ReconcileOperationRequest) error {
	if r.executions == nil {
		return ErrExecutionStoreRequired
	}
	request.ExecutionID = strings.TrimSpace(request.ExecutionID)
	request.ExpectedAttemptID = strings.TrimSpace(request.ExpectedAttemptID)
	request.Actor = strings.TrimSpace(request.Actor)
	request.Message = strings.TrimSpace(request.Message)
	if request.ExecutionID == "" || request.ExpectedAttemptID == "" || request.Actor == "" || request.Message == "" {
		return fmt.Errorf("%w: execution_id, expected_attempt_id, actor, and message are required", ErrInvalidReconciliation)
	}
	if len(request.Evidence) > 0 && !json.Valid(request.Evidence) {
		return fmt.Errorf("%w: evidence must be valid JSON", ErrInvalidReconciliation)
	}
	execution, err := r.executions.GetExecution(ctx, request.ExecutionID)
	if err != nil {
		return err
	}
	if execution.AttemptID != request.ExpectedAttemptID {
		return fmt.Errorf("%w: execution %s current attempt is %q", ErrOperationAttemptLost, execution.ID, execution.AttemptID)
	}
	// A started attempt may still be inside its executor. Reconciliation must
	// not invalidate that attempt while its side effect can still commit.
	// The runtime first moves failed/cancelled attempts to unknown; only then
	// may an operator decide whether retry or completion is safe.
	if execution.Status != OperationExecutionExecuted && execution.Status != OperationExecutionUnknown {
		return fmt.Errorf("%w: execution %s status %q cannot be reconciled", ErrInvalidReconciliation, execution.ID, execution.Status)
	}
	transition := OperationExecutionTransition{
		ID: r.newID(), ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: request.Actor, Message: request.Message,
		From: execution.Status, Evidence: append(json.RawMessage(nil), request.Evidence...), CreatedAt: r.now(),
	}
	switch request.Action {
	case OperationReconciliationRetry:
		if execution.Status == OperationExecutionExecuted {
			return fmt.Errorf("%w: executed operation %s cannot be retried; complete its verification instead", ErrInvalidReconciliation, execution.ID)
		}
		transition.To = OperationExecutionRetryable
	case OperationReconciliationComplete:
		if len(request.Result.Output) == 0 || !json.Valid(request.Result.Output) {
			return fmt.Errorf("%w: completed result output must be valid JSON", ErrInvalidReconciliation)
		}
		if len(request.Result.Receipt) > 0 && !json.Valid(request.Result.Receipt) {
			return fmt.Errorf("%w: completed result receipt must be valid JSON", ErrInvalidReconciliation)
		}
		if err := validateResultArtifacts(request.Result.Artifacts); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
		}
		registeredOperation, ok := r.operations.Get(execution.Name)
		if !ok {
			return fmt.Errorf("%w: %s", ErrOperationNotFound, execution.Name)
		}
		if err := r.operations.ValidateOutput(execution.Name, request.Result.Output); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
		}
		if err := validateOperationResultProtocol(registeredOperation, request.Result); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidReconciliation, err)
		}
		if execution.Status == OperationExecutionExecuted && !equalOperationResult(execution.Result, request.Result) {
			return fmt.Errorf("%w: completed result does not match the durably executed result for %s", ErrInvalidReconciliation, execution.ID)
		}
		transition.To = OperationExecutionCompleted
		transition.Result = cloneOperationResult(request.Result)
	case OperationReconciliationFail:
		if hasOperationResult(request.Result) {
			return fmt.Errorf("%w: failed reconciliation cannot contain a result", ErrInvalidReconciliation)
		}
		transition.To = OperationExecutionRecoveryFailed
	default:
		return fmt.Errorf("%w: unsupported action %q", ErrInvalidReconciliation, request.Action)
	}
	if _, err := r.executions.TransitionExecution(ctx, transition); err != nil {
		return fmt.Errorf("agent: reconcile operation execution: %w", err)
	}
	return nil
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

type terminalSessionArtifactProjection struct {
	Type           string          `json:"artifact_type"`
	SessionSummary json.RawMessage `json:"session_summary"`
}

type terminalSessionHistoryPayload struct {
	RecordType           string                              `json:"record_type"`
	InstructionAuthority string                              `json:"instruction_authority"`
	Artifacts            []terminalSessionArtifactProjection `json:"artifacts"`
}

func projectTerminalSessionTranscript(transcript []ModelInputItem, artifacts []ResultArtifact) ([]ModelInputItem, error) {
	projections := terminalArtifactProjections(artifacts)
	if len(projections) == 0 {
		return cloneModelInputItems(transcript), nil
	}
	if err := validateResultArtifacts(artifacts); err != nil {
		return nil, fmt.Errorf("invalid terminal artifact projection: %w", err)
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

func terminalArtifactProjections(artifacts []ResultArtifact) []terminalSessionArtifactProjection {
	projections := make([]terminalSessionArtifactProjection, 0, len(artifacts))
	for _, artifact := range artifacts {
		if len(artifact.SessionSummary) == 0 {
			continue
		}
		projections = append(projections, terminalSessionArtifactProjection{
			Type:           strings.TrimSpace(artifact.Type),
			SessionSummary: append(json.RawMessage(nil), artifact.SessionSummary...),
		})
	}
	return projections
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
		callID := strings.TrimSpace(resultItem.CallID)
		if callID == "" || callID != resultItem.CallID {
			return nil, nil, 0, errors.New("terminal transcript ends with an unnormalized tool result")
		}
		if _, duplicate := seenCallIDs[callID]; duplicate {
			return nil, nil, 0, fmt.Errorf("terminal tool result %q is ambiguous", callID)
		}
		seenCallIDs[callID] = struct{}{}
		if len(resultItem.Output) == 0 || !json.Valid(resultItem.Output) {
			return nil, nil, 0, fmt.Errorf("terminal tool result %q is not valid JSON", callID)
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
			if item.Type == ModelInputToolResult && strings.TrimSpace(item.CallID) == callID {
				matchingResults++
			}
			if index >= resultStart || item.Type != ModelInputAssistantOutput || item.OutputType != ModelOutputFunctionCall || strings.TrimSpace(item.CallID) != callID {
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
	if len(item.Raw) == 0 || !json.Valid(item.Raw) {
		return fmt.Errorf("terminal function call %q raw payload is invalid", callID)
	}
	var envelope struct {
		Type   ModelOutputItemType `json:"type"`
		CallID string              `json:"call_id"`
		Call   *struct {
			ID string `json:"id"`
		} `json:"call"`
	}
	if err := json.Unmarshal(item.Raw, &envelope); err != nil {
		return fmt.Errorf("decode terminal function call %q: %w", callID, err)
	}
	rawCallID := strings.TrimSpace(envelope.CallID)
	if envelope.Call != nil {
		nestedCallID := strings.TrimSpace(envelope.Call.ID)
		if rawCallID != "" && nestedCallID != "" && rawCallID != nestedCallID {
			return fmt.Errorf("terminal function call %q raw payload contains conflicting call ids", callID)
		}
		if rawCallID == "" {
			rawCallID = nestedCallID
		}
	}
	if envelope.Type != ModelOutputFunctionCall || rawCallID != callID {
		return fmt.Errorf("terminal function call %q raw payload does not identify the paired call", callID)
	}
	return nil
}

func terminalSessionHistoryItem(callID string, artifacts []terminalSessionArtifactProjection) (ModelInputItem, error) {
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
