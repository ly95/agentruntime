package agentruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type OperationEffect string

const (
	OperationEffectRead  OperationEffect = "read"
	OperationEffectWrite OperationEffect = "write"
	// MaxTerminalBatchLimit bounds one model turn so a malformed response cannot
	// fan out an unbounded number of otherwise valid terminal writes.
	MaxTerminalBatchLimit = 10
)

type ConfirmationMode string

const (
	ConfirmationNone     ConfirmationMode = "none"
	ConfirmationRequired ConfirmationMode = "required"
)

type ConfirmationSpec struct {
	Mode        ConfirmationMode `json:"mode"`
	Description string           `json:"description,omitempty"`
}

type Operation struct {
	Name string
	// PreviousNames are accepted only when replaying persisted model transcript
	// items after an operation rename. They are never registered as executable
	// operation names or exposed as current tools to the model.
	PreviousNames []string
	Description   string
	InputSchema   json.RawMessage
	OutputSchema  json.RawMessage
	// NormalizeInput canonicalizes schema-validated arguments before execution
	// IDs, durable plans, approval previews, and executions are created. It must
	// not mutate its input and its result is validated again.
	NormalizeInput func(arguments any) (any, error)
	Effect         OperationEffect
	Capabilities   []string
	Confirmation   ConfirmationSpec
	// ApprovalPreview builds a safe, operation-specific JSON object from
	// schema-validated arguments. Raw arguments never cross the browser trust
	// boundary; writes that require confirmation must provide one so a policy
	// can safely route them through approval.
	ApprovalPreview func(arguments any) (json.RawMessage, error)
	// Terminal ends the current agent turn after this operation completes
	// successfully, or after every call in an allowed homogeneous terminal batch
	// completes. The executor must return FinalResponse for each call.
	Terminal bool
	// TerminalBatchLimit permits 2..N homogeneous calls to this terminal write
	// operation in one model turn. Zero preserves the default single-call rule.
	// Runtime still plans, fences, executes, validates, and persists every call
	// independently before completing the turn with their combined artifacts.
	TerminalBatchLimit int
}

type OperationSummary struct {
	Name               string           `json:"name"`
	PreviousNames      []string         `json:"-"`
	Description        string           `json:"description,omitempty"`
	InputSchema        json.RawMessage  `json:"input_schema"`
	OutputSchema       json.RawMessage  `json:"output_schema"`
	Effect             OperationEffect  `json:"effect"`
	Capabilities       []string         `json:"capabilities,omitempty"`
	Confirmation       ConfirmationSpec `json:"confirmation"`
	Terminal           bool             `json:"terminal,omitempty"`
	TerminalBatchLimit int              `json:"terminal_batch_limit,omitempty"`
}

type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type OperationRequest struct {
	RunID     string
	SessionID string
	// ExecutionID is stable for one write operation in the persisted request
	// plan, or for one terminal read operation that produces a durable artifact.
	// Write executors must enforce it at their own side-effect boundary.
	ExecutionID string
	// AttemptID fences the current execution owner. Executors that coordinate
	// retries must reject stale attempts before mutating state.
	AttemptID string
	// SessionLease is the fencing token for the session owner. Write executors
	// must validate its generation atomically at their side-effect boundary.
	SessionLease SessionLeaseFence
	Input        Input
	Operation    OperationSummary
	Call         ToolCall
	// Arguments may contain nil for properties that are optional in InputSchema:
	// strict tool schemas encode omission as an explicit JSON null.
	Arguments any
}

type OperationRegistry struct {
	mu            sync.RWMutex
	ops           map[string]Operation
	inputSchemas  map[string]operationInputSchemas
	outputSchemas map[string]*jsonschema.Schema
	provided      map[string]struct{}
	summaries     []OperationSummary
	frozen        bool
}

type operationInputSchemas struct {
	declared     *jsonschema.Schema
	openAIStrict *jsonschema.Schema
}

type compiledOperationSchemas struct {
	input  operationInputSchemas
	output *jsonschema.Schema
}

func NewOperationRegistry() *OperationRegistry {
	return &OperationRegistry{
		ops:           make(map[string]Operation),
		inputSchemas:  make(map[string]operationInputSchemas),
		outputSchemas: make(map[string]*jsonschema.Schema),
		provided:      make(map[string]struct{}),
	}
}

