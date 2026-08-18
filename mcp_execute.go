package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpOperationEnvelope struct {
	Result *OperationResult   `json:"result,omitempty"`
	Error  *mcpOperationError `json:"error,omitempty"`
}

type mcpOperationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (local *localMCP) Execute(
	ctx context.Context,
	request OperationRequest,
) (OperationResult, error) {
	arguments, err := json.Marshal(request.Arguments)
	if err != nil {
		return OperationResult{}, fmt.Errorf("agent: encode MCP tool arguments: %w", err)
	}
	if len(arguments) == 0 || !json.Valid(arguments) {
		return OperationResult{}, errors.New("agent: MCP tool arguments must be valid JSON")
	}
	token := randomID()
	local.pendingMu.Lock()
	local.pending[token] = mcpInvocation{
		request: request, arguments: append(json.RawMessage(nil), arguments...),
	}
	local.pendingMu.Unlock()
	defer local.removeInvocation(token)

	serverSession, clientSession, err := local.connect(ctx)
	if err != nil {
		return OperationResult{}, fmt.Errorf("agent: connect MCP execution session: %w", err)
	}
	result, callErr := clientSession.CallTool(ctx, &mcpsdk.CallToolParams{
		Meta: mcpsdk.Meta{mcpInvocationMetaKey: token},
		Name: request.Operation.Name, Arguments: json.RawMessage(arguments),
	})
	closeErr := closeMCPSessions(clientSession, serverSession)
	if completion, found := local.takeCompletion(token); found {
		// FALLBACK: the trusted handler completed before any transport or close
		// error. Its exact domain result is authoritative and preserves sentinel
		// identity for the runtime's retry and outcome-unknown decisions.
		if completion.err != nil {
			return OperationResult{}, completion.err
		}
		return cloneOperationResult(completion.result), nil
	}
	if callErr != nil || closeErr != nil {
		return OperationResult{}, errors.Join(callErr, closeErr)
	}
	return decodeMCPResult(result)
}

func (local *localMCP) handleTool(
	ctx context.Context,
	request *mcpsdk.CallToolRequest,
) (*mcpsdk.CallToolResult, error) {
	if request == nil || request.Params == nil {
		return nil, errors.New("agent: MCP tool request is missing parameters")
	}
	token, _ := request.Params.Meta[mcpInvocationMetaKey].(string)
	invocation, err := local.takeInvocation(
		token, request.Params.Name, request.Params.Arguments,
	)
	if err != nil {
		return nil, err
	}
	result, executeErr := local.executor.Execute(ctx, invocation.request)
	executeErr = validateUTF8Error("operation executor", executeErr)
	if executeErr == nil {
		executeErr = validateUTF8Boundary("operation result", result)
	}
	local.completeInvocation(token, result, executeErr)
	if executeErr != nil {
		return encodeMCPError(executeErr)
	}
	return encodeMCPSuccess(result)
}

func (local *localMCP) takeInvocation(
	token string,
	toolName string,
	arguments json.RawMessage,
) (mcpInvocation, error) {
	if token == "" {
		return mcpInvocation{}, errors.New("agent: MCP invocation token is required")
	}
	local.pendingMu.Lock()
	invocation, found := local.pending[token]
	if found {
		delete(local.pending, token)
	}
	local.pendingMu.Unlock()
	if !found {
		return mcpInvocation{}, errors.New("agent: MCP invocation token is invalid or already used")
	}
	if invocation.request.Operation.Name != toolName ||
		!jsonSemanticallyEqual(invocation.arguments, arguments) {
		return mcpInvocation{}, errors.New("agent: MCP invocation differs from the authorized request")
	}
	return invocation, nil
}

func (local *localMCP) removeInvocation(token string) {
	local.pendingMu.Lock()
	delete(local.pending, token)
	delete(local.completed, token)
	local.pendingMu.Unlock()
}

