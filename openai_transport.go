package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type OpenAIModelConfig struct {
	Model     shared.ResponsesModel
	Reasoning *shared.ReasoningParam
}

type OpenAIModel struct {
	client    openai.Client
	model     shared.ResponsesModel
	reasoning *shared.ReasoningParam
	toolMu    sync.RWMutex
	toolCache map[string][]responses.ToolUnionParam
}

// NewOpenAIModel constructs a Responses API model over client. The client owns
// authentication, endpoint, middleware, timeout, and transport retry policy.
func NewOpenAIModel(client openai.Client, cfg OpenAIModelConfig) (*OpenAIModel, error) {
	cfg.Model = shared.ResponsesModel(strings.TrimSpace(string(cfg.Model)))
	if cfg.Model == "" {
		return nil, errors.New("agent: OpenAI model is required")
	}
	var reasoning *shared.ReasoningParam
	if cfg.Reasoning != nil {
		params := *cfg.Reasoning
		if !validOpenAIReasoningEffort(params.Effort) {
			return nil, fmt.Errorf("agent: unsupported OpenAI reasoning effort %q", params.Effort)
		}
		if !validOpenAIReasoningSummary(params.Summary) {
			return nil, fmt.Errorf("agent: unsupported OpenAI reasoning summary %q", params.Summary)
		}
		if !validOpenAIReasoningGenerateSummary(params.GenerateSummary) {
			return nil, fmt.Errorf("agent: unsupported deprecated OpenAI reasoning generate summary %q", params.GenerateSummary)
		}
		if params.Summary != "" && params.GenerateSummary != "" {
			return nil, errors.New("agent: OpenAI reasoning summary and deprecated generate summary cannot both be set")
		}
		if params.Effort == shared.ReasoningEffortNone && (params.Summary != "" || params.GenerateSummary != "") {
			return nil, errors.New("agent: OpenAI reasoning summary must be omitted when effort is none")
		}
		reasoning = &params
	}
	return &OpenAIModel{
		client:    client,
		model:     cfg.Model,
		reasoning: reasoning,
		toolCache: make(map[string][]responses.ToolUnionParam),
	}, nil
}

func (m *OpenAIModel) Complete(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	if m == nil {
		return nil, errors.New("agent: OpenAI model is nil")
	}
	params, err := m.buildResponseParams(req)
	if err != nil {
		return nil, err
	}
	stream := m.client.Responses.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	state := newOpenAIStreamState(req.StreamSink)
	for stream.Next() {
		if err := state.accept(stream.Current()); err != nil {
			return nil, err
		}
	}
	return state.finish(stream.Err())
}

func (m *OpenAIModel) buildResponseParams(req ModelRequest) (responses.ResponseNewParams, error) {
	var params responses.ResponseNewParams
	params.Model = m.model
	params.Store = openai.Bool(false)
	params.Include = []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent}
	if req.DisableReasoning {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortNone}
	} else if m.reasoning != nil {
		params.Reasoning = *m.reasoning
	}
	if strings.TrimSpace(req.Instructions) == "" {
		return responses.ResponseNewParams{}, errors.New("agent: model instructions are required")
	}
	params.Instructions = openai.String(req.Instructions)
	items, err := buildOpenAIInputItems(req.Input)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	if len(req.Tools) > 0 {
		tools, err := m.cachedOpenAITools(req.ToolSetID, req.Tools)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Tools = tools
	}
	return params, nil
}

type openAIStreamState struct {
	completed     *responses.Response
	responseID    string
	sink          ModelStreamSink
	messagePhases map[string]responses.ResponseOutputMessagePhase
	callIDs       map[string]string
	functionCalls map[string]openAIStreamFunctionCall
}

type openAIStreamFunctionCall struct {
	CallID    string
	Name      string
	Arguments string
	Finalized bool
}

func newOpenAIStreamState(sink ModelStreamSink) *openAIStreamState {
	return &openAIStreamState{
		sink:          sink,
		messagePhases: make(map[string]responses.ResponseOutputMessagePhase),
		callIDs:       make(map[string]string),
		functionCalls: make(map[string]openAIStreamFunctionCall),
	}
}

func (state *openAIStreamState) emit(chunk ModelStreamEvent) {
	if state.sink == nil {
		return
	}
	if chunk.ResponseID == "" {
		chunk.ResponseID = state.responseID
	}
	if chunk.CallID == "" && chunk.ItemID != "" {
		chunk.CallID = state.callIDs[chunk.ItemID]
	}
	state.sink(chunk)
}

