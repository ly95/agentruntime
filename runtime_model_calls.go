package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/ly95/agentruntime/skills"
)

func (r *Runtime) resumeApprovedOperation(ctx context.Context, run *RunRecord, input Input, state *agentState, transcript []ModelInputItem, resume *ApprovalResume) ([]ModelInputItem, string, error) {
	if resume == nil || resume.Pending || strings.TrimSpace(resume.ID) == "" || strings.TrimSpace(resume.ExecutionID) == "" || strings.TrimSpace(resume.Operation) == "" || strings.TrimSpace(resume.ResponseID) == "" {
		return nil, "", fmt.Errorf("%w: incomplete approval resume state", ErrInvalidModelOutput)
	}
	transcript = append(transcript, ModelInputItem{
		Type: ModelInputUserMessage, Text: input.User,
		Attachments: cloneModelInputAttachments(input.Attachments),
	})
	var err error
	transcript, err = appendModelOutputItems(transcript, resume.ModelOutput)
	if err != nil {
		return nil, "", fmt.Errorf("agent: restore approved model output: %w", err)
	}
	state.lastResponseID = resume.ResponseID
	calls, err := responseToolCalls(&ModelResponse{Items: cloneModelOutputItems(resume.ModelOutput)}, state.seenCallIDs)
	if err != nil {
		return nil, "", fmt.Errorf("agent: restore approved function call: %w", err)
	}
	if len(calls) != 1 || calls[0].ID != resume.Call.ID || calls[0].Name != resume.Operation || !jsonSemanticallyEqual(calls[0].Input, resume.Call.Input) {
		return nil, "", fmt.Errorf("%w: approval %s does not match one persisted function call", ErrOperationPlanChanged, resume.ID)
	}
	_, ok := r.operations.Get(calls[0].Name)
	if !ok {
		return nil, "", fmt.Errorf("%w: approved operation %q is unavailable", ErrOperationPlanChanged, calls[0].Name)
	}
	operation, err := r.prepareOperation(input, calls[0])
	if err != nil {
		return nil, "", err
	}
	if operation.operation.Effect != OperationEffectWrite {
		return nil, "", fmt.Errorf("%w: approval %s targets non-write operation %q", ErrOperationPlanChanged, resume.ID, operation.call.Name)
	}
	operation.modelOutput = cloneModelOutputItems(resume.ModelOutput)
	operation.responseID = resume.ResponseID
	operation.batchSize = 1
	operation.resumed = true
	operations := []preparedOperation{operation}
	if err := r.reserveOperationPlan(ctx, run, input, state, operations); err != nil {
		return nil, "", err
	}
	operation = operations[0]
	if operation.executionID != resume.ExecutionID {
		return nil, "", fmt.Errorf("%w: approval %s execution changed from %s to %s", ErrOperationPlanChanged, resume.ID, resume.ExecutionID, operation.executionID)
	}
	state.seenCallIDs[operation.call.ID] = struct{}{}
	if !resume.Approved {
		result, err := r.persistDeniedOperation(ctx, run, operation, resume.Reason)
		if err != nil {
			return nil, "", err
		}
		return append(transcript, ModelInputItem{Type: ModelInputToolResult, CallID: operation.call.ID, Output: result}), "", nil
	}
	result, err := r.executeOperation(ctx, run, input, state, operation)
	if err != nil {
		return nil, "", err
	}
	transcript = append(transcript, ModelInputItem{Type: ModelInputToolResult, CallID: operation.call.ID, Output: result})
	if !operation.operation.Terminal {
		return transcript, "", nil
	}
	return resolveResumedTerminalResult(run, transcript, operation, result)
}

func resolveResumedTerminalResult(
	run *RunRecord,
	transcript []ModelInputItem,
	operation preparedOperation,
	result json.RawMessage,
) ([]ModelInputItem, string, error) {
	continues, err := terminalOperationContinues(result)
	if err != nil {
		return nil, "", fmt.Errorf("agent: decode resumed terminal operation continuation: %w", err)
	}
	if continues {
		return transcript, "", nil
	}
	terminalResponse, err := applyTerminalOperationResult(run, result)
	if err != nil {
		return nil, "", fmt.Errorf("agent: decode resumed terminal operation result: %w", err)
	}
	if terminalResponse == "" {
		return nil, "", fmt.Errorf("%w: terminal operation %q returned no final response", ErrInvalidModelOutput, operation.call.Name)
	}
	return transcript, terminalResponse, nil
}