func (r *OperationRegistry) Register(op Operation) error {
	if r == nil {
		return fmt.Errorf("agent: nil operation registry")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("agent: operation registry is frozen")
	}
	if err := normalizeAndValidateOperation(&op); err != nil {
		return err
	}
	if _, exists := r.ops[op.Name]; exists {
		return fmt.Errorf("agent: operation already registered: %s", op.Name)
	}
	schemas, err := compileOperationSchemas(op)
	if err != nil {
		return err
	}
	op.InputSchema = append(json.RawMessage(nil), op.InputSchema...)
	op.OutputSchema = append(json.RawMessage(nil), op.OutputSchema...)
	op.Capabilities = normalizeNames(op.Capabilities)
	r.ops[op.Name] = op
	r.inputSchemas[op.Name] = schemas.input
	r.outputSchemas[op.Name] = schemas.output
	r.provided[op.Name] = struct{}{}
	for _, capability := range op.Capabilities {
		r.provided[capability] = struct{}{}
	}
	return nil
}

func normalizeAndValidateOperation(op *Operation) error {
	op.Name = strings.TrimSpace(op.Name)
	if op.Name == "" {
		return fmt.Errorf("agent: operation name is required")
	}
	op.PreviousNames = normalizeNames(op.PreviousNames)
	for _, previousName := range op.PreviousNames {
		if previousName == op.Name {
			return fmt.Errorf("agent: operation %q has invalid previous name %q", op.Name, previousName)
		}
	}
	if len(op.InputSchema) == 0 || !json.Valid(op.InputSchema) {
		return fmt.Errorf("agent: operation input schema is required and must be valid JSON: %s", op.Name)
	}
	if len(op.OutputSchema) == 0 || !json.Valid(op.OutputSchema) {
		return fmt.Errorf("agent: operation output schema is required and must be valid JSON: %s", op.Name)
	}
	if err := validateOperationInputSchema(op.InputSchema); err != nil {
		return fmt.Errorf("agent: operation %q input schema: %w", op.Name, err)
	}
	if op.Effect != OperationEffectRead && op.Effect != OperationEffectWrite {
		return fmt.Errorf("agent: operation %q effect must be read or write", op.Name)
	}
	if op.TerminalBatchLimit != 0 {
		if !op.Terminal || op.Effect != OperationEffectWrite {
			return fmt.Errorf("agent: operation %q terminal batching requires a terminal write operation", op.Name)
		}
		if op.TerminalBatchLimit < 2 || op.TerminalBatchLimit > MaxTerminalBatchLimit {
			return fmt.Errorf(
				"agent: operation %q terminal batch limit must be between 2 and %d",
				op.Name, MaxTerminalBatchLimit,
			)
		}
	}
	if op.Confirmation.Mode != ConfirmationNone && op.Confirmation.Mode != ConfirmationRequired {
		return fmt.Errorf("agent: operation %q confirmation mode must be none or required", op.Name)
	}
	op.Confirmation.Description = strings.TrimSpace(op.Confirmation.Description)
	if op.Confirmation.Mode == ConfirmationRequired && op.Confirmation.Description == "" {
		return fmt.Errorf("agent: operation %q confirmation description is required", op.Name)
	}
	if op.Confirmation.Mode != ConfirmationRequired && op.Confirmation.Description != "" {
		return fmt.Errorf("agent: operation %q without confirmation cannot declare a confirmation description", op.Name)
	}
	if op.Effect == OperationEffectWrite &&
		op.Confirmation.Mode == ConfirmationRequired && op.ApprovalPreview == nil {
		return fmt.Errorf("agent: write operation %q requires a safe approval preview", op.Name)
	}
	if op.Effect == OperationEffectWrite &&
		op.Confirmation.Mode != ConfirmationRequired && op.ApprovalPreview != nil {
		return fmt.Errorf("agent: direct write operation %q cannot declare an approval preview", op.Name)
	}
	if err := validateOperationCapabilities(*op); err != nil {
		return err
	}
	return nil
}

