package agentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
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
		return nil, fmt.Errorf("agent: build approval preview for %q: %w", name, validateUTF8Error("approval preview", err))
	}
	if len(preview) == 0 {
		return nil, fmt.Errorf("agent: approval preview for %q must be valid JSON", name)
	}
	value, err := decodeExactJSON(preview)
	if err != nil {
		return nil, fmt.Errorf("agent: approval preview for %q must be unambiguous valid JSON: %w", name, err)
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("agent: approval preview for %q must be a JSON object", name)
	}
	if len(object) == 0 {
		return nil, fmt.Errorf("agent: approval preview for %q must describe at least one change", name)
	}
	return append(json.RawMessage(nil), preview...), nil
}

func compileOperationSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	document, err := decodeExactJSON(raw)
	if err != nil {
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
	value, err := decodeExactJSON(raw)
	if err != nil {
		return err
	}
	document, ok := value.(map[string]any)
	if !ok || document == nil {
		return errors.New("must be a JSON object schema")
	}
	typeName, _ := document["type"].(string)
	if typeName != "object" {
		return errors.New(`top-level type must be "object"`)
	}
	return nil
}

func operationSummary(op Operation) OperationSummary {
	confirmation := op.Confirmation
	confirmation.Description = strings.TrimSpace(confirmation.Description)
	summary := OperationSummary{
		Name:               strings.TrimSpace(op.Name),
		PreviousNames:      normalizeNames(op.PreviousNames),
		Description:        strings.TrimSpace(op.Description),
		InputSchema:        append(json.RawMessage(nil), op.InputSchema...),
		OutputSchema:       append(json.RawMessage(nil), op.OutputSchema...),
		Effect:             op.Effect,
		Capabilities:       normalizeNames(op.Capabilities),
		Confirmation:       confirmation,
		Terminal:           op.Terminal,
		TerminalBatchLimit: op.TerminalBatchLimit,
	}
	summary.ContractID = operationContractID(summary, op)
	return summary
}

func operationContractID(summary OperationSummary, op Operation) string {
	digest := sha256.New()
	writeHashField(digest, []byte(summary.Name))
	writeHashField(digest, []byte(strings.TrimSpace(op.ContractVersion)))
	for _, previousName := range summary.PreviousNames {
		writeHashField(digest, []byte(previousName))
	}
	writeHashField(digest, []byte(summary.Description))
	writeHashField(digest, operationSchemaIdentity(op.InputSchema, op.inputSchemaIdentity))
	writeHashField(digest, operationSchemaIdentity(op.OutputSchema, op.outputSchemaIdentity))
	writeHashField(digest, []byte(summary.Effect))
	for _, capability := range summary.Capabilities {
		writeHashField(digest, []byte(capability))
	}
	writeHashField(digest, []byte(summary.Confirmation.Mode))
	writeHashField(digest, []byte(summary.Confirmation.Description))
	writeHashField(digest, []byte(strconv.FormatBool(op.NormalizeInput != nil)))
	writeHashField(digest, []byte(strconv.FormatBool(op.ApprovalPreview != nil)))
	writeHashField(digest, []byte(strconv.FormatBool(op.ProjectTerminalSession != nil)))
	writeHashField(digest, []byte(strconv.FormatBool(summary.Terminal)))
	writeHashField(digest, []byte(strconv.Itoa(summary.TerminalBatchLimit)))
	return "contract_" + hex.EncodeToString(digest.Sum(nil))
}

func operationSchemaIdentity(raw, registeredIdentity json.RawMessage) []byte {
	if len(registeredIdentity) != 0 {
		return registeredIdentity
	}
	canonical, err := canonicalJSONIdentity(raw)
	if err != nil {
		// Unregistered Operations can be summarized by package-internal callers,
		// but Register rejects this invalid schema before it can enter Runtime.
		return append([]byte(nil), raw...)
	}
	return canonical
}

func operationSetID(summaries []OperationSummary) string {
	if len(summaries) == 0 {
		return ""
	}
	digest := sha256.New()
	for _, summary := range summaries {
		writeHashField(digest, []byte(summary.Name))
		writeHashField(digest, []byte(summary.ContractID))
	}
	return "operation_set_" + hex.EncodeToString(digest.Sum(nil))
}

func cloneOperation(op Operation) Operation {
	op.InputSchema = append(json.RawMessage(nil), op.InputSchema...)
	op.OutputSchema = append(json.RawMessage(nil), op.OutputSchema...)
	op.inputSchemaIdentity = append(json.RawMessage(nil), op.inputSchemaIdentity...)
	op.outputSchemaIdentity = append(json.RawMessage(nil), op.outputSchemaIdentity...)
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

// TerminalSessionProjection is the bounded, public portion of a terminal
// artifact that Runtime may retain as future model context. Terminal writes
// declare these projections before execution through ProjectTerminalSession.
type TerminalSessionProjection struct {
	Type           string          `json:"artifact_type"`
	SessionSummary json.RawMessage `json:"session_summary"`
}

func (r *OperationRegistry) BuildTerminalSessionProjection(name string, arguments any) ([]TerminalSessionProjection, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	r.mu.RLock()
	op, ok := r.ops[strings.TrimSpace(name)]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrOperationNotFound, name)
	}
	if !op.Terminal || op.Effect != OperationEffectWrite || op.ProjectTerminalSession == nil {
		return nil, fmt.Errorf("agent: operation %q has no terminal write session projection", name)
	}
	projections, err := op.ProjectTerminalSession(arguments)
	if err != nil {
		return nil, fmt.Errorf("agent: project terminal session for %q: %w", name, validateUTF8Error("terminal session projector", err))
	}
	projections = cloneTerminalSessionProjections(projections)
	if err := validateTerminalSessionProjections(projections); err != nil {
		return nil, fmt.Errorf("agent: terminal session projection for %q: %w", name, err)
	}
	return projections, nil
}

