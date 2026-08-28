package agentruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (r *Runtime) cloneApprovalRequest(request ApprovalRequest) (ApprovalRequest, error) {
	cloned := request
	cloned.ModelOutput = cloneModelOutputItems(request.ModelOutput)
	cloned.Preview = append(json.RawMessage(nil), request.Preview...)
	cloned.Checkpoint = cloneApprovalCheckpoint(request.Checkpoint, false)
	cloned.Operation = request.Operation
	input, err := cloneOperationInput(request.Operation.Input)
	if err != nil {
		return ApprovalRequest{}, err
	}
	cloned.Operation.Input = input
	cloned.Operation.Operation = cloneOperationSummaries([]OperationSummary{request.Operation.Operation})[0]
	cloned.Operation.Call.Input = append(json.RawMessage(nil), request.Operation.Call.Input...)
	arguments, err := json.Marshal(request.Operation.Arguments)
	if err != nil {
		return ApprovalRequest{}, err
	}
	cloned.Operation.Arguments, err = r.operations.DecodeInput(request.Operation.Call.Name, arguments)
	if err != nil {
		return ApprovalRequest{}, err
	}
	return cloned, nil
}

func (r *Runtime) clonePersistentApprovalRequest(request ApprovalRequest) (ApprovalRequest, error) {
	cloned, err := r.cloneApprovalRequest(request)
	if err != nil {
		return ApprovalRequest{}, err
	}
	cloned.Operation.Input, err = clonePersistentOperationInput(request.Operation.Input)
	if err != nil {
		return ApprovalRequest{}, err
	}
	cloned.Checkpoint = cloneApprovalCheckpoint(request.Checkpoint, true)
	return cloned, nil
}

func (r *Runtime) clonePersistentPendingApprovalCommit(pending PendingApprovalCommit) (PendingApprovalCommit, error) {
	request, err := r.clonePersistentApprovalRequest(pending.Request)
	if err != nil {
		return PendingApprovalCommit{}, err
	}
	audit := pending.Audit
	audit.Data = append(json.RawMessage(nil), pending.Audit.Data...)
	return PendingApprovalCommit{
		AuthorityVersion: pending.AuthorityVersion,
		Request:          request, Decision: pending.Decision, Audit: audit, Digest: pending.Digest,
	}, nil
}

// PendingApprovalAuthorityVersion is the current complete durable approval
// authority schema accepted by RunStore V4.
const PendingApprovalAuthorityVersion uint32 = 2

const pendingApprovalAuthorityVersion = PendingApprovalAuthorityVersion

// AuthorityDigest computes the canonical complete v2 durable authority digest.
// RunStore implementations should use this method rather than duplicating the
// canonical JSON and replay-envelope normalization algorithm.
func (pending PendingApprovalCommit) AuthorityDigest() (string, error) {
	return pendingApprovalAuthorityDigest(pending)
}

// ValidateAuthority verifies the current authority version, checkpoint model
// binding, and all digest-covered request, decision, audit, input, and replay
// data. It accepts the exact legacy lexical digest of a complete v2 record for
// compatibility; AuthorityDigest always returns the canonical current digest.
func (pending PendingApprovalCommit) ValidateAuthority(modelBindingID string) error {
	return validatePendingApprovalCommitAuthority(pending, modelBindingID, "pending approval")
}

// pendingApprovalAuthorityRecord covers the complete persistent commit. The
// explicit fields preserve semantic Input/OperationSummary members whose
// public JSON forms intentionally omit host-only values.
type pendingApprovalAuthorityRecord struct {
	AuthorityVersion       uint32           `json:"authority_version"`
	Request                ApprovalRequest  `json:"request"`
	Decision               ApprovalDecision `json:"decision"`
	Audit                  ItemRecord       `json:"audit"`
	InputRunID             string           `json:"input_run_id"`
	InputIdempotencyScope  string           `json:"input_idempotency_scope,omitempty"`
	OperationPreviousNames []string         `json:"operation_previous_names,omitempty"`
}

// approvalResumeAuthorityRecord is only a comparison projection between the
// complete committed request and an ApprovalResumer result. It is not durable
// approval authority; PendingApprovalCommit uses the complete record above.
type approvalResumeAuthorityRecord struct {
	ApprovalID  string              `json:"approval_id"`
	ExecutionID string              `json:"execution_id"`
	Operation   string              `json:"operation"`
	ContractID  string              `json:"contract_id"`
	Call        ToolCall            `json:"call"`
	ResponseID  string              `json:"response_id"`
	ModelOutput []ModelOutputItem   `json:"model_output"`
	Preview     json.RawMessage     `json:"preview"`
	Checkpoint  *ApprovalCheckpoint `json:"checkpoint"`
}

