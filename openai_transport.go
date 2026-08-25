package agentruntime

import (
	"context"
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
		//lint:ignore SA1019 The SDK field remains accepted for pre-v1 compatibility and is validated explicitly.
		generateSummary := params.GenerateSummary
		if !validOpenAIReasoningGenerateSummary(generateSummary) {
			return nil, fmt.Errorf("agent: unsupported deprecated OpenAI reasoning generate summary %q", generateSummary)
		}
		if params.Summary != "" && generateSummary != "" {
			return nil, errors.New("agent: OpenAI reasoning summary and deprecated generate summary cannot both be set")
		}
		if params.Effort == shared.ReasoningEffortNone && (params.Summary != "" || generateSummary != "") {
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
	response, err := state.finish(stream.Err())
	if err != nil {
		return nil, classifyOpenAIError(err)
	}
	return response, nil
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
