package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpClientName        = "agentruntime-host"
	mcpServerName        = "agentruntime-tools"
	mcpImplementationVer = "1"
	mcpInvocationMetaKey = "com.github.ly95.agentruntime/invocation"
	mcpResultMetaKey     = "com.github.ly95.agentruntime/operation-result"
)

type localMCP struct {
	server       *mcpsdk.Server
	client       *mcpsdk.Client
	executor     OperationExecutor
	operations   *OperationRegistry
	instructions string
	tools        []ToolDefinition
	serverInfo   MCPServerInfo

	pendingMu sync.Mutex
	pending   map[string]mcpInvocation
	completed map[string]mcpCompletion
}

type MCPServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
}

type mcpInvocation struct {
	request   OperationRequest
	arguments json.RawMessage
}

type mcpCompletion struct {
	result OperationResult
	err    error
}

func newLocalMCP(
	operations *OperationRegistry,
	executor OperationExecutor,
	instructions string,
) (*localMCP, error) {
	if operations == nil {
		return nil, errors.New("agent: MCP operation registry is required")
	}
	summaries := operations.Summaries()
	if len(summaries) > 0 && isNilDependency(executor) {
		return nil, errors.New("agent: MCP operation executor is required")
	}
	serverInstructions := buildMCPServerInstructions(instructions)
	local := &localMCP{
		executor: executor, operations: operations,
		instructions: serverInstructions,
		pending:      make(map[string]mcpInvocation),
		completed:    make(map[string]mcpCompletion),
	}
	local.server = mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: mcpServerName, Version: mcpImplementationVer},
		&mcpsdk.ServerOptions{
			Instructions: serverInstructions,
			Capabilities: &mcpsdk.ServerCapabilities{},
		},
	)
	local.client = mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: mcpClientName, Version: mcpImplementationVer},
		&mcpsdk.ClientOptions{Capabilities: &mcpsdk.ClientCapabilities{}},
	)
	for _, operation := range summaries {
		local.registerTool(operation)
	}
	if err := local.discover(context.Background(), summaries); err != nil {
		return nil, err
	}
	return local, nil
}

func (local *localMCP) registerTool(operation OperationSummary) {
	readOnly := operation.Effect == OperationEffectRead
	destructive := operation.Effect == OperationEffectWrite
	local.server.AddTool(&mcpsdk.Tool{
		Name:         operation.Name,
		Description:  operationToolDescription(operation),
		InputSchema:  append(json.RawMessage(nil), operation.InputSchema...),
		OutputSchema: append(json.RawMessage(nil), operation.OutputSchema...),
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint:    readOnly,
			DestructiveHint: &destructive,
			IdempotentHint:  true,
		},
	}, local.handleTool)
}

func (local *localMCP) discover(
	ctx context.Context,
	expected []OperationSummary,
) error {
	serverSession, clientSession, err := local.connect(ctx)
	if err != nil {
		return fmt.Errorf("agent: connect MCP discovery session: %w", err)
	}
	tools, discoverErr := listMCPTools(ctx, clientSession)
	initialize := clientSession.InitializeResult()
	closeErr := closeMCPSessions(clientSession, serverSession)
	if discoverErr != nil || closeErr != nil {
		return errors.Join(discoverErr, closeErr)
	}
	if initialize == nil || initialize.ServerInfo == nil {
		return errors.New("agent: MCP server returned incomplete initialization data")
	}
	if initialize.ServerInfo.Name != mcpServerName ||
		initialize.ServerInfo.Version != mcpImplementationVer ||
		strings.TrimSpace(initialize.ProtocolVersion) == "" {
		return errors.New("agent: MCP server identity changed during negotiation")
	}
	if initialize.Instructions != local.instructions {
		return errors.New("agent: MCP server instructions changed during negotiation")
	}
	definitions, err := validateDiscoveredTools(tools, expected)
	if err != nil {
		return err
	}
	local.tools = definitions
	local.serverInfo = MCPServerInfo{
		Name: initialize.ServerInfo.Name, Version: initialize.ServerInfo.Version,
		ProtocolVersion: initialize.ProtocolVersion,
	}
	return nil
}

func (local *localMCP) connect(
	ctx context.Context,
) (*mcpsdk.ServerSession, *mcpsdk.ClientSession, error) {
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverSession, err := local.server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, nil, err
	}
	clientSession, err := local.client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, nil, errors.Join(err, serverSession.Close())
	}
	return serverSession, clientSession, nil
}

