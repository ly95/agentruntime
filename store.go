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
	ID        string
	SessionID string
	// ModelBindingID is the versioned digest of the immutable BoundModel identity.
	// Durable records store this ID, never endpoint or credential-principal text.
	ModelBindingID string `json:"ModelBindingID,omitempty"`
	SkillSetID     string `json:"SkillSetID,omitempty"`
	OperationSetID string `json:"OperationSetID,omitempty"`
	// PendingApprovalDigest binds a waiting run to the exact immutable approval
	// checkpoint atomically committed with it.
	PendingApprovalDigest string `json:"PendingApprovalDigest,omitempty"`
	Status                RunStatus
	Input                 Input
	Result                string
	Artifacts             []ResultArtifact
	ErrorCode             string
	Error                 string
	FailureAuditStatus    FailureAuditStatus
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// FailureAuditStatus records whether a terminal error audit item was committed
// atomically with the failed/interrupted/cancelled RunRecord.
type FailureAuditStatus string

const (
	FailureAuditCommitted FailureAuditStatus = "committed"
	FailureAuditMissing   FailureAuditStatus = "audit_missing"
)

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
	ID string
	// ModelBindingID permanently binds this session to one versioned model adapter
	// identity. An empty legacy value is not inferred or upgraded by RunStore V4.
	ModelBindingID string `json:"ModelBindingID,omitempty"`
	SkillSetID     string `json:"SkillSetID,omitempty"`
	OperationSetID string `json:"OperationSetID,omitempty"`
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

// RunHandle proves ownership of the session lease acquired by CreateRunV4 or
// ResumeRunV4.
// The store, not Runtime, owns lease recovery for abandoned runs.
type RunHandle struct {
	RunID           string
	SessionID       string
	LeaseID         string
	LeaseGeneration uint64
	LeaseDeadline   time.Time
	SessionRevision uint64
}

type CreateRunRequest struct {
	Run      RunRecord
	LeaseID  string
	LeaseTTL time.Duration
}

// Validate checks the context-free CreateRunV4 request contract. Store
// implementations should call it before opening a transaction; Runtime calls
// it before invoking the store as an additional fail-explicit boundary.
func (request CreateRunRequest) Validate() error {
	if err := validateRunStartRequest(request.Run, request.LeaseID, request.LeaseTTL); err != nil {
		return fmt.Errorf("%w: create run: %v", ErrRunStoreProtocol, err)
	}
	return nil
}

type RunStart struct {
	Handle  RunHandle
	Session *SessionState
}

type ResumeRunRequest struct {
	Run         RunRecord
	LeaseID     string
	LeaseTTL    time.Duration
	InputDigest string
}

// Validate checks the context-free ResumeRunV4 request contract.
func (request ResumeRunRequest) Validate() error {
	if err := validateRunStartRequest(request.Run, request.LeaseID, request.LeaseTTL); err != nil {
		return fmt.Errorf("%w: resume run: %v", ErrRunStoreProtocol, err)
	}
	if strings.TrimSpace(request.InputDigest) == "" {
		return fmt.Errorf("%w: resume run input digest is required", ErrRunStoreProtocol)
	}
	return nil
}

func validateRunStartRequest(run RunRecord, leaseID string, leaseTTL time.Duration) error {
	if err := validateUTF8Boundary("run start request", struct {
		Run     RunRecord
		LeaseID string
	}{Run: run, LeaseID: leaseID}); err != nil {
		return err
	}
	if err := requireCanonicalIdentity(run.ID, "run id"); err != nil {
		return err
	}
	if err := requireCanonicalIdentity(run.ModelBindingID, "model binding id"); err != nil {
		return err
	}
	if run.SessionID != "" {
		if err := requireCanonicalIdentity(run.SessionID, "session id"); err != nil {
			return err
		}
		if err := requireCanonicalIdentity(leaseID, "lease id"); err != nil {
			return err
		}
	} else if leaseID != "" {
		return fmt.Errorf("stateless run cannot carry a lease id")
	}
	if leaseTTL <= 0 {
		return fmt.Errorf("lease TTL must be positive")
	}
	if run.Status != RunStatusRunning {
		return fmt.Errorf("run status must be %q", RunStatusRunning)
	}
	if run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() || run.UpdatedAt.Before(run.CreatedAt) {
		return fmt.Errorf("run timestamps are invalid")
	}
	if run.Input.RunID != "" && run.Input.RunID != run.ID {
		return fmt.Errorf("input run id does not match run")
	}
	if run.Input.SessionID != run.SessionID {
		return fmt.Errorf("input session id does not match run")
	}
	if run.Input.ImageAttachmentResolver != nil || run.Input.TrustedContext != "" {
		return fmt.Errorf("persistent run input contains transient trusted dependencies")
	}
	if run.PendingApprovalDigest != "" || run.Result != "" || len(run.Artifacts) != 0 || run.ErrorCode != "" || run.Error != "" {
		return fmt.Errorf("running run contains terminal or approval state")
	}
	return nil
}