func (local *localMCP) completeInvocation(
	token string,
	result OperationResult,
	err error,
) {
	local.pendingMu.Lock()
	local.completed[token] = mcpCompletion{
		result: cloneOperationResult(result), err: err,
	}
	local.pendingMu.Unlock()
}

func (local *localMCP) takeCompletion(token string) (mcpCompletion, bool) {
	local.pendingMu.Lock()
	completion, found := local.completed[token]
	if found {
		delete(local.completed, token)
	}
	local.pendingMu.Unlock()
	return completion, found
}

func encodeMCPSuccess(result OperationResult) (*mcpsdk.CallToolResult, error) {
	meta, err := encodeMCPEnvelope(mcpOperationEnvelope{Result: &result})
	if err != nil {
		return nil, err
	}
	response := &mcpsdk.CallToolResult{
		Meta:    mcpsdk.Meta{mcpResultMetaKey: meta},
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(result.Output)}},
	}
	var output map[string]any
	if json.Unmarshal(result.Output, &output) == nil && output != nil {
		response.StructuredContent = output
	}
	return response, nil
}

func encodeMCPError(cause error) (*mcpsdk.CallToolResult, error) {
	operationError := &mcpOperationError{
		Code: classifyMCPExecutionError(cause), Message: cause.Error(),
	}
	meta, err := encodeMCPEnvelope(mcpOperationEnvelope{Error: operationError})
	if err != nil {
		return nil, err
	}
	return &mcpsdk.CallToolResult{
		Meta:    mcpsdk.Meta{mcpResultMetaKey: meta},
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "The tool execution failed."}},
		IsError: true,
	}, nil
}

func encodeMCPEnvelope(envelope mcpOperationEnvelope) (map[string]any, error) {
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("agent: encode MCP operation result: %w", err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("agent: encode MCP operation result metadata: %w", err)
	}
	return value, nil
}

func decodeMCPResult(result *mcpsdk.CallToolResult) (OperationResult, error) {
	if result == nil {
		return OperationResult{}, errors.New("agent: MCP tool returned no result")
	}
	value, found := result.Meta[mcpResultMetaKey]
	if !found {
		return OperationResult{}, errors.New("agent: MCP tool result is missing host metadata")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return OperationResult{}, fmt.Errorf("agent: decode MCP tool result metadata: %w", err)
	}
	var envelope mcpOperationEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return OperationResult{}, fmt.Errorf("agent: decode MCP tool result: %w", err)
	}
	if result.IsError {
		if envelope.Error == nil {
			return OperationResult{}, errors.New("agent: MCP tool returned an unclassified error")
		}
		return OperationResult{}, restoreMCPExecutionError(*envelope.Error)
	}
	if envelope.Result == nil || envelope.Error != nil {
		return OperationResult{}, errors.New("agent: MCP tool returned an invalid result envelope")
	}
	return cloneOperationResult(*envelope.Result), nil
}

func classifyMCPExecutionError(cause error) string {
	switch {
	case errors.Is(cause, ErrOperationNotApplied):
		return "not_applied"
	case errors.Is(cause, ErrOperationOutcomeUnknown):
		return "outcome_unknown"
	default:
		return "execution_failed"
	}
}

func restoreMCPExecutionError(operationError mcpOperationError) error {
	cause := errors.New(operationError.Message)
	switch operationError.Code {
	case "not_applied":
		return MarkOperationNotApplied(cause)
	case "outcome_unknown":
		return errors.Join(ErrOperationOutcomeUnknown, cause)
	case "execution_failed":
		return cause
	default:
		return fmt.Errorf("agent: MCP tool returned unknown error code %q", operationError.Code)
	}
}

func closeMCPSessions(
	clientSession *mcpsdk.ClientSession,
	serverSession *mcpsdk.ServerSession,
) error {
	var errs []error
	if clientSession != nil {
		errs = append(errs, clientSession.Close())
	}
	if serverSession != nil {
		errs = append(errs, serverSession.Close())
	}
	return errors.Join(errs...)
}