func (state *openAIStreamState) accept(event responses.ResponseStreamEventUnion) error {
	switch event.Type {
	case "response.created":
		state.responseID = event.AsResponseCreated().Response.ID
		chunk := newOpenAIStreamChunk(event, ModelStreamResponseStarted)
		chunk.ResponseID = state.responseID
		state.emit(chunk)
	case "response.output_item.added", "response.output_item.done",
		"response.function_call_arguments.done":
		if err := state.acceptItemEvent(event); err != nil {
			return err
		}
	case "response.output_text.delta", "response.reasoning_summary_text.delta",
		"response.refusal.delta", "response.function_call_arguments.delta":
		state.acceptDeltaEvent(event)
	case "response.completed":
		response := event.AsResponseCompleted().Response
		chunk := newOpenAIStreamChunk(event, ModelStreamResponseDone)
		chunk.ResponseID = response.ID
		state.emit(chunk)
		state.completed = &response
	case "response.failed":
		response := event.AsResponseFailed().Response
		return state.responseFailure(event, "failed", string(response.Error.Code), response)
	case "response.incomplete":
		response := event.AsResponseIncomplete().Response
		return state.responseFailure(event, "incomplete", "incomplete", response)
	case "error":
		errorEvent := event.AsError()
		chunk := newOpenAIStreamChunk(event, ModelStreamError)
		chunk.ErrorCode = errorEvent.Code
		chunk.ErrorMessage = errorEvent.Message
		state.emit(chunk)
		return fmt.Errorf("agent: OpenAI stream error %s: %s", errorEvent.Code, errorEvent.Message)
	}
	return nil
}

func (state *openAIStreamState) acceptItemEvent(event responses.ResponseStreamEventUnion) error {
	switch event.Type {
	case "response.output_item.added":
		added := event.AsResponseOutputItemAdded()
		if added.Item.Type == string(ModelOutputMessage) {
			state.messagePhases[added.Item.ID] = added.Item.Phase
		}
		if added.Item.Type == string(ModelOutputFunctionCall) {
			if err := state.recordFunctionCall(
				added.Item.ID,
				added.Item.CallID,
				added.Item.Name,
				"",
				false,
			); err != nil {
				return err
			}
		}
		chunk := newOpenAIStreamChunk(event, ModelStreamItemAdded)
		chunk.ItemID, chunk.CallID, chunk.Name = added.Item.ID, added.Item.CallID, added.Item.Name
		chunk.Phase = string(added.Item.Phase)
		state.emit(chunk)
	case "response.function_call_arguments.done":
		done := event.AsResponseFunctionCallArgumentsDone()
		if err := state.recordFunctionCall(
			done.ItemID,
			"",
			done.Name,
			done.Arguments,
			true,
		); err != nil {
			return err
		}
		chunk := newOpenAIStreamChunk(event, ModelStreamToolArgumentsDone)
		chunk.Arguments, chunk.Name = done.Arguments, done.Name
		state.emit(chunk)
	case "response.output_item.done":
		done := event.AsResponseOutputItemDone()
		if done.Item.Type == string(ModelOutputFunctionCall) {
			call := done.Item.AsFunctionCall()
			if err := state.recordFunctionCall(
				done.Item.ID,
				call.CallID,
				call.Name,
				call.Arguments,
				true,
			); err != nil {
				return err
			}
		}
		chunk := newOpenAIStreamChunk(event, ModelStreamItemDone)
		chunk.ItemID, chunk.CallID, chunk.Name = done.Item.ID, done.Item.CallID, done.Item.Name
		chunk.Phase = string(done.Item.Phase)
		state.emit(chunk)
	}
	return nil
}