type ResumedRun struct {
	RunStart
	PendingApprovalDigest string
	// PendingApproval is the complete immutable authority atomically associated
	// with PendingApprovalDigest. Runtime recomputes and validates this payload
	// while the ResumeRunV4 transaction is still abortable.
	PendingApproval *PendingApprovalCommit
}

// AcceptRunStart is invoked by RunStore inside its transaction after it has
// computed the exact post-commit handle/session view and before it mutates any
// run, approval, session, lease, or identity state. A non-nil error aborts the
// transaction without mutation. The callback must be invoked exactly once on a
// successful CreateRunV4 or ResumeRunV4 call.
type AcceptRunStart func(RunStart) error

// AcceptResumedRun has the same pre-commit semantics for a waiting-run resume.
type AcceptResumedRun func(ResumedRun) error

type FinishRunRequest struct {
	Handle          RunHandle
	Run             RunRecord
	Session         *SessionState
	PendingApproval *PendingApprovalCommit
	// FailureItem is appended atomically with a failed, interrupted, or
	// cancelled run. Stores must not terminalize the run if this append fails.
	FailureItem *ItemRecord
}

// Validate checks the context-free FinishRun request contract. Durable stores
// remain responsible for comparing the request with current lease, session,
// run, and approval authority inside one transaction.
func (request FinishRunRequest) Validate() error {
	if err := validateUTF8Boundary("finish run request", request); err != nil {
		return fmt.Errorf("%w: finish run: %v", ErrRunStoreProtocol, err)
	}
	run := request.Run
	if err := requireCanonicalIdentity(run.ID, "run id"); err != nil {
		return fmt.Errorf("%w: finish run: %v", ErrRunStoreProtocol, err)
	}
	if err := requireCanonicalIdentity(run.ModelBindingID, "model binding id"); err != nil {
		return fmt.Errorf("%w: finish run: %v", ErrRunStoreProtocol, err)
	}
	switch run.Status {
	case RunStatusWaitingUser, RunStatusCompleted, RunStatusFailed, RunStatusInterrupted, RunStatusCancelled:
	default:
		return fmt.Errorf("%w: finish run has invalid status %q", ErrRunStoreProtocol, run.Status)
	}
	if request.Handle.RunID != run.ID || request.Handle.SessionID != run.SessionID {
		return fmt.Errorf("%w: finish handle does not match run", ErrRunStoreProtocol)
	}
	if run.SessionID == "" {
		if request.Handle.LeaseID != "" || request.Handle.LeaseGeneration != 0 ||
			!request.Handle.LeaseDeadline.IsZero() || request.Handle.SessionRevision != 0 || request.Session != nil {
			return fmt.Errorf("%w: stateless finish carries session authority", ErrRunStoreProtocol)
		}
	} else {
		if err := requireCanonicalIdentity(request.Handle.LeaseID, "finish lease id"); err != nil {
			return fmt.Errorf("%w: finish run: %v", ErrRunStoreProtocol, err)
		}
		if request.Handle.LeaseGeneration == 0 || request.Handle.LeaseDeadline.IsZero() {
			return fmt.Errorf("%w: finish handle has invalid lease authority", ErrRunStoreProtocol)
		}
		if request.Session != nil {
			session := request.Session
			if session.ID != run.SessionID || session.LastRunID != run.ID {
				return fmt.Errorf("%w: finish session does not identify the run", ErrRunStoreProtocol)
			}
			if request.Handle.SessionRevision == ^uint64(0) || session.Revision != request.Handle.SessionRevision+1 {
				return fmt.Errorf("%w: finish session revision does not advance the handle", ErrRunStoreProtocol)
			}
			if session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() || session.UpdatedAt.Before(session.CreatedAt) {
				return fmt.Errorf("%w: finish session timestamps are invalid", ErrRunStoreProtocol)
			}
		}
	}
	if run.UpdatedAt.IsZero() || run.UpdatedAt.Before(run.CreatedAt) {
		return fmt.Errorf("%w: finish run timestamps are invalid", ErrRunStoreProtocol)
	}
	if run.Input.ImageAttachmentResolver != nil || run.Input.TrustedContext != "" {
		return fmt.Errorf("%w: finished run input contains transient trusted dependencies", ErrRunStoreProtocol)
	}
	if run.Status == RunStatusWaitingUser {
		if strings.TrimSpace(run.PendingApprovalDigest) == "" {
			return fmt.Errorf("%w: waiting run has no approval digest", ErrRunStoreProtocol)
		}
	} else if run.PendingApprovalDigest != "" || request.PendingApproval != nil {
		return fmt.Errorf("%w: terminal run carries pending approval authority", ErrRunStoreProtocol)
	}
	switch run.Status {
	case RunStatusFailed, RunStatusInterrupted, RunStatusCancelled:
		if strings.TrimSpace(run.ErrorCode) == "" || strings.TrimSpace(run.Error) == "" || run.Result != "" {
			return fmt.Errorf("%w: terminal error run has an invalid status payload", ErrRunStoreProtocol)
		}
		if request.FailureItem != nil {
			if run.FailureAuditStatus != FailureAuditCommitted {
				return fmt.Errorf("%w: terminal error item is not marked committed", ErrRunStoreProtocol)
			}
			if request.FailureItem.Type != ItemTypeError || request.FailureItem.RunID != run.ID || request.FailureItem.SessionID != run.SessionID {
				return fmt.Errorf("%w: terminal error item does not match run", ErrRunStoreProtocol)
			}
			if err := validateStoredItem(*request.FailureItem); err != nil {
				return fmt.Errorf("%w: terminal error item: %v", ErrRunStoreProtocol, err)
			}
		} else if run.FailureAuditStatus != FailureAuditMissing {
			return fmt.Errorf("%w: terminal error run must record audit_missing", ErrRunStoreProtocol)
		}
	case RunStatusCompleted, RunStatusWaitingUser:
		if run.ErrorCode != "" || run.Error != "" || (run.Status == RunStatusWaitingUser && run.Result != "") {
			return fmt.Errorf("%w: successful or waiting run has an invalid status payload", ErrRunStoreProtocol)
		}
		if request.FailureItem != nil || run.FailureAuditStatus != "" {
			return fmt.Errorf("%w: successful or waiting run carries failure audit state", ErrRunStoreProtocol)
		}
	}
	return nil
}

