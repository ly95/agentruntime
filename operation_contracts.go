package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func (r *OperationRegistry) BuildApprovalPreview(name string, arguments any) (json.RawMessage, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	r.mu.RLock()
	op, ok := r.ops[strings.TrimSpace(name)]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	if op.Effect != OperationEffectWrite || op.ApprovalPreview == nil {
		return nil, fmt.Errorf("agent: operation %q has no approval preview", name)
	}
	preview, err := op.ApprovalPreview(arguments)
	if err != nil {
		return nil, fmt.Errorf("agent: build approval preview for %q: %w", name, err)
	}
	if len(preview) == 0 || !json.Valid(preview) {
		return nil, fmt.Errorf("agent: approval preview for %q must be valid JSON", name)
	}
	var object map[string]any
	if err := json.Unmarshal(preview, &object); err != nil || object == nil {
		return nil, fmt.Errorf("agent: approval preview for %q must be a JSON object", name)
	}
	if len(object) == 0 {
		return nil, fmt.Errorf("agent: approval preview for %q must describe at least one change", name)
	}
	return append(json.RawMessage(nil), preview...), nil
}

func compileOperationSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	location := "urn:agent:operation:" + name
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(location, document); err != nil {
		return nil, err
	}
	return compiler.Compile(location)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}

func validateOperationInputSchema(raw json.RawMessage) error {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	if document == nil {
		return errors.New("must be a JSON object schema")
	}
	typeName, _ := document["type"].(string)
	if typeName != "object" {
		return errors.New(`top-level type must be "object"`)
	}
	return nil
}

func operationSummary(op Operation) OperationSummary {
	return OperationSummary{
		Name:               op.Name,
		PreviousNames:      append([]string(nil), op.PreviousNames...),
		Description:        op.Description,
		InputSchema:        append(json.RawMessage(nil), op.InputSchema...),
		OutputSchema:       append(json.RawMessage(nil), op.OutputSchema...),
		Effect:             op.Effect,
		Capabilities:       append([]string(nil), op.Capabilities...),
		Confirmation:       op.Confirmation,
		Terminal:           op.Terminal,
		TerminalBatchLimit: op.TerminalBatchLimit,
	}
}

func cloneOperation(op Operation) Operation {
	op.InputSchema = append(json.RawMessage(nil), op.InputSchema...)
	op.OutputSchema = append(json.RawMessage(nil), op.OutputSchema...)
	op.Capabilities = append([]string(nil), op.Capabilities...)
	op.PreviousNames = append([]string(nil), op.PreviousNames...)
	return op
}

func cloneOperationSummaries(summaries []OperationSummary) []OperationSummary {
	out := make([]OperationSummary, len(summaries))
	for i, summary := range summaries {
		out[i] = summary
		out[i].InputSchema = append(json.RawMessage(nil), summary.InputSchema...)
		out[i].OutputSchema = append(json.RawMessage(nil), summary.OutputSchema...)
		out[i].Capabilities = append([]string(nil), summary.Capabilities...)
		out[i].PreviousNames = append([]string(nil), summary.PreviousNames...)
	}
	return out
}

type PolicyAction string

const (
	PolicyAllow           PolicyAction = "allow"
	PolicyDeny            PolicyAction = "deny"
	PolicyRequireApproval PolicyAction = "require_approval"
)

type PolicyDecision struct {
	Action PolicyAction
	Reason string
}

type OperationPolicy interface {
	Evaluate(ctx context.Context, req OperationRequest) (PolicyDecision, error)
}

type OperationPolicyFunc func(ctx context.Context, req OperationRequest) (PolicyDecision, error)

func (f OperationPolicyFunc) Evaluate(ctx context.Context, req OperationRequest) (PolicyDecision, error) {
	return f(ctx, req)
}

type OperationExecutor interface {
	Execute(ctx context.Context, req OperationRequest) (OperationResult, error)
}

type OperationResult struct {
	Output        json.RawMessage  `json:"output"`
	Receipt       json.RawMessage  `json:"receipt,omitempty"`
	FinalResponse string           `json:"final_response,omitempty"`
	Artifacts     []ResultArtifact `json:"artifacts,omitempty"`
	// Continue lets a terminal read operation return a successful,
	// schema-validated correction result to the model without completing the
	// Run. It is only valid when no final response, receipt, or artifacts were
	// produced.
	Continue bool `json:"continue,omitempty"`
}