func cloneTerminalSessionProjections(projections []TerminalSessionProjection) []TerminalSessionProjection {
	out := make([]TerminalSessionProjection, len(projections))
	for index := range projections {
		out[index] = TerminalSessionProjection{
			Type:           strings.TrimSpace(projections[index].Type),
			SessionSummary: append(json.RawMessage(nil), projections[index].SessionSummary...),
		}
	}
	return out
}

func validateTerminalSessionProjections(projections []TerminalSessionProjection) error {
	for index, projection := range projections {
		if !utf8.ValidString(projection.Type) {
			return fmt.Errorf("projection %d type must be valid UTF-8", index)
		}
		if strings.TrimSpace(projection.Type) == "" {
			return fmt.Errorf("projection %d type is required", index)
		}
		if len(projection.SessionSummary) == 0 || len(projection.SessionSummary) > MaxResultArtifactSessionSummaryBytes {
			return fmt.Errorf("projection %d session summary must be between 1 and %d bytes", index, MaxResultArtifactSessionSummaryBytes)
		}
		if !utf8.Valid(projection.SessionSummary) {
			return fmt.Errorf("projection %d session summary must be valid UTF-8 JSON", index)
		}
		trimmed := bytes.TrimSpace(projection.SessionSummary)
		value, err := decodeExactJSON(trimmed)
		if err != nil {
			return fmt.Errorf("projection %d session summary must be unambiguous valid JSON: %w", index, err)
		}
		object, ok := value.(map[string]any)
		if len(trimmed) == 0 || trimmed[0] != '{' || !ok || object == nil {
			return fmt.Errorf("projection %d session summary must be a JSON object", index)
		}
	}
	if _, err := terminalSessionHistoryItem("validation", projections); err != nil {
		return err
	}
	return nil
}

func equalTerminalSessionProjections(left, right []TerminalSessionProjection) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if strings.TrimSpace(left[index].Type) != strings.TrimSpace(right[index].Type) ||
			!jsonSemanticallyEqual(left[index].SessionSummary, right[index].SessionSummary) {
			return false
		}
	}
	return true
}

func validateResultArtifacts(artifacts []ResultArtifact) error {
	for index, artifact := range artifacts {
		if !utf8.ValidString(artifact.Type) {
			return fmt.Errorf("result artifact %d type must be valid UTF-8", index)
		}
		if strings.TrimSpace(artifact.Type) == "" {
			return fmt.Errorf("result artifact %d type is required", index)
		}
		if len(artifact.Data) == 0 {
			return fmt.Errorf("result artifact %d data must be valid JSON", index)
		}
		if _, err := decodeExactJSON(artifact.Data); err != nil {
			return fmt.Errorf("result artifact %d data must be valid JSON and unambiguous: %w", index, err)
		}
		if len(artifact.InternalData) > 0 {
			if _, err := decodeExactJSON(artifact.InternalData); err != nil {
				return fmt.Errorf("result artifact %d internal data must be valid JSON and unambiguous: %w", index, err)
			}
		}
		if len(artifact.SessionSummary) > 0 {
			if len(artifact.SessionSummary) > MaxResultArtifactSessionSummaryBytes {
				return fmt.Errorf("result artifact %d session summary exceeds %d bytes", index, MaxResultArtifactSessionSummaryBytes)
			}
			if !utf8.Valid(artifact.SessionSummary) {
				return fmt.Errorf("result artifact %d session summary must be valid UTF-8 JSON", index)
			}
			trimmed := bytes.TrimSpace(artifact.SessionSummary)
			value, err := decodeExactJSON(trimmed)
			if err != nil {
				return fmt.Errorf("result artifact %d session summary must be unambiguous valid JSON: %w", index, err)
			}
			object, ok := value.(map[string]any)
			if len(trimmed) == 0 || trimmed[0] != '{' || !ok || object == nil {
				return fmt.Errorf("result artifact %d session summary must be a JSON object", index)
			}
		}
	}
	if projections := terminalArtifactProjections(artifacts); len(projections) > 0 {
		if err := validateTerminalSessionProjections(projections); err != nil {
			return fmt.Errorf("terminal session projection: %w", err)
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
	Checkpoint  *ApprovalCheckpoint
}

// ApprovalCheckpoint is the immutable in-run state required to resume one
// pending approval without replaying or renumbering earlier operation batches.
type ApprovalCheckpoint struct {
	Transcript              []ModelInputItem
	ContextCheckpoint       *ContextCheckpoint
	SeenCallIDs             []string
	OperationBatchCount     uint64
	PlanBatchIndex          uint64
	PlanCallID              string
	PlanExecutionID         string
	InputDigest             string
	ExpectedSessionRevision uint64 `json:"ExpectedSessionRevision,omitempty"`
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
	ContractID  string
	Call        ToolCall
	ResponseID  string
	ModelOutput []ModelOutputItem
	Preview     json.RawMessage
	Checkpoint  *ApprovalCheckpoint
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
