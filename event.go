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
	EventMCPConnected               EventType = "mcp_connected"
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
	MCPServer       string            `json:"mcp_server,omitempty"`
	MCPVersion      string            `json:"mcp_version,omitempty"`
	MCPProtocol     string            `json:"mcp_protocol,omitempty"`
	MCPToolCount    int               `json:"mcp_tool_count,omitempty"`
	Operation       string            `json:"operation,omitempty"`
	ModelCallID     string            `json:"model_call_id,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`
	PlanBatch       uint64            `json:"plan_batch"`
	CallID          string            `json:"call_id,omitempty"`
	ExecutionID     string            `json:"execution_id,omitempty"`
	ApprovalID      string            `json:"approval_id,omitempty"`
	ApprovalPreview json.RawMessage   `json:"approval_preview,omitempty"`
	AttemptID       string            `json:"attempt_id,omitempty"`
	ResponseID      string            `json:"response_id,omitempty"`
	Text            string            `json:"text,omitempty"`
	InputTokens     int               `json:"input_tokens,omitempty"`
	CompactedItems  int               `json:"compacted_items,omitempty"`
	Chunk           *ModelStreamEvent `json:"chunk,omitempty"`
	// Data is trusted audit payload and never crosses the default JSON boundary.
	Data      json.RawMessage `json:"-"`
	ErrorCode string          `json:"error_code,omitempty"`
	Error     string          `json:"-"`
}

type EventSink func(Event)