// MaxResultArtifactSessionSummaryBytes bounds one host-provided artifact
// projection and the complete historical record Runtime persists from it.
const MaxResultArtifactSessionSummaryBytes = 8 * 1024

// ResultArtifact carries a domain-neutral terminal result to the host. Data is
// safe for the model transcript and host protocol; InternalData is retained
// only on the terminal RunRecord so the host can materialize private state.
// SessionSummary is a bounded host-authored projection used only when Runtime
// persists future model context. Runtime events, operation-result items, and
// tool results always receive a public-only copy without either private field.
type ResultArtifact struct {
	Type           string          `json:"type"`
	Data           json.RawMessage `json:"data"`
	InternalData   json.RawMessage `json:"internal_data,omitempty"`
	SessionSummary json.RawMessage `json:"session_summary,omitempty"`
}

func validateResultArtifacts(artifacts []ResultArtifact) error {
	for index, artifact := range artifacts {
		if strings.TrimSpace(artifact.Type) == "" {
			return fmt.Errorf("result artifact %d type is required", index)
		}
		if len(artifact.Data) == 0 || !json.Valid(artifact.Data) {
			return fmt.Errorf("result artifact %d data must be valid JSON", index)
		}
		if len(artifact.InternalData) > 0 && !json.Valid(artifact.InternalData) {
			return fmt.Errorf("result artifact %d internal data must be valid JSON", index)
		}
		if len(artifact.SessionSummary) > 0 {
			if len(artifact.SessionSummary) > MaxResultArtifactSessionSummaryBytes {
				return fmt.Errorf("result artifact %d session summary exceeds %d bytes", index, MaxResultArtifactSessionSummaryBytes)
			}
			if !utf8.Valid(artifact.SessionSummary) || !json.Valid(artifact.SessionSummary) {
				return fmt.Errorf("result artifact %d session summary must be valid UTF-8 JSON", index)
			}
			trimmed := bytes.TrimSpace(artifact.SessionSummary)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				return fmt.Errorf("result artifact %d session summary must be a JSON object", index)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
				return fmt.Errorf("result artifact %d session summary must be a JSON object", index)
			}
		}
	}
	return nil
}

type OperationExecutorFunc func(ctx context.Context, req OperationRequest) (OperationResult, error)

func (f OperationExecutorFunc) Execute(ctx context.Context, req OperationRequest) (OperationResult, error) {
	return f(ctx, req)
}

type VerificationRequest struct {
	Operation OperationRequest
	Result    OperationResult
	Output    any
}

type VerificationResult struct {
	Confirmed bool            `json:"confirmed"`
	Message   string          `json:"message,omitempty"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
}

type ResultVerifier interface {
	Verify(ctx context.Context, req VerificationRequest) (VerificationResult, error)
}

type ResultVerifierFunc func(ctx context.Context, req VerificationRequest) (VerificationResult, error)

func (f ResultVerifierFunc) Verify(ctx context.Context, req VerificationRequest) (VerificationResult, error) {
	return f(ctx, req)
}

type ApprovalRequest struct {
	Operation   OperationRequest
	Reason      string
	ResponseID  string
	ModelOutput []ModelOutputItem
	Preview     json.RawMessage
}

type ApprovalDecision struct {
	ID       string `json:"id,omitempty"`
	Approved bool   `json:"approved"`
	Pending  bool   `json:"pending,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type Approver interface {
	RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}

type ApprovalResume struct {
	ID          string
	ExecutionID string
	Operation   string
	Call        ToolCall
	ResponseID  string
	ModelOutput []ModelOutputItem
	Preview     json.RawMessage
	Pending     bool
	Approved    bool
	Reason      string
}

type ApprovalResumer interface {
	ResumeApproval(ctx context.Context, runID string) (*ApprovalResume, error)
}

type ApproverFunc func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)

func (f ApproverFunc) RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	return f(ctx, req)
}

func normalizeNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