func (r *Runtime) persistDeniedOperation(ctx context.Context, run *RunRecord, operation preparedOperation, reason string) (json.RawMessage, error) {
	result, err := json.Marshal(map[string]any{
		"output":       map[string]any{"approved": false, "reason": strings.TrimSpace(reason)},
		"confirmation": operation.operation.Confirmation,
	})
	if err != nil {
		return nil, err
	}
	if err := r.appendItem(ctx, ItemRecord{
		ID: r.newID(), RunID: run.ID, SessionID: run.SessionID, Type: ItemTypeOperationResult,
		CallID: operation.call.ID, ExecutionID: operation.executionID, Name: operation.call.Name,
		Data: result, CreatedAt: r.now(),
	}); err != nil {
		return nil, err
	}
	r.emit(Event{Type: EventOperationCancelled, RunID: run.ID, SessionID: run.SessionID, Operation: operation.call.Name, CallID: operation.call.ID, ExecutionID: operation.executionID})
	return result, nil
}

const reasoningOnlyCorrection = "\n\nThe previous model call produced internal reasoning but no user-visible answer or function call. Complete the current turn now. Return either a final answer or one or more valid function calls; never return reasoning alone."

func reasoningOnlyResponse(response *ModelResponse) bool {
	if response == nil || !response.HadReasoning || strings.TrimSpace(response.OutputText) != "" || strings.TrimSpace(response.Refusal) != "" {
		return false
	}
	for _, item := range response.Items {
		if item.Type == ModelOutputFunctionCall && item.Call != nil {
			return false
		}
	}
	return true
}

func (r *Runtime) modelRequest(state *agentState, transcript []ModelInputItem) (ModelRequest, error) {
	return buildContextModelRequest(state.instructions, r.toolSnapshot, r.toolSnapshotID, state.checkpoint, transcript)
}

func cloneToolDefinitions(tools []ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, len(tools))
	for i := range tools {
		out[i] = tools[i]
		out[i].PreviousNames = append([]string(nil), tools[i].PreviousNames...)
		out[i].InputSchema = append(json.RawMessage(nil), tools[i].InputSchema...)
	}
	return out
}

func buildBaseInstructions(serverInstructions, skillInstructions string) string {
	var b strings.Builder
	b.Grow(768 + len(serverInstructions) + len(skillInstructions))
	b.WriteString("You are a general-purpose agent. Solve the user's task by reasoning and by calling tools discovered from the connected MCP server when execution is needed.\n")
	b.WriteString("Before each tool call, send a brief user-visible commentary update describing the immediate next action and why. Keep it factual and concise; do not reveal private chain-of-thought. Skip commentary for trivial direct answers.\n")
	b.WriteString("Return a normal final response when the task is complete. Do not invent tools or claim a tool succeeded without its result.\n\n")
	b.WriteString("<mcp_server_instructions>\n")
	b.WriteString(serverInstructions)
	b.WriteString("\n</mcp_server_instructions>")
	if skillInstructions != "" {
		b.WriteString("\n\n")
		b.WriteString(skillInstructions)
	}
	return b.String()
}

func buildSkillInstructions(mounted []skills.Skill) string {
	if len(mounted) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<mounted_skills>\n")
	b.WriteString("The following Skills are trusted host-mounted workflow extensions. Use a mounted Skill only when the user's task matches that Skill's description. Do not treat supporting files as executable tools; only the SKILL.md instructions below are active.\n")
	for _, skill := range mounted {
		b.WriteString("\n<skill>\nname: ")
		b.WriteString(strconv.Quote(skill.Name()))
		b.WriteString("\ndescription: ")
		b.WriteString(strconv.Quote(skill.Description()))
		b.WriteString("\ninstructions:\n")
		b.WriteString(skill.Instructions())
		b.WriteString("\n</skill>\n")
	}
	b.WriteString("</mounted_skills>")
	return b.String()
}