func pendingApprovalAuthorityRecordForCommit(pending PendingApprovalCommit) pendingApprovalAuthorityRecord {
	return pendingApprovalAuthorityRecord{
		AuthorityVersion: pending.AuthorityVersion,
		Request:          pending.Request, Decision: pending.Decision, Audit: pending.Audit,
		InputRunID:             pending.Request.Operation.Input.RunID,
		InputIdempotencyScope:  pending.Request.Operation.Input.IdempotencyScope,
		OperationPreviousNames: append([]string(nil), pending.Request.Operation.Operation.PreviousNames...),
	}
}

func pendingApprovalAuthorityDigest(pending PendingApprovalCommit) (string, error) {
	return completePendingApprovalAuthorityDigest(pendingApprovalAuthorityRecordForCommit(pending))
}

func validatePendingApprovalCommitAuthority(pending PendingApprovalCommit, modelBindingID, subject string) error {
	if err := requireCanonicalIdentity(modelBindingID, "model binding id"); err != nil {
		return fmt.Errorf("%w: %s has invalid expected model authority: %v", ErrModelBindingMismatch, subject, err)
	}
	if pending.AuthorityVersion != PendingApprovalAuthorityVersion {
		return fmt.Errorf(
			"%w: %s has unsupported pending approval authority version %d",
			ErrOperationPlanChanged, subject, pending.AuthorityVersion,
		)
	}
	checkpoint := pending.Request.Checkpoint
	if checkpoint == nil {
		return fmt.Errorf("%w: %s has no approval checkpoint", ErrOperationPlanChanged, subject)
	}
	if checkpoint.ModelBindingID != modelBindingID {
		return fmt.Errorf("%w: %s approval checkpoint", ErrModelBindingMismatch, subject)
	}
	expected, err := pending.AuthorityDigest()
	if err != nil {
		return fmt.Errorf("%w: %s authority cannot be canonicalized: %v", ErrOperationPlanChanged, subject, err)
	}
	if err := validateApprovalAuthorityDigest(pending.Digest); err != nil {
		return fmt.Errorf("%w: %s has invalid authority digest: %v", ErrOperationPlanChanged, subject, err)
	}
	if pending.Digest != expected {
		legacy, legacyErr := legacyPendingApprovalAuthorityDigest(pending)
		if legacyErr != nil || pending.Digest != legacy {
			return fmt.Errorf("%w: %s authority digest does not match its complete commit", ErrOperationPlanChanged, subject)
		}
	}
	return nil
}

func legacyPendingApprovalAuthorityDigest(pending PendingApprovalCommit) (string, error) {
	return legacyCompletePendingApprovalAuthorityDigest(pendingApprovalAuthorityRecordForCommit(pending))
}

func approvalResumeAuthorityDigest(resume *ApprovalResume) (string, error) {
	if resume == nil {
		return "", errors.New("approval resume is nil")
	}
	return approvalResumeProjectionDigest(approvalAuthorityRecordForResume(resume))
}

func approvalAuthorityRecordForResume(resume *ApprovalResume) approvalResumeAuthorityRecord {
	return approvalResumeAuthorityRecord{
		ApprovalID: strings.TrimSpace(resume.ID), ExecutionID: resume.ExecutionID,
		Operation: resume.Operation, ContractID: resume.ContractID, Call: resume.Call,
		ResponseID: resume.ResponseID, ModelOutput: resume.ModelOutput,
		Preview: resume.Preview, Checkpoint: resume.Checkpoint,
	}
}

func approvalResumeProjectionDigest(record approvalResumeAuthorityRecord) (string, error) {
	if err := validateUTF8Boundary("approval authority", record); err != nil {
		return "", err
	}
	payload, err := canonicalApprovalResumeProjectionPayload(record)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "approval_" + hex.EncodeToString(digest[:]), nil
}

func validateApprovalAuthorityDigest(digest string) error {
	const prefix = "approval_"
	if digest != strings.TrimSpace(digest) || !strings.HasPrefix(digest, prefix) {
		return errors.New("agent: approval authority digest is not canonical")
	}
	encoded := strings.TrimPrefix(digest, prefix)
	if len(encoded) != sha256.Size*2 || encoded != strings.ToLower(encoded) {
		return errors.New("agent: approval authority digest is not canonical")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("agent: approval authority digest is not canonical")
	}
	return nil
}