func compileOperationSchemas(op Operation) (compiledOperationSchemas, error) {
	compiledInput, err := compileOperationSchema(op.Name+":input", op.InputSchema)
	if err != nil {
		return compiledOperationSchemas{}, fmt.Errorf("agent: compile operation input schema %q: %w", op.Name, err)
	}
	strictInput, err := strictOpenAIParameters(op.InputSchema)
	if err != nil {
		return compiledOperationSchemas{}, fmt.Errorf("agent: build OpenAI strict input schema %q: %w", op.Name, err)
	}
	strictInputRaw, err := json.Marshal(strictInput)
	if err != nil {
		return compiledOperationSchemas{}, fmt.Errorf("agent: marshal OpenAI strict input schema %q: %w", op.Name, err)
	}
	compiledStrictInput, err := compileOperationSchema(op.Name+":openai-strict-input", strictInputRaw)
	if err != nil {
		return compiledOperationSchemas{}, fmt.Errorf("agent: compile OpenAI strict input schema %q: %w", op.Name, err)
	}
	compiledOutput, err := compileOperationSchema(op.Name+":output", op.OutputSchema)
	if err != nil {
		return compiledOperationSchemas{}, fmt.Errorf("agent: compile operation output schema %q: %w", op.Name, err)
	}
	return compiledOperationSchemas{
		input:  operationInputSchemas{declared: compiledInput, openAIStrict: compiledStrictInput},
		output: compiledOutput,
	}, nil
}

func validateOperationCapabilities(op Operation) error {
	for i, capability := range op.Capabilities {
		if strings.TrimSpace(capability) == "" {
			return fmt.Errorf("agent: operation %q capability %d is empty", op.Name, i)
		}
	}
	return nil
}

func (r *OperationRegistry) Freeze() error {
	if r == nil {
		return errors.New("agent: nil operation registry")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return nil
	}
	r.summaries = r.buildSummariesLocked()
	r.frozen = true
	return nil
}

func (r *OperationRegistry) Get(name string) (Operation, bool) {
	if r == nil {
		return Operation{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	op, ok := r.ops[strings.TrimSpace(name)]
	return cloneOperation(op), ok
}

func (r *OperationRegistry) Summaries() []OperationSummary {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.frozen {
		return cloneOperationSummaries(r.summaries)
	}
	return r.buildSummariesLocked()
}

func (r *OperationRegistry) buildSummariesLocked() []OperationSummary {
	out := make([]OperationSummary, 0, len(r.ops))
	for _, op := range r.ops {
		out = append(out, operationSummary(op))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *OperationRegistry) Provides(requirement string) bool {
	requirement = strings.TrimSpace(requirement)
	if requirement == "" || r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.provided[requirement]
	return ok
}

func (r *OperationRegistry) ValidateInput(name string, input json.RawMessage) error {
	_, err := r.DecodeInput(name, input)
	return err
}

func (r *OperationRegistry) DecodeInput(name string, input json.RawMessage) (any, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	r.mu.RLock()
	schemas, ok := r.inputSchemas[strings.TrimSpace(name)]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("agent: operation %q input is invalid JSON: %w", name, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("agent: operation %q input is invalid JSON: %w", name, err)
	}
	declaredErr := schemas.declared.Validate(value)
	if declaredErr != nil {
		strictErr := schemas.openAIStrict.Validate(value)
		if strictErr != nil {
			return nil, fmt.Errorf(
				"agent: operation %q input does not match schema (declared or OpenAI strict): declared: %w; OpenAI strict: %v",
				name,
				declaredErr,
				strictErr,
			)
		}
	}
	return value, nil
}

func (r *OperationRegistry) NormalizeInput(name string, arguments any) (any, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	r.mu.RLock()
	op, ok := r.ops[strings.TrimSpace(name)]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	if op.NormalizeInput == nil {
		return arguments, nil
	}
	normalized, err := op.NormalizeInput(arguments)
	if err != nil {
		return nil, fmt.Errorf("agent: normalize operation %q input: %w", name, err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal normalized operation %q input: %w", name, err)
	}
	validated, err := r.DecodeInput(name, raw)
	if err != nil {
		return nil, fmt.Errorf("agent: normalized operation %q input is invalid: %w", name, err)
	}
	return validated, nil
}

func (r *OperationRegistry) ValidateOutput(name string, output json.RawMessage) error {
	_, err := r.DecodeOutput(name, output)
	return err
}

func (r *OperationRegistry) DecodeOutput(name string, output json.RawMessage) (any, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	r.mu.RLock()
	schema, ok := r.outputSchemas[strings.TrimSpace(name)]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("agent: operation %q output is invalid JSON: %w", name, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("agent: operation %q output is invalid JSON: %w", name, err)
	}
	if err := schema.Validate(value); err != nil {
		return nil, fmt.Errorf("agent: operation %q output does not match schema: %w", name, err)
	}
	return value, nil
}