// PendingApprovalCommit carries the approval request and its audit item into
// the RunStore-owned FinishRun transaction. Stores must create or validate the
// pending approval and append Audit atomically with the waiting_user Run
// transition; an error must leave none of those mutations visible.
type PendingApprovalCommit struct {
	// AuthorityVersion identifies the complete authority schema and must equal
	// PendingApprovalAuthorityVersion. Version zero, version 1, and every other
	// non-current schema are intentionally not resumable: omitted authority fields
	// cannot be proven after restart.
	AuthorityVersion uint32
	Request          ApprovalRequest
	Decision         ApprovalDecision
	Audit            ItemRecord
	Digest           string
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
// CreateRunV4 atomically creates a previously absent running run and acquires
// the session lease. It rejects every existing Run.ID, including waiting runs.
// ResumeRunV4 atomically resumes only the exact waiting Run.ID and returns
// ErrRunNotFound without mutation when no RunRecord owns that ID. Runtime uses
// that read-only absence result to create an explicitly host-selected ID; a
// generated ID is sent only to CreateRunV4.
// Every create and resume carries a canonical non-empty Run.ModelBindingID. For
// every new stateful session, CreateRunV4 must also create and return a
// binding-only SessionState at revision zero. The model binding is immutable:
// before changing a stored session, resuming a waiting run, or fencing an active
// run, stores must compare it with every applicable RunRecord, SessionState, and
// waiting approval checkpoint. Every applicable durable pending approval must
// use PendingApprovalAuthorityVersion and have a non-nil checkpoint; a version
// mismatch or absence returns ErrOperationPlanChanged. Any model binding
// difference, including an empty legacy binding, returns
// ErrModelBindingMismatch without mutation or callback invocation; stores must
// never upgrade an empty model binding implicitly.
// Skill-set and operation-set bindings are also independent of transcript
// readiness and must survive nil-Session terminal writes and abandoned-lease
// recovery. A SkillSet mismatch returns ErrSkillSetMismatch; an operation-set
// mismatch returns ErrOperationPlanChanged. When resuming a waiting approval,
// ResumeRunV4 must also compare InputDigest with the atomically stored approval
// checkpoint before changing the run or acquiring a lease. A waiting run with
// no non-empty PendingApprovalDigest or no exactly matching durable approval is
// invalid and must be rejected before any mutation. No binding or input mismatch
// may mutate state or acquire or fence a lease. A legacy record can have an
// empty operation-set binding; Runtime upgrades it only on a successful
// write-free completion, never while polling, pausing for approval, or rolling
// back a failure.
// Stores must permit a new run to fence an expired lease, assign a monotonically
// increasing lease generation, and return the store-owned deadline in the handle.
// RenewRunLease extends only a live matching generation. ValidateRunLease lets
// Runtime reject a stale owner immediately after run-start return and before a
// write side effect.
// FinishRun atomically terminalizes or pauses a currently running run, commits
// the next session snapshot when supplied, and releases the lease. Duplicate or
// stale finish requests must return ErrRunStoreProtocol without mutation. Lease
// renewal remains active while FinishRun executes, so stores must validate the
// live owner fields and deadline; the request handle's observed LeaseDeadline
// may lag a renewal.
// A stateless finish must not carry lease or session authority. Every supplied
// or durable PendingApprovalCommit must use PendingApprovalAuthorityVersion and
// contain a checkpoint; a version mismatch or missing checkpoint returns
// ErrOperationPlanChanged. A supplied Session and every supplied or durable
// pending approval checkpoint must retain the finishing Run.ModelBindingID
// exactly. A supplied Session must also identify
// the finishing run through ID and LastRunID, advance the handle revision exactly
// once, retain a non-zero existing CreatedAt, and never move UpdatedAt backward.
// Waiting and completed runs carry no error payload;
// failed, interrupted, and cancelled runs carry a non-empty error code and
// message and no successful Result.
// Run.CreatedAt and persistent Run.Input are immutable after CreateRunV4.
// ResumeRunV4 and FinishRun must retain them rather than replacing them with a
// caller's current-run values, and neither may move Run.UpdatedAt backward.
// When PendingApproval is supplied, FinishRun must also atomically persist that
// complete approval record, its Digest, and its audit item. The approval
// checkpoint's ExpectedSessionRevision must equal the session revision committed
// by that same transaction, or the unchanged current revision when Session is
// nil. Before mutating a waiting run, acquiring its lease, or fencing another
// owner, a later ResumeRunV4 must compare the current session revision with that
// expected revision and fail on mismatch. It then returns the approval digest
// and a complete cloned PendingApprovalCommit before Runtime accepts
// ApprovalResumer output. ResumeRunV4 must reject any durable authority whose
// AuthorityVersion differs from PendingApprovalAuthorityVersion before invoking
// the callback. PendingApprovalAuthorityVersion hashes the complete persistent
// request, decision, audit, normalized arguments, operation summary, input
// identity, checkpoint, and replay. Version zero, version 1, and every other
// non-current authority are not resumable because omitted fields cannot be proved.
// Runtime recomputes the complete authority and binds it to the proposed session
// inside the abortable callback; a digest-only acknowledgement is invalid. Pure pending polls retain the
// same authority and must not write a new Session snapshot or advance its
// revision. When immutable runtime configuration is already bound, Runtime
// supplies the last stable Session snapshot for the initial pause so that the
// binding survives resume. For a legacy empty operation-set binding, Runtime
// passes a nil Session while polling or pausing for approval; the store must
// preserve the exact existing snapshot. A failed run whose session could not be
// validated may do the same. A non-nil Session must retain the binding
// established by CreateRunV4 or ResumeRunV4.
// Run.ID and every ItemRecord.ID share one durable identity namespace. CreateRunV4,
// AppendItem, and a FinishRun carrying PendingApproval must atomically reject an
// identity already assigned to another run or item with ErrIdentityConflict and
// leave no mutation visible. CreateRunV4 and ResumeRunV4 are distinct so an
// adapter cannot lose create-versus-resume intent by rewriting a request bit.
// Both methods must invoke their acceptance callback with the exact proposed
// result while the transaction is still abortable; callback rejection leaves
// the prior run, approval, session, lease, and identity state unchanged. The
// callback is invoked exactly once, synchronously, and its first result is
// final; stores must not retry it or invoke it concurrently. Stores must also
// check ctx and lease expiry again after callback acceptance and immediately
// before commit. The proposed state contains the exact requested LeaseID, a
// store-owned deadline live at callback completion, no session/lease authority
// for stateless runs, and complete approval authority for resume. The V4 method
// names intentionally fence older implementations until they adopt model
// binding authority in addition to both split authority paths and the pre-commit
// acceptance protocol. An error from either
// method must leave no mutation.
type RunStore interface {
	CreateRunV4(ctx context.Context, request CreateRunRequest, accept AcceptRunStart) error
	ResumeRunV4(ctx context.Context, request ResumeRunRequest, accept AcceptResumedRun) error
	RenewRunLease(ctx context.Context, request RenewRunLeaseRequest) (RunHandle, error)
	ValidateRunLease(ctx context.Context, handle RunHandle) (RunHandle, error)
	AppendItem(ctx context.Context, item ItemRecord) error
	FinishRun(ctx context.Context, request FinishRunRequest) error
}

type OperationPlanStep struct {
	ExecutionID string
	Name        string
	ContractID  string
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

// Validate checks one immutable write-plan batch before reservation.
func (batch OperationPlanBatch) Validate() error {
	if err := validateUTF8Boundary("operation plan batch", batch); err != nil {
		return fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
	}
	if err := requireCanonicalIdentity(batch.RequestID, "plan request id"); err != nil {
		return fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
	}
	if strings.TrimSpace(batch.IdempotencyKey) == "" {
		return fmt.Errorf("%w: plan idempotency key is required", ErrOperationPlanChanged)
	}
	if batch.SessionID == "" && strings.TrimSpace(batch.IdempotencyScope) == "" {
		return fmt.Errorf("%w: stateless plan idempotency scope is required", ErrOperationPlanChanged)
	}
	if batch.CreatedAt.IsZero() || len(batch.Steps) == 0 {
		return fmt.Errorf("%w: plan timestamp and at least one step are required", ErrOperationPlanChanged)
	}
	seen := make(map[string]struct{}, len(batch.Steps))
	for index, step := range batch.Steps {
		if err := requireCanonicalIdentity(step.ExecutionID, fmt.Sprintf("plan step %d execution id", index)); err != nil {
			return fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
		}
		if strings.TrimSpace(step.Name) == "" || strings.TrimSpace(step.ContractID) == "" || len(step.Arguments) == 0 {
			return fmt.Errorf("%w: plan step %d name, contract id, and arguments are required", ErrOperationPlanChanged, index)
		}
		if _, err := decodeExactJSON(step.Arguments); err != nil {
			return fmt.Errorf("%w: plan step %d arguments are ambiguous or invalid: %v", ErrOperationPlanChanged, index, err)
		}
		if _, exists := seen[step.ExecutionID]; exists {
			return fmt.Errorf("%w: plan repeats execution id %q", ErrOperationPlanChanged, step.ExecutionID)
		}
		seen[step.ExecutionID] = struct{}{}
	}
	return nil
}

type OperationPlanSeal struct {
	RequestID        string
	SessionID        string
	IdempotencyKey   string
	IdempotencyScope string
	BatchCount       uint64
	SealedAt         time.Time
}

// Validate checks one immutable write-plan seal.
func (seal OperationPlanSeal) Validate() error {
	if err := validateUTF8Boundary("operation plan seal", seal); err != nil {
		return fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
	}
	if err := requireCanonicalIdentity(seal.RequestID, "plan request id"); err != nil {
		return fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
	}
	if strings.TrimSpace(seal.IdempotencyKey) == "" {
		return fmt.Errorf("%w: plan idempotency key is required", ErrOperationPlanChanged)
	}
	if seal.SessionID == "" && strings.TrimSpace(seal.IdempotencyScope) == "" {
		return fmt.Errorf("%w: stateless plan idempotency scope is required", ErrOperationPlanChanged)
	}
	if seal.SealedAt.IsZero() {
		return fmt.Errorf("%w: plan seal timestamp is required", ErrOperationPlanChanged)
	}
	return nil
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
	ID                   string
	IdempotencyKey       string
	IdempotencyScope     string
	RunID                string
	SessionID            string
	CallID               string
	AttemptID            string
	Name                 string
	ContractID           string
	VerificationRequired bool
	Arguments            json.RawMessage
	Status               OperationExecutionStatus
	Result               OperationResult
	Verification         *VerificationResult
	Error                string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type ExecutionAcquireDisposition string

const (
	ExecutionAcquired ExecutionAcquireDisposition = "acquired"
	ExecutionReplay   ExecutionAcquireDisposition = "replay"
	ExecutionBlocked  ExecutionAcquireDisposition = "blocked"
)

type OperationExecutionTransition struct {
	ID                   string
	ExecutionID          string
	AttemptID            string
	RunID                string
	CallID               string
	Actor                string
	Message              string
	VerificationRequired bool
	From                 OperationExecutionStatus
	To                   OperationExecutionStatus
	Result               OperationResult
	Verification         *VerificationResult
	Evidence             json.RawMessage
	CreatedAt            time.Time
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
	if err := validateUTF8Boundary("execution acquisition request", request); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidExecutionTransition, err)
	}
	execution := request.Execution
	transition := request.Transition
	if strings.TrimSpace(execution.ID) == "" || strings.TrimSpace(execution.RunID) == "" ||
		strings.TrimSpace(execution.CallID) == "" || strings.TrimSpace(execution.AttemptID) == "" ||
		strings.TrimSpace(execution.Name) == "" {
		return fmt.Errorf("%w: execution id, run id, call id, attempt id, and name are required", ErrInvalidExecutionTransition)
	}
	if strings.TrimSpace(execution.ContractID) == "" {
		return fmt.Errorf("%w: execution contract id is required", ErrInvalidExecutionTransition)
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
	if execution.CreatedAt.After(transition.CreatedAt) || !execution.UpdatedAt.Equal(transition.CreatedAt) {
		return fmt.Errorf("%w: acquisition update timestamp must equal the transition timestamp and creation cannot follow it", ErrInvalidExecutionTransition)
	}
	if len(execution.Arguments) == 0 {
		return fmt.Errorf("%w: execution arguments must be valid JSON", ErrInvalidExecutionTransition)
	}
	if _, err := decodeExactJSON(execution.Arguments); err != nil {
		return fmt.Errorf("%w: execution arguments must be unambiguous valid JSON: %v", ErrInvalidExecutionTransition, err)
	}
	if hasOperationResult(execution.Result) || execution.Verification != nil || strings.TrimSpace(execution.Error) != "" {
		return fmt.Errorf("%w: started execution cannot contain a result, verification, or error", ErrInvalidExecutionTransition)
	}
	if transition.ExecutionID != execution.ID || transition.AttemptID != execution.AttemptID || transition.RunID != execution.RunID || transition.CallID != execution.CallID {
		return fmt.Errorf("%w: acquisition transition does not match execution", ErrInvalidExecutionTransition)
	}
	if transition.VerificationRequired != execution.VerificationRequired {
		return fmt.Errorf("%w: acquisition verification requirement does not match execution", ErrInvalidExecutionTransition)
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
			transition.To == OperationExecutionRetryable || transition.To == OperationExecutionCompleted
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
	if transition.From == OperationExecutionStarted && transition.To == OperationExecutionCompleted && len(transition.Evidence) == 0 {
		return fmt.Errorf("%w: started completion requires durable reconciliation evidence", ErrInvalidExecutionTransition)
	}
	if transition.From == OperationExecutionUnknown &&
		(transition.To == OperationExecutionCompleted || transition.To == OperationExecutionRetryable) && len(transition.Evidence) == 0 {
		return fmt.Errorf("%w: resolving an unknown outcome requires durable reconciliation evidence", ErrInvalidExecutionTransition)
	}
	switch transition.To {
	case OperationExecutionExecuted, OperationExecutionCompleted:
		if len(transition.Result.Output) == 0 {
			return fmt.Errorf("%w: transition result output must be valid JSON", ErrInvalidExecutionTransition)
		}
		if _, err := decodeExactJSON(transition.Result.Output); err != nil {
			return fmt.Errorf("%w: transition result output must be valid JSON and unambiguous: %v", ErrInvalidExecutionTransition, err)
		}
		if len(transition.Result.Receipt) > 0 {
			if _, err := decodeExactJSON(transition.Result.Receipt); err != nil {
				return fmt.Errorf("%w: completed receipt must be valid JSON and unambiguous: %v", ErrInvalidExecutionTransition, err)
			}
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
	if transition.To == OperationExecutionCompleted && transition.VerificationRequired && transition.Verification == nil {
		return fmt.Errorf("%w: confirmation-required completion must contain verification", ErrInvalidExecutionTransition)
	}
	if transition.To == OperationExecutionCompleted && !transition.VerificationRequired && transition.Verification != nil {
		return fmt.Errorf("%w: direct completion cannot contain verification", ErrInvalidExecutionTransition)
	}
	if transition.Verification != nil {
		if _, err := normalizePositiveVerificationResult(*transition.Verification); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidExecutionTransition, err)
		}
	}
	return nil
}

func hasOperationResult(result OperationResult) bool {
	return len(result.Output) > 0 || len(result.Receipt) > 0 || strings.TrimSpace(result.FinalResponse) != "" || len(result.Artifacts) > 0 || result.Continue
}

func (transition OperationExecutionTransition) validateFields() error {
	if err := validateUTF8Boundary("execution transition", transition); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidExecutionTransition, err)
	}
	if strings.TrimSpace(transition.ID) == "" || strings.TrimSpace(transition.ExecutionID) == "" || strings.TrimSpace(transition.AttemptID) == "" ||
		strings.TrimSpace(transition.RunID) == "" || strings.TrimSpace(transition.CallID) == "" ||
		strings.TrimSpace(transition.Actor) == "" || strings.TrimSpace(transition.Message) == "" {
		return fmt.Errorf("%w: transition id, execution id, attempt id, run id, call id, actor, and message are required", ErrInvalidExecutionTransition)
	}
	if len(transition.Evidence) > 0 {
		if err := validateNonNullExactJSON(transition.Evidence); err != nil {
			return fmt.Errorf("%w: transition evidence must be non-null unambiguous valid JSON: %v", ErrInvalidExecutionTransition, err)
		}
	}
	if transition.CreatedAt.IsZero() {
		return fmt.Errorf("%w: transition timestamp is required", ErrInvalidExecutionTransition)
	}
	return nil
}

// ExecutionStore owns durable write plans and write-operation state. Stores
// must compare each plan step and execution ContractID and the execution's
// VerificationRequired bit on every reservation, acquisition, and transition.
// Each planned ExecutionID belongs to exactly one batch across the store;
// ReservePlanBatch rejects a second assignment with ErrIdentityConflict before
// it can make the first plan unexecutable. New batch timestamps never precede
// an earlier batch, and an initial seal timestamp never precedes its batches.
// Reservation and seal retries compare semantic identity, retain the first
// stored observation timestamps, and return them with Created=false.
// ReservePlanBatch preserves the first batch at each index and rejects new
// batches after SealPlan. AcquireExecution and TransitionExecution atomically
// update the current record and append an immutable transition. Every persisted
// OperationExecutionTransition.ID is unique across all execution histories;
// every acquired AttemptID is unique within its execution history so an old
// owner token cannot become current again. Reuse must return ErrIdentityConflict
// without mutation. Every returned execution record must preserve its immutable
// identity and satisfy the status payload contract: started has no
// result/error/verification; executed has a
// result only; completed has a result and exactly the verification required by
// its contract; unknown and retryable have only the transition error; and
// recovery_failed preserves any prior executed result, has the transition
// error, and has no verification. A newly acquired record must return UpdatedAt
// exactly equal to the acquisition event time (with CreatedAt no later than that
// event so retries can preserve the original creation time), and every
// TransitionExecution acknowledgement must return UpdatedAt exactly equal to
// the requested transition time. Acquisition and transition timestamps must
// never move UpdatedAt backward. Started may transition directly to completed
// only for an evidence-bearing reconciliation of the exact fenced attempt after
// the executor may have committed but post-effect journaling could not be proved.
// Unknown may transition to retryable or completed only with durable evidence
// proving respectively that the side effect was not applied or was committed.
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
	// OperationReconciliationAbandon releases a started attempt only after a
	// trusted reconciler proves from supplied evidence that its executor never began.
	OperationReconciliationAbandon OperationReconciliationAction = "abandon"
)

type ReconcileOperationRequest struct {
	ExecutionID       string
	ExpectedAttemptID string
	Action            OperationReconciliationAction
	Result            OperationResult
	Verification      *VerificationResult
	Actor             string
	Message           string
	Evidence          json.RawMessage
}