func (state *openAIStreamState) recordFunctionCall(
	itemID string,
	callID string,
	name string,
	arguments string,
	finalized bool,
) error {
	itemID = strings.TrimSpace(itemID)
	callID = strings.TrimSpace(callID)
	name = strings.TrimSpace(name)
	if itemID == "" {
		return fmt.Errorf("%w: OpenAI stream function call is missing its item id", ErrInvalidModelOutput)
	}
	current := state.functionCalls[itemID]
	if current.CallID != "" && callID != "" && current.CallID != callID {
		return fmt.Errorf(
			"%w: OpenAI stream function call %q changed call id",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if current.Name != "" && name != "" && current.Name != name {
		return fmt.Errorf(
			"%w: OpenAI stream function call %q changed name",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if current.Arguments != "" && arguments != "" &&
		!jsonSemanticallyEqual(json.RawMessage(current.Arguments), json.RawMessage(arguments)) {
		return fmt.Errorf(
			"%w: OpenAI stream function call %q changed finalized arguments",
			ErrInvalidModelOutput,
			itemID,
		)
	}
	if callID != "" {
		current.CallID = callID
		state.callIDs[itemID] = callID
	}
	if name != "" {
		current.Name = name
	}
	if arguments != "" {
		if !json.Valid([]byte(arguments)) {
			return fmt.Errorf(
				"%w: OpenAI stream function call %q finalized invalid JSON arguments",
				ErrInvalidModelOutput,
				itemID,
			)
		}
		current.Arguments = arguments
	}
	current.Finalized = current.Finalized || finalized
	state.functionCalls[itemID] = current
	return nil
}

func (state *openAIStreamState) acceptDeltaEvent(event responses.ResponseStreamEventUnion) {
	if event.Delta == "" {
		return
	}
	eventType := ModelStreamTextDelta
	switch event.Type {
	case "response.output_text.delta":
		if state.messagePhases[event.ItemID] == responses.ResponseOutputMessagePhaseCommentary {
			eventType = ModelStreamCommentaryDelta
		}
	case "response.reasoning_summary_text.delta":
		eventType = ModelStreamReasoningSummaryDelta
	case "response.refusal.delta":
		eventType = ModelStreamRefusalDelta
	case "response.function_call_arguments.delta":
		eventType = ModelStreamToolArgumentsDelta
	}
	state.emit(newOpenAIStreamChunk(event, eventType))
}

func (state *openAIStreamState) responseFailure(
	event responses.ResponseStreamEventUnion,
	status string,
	errorCode string,
	response responses.Response,
) error {
	err := openAIStreamResponseError(status, response)
	chunk := newOpenAIStreamChunk(event, ModelStreamError)
	chunk.ResponseID = response.ID
	chunk.ErrorCode = errorCode
	chunk.ErrorMessage = err.Error()
	state.emit(chunk)
	return err
}

func (state *openAIStreamState) finish(streamErr error) (*ModelResponse, error) {
	if streamErr != nil {
		state.emit(ModelStreamEvent{
			Type: ModelStreamError, ProviderType: "transport.error",
			ErrorCode: "transport_error", ErrorMessage: streamErr.Error(),
		})
		return nil, streamErr
	}
	if state.completed == nil {
		err := errors.New("agent: OpenAI stream ended without response.completed")
		state.emit(ModelStreamEvent{
			Type: ModelStreamError, ProviderType: "stream.ended",
			ErrorCode: "incomplete_stream", ErrorMessage: err.Error(),
		})
		return nil, err
	}
	return parseOpenAIResponseWithFinalizedCalls(state.completed, state.functionCalls)
}

func newOpenAIStreamChunk(event responses.ResponseStreamEventUnion, eventType ModelStreamEventType) ModelStreamEvent {
	chunk := ModelStreamEvent{
		Type:         eventType,
		ProviderType: event.Type,
		ItemID:       event.ItemID,
		Delta:        event.Delta,
		RawJSON:      event.RawJSON(),
	}
	if event.JSON.SequenceNumber.Valid() {
		sequence := event.SequenceNumber
		chunk.SequenceNumber = &sequence
	}
	if event.JSON.OutputIndex.Valid() {
		index := event.OutputIndex
		chunk.OutputIndex = &index
	}
	return chunk
}

func validOpenAIReasoningEffort(effort shared.ReasoningEffort) bool {
	switch effort {
	case "", shared.ReasoningEffortNone, shared.ReasoningEffortMinimal, shared.ReasoningEffortLow,
		shared.ReasoningEffortMedium, shared.ReasoningEffortHigh, shared.ReasoningEffortXhigh:
		return true
	default:
		return false
	}
}

func validOpenAIReasoningSummary(summary shared.ReasoningSummary) bool {
	switch summary {
	case "", shared.ReasoningSummaryAuto, shared.ReasoningSummaryConcise, shared.ReasoningSummaryDetailed:
		return true
	default:
		return false
	}
}

func validOpenAIReasoningGenerateSummary(summary shared.ReasoningGenerateSummary) bool {
	switch summary {
	case "", shared.ReasoningGenerateSummaryAuto, shared.ReasoningGenerateSummaryConcise,
		shared.ReasoningGenerateSummaryDetailed:
		return true
	default:
		return false
	}
}

func openAIStreamResponseError(status string, response responses.Response) error {
	code := strings.TrimSpace(string(response.Error.Code))
	message := strings.TrimSpace(response.Error.Message)
	if status == "incomplete" && message == "" {
		message = strings.TrimSpace(response.IncompleteDetails.Reason)
	}
	if code == "" {
		code = "unknown_error"
	}
	if message == "" {
		message = "no error details"
	}
	return fmt.Errorf("agent: OpenAI response %s: %s: %s", status, code, message)
}

func (m *OpenAIModel) cachedOpenAITools(
	toolSetID string,
	tools []ToolDefinition,
) ([]responses.ToolUnionParam, error) {
	key := strings.TrimSpace(toolSetID)
	definitionsID := toolDefinitionsID(tools)
	if key != "" && key != definitionsID {
		return nil, fmt.Errorf("agent: model tool set id does not match its definitions")
	}
	key = definitionsID
	m.toolMu.RLock()
	cached, ok := m.toolCache[key]
	m.toolMu.RUnlock()
	if ok {
		return cached, nil
	}
	mapped, err := mapOpenAITools(tools)
	if err != nil {
		return nil, err
	}
	m.toolMu.Lock()
	if cached, ok := m.toolCache[key]; ok {
		m.toolMu.Unlock()
		return cached, nil
	}
	m.toolCache[key] = mapped
	m.toolMu.Unlock()
	return mapped, nil
}