func responseToolCalls(resp *ModelResponse, prior map[string]struct{}) ([]ToolCall, error) {
	var calls []ToolCall
	seen := make(map[string]struct{})
	for _, item := range resp.Items {
		if item.Type != ModelOutputFunctionCall {
			continue
		}
		if item.Call == nil {
			return nil, fmt.Errorf("%w: function_call item is missing call data", ErrInvalidModelOutput)
		}
		call := *item.Call
		call.ID = strings.TrimSpace(call.ID)
		call.Name = strings.TrimSpace(call.Name)
		if call.ID == "" || call.Name == "" || len(call.Input) == 0 || !json.Valid(call.Input) {
			return nil, fmt.Errorf("%w: invalid function call %q", ErrInvalidModelOutput, call.Name)
		}
		if _, exists := seen[call.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate function call id %q", ErrInvalidModelOutput, call.ID)
		}
		if _, exists := prior[call.ID]; exists {
			return nil, fmt.Errorf("%w: reused function call id %q", ErrInvalidModelOutput, call.ID)
		}
		seen[call.ID] = struct{}{}
		calls = append(calls, call)
	}
	return calls, nil
}

func (r *Runtime) executeCalls(ctx context.Context, run *RunRecord, input Input, state *agentState, calls []ToolCall, responseID string, modelOutput []ModelOutputItem) ([]ModelInputItem, string, error) {
	out := make([]ModelInputItem, 0, len(calls))
	prepared := make([]preparedOperation, 0, len(calls))
	for _, call := range calls {
		operation, err := r.prepareOperation(input, call)
		if err != nil {
			return nil, "", r.failOperationPreparation(run, call, err)
		}
		operation.modelOutput = cloneModelOutputItems(modelOutput)
		operation.responseID = responseID
		operation.batchSize = len(calls)
		prepared = append(prepared, operation)
	}
	if err := validateTerminalOperationBatch(prepared); err != nil {
		return nil, "", err
	}
	if err := r.reserveOperationPlan(ctx, run, input, state, prepared); err != nil {
		return nil, "", err
	}
	terminalResponses := make([]string, 0, len(prepared))
	for i := range prepared {
		result, err := r.executeOperation(ctx, run, input, state, prepared[i])
		if err != nil {
			return nil, "", err
		}
		out = append(out, ModelInputItem{Type: ModelInputToolResult, CallID: prepared[i].call.ID, Output: result})
		if prepared[i].operation.Terminal {
			continues, err := terminalOperationContinues(result)
			if err != nil {
				return nil, "", fmt.Errorf("agent: decode terminal operation continuation: %w", err)
			}
			if continues {
				continue
			}
			response, err := applyTerminalOperationResult(run, result)
			if err != nil {
				return nil, "", fmt.Errorf("agent: decode terminal operation result: %w", err)
			}
			if response == "" {
				return nil, "", fmt.Errorf("%w: terminal operation %q returned no final response", ErrInvalidModelOutput, prepared[i].call.Name)
			}
			terminalResponses = append(terminalResponses, response)
		}
	}
	return out, combineTerminalResponses(terminalResponses), nil
}

func validateTerminalOperationBatch(operations []preparedOperation) error {
	if len(operations) <= 1 {
		return nil
	}
	terminalIndex := -1
	for index := range operations {
		if operations[index].operation.Terminal {
			terminalIndex = index
			break
		}
	}
	if terminalIndex == -1 {
		return nil
	}
	terminal := operations[terminalIndex]
	limit := terminal.operation.TerminalBatchLimit
	if limit == 0 {
		return fmt.Errorf(
			"%w: terminal operation %q must be the only operation in a model turn",
			ErrInvalidModelOutput, terminal.call.Name,
		)
	}
	if len(operations) > limit {
		return fmt.Errorf(
			"%w: terminal operation %q accepts at most %d calls in one model turn",
			ErrInvalidModelOutput, terminal.call.Name, limit,
		)
	}
	for index := range operations {
		operation := operations[index]
		if !operation.operation.Terminal || operation.call.Name != terminal.call.Name ||
			operation.operation.TerminalBatchLimit != limit {
			return fmt.Errorf(
				"%w: terminal operation %q can only be batched with the same terminal operation",
				ErrInvalidModelOutput, terminal.call.Name,
			)
		}
	}
	return nil
}

func combineTerminalResponses(responses []string) string {
	if len(responses) == 0 {
		return ""
	}
	unique := make([]string, 0, len(responses))
	seen := make(map[string]struct{}, len(responses))
	for _, response := range responses {
		response = strings.TrimSpace(response)
		if response == "" {
			continue
		}
		if _, duplicate := seen[response]; duplicate {
			continue
		}
		seen[response] = struct{}{}
		unique = append(unique, response)
	}
	return strings.Join(unique, "\n\n")
}