func (local *localMCP) Tools() []ToolDefinition {
	return cloneToolDefinitions(local.tools)
}

func (local *localMCP) Instructions() string {
	return local.instructions
}

func (local *localMCP) ServerInfo() MCPServerInfo {
	return local.serverInfo
}

func listMCPTools(
	ctx context.Context,
	session *mcpsdk.ClientSession,
) ([]*mcpsdk.Tool, error) {
	var tools []*mcpsdk.Tool
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("agent: MCP tools/list: %w", err)
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		if result.NextCursor == cursor {
			return nil, errors.New("agent: MCP tools/list cursor did not advance")
		}
		cursor = result.NextCursor
	}
}

func validateDiscoveredTools(
	discovered []*mcpsdk.Tool,
	expected []OperationSummary,
) ([]ToolDefinition, error) {
	if len(discovered) != len(expected) {
		return nil, fmt.Errorf(
			"agent: MCP discovered %d tools, expected %d", len(discovered), len(expected),
		)
	}
	expectedByName := make(map[string]OperationSummary, len(expected))
	for _, operation := range expected {
		expectedByName[operation.Name] = operation
	}
	definitions := make([]ToolDefinition, 0, len(discovered))
	for _, tool := range discovered {
		if tool == nil {
			return nil, errors.New("agent: MCP discovered a nil tool")
		}
		operation, found := expectedByName[tool.Name]
		if !found {
			return nil, fmt.Errorf("agent: MCP discovered untrusted tool %q", tool.Name)
		}
		definition, err := validateDiscoveredTool(tool, operation)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
		delete(expectedByName, tool.Name)
	}
	if len(expectedByName) > 0 {
		missing := make([]string, 0, len(expectedByName))
		for name := range expectedByName {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("agent: MCP did not expose registered tools: %s", strings.Join(missing, ", "))
	}
	sort.Slice(definitions, func(i, j int) bool {
		return definitions[i].Name < definitions[j].Name
	})
	return definitions, nil
}

func validateDiscoveredTool(
	tool *mcpsdk.Tool,
	operation OperationSummary,
) (ToolDefinition, error) {
	input, err := marshalMCPSchema(tool.InputSchema)
	if err != nil {
		return ToolDefinition{}, fmt.Errorf("agent: MCP tool %q input schema: %w", tool.Name, err)
	}
	output, err := marshalMCPSchema(tool.OutputSchema)
	if err != nil {
		return ToolDefinition{}, fmt.Errorf("agent: MCP tool %q output schema: %w", tool.Name, err)
	}
	if tool.Description != operationToolDescription(operation) ||
		!jsonSemanticallyEqual(input, operation.InputSchema) ||
		!jsonSemanticallyEqual(output, operation.OutputSchema) {
		return ToolDefinition{}, fmt.Errorf(
			"agent: MCP tool %q contract differs from host policy", tool.Name,
		)
	}
	if tool.Annotations == nil ||
		tool.Annotations.ReadOnlyHint != (operation.Effect == OperationEffectRead) ||
		tool.Annotations.DestructiveHint == nil ||
		*tool.Annotations.DestructiveHint != (operation.Effect == OperationEffectWrite) ||
		!tool.Annotations.IdempotentHint {
		return ToolDefinition{}, fmt.Errorf(
			"agent: MCP tool %q annotations differ from host policy", tool.Name,
		)
	}
	return ToolDefinition{
		Name: tool.Name, PreviousNames: append([]string(nil), operation.PreviousNames...),
		Description: tool.Description, InputSchema: input,
	}, nil
}

func marshalMCPSchema(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, errors.New("schema is missing")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, errors.New("schema is invalid JSON")
	}
	return raw, nil
}

func operationToolDescription(operation OperationSummary) string {
	description := operation.Description
	if operation.Confirmation.Mode == ConfirmationRequired {
		description += "\nResult confirmation contract: " +
			operation.Confirmation.Description
	}
	return description
}

func buildMCPServerInstructions(domainInstructions string) string {
	var instructions strings.Builder
	instructions.WriteString(
		"Use only tools returned by this MCP server. Tool availability is not authorization: " +
			"the Agent Host independently enforces policy, approval, idempotency, and verification. " +
			"Never claim a write succeeded until its tool result confirms it.",
	)
	if domainInstructions = strings.TrimSpace(domainInstructions); domainInstructions != "" {
		instructions.WriteString("\n\n")
		instructions.WriteString(domainInstructions)
	}
	return instructions.String()
}