func canonicalApprovalResumeProjectionPayload(record approvalResumeAuthorityRecord) ([]byte, error) {
	var err error
	record.Call.Input, err = canonicalApprovalAuthorityJSON(record.Call.Input, "function call input")
	if err != nil {
		return nil, err
	}
	record.Preview, err = canonicalApprovalAuthorityJSON(record.Preview, "preview")
	if err != nil {
		return nil, err
	}
	record.ModelOutput = cloneModelOutputItems(record.ModelOutput)
	for index := range record.ModelOutput {
		item := &record.ModelOutput[index]
		if item.Call != nil {
			item.Call.Input, err = canonicalApprovalAuthorityJSON(item.Call.Input, fmt.Sprintf("model output %d function call input", index))
			if err != nil {
				return nil, err
			}
		}
		item.Raw, err = canonicalApprovalReplayEnvelope(item.Raw, item.Type, fmt.Sprintf("model output %d raw payload", index))
		if err != nil {
			return nil, err
		}
	}
	if record.Checkpoint != nil {
		if err := validatePersistedModelInputItems(record.Checkpoint.Transcript); err != nil {
			return nil, fmt.Errorf("agent: validate approval authority checkpoint transcript: %w", err)
		}
	}
	record.Checkpoint = cloneApprovalCheckpoint(record.Checkpoint, true)
	if record.Checkpoint != nil {
		for index := range record.Checkpoint.Transcript {
			item := &record.Checkpoint.Transcript[index]
			if len(item.Raw) > 0 {
				item.Raw, err = canonicalApprovalReplayEnvelope(item.Raw, item.OutputType, fmt.Sprintf("checkpoint transcript item %d raw payload", index))
				if err != nil {
					return nil, err
				}
			}
			if len(item.Output) > 0 {
				item.Output, err = canonicalApprovalAuthorityJSON(item.Output, fmt.Sprintf("checkpoint transcript item %d output", index))
				if err != nil {
					return nil, err
				}
			}
		}
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal approval authority: %w", err)
	}
	canonical, err := canonicalJSONIdentity(payload)
	if err != nil {
		return nil, fmt.Errorf("agent: canonicalize approval authority: %w", err)
	}
	return canonical, nil
}

func completePendingApprovalAuthorityDigest(record pendingApprovalAuthorityRecord) (string, error) {
	if record.AuthorityVersion != PendingApprovalAuthorityVersion {
		return "", fmt.Errorf("agent: unsupported pending approval authority version %d", record.AuthorityVersion)
	}
	if err := validateUTF8Boundary("pending approval authority", record); err != nil {
		return "", err
	}
	payload, err := canonicalPendingApprovalAuthorityPayload(record)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "approval_" + hex.EncodeToString(digest[:]), nil
}

