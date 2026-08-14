package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type RunStatus string

const (
	RunStatusRunning     RunStatus = "running"
	RunStatusWaitingUser RunStatus = "waiting_user"
	RunStatusCompleted   RunStatus = "completed"
	RunStatusFailed      RunStatus = "failed"
	RunStatusInterrupted RunStatus = "interrupted"
	RunStatusCancelled   RunStatus = "cancelled"
)

type ItemType string

const (
	ItemTypeUserMessage       ItemType = "user_message"
	ItemTypeModelRequest      ItemType = "model_request"
	ItemTypeModelResponse     ItemType = "model_response"
	ItemTypeOperationPlan     ItemType = "operation_plan"
	ItemTypeOperationCall     ItemType = "operation_call"
	ItemTypeOperationResult   ItemType = "operation_result"
	ItemTypeVerification      ItemType = "verification"
	ItemTypeApproval          ItemType = "approval"
	ItemTypeError             ItemType = "error"
	ItemTypeContextCheckpoint ItemType = "context_checkpoint"
)

type RunRecord struct {
	ID         string
	SessionID  string
	SkillSetID string `json:"SkillSetID,omitempty"`
	Status     RunStatus
	Input      Input
	Result     string
	Artifacts  []ResultArtifact
	ErrorCode  string
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type ItemRecord struct {
	ID             string
	RunID          string
	SessionID      string
	Type           ItemType
	ModelCallID    string
	ResponseID     string
	ProviderItemID string
	RequestID      string
	PlanBatch      uint64
	CallID         string
	ExecutionID    string
	AttemptID      string
	Name           string
	Data           json.RawMessage
	Error          string
	CreatedAt      time.Time
}

type SessionState struct {
	ID             string
	SkillSetID     string `json:"SkillSetID,omitempty"`
	Revision       uint64
	Transcript     []ModelInputItem
	Checkpoint     *ContextCheckpoint
	SeenCallIDs    []string
	LastResponseID string
	LastRunID      string
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RunHandle proves ownership of the session lease acquired by BeginRun.
// The store, not Runtime, owns lease recovery for abandoned runs.
type RunHandle struct {
	RunID           string
	SessionID       string
	LeaseID         string
	LeaseGeneration uint64
	LeaseDeadline   time.Time
	SessionRevision uint64
}

type BeginRunRequest struct {
	Run      RunRecord
	LeaseID  string
	LeaseTTL time.Duration
}

type BeginRunResult struct {
	Handle  RunHandle
	Session *SessionState
}

type FinishRunRequest struct {
	Handle          RunHandle
	Run             RunRecord
	Session         *SessionState
	PendingApproval *PendingApprovalCommit
}

// PendingApprovalCommit carries the approval request and its audit item into
// the RunStore-owned FinishRun transaction. Stores must create or validate the
// pending approval and append Audit atomically with the waiting_user Run
// transition; an error must leave none of those mutations visible.
type PendingApprovalCommit struct {
	Request  ApprovalRequest
	Decision ApprovalDecision
	Audit    ItemRecord
}

type RenewRunLeaseRequest struct {
	Handle   RunHandle
	LeaseTTL time.Duration
}

type SessionLeaseFence struct {
	RunID           string
	SessionID       string
	LeaseID         string
	Generation      uint64
	Deadline        time.Time
	SessionRevision uint64
}

// RunStore owns the transaction boundary for a run and its session.
// BeginRun atomically creates the running run and acquires the session lease.
// For a new session with a non-empty Run.SkillSetID, BeginRun must also create
// and return a binding-only SessionState at revision zero. That binding is
// independent of transcript readiness and must survive nil-Session terminal
// writes and abandoned-lease recovery. A new run with an empty SkillSet ID
// preserves the legacy behavior and does not require an initial SessionState.
// Before changing a stored session, resuming a waiting run, or fencing an
// active run, stores must compare Run.SkillSetID with every applicable stored
// SessionState or RunRecord binding. A mismatch returns ErrSkillSetMismatch
// without changing run status, acquiring a lease, fencing an owner, or creating
// a session. A legacy record with no binding has the empty SkillSet ID.
// Stores must permit a new run to fence an expired lease, assign a monotonically
// increasing lease generation, and return the store-owned deadline in the handle.
// RenewRunLease extends only a live matching generation. ValidateRunLease lets
// Runtime reject a stale owner immediately before a write side effect.
// FinishRun atomically terminalizes or pauses the run, commits the next session
// snapshot when supplied, and releases the lease. Lease renewal remains active
// while FinishRun executes, so stores must validate the live owner fields and
// deadline; the request handle's observed LeaseDeadline may lag a renewal.
// When PendingApproval is supplied, FinishRun must also atomically persist that
// approval and its audit item. When immutable runtime configuration is mounted,
// Runtime also supplies the last stable Session snapshot so that configuration
// remains bound across pause and resume. With no such binding, Runtime preserves
// the legacy nil-Session pause request. A failed run whose session could not be
// validated may also pass a nil Session; the store must then leave any existing
// snapshot, including a binding-only revision-zero state, unchanged. A non-nil
// Session must retain the binding established by BeginRun.
// An error from either method must leave no mutation.
type RunStore interface {
	BeginRun(ctx context.Context, request BeginRunRequest) (BeginRunResult, error)
	RenewRunLease(ctx context.Context, request RenewRunLeaseRequest) (RunHandle, error)
	ValidateRunLease(ctx context.Context, handle RunHandle) (RunHandle, error)
	AppendItem(ctx context.Context, item ItemRecord) error
	FinishRun(ctx context.Context, request FinishRunRequest) error
}

type OperationPlanStep struct {
	ExecutionID string
	Name        string
	Arguments   json.RawMessage
}

type OperationPlanBatch struct {
	RequestID        string
	SessionID        string
	IdempotencyKey   string
	IdempotencyScope string
	Index            uint64
	Steps            []OperationPlanStep
	CreatedAt        time.Time
}

type OperationPlanSeal struct {
	RequestID        string
	SessionID        string
	IdempotencyKey   string
	IdempotencyScope string
	BatchCount       uint64
	SealedAt         time.Time
}

type PlanBatchReservation struct {
	Batch   OperationPlanBatch
	Created bool
}

type PlanSealResult struct {
	Seal    OperationPlanSeal
	Created bool
}

type OperationExecutionStatus string

const (
	OperationExecutionStarted        OperationExecutionStatus = "started"
	OperationExecutionExecuted       OperationExecutionStatus = "executed"
	OperationExecutionCompleted      OperationExecutionStatus = "completed"
	OperationExecutionUnknown        OperationExecutionStatus = "unknown"
	OperationExecutionRetryable      OperationExecutionStatus = "retryable"
	OperationExecutionRecoveryFailed OperationExecutionStatus = "recovery_failed"
)

type OperationExecutionRecord struct {
	ID               string
	IdempotencyKey   string
	IdempotencyScope string
	RunID            string
	SessionID        string
	CallID           string
	AttemptID        string
	Name             string
	Arguments        json.RawMessage
	Status           OperationExecutionStatus
	Result           OperationResult
	Verification     *VerificationResult
	Error            string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type ExecutionAcquireDisposition string

const (
	ExecutionAcquired ExecutionAcquireDisposition = "acquired"
	ExecutionReplay   ExecutionAcquireDisposition = "replay"
	ExecutionBlocked  ExecutionAcquireDisposition = "blocked"
)

type OperationExecutionTransition struct {
	ID           string
	ExecutionID  string
	AttemptID    string
	RunID        string
	CallID       string
	Actor        string
	Message      string
	From         OperationExecutionStatus
	To           OperationExecutionStatus
	Result       OperationResult
	Verification *VerificationResult
	Evidence     json.RawMessage
	CreatedAt    time.Time
}

type AcquireExecutionRequest struct {
	Execution  OperationExecutionRecord
	Transition OperationExecutionTransition
}

type AcquireExecutionResult struct {
	Execution   OperationExecutionRecord
	Disposition ExecutionAcquireDisposition
}

func (request AcquireExecutionRequest) Validate() error {
	execution := request.Execution
	transition := request.Transition
	if strings.TrimSpace(execution.ID) == "" || strings.TrimSpace(execution.RunID) == "" ||
		strings.TrimSpace(execution.CallID) == "" || strings.TrimSpace(execution.AttemptID) == "" ||
		strings.TrimSpace(execution.Name) == "" {
		return fmt.Errorf("%w: execution id, run id, call id, attempt id, and name are required", ErrInvalidExecutionTransition)
	}
	if strings.TrimSpace(execution.IdempotencyKey) == "" {
		return fmt.Errorf("%w: execution idempotency key is required", ErrInvalidExecutionTransition)
	}
	if strings.TrimSpace(execution.SessionID) == "" && strings.TrimSpace(execution.IdempotencyScope) == "" {
		return fmt.Errorf("%w: stateless execution idempotency scope is required", ErrInvalidExecutionTransition)
	}
	if execution.Status != OperationExecutionStarted {
		return fmt.Errorf("%w: acquisition status must be started", ErrInvalidExecutionTransition)
	}
	if execution.CreatedAt.IsZero() || execution.UpdatedAt.IsZero() || execution.UpdatedAt.Before(execution.CreatedAt) {
		return fmt.Errorf("%w: execution timestamps are invalid", ErrInvalidExecutionTransition)
	}
	if len(execution.Arguments) == 0 || !json.Valid(execution.Arguments) {
		return fmt.Errorf("%w: execution arguments must be valid JSON", ErrInvalidExecutionTransition)
	}
	if hasOperationResult(execution.Result) || execution.Verification != nil || strings.TrimSpace(execution.Error) != "" {
		return fmt.Errorf("%w: started execution cannot contain a result, verification, or error", ErrInvalidExecutionTransition)
	}
	if transition.ExecutionID != execution.ID || transition.AttemptID != execution.AttemptID || transition.RunID != execution.RunID || transition.CallID != execution.CallID {
		return fmt.Errorf("%w: acquisition transition does not match execution", ErrInvalidExecutionTransition)
	}
	if transition.From != "" || transition.To != OperationExecutionStarted {
		return fmt.Errorf("%w: acquisition transition must target started", ErrInvalidExecutionTransition)
	}
	if hasOperationResult(transition.Result) || transition.Verification != nil || len(transition.Evidence) > 0 {
		return fmt.Errorf("%w: acquisition transition cannot contain a result, verification, or evidence", ErrInvalidExecutionTransition)
	}
	return transition.validateFields()
}

func (transition OperationExecutionTransition) Validate() error {
	if err := transition.validateFields(); err != nil {
		return err
	}
	valid := false
	switch transition.From {
	case OperationExecutionStarted:
		valid = transition.To == OperationExecutionExecuted || transition.To == OperationExecutionUnknown ||
			transition.To == OperationExecutionRetryable
	case OperationExecutionExecuted:
		valid = transition.To == OperationExecutionCompleted || transition.To == OperationExecutionRecoveryFailed
	case OperationExecutionUnknown:
		valid = transition.To == OperationExecutionCompleted || transition.To == OperationExecutionRetryable ||
			transition.To == OperationExecutionRecoveryFailed
	case OperationExecutionRetryable:
		valid = transition.To == OperationExecutionStarted
	}
	if !valid {
		return fmt.Errorf("%w: unsupported status change %q -> %q", ErrInvalidExecutionTransition, transition.From, transition.To)
	}
	switch transition.To {
	case OperationExecutionExecuted, OperationExecutionCompleted:
		if len(transition.Result.Output) == 0 || !json.Valid(transition.Result.Output) {
			return fmt.Errorf("%w: transition result output must be valid JSON", ErrInvalidExecutionTransition)
		}
		if len(transition.Result.Receipt) > 0 && !json.Valid(transition.Result.Receipt) {
			return fmt.Errorf("%w: completed receipt must be valid JSON", ErrInvalidExecutionTransition)
		}
		if err := validateResultArtifacts(transition.Result.Artifacts); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidExecutionTransition, err)
		}
	case OperationExecutionStarted, OperationExecutionUnknown, OperationExecutionRetryable, OperationExecutionRecoveryFailed:
		if hasOperationResult(transition.Result) {
			return fmt.Errorf("%w: transition to %q cannot contain a result", ErrInvalidExecutionTransition, transition.To)
		}
	}
	if transition.To != OperationExecutionCompleted && transition.Verification != nil {
		return fmt.Errorf("%w: only completed state can contain verification", ErrInvalidExecutionTransition)
	}
	if transition.Verification != nil {
		if !transition.Verification.Confirmed {
			return fmt.Errorf("%w: completed verification must be confirmed", ErrInvalidExecutionTransition)
		}
		if len(transition.Verification.Evidence) > 0 && !json.Valid(transition.Verification.Evidence) {
			return fmt.Errorf("%w: verification evidence must be valid JSON", ErrInvalidExecutionTransition)
		}
	}
	return nil
}

func hasOperationResult(result OperationResult) bool {
	return len(result.Output) > 0 || len(result.Receipt) > 0 || strings.TrimSpace(result.FinalResponse) != "" || len(result.Artifacts) > 0
}

func (transition OperationExecutionTransition) validateFields() error {
	if strings.TrimSpace(transition.ID) == "" || strings.TrimSpace(transition.ExecutionID) == "" || strings.TrimSpace(transition.AttemptID) == "" ||
		strings.TrimSpace(transition.RunID) == "" || strings.TrimSpace(transition.CallID) == "" ||
		strings.TrimSpace(transition.Actor) == "" || strings.TrimSpace(transition.Message) == "" {
		return fmt.Errorf("%w: transition id, execution id, attempt id, run id, call id, actor, and message are required", ErrInvalidExecutionTransition)
	}
	if len(transition.Evidence) > 0 && !json.Valid(transition.Evidence) {
		return fmt.Errorf("%w: transition evidence must be valid JSON", ErrInvalidExecutionTransition)
	}
	if transition.CreatedAt.IsZero() {
		return fmt.Errorf("%w: transition timestamp is required", ErrInvalidExecutionTransition)
	}
	return nil
}

// ExecutionStore owns durable write plans and write-operation state.
// ReservePlanBatch preserves the first batch at each index and rejects new
// batches after SealPlan. AcquireExecution and TransitionExecution atomically
// update the current record and append an immutable transition.
// ValidateExecutionAttempt rejects owners fenced by reconciliation or retry;
// write executors must still perform the same check atomically with their
// external side effect.
type ExecutionStore interface {
	ReservePlanBatch(ctx context.Context, batch OperationPlanBatch) (PlanBatchReservation, error)
	SealPlan(ctx context.Context, seal OperationPlanSeal) (PlanSealResult, error)
	AcquireExecution(ctx context.Context, request AcquireExecutionRequest) (AcquireExecutionResult, error)
	ValidateExecutionAttempt(ctx context.Context, executionID, attemptID string) error
	TransitionExecution(ctx context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error)
	GetExecution(ctx context.Context, executionID string) (OperationExecutionRecord, error)
	ListExecutionTransitions(ctx context.Context, executionID string) ([]OperationExecutionTransition, error)
}

type OperationReconciliationAction string

const (
	OperationReconciliationRetry    OperationReconciliationAction = "retry"
	OperationReconciliationComplete OperationReconciliationAction = "complete"
	OperationReconciliationFail     OperationReconciliationAction = "fail"
)

type ReconcileOperationRequest struct {
	ExecutionID       string
	ExpectedAttemptID string
	Action            OperationReconciliationAction
	Result            OperationResult
	Actor             string
	Message           string
	Evidence          json.RawMessage
}
