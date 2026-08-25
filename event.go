package agentruntime

import "encoding/json"

type EventType string

const (
	EventRunStarted                 EventType = "run_started"
	EventModelStarted               EventType = "model_started"
	EventModelStreamChunk           EventType = "model_stream_chunk"
	EventModelCompleted             EventType = "model_completed"
	EventModelFailed                EventType = "model_failed"
	EventContextCompactionStarted   EventType = "context_compaction_started"
	EventContextCompactionCompleted EventType = "context_compaction_completed"
	EventContextCompactionFailed    EventType = "context_compaction_failed"
	EventSessionLeaseRenewed        EventType = "session_lease_renewed"
	EventOperationPlanReserved      EventType = "operation_plan_reserved"
	EventOperationPlanRejected      EventType = "operation_plan_rejected"
	EventOperationPlanSealed        EventType = "operation_plan_sealed"
	EventOperationRequested         EventType = "operation_requested"
	EventOperationStarted           EventType = "operation_started"
	EventOperationCompleted         EventType = "operation_completed"
	EventOperationCancelled         EventType = "operation_cancelled"
	EventOperationFailed            EventType = "operation_failed"
	EventVerificationStarted        EventType = "verification_started"
	EventVerificationCompleted      EventType = "verification_completed"
	EventVerificationFailed         EventType = "verification_failed"
	EventReconciliationStarted      EventType = "reconciliation_started"
	EventReconciliationCompleted    EventType = "reconciliation_completed"
	EventReconciliationFailed       EventType = "reconciliation_failed"
	EventApprovalRequested          EventType = "approval_requested"
	EventApprovalCompleted          EventType = "approval_completed"
	EventApprovalFailed             EventType = "approval_failed"
	EventRunWaitingUser             EventType = "run_waiting_user"
	EventRunCompleted               EventType = "run_completed"
	EventRunFailed                  EventType = "run_failed"
	EventRunInterrupted             EventType = "run_interrupted"
	EventRunCancelled               EventType = "run_cancelled"
)

type Event struct {
	Type            EventType         `json:"type"`
	RunID           string            `json:"run_id"`
	SessionID       string            `json:"session_id,omitempty"`
	Operation       string            `json:"operation,omitempty"`
	ModelCallID     string            `json:"model_call_id,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`
	PlanBatch       uint64            `json:"plan_batch,omitempty"`
	CallID          string            `json:"call_id,omitempty"`
	ExecutionID     string            `json:"execution_id,omitempty"`
	ApprovalID      string            `json:"approval_id,omitempty"`
	ApprovalDigest  string            `json:"approval_digest,omitempty"`
	ApprovalReason  string            `json:"approval_reason,omitempty"`
	ApprovalPreview json.RawMessage   `json:"approval_preview,omitempty"`
	AttemptID       string            `json:"attempt_id,omitempty"`
	Reconciliation  string            `json:"reconciliation,omitempty"`
	ResponseID      string            `json:"response_id,omitempty"`
	Text            string            `json:"text,omitempty"`
	InputTokens     int               `json:"input_tokens,omitempty"`
	OutputTokens    int               `json:"output_tokens,omitempty"`
	TotalTokens     int               `json:"total_tokens,omitempty"`
	LeaseGeneration uint64            `json:"lease_generation,omitempty"`
	SessionRevision uint64            `json:"session_revision,omitempty"`
	CompactedItems  int               `json:"compacted_items,omitempty"`
	Chunk           *ModelStreamEvent `json:"chunk,omitempty"`
	// Data is trusted audit payload and never crosses the default JSON boundary.
	Data      json.RawMessage `json:"-"`
	ErrorCode string          `json:"error_code,omitempty"`
	Error     string          `json:"-"`
}

type EventSink func(Event)

// PendingApprovalSummary is the public, restart-safe subset of a durable
// approval checkpoint. Preview is operation-authored JSON intended for safe,
// schema-aware rendering; hosts must still escape all displayed strings.
type PendingApprovalSummary struct {
	ID          string          `json:"id"`
	Digest      string          `json:"digest"`
	Operation   string          `json:"operation"`
	ExecutionID string          `json:"execution_id"`
	Reason      string          `json:"reason,omitempty"`
	Preview     json.RawMessage `json:"preview"`
}