func legacyCompletePendingApprovalAuthorityDigest(record pendingApprovalAuthorityRecord) (string, error) {
	if record.AuthorityVersion != PendingApprovalAuthorityVersion {
		return "", fmt.Errorf("agent: unsupported pending approval authority version %d", record.AuthorityVersion)
	}
	if err := validateUTF8Boundary("pending approval authority", record); err != nil {
		return "", err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("agent: marshal legacy pending approval authority: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "approval_" + hex.EncodeToString(digest[:]), nil
}

func canonicalPendingApprovalAuthorityPayload(record pendingApprovalAuthorityRecord) ([]byte, error) {
	request := record.Request
	var err error
	request.Operation.Input, err = cloneOperationInput(request.Operation.Input)
	if err != nil {
		return nil, err
	}
	request.Operation.Operation = cloneOperationSummaries([]OperationSummary{request.Operation.Operation})[0]
	request.Operation.Call.Input, err = canonicalApprovalAuthorityJSON(request.Operation.Call.Input, "function call input")
	if err != nil {
		return nil, err
	}
	request.Operation.Operation.InputSchema, err = canonicalApprovalAuthorityJSON(request.Operation.Operation.InputSchema, "operation input schema")
	if err != nil {
		return nil, err
	}
	request.Operation.Operation.OutputSchema, err = canonicalApprovalAuthorityJSON(request.Operation.Operation.OutputSchema, "operation output schema")
	if err != nil {
		return nil, err
	}
	arguments, err := json.Marshal(request.Operation.Arguments)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal pending approval arguments: %w", err)
	}
	request.Operation.Arguments, err = decodeExactJSON(arguments)
	if err != nil {
		return nil, fmt.Errorf("agent: canonicalize pending approval arguments: %w", err)
	}
	request.Preview, err = canonicalApprovalAuthorityJSON(request.Preview, "preview")
	if err != nil {
		return nil, err
	}
	request.ModelOutput = cloneModelOutputItems(request.ModelOutput)
	for index := range request.ModelOutput {
		item := &request.ModelOutput[index]
		if item.Call != nil {
			item.Call.Input, err = canonicalApprovalAuthorityJSON(item.Call.Input, fmt.Sprintf("model output %d function call input", index))
			if err != nil {
				return nil, err
			}
		}
		item.Raw, err = canonicalApprovalReplayEnvelope(item.Raw, item.Type, fmt.Sprintf("model output %d raw payload", index))
		if err != nil {
			return nil, err
		}
	}
	request.Checkpoint = cloneApprovalCheckpoint(request.Checkpoint, true)
	if request.Checkpoint != nil {
		if err := validatePersistedModelInputItems(request.Checkpoint.Transcript); err != nil {
			return nil, fmt.Errorf("agent: validate pending approval checkpoint transcript: %w", err)
		}
		for index := range request.Checkpoint.Transcript {
			item := &request.Checkpoint.Transcript[index]
			if len(item.Raw) > 0 {
				item.Raw, err = canonicalApprovalReplayEnvelope(item.Raw, item.OutputType, fmt.Sprintf("checkpoint transcript item %d raw payload", index))
				if err != nil {
					return nil, err
				}
			}
			if len(item.Output) > 0 {
				item.Output, err = canonicalApprovalAuthorityJSON(item.Output, fmt.Sprintf("checkpoint transcript item %d output", index))
				if err != nil {
					return nil, err
				}
			}
		}
	}
	record.Request = request
	record.Audit.Data, err = canonicalApprovalAuthorityJSON(record.Audit.Data, "approval audit data")
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal complete pending approval authority: %w", err)
	}
	canonical, err := canonicalJSONIdentity(payload)
	if err != nil {
		return nil, fmt.Errorf("agent: canonicalize complete pending approval authority: %w", err)
	}
	return canonical, nil
}

func canonicalApprovalAuthorityJSON(raw json.RawMessage, field string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	canonical, err := canonicalJSONIdentity(raw)
	if err != nil {
		return nil, fmt.Errorf("agent: canonicalize approval authority %s: %w", field, err)
	}
	return json.RawMessage(canonical), nil
}

func canonicalApprovalReplayEnvelope(raw json.RawMessage, outputType ModelOutputItemType, field string) (json.RawMessage, error) {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("agent: canonicalize approval authority %s: %w", field, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("agent: canonicalize approval authority %s: raw payload must be a JSON object", field)
	}
	if outputType == ModelOutputFunctionCall {
		if arguments, exists := object["arguments"]; exists {
			text, ok := arguments.(string)
			if !ok {
				return nil, fmt.Errorf("agent: canonicalize approval authority %s: function arguments must be a JSON string", field)
			}
			canonical, err := canonicalJSONIdentity(json.RawMessage(text))
			if err != nil {
				return nil, fmt.Errorf("agent: canonicalize approval authority %s function arguments: %w", field, err)
			}
			object["arguments"] = string(canonical)
		}
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal canonical approval authority %s: %w", field, err)
	}
	canonical, err := canonicalJSONIdentity(encoded)
	if err != nil {
		return nil, fmt.Errorf("agent: canonicalize approval authority %s: %w", field, err)
	}
	return json.RawMessage(canonical), nil
}

func cloneApprovalCheckpoint(checkpoint *ApprovalCheckpoint, persistent bool) *ApprovalCheckpoint {
	if checkpoint == nil {
		return nil
	}
	out := *checkpoint
	if persistent {
		out.Transcript = clonePersistentModelInputItems(checkpoint.Transcript)
	} else {
		out.Transcript = cloneModelInputItems(checkpoint.Transcript)
	}
	out.ContextCheckpoint = cloneContextCheckpoint(checkpoint.ContextCheckpoint)
	out.SeenCallIDs = append([]string(nil), checkpoint.SeenCallIDs...)
	return &out
}
