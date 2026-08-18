package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func (s *recordingStore) AcquireExecution(_ context.Context, request AcquireExecutionRequest) (AcquireExecutionResult, error) {
	if err := request.Validate(); err != nil {
		return AcquireExecutionResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.executions == nil {
		s.executions = make(map[string]OperationExecutionRecord)
	}
	if s.transitions == nil {
		s.transitions = make(map[string][]OperationExecutionTransition)
	}
	execution := request.Execution
	if existing, ok := s.executions[execution.ID]; ok {
		if existing.IdempotencyKey != execution.IdempotencyKey || existing.IdempotencyScope != execution.IdempotencyScope ||
			existing.SessionID != execution.SessionID || existing.Name != execution.Name || existing.ContractID != execution.ContractID ||
			existing.VerificationRequired != execution.VerificationRequired || !bytes.Equal(existing.Arguments, execution.Arguments) {
			return AcquireExecutionResult{}, ErrOperationPlanChanged
		}
		if existing.Status == OperationExecutionRetryable {
			if s.executionAttemptIdentityExistsLocked(execution.ID, execution.AttemptID) {
				return AcquireExecutionResult{}, fmt.Errorf("%w: attempt id %q is already assigned to execution %q", ErrIdentityConflict, execution.AttemptID, execution.ID)
			}
			if s.transitionIdentityExistsLocked(request.Transition.ID) {
				return AcquireExecutionResult{}, fmt.Errorf("%w: transition id %q is already assigned", ErrIdentityConflict, request.Transition.ID)
			}
			transition := cloneOperationTransitionForTest(request.Transition)
			transition.From = OperationExecutionRetryable
			transition.To = OperationExecutionStarted
			existing.Status = OperationExecutionStarted
			existing.RunID = execution.RunID
			existing.CallID = execution.CallID
			existing.AttemptID = execution.AttemptID
			existing.Error = ""
			existing.Result = OperationResult{}
			existing.Verification = nil
			existing.UpdatedAt = execution.UpdatedAt
			s.executions[execution.ID] = existing
			s.transitions[execution.ID] = append(s.transitions[execution.ID], transition)
			return AcquireExecutionResult{Execution: cloneOperationExecutionRecord(existing), Disposition: ExecutionAcquired}, nil
		}
		disposition := ExecutionBlocked
		if existing.Status == OperationExecutionExecuted || existing.Status == OperationExecutionCompleted {
			disposition = ExecutionReplay
		}
		return AcquireExecutionResult{Execution: cloneOperationExecutionRecord(existing), Disposition: disposition}, nil
	}
	if s.transitionIdentityExistsLocked(request.Transition.ID) {
		return AcquireExecutionResult{}, fmt.Errorf("%w: transition id %q is already assigned", ErrIdentityConflict, request.Transition.ID)
	}
	execution = cloneOperationExecutionRecord(execution)
	s.executions[execution.ID] = execution
	transition := cloneOperationTransitionForTest(request.Transition)
	transition.From = ""
	transition.To = OperationExecutionStarted
	s.transitions[execution.ID] = append(s.transitions[execution.ID], transition)
	return AcquireExecutionResult{Execution: cloneOperationExecutionRecord(execution), Disposition: ExecutionAcquired}, nil
}

func (s *recordingStore) GetExecution(_ context.Context, executionID string) (OperationExecutionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	execution, ok := s.executions[executionID]
	if !ok {
		return OperationExecutionRecord{}, ErrOperationExecutionNotFound
	}
	return cloneOperationExecutionRecord(execution), nil
}

func (s *recordingStore) ValidateExecutionAttempt(ctx context.Context, executionID, attemptID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	execution, ok := s.executions[executionID]
	if !ok {
		return ErrOperationExecutionNotFound
	}
	if execution.Status != OperationExecutionStarted || execution.AttemptID != attemptID {
		return ErrOperationAttemptLost
	}
	return nil
}

func (s *recordingStore) TransitionExecution(_ context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error) {
	if err := transition.Validate(); err != nil {
		return OperationExecutionRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	execution, ok := s.executions[transition.ExecutionID]
	if !ok {
		return OperationExecutionRecord{}, ErrOperationExecutionNotFound
	}
	if execution.AttemptID != transition.AttemptID || execution.Status != transition.From {
		return OperationExecutionRecord{}, ErrOperationAttemptLost
	}
	if execution.RunID != transition.RunID || execution.CallID != transition.CallID {
		return OperationExecutionRecord{}, ErrInvalidExecutionTransition
	}
	if execution.VerificationRequired != transition.VerificationRequired {
		return OperationExecutionRecord{}, ErrInvalidExecutionTransition
	}
	if transition.To == OperationExecutionStarted {
		return OperationExecutionRecord{}, ErrInvalidExecutionTransition
	}
	if s.transitionIdentityExistsLocked(transition.ID) {
		return OperationExecutionRecord{}, fmt.Errorf("%w: transition id %q is already assigned", ErrIdentityConflict, transition.ID)
	}
	execution.Status = transition.To
	execution.UpdatedAt = transition.CreatedAt
	switch transition.To {
	case OperationExecutionUnknown, OperationExecutionRetryable:
		execution.Error = transition.Message
		execution.Result = OperationResult{}
		execution.Verification = nil
	case OperationExecutionRecoveryFailed:
		execution.Error = transition.Message
		execution.Verification = nil
	case OperationExecutionExecuted, OperationExecutionCompleted:
		execution.Error = ""
		execution.Result = cloneOperationResult(transition.Result)
		execution.Verification = cloneVerificationPointerForTest(transition.Verification)
	}
	s.executions[execution.ID] = execution
	s.transitions[execution.ID] = append(s.transitions[execution.ID], cloneOperationTransitionForTest(transition))
	return cloneOperationExecutionRecord(execution), nil
}

func (s *recordingStore) transitionIdentityExistsLocked(id string) bool {
	for _, history := range s.transitions {
		for _, transition := range history {
			if transition.ID == id {
				return true
			}
		}
	}
	return false
}

func (s *recordingStore) executionAttemptIdentityExistsLocked(executionID, attemptID string) bool {
	for _, transition := range s.transitions[executionID] {
		if transition.AttemptID == attemptID {
			return true
		}
	}
	return false
}

func (s *recordingStore) ListExecutionTransitions(_ context.Context, executionID string) ([]OperationExecutionTransition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.executions[executionID]; !ok {
		return nil, ErrOperationExecutionNotFound
	}
	history := s.transitions[executionID]
	out := make([]OperationExecutionTransition, len(history))
	for i := range history {
		out[i] = cloneOperationTransitionForTest(history[i])
	}
	return out, nil
}

func (s *recordingStore) CreateRunV3(ctx context.Context, request CreateRunRequest, accept AcceptRunStart) error {
	return s.beginRunV3(ctx, ResumeRunRequest{
		Run: request.Run, LeaseID: request.LeaseID, LeaseTTL: request.LeaseTTL,
	}, false, accept, nil)
}

func (s *recordingStore) ResumeRunV3(ctx context.Context, request ResumeRunRequest, accept AcceptResumedRun) error {
	return s.beginRunV3(ctx, request, true, nil, accept)
}

func (s *recordingStore) beginRunV3(
	ctx context.Context,
	request ResumeRunRequest,
	resume bool,
	acceptCreate AcceptRunStart,
	acceptResume AcceptResumedRun,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if (!resume && acceptCreate == nil) || (resume && acceptResume == nil) {
		return errors.New("recordingStore: pre-commit acceptance is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireCanonicalIdentity(request.Run.ID, "recordingStore run id"); err != nil {
		return err
	}
	for _, item := range s.items {
		if item.ID == request.Run.ID {
			return fmt.Errorf("%w: run id %q is already assigned to an item", ErrIdentityConflict, request.Run.ID)
		}
	}
	if request.Run.Status != RunStatusRunning {
		return errors.New("recordingStore: run must be running")
	}
	handle := RunHandle{RunID: request.Run.ID, SessionID: request.Run.SessionID}
	runIndex := -1
	pendingApprovalDigest := ""
	var pendingApproval *PendingApprovalCommit
	for i := range s.runs {
		if s.runs[i].ID != request.Run.ID {
			continue
		}
		if !resume {
			return fmt.Errorf("%w: run id %q is already assigned", ErrIdentityConflict, request.Run.ID)
		}
		if s.runs[i].Status != RunStatusWaitingUser {
			return fmt.Errorf("%w: run id %q is already assigned", ErrIdentityConflict, request.Run.ID)
		}
		if s.runs[i].SkillSetID != request.Run.SkillSetID {
			return fmt.Errorf("%w: waiting run %s", ErrSkillSetMismatch, request.Run.ID)
		}
		if s.runs[i].OperationSetID != request.Run.OperationSetID {
			return fmt.Errorf("%w: waiting run %s operation set", ErrOperationPlanChanged, request.Run.ID)
		}
		runIndex = i
		pendingApprovalDigest = s.runs[i].PendingApprovalDigest
		break
	}
	if resume && runIndex < 0 {
		return ErrRunNotFound
	}
	if resume {
		if pendingApprovalDigest == "" {
			return fmt.Errorf("%w: waiting run %s has no durable approval authority", ErrOperationPlanChanged, request.Run.ID)
		}
		if err := validateApprovalAuthorityDigest(pendingApprovalDigest); err != nil {
			return fmt.Errorf("%w: waiting run %s has invalid durable approval authority: %v", ErrOperationPlanChanged, request.Run.ID, err)
		}
		pending, ok := s.pendingApprovals[request.Run.ID]
		if !ok || pending.Digest != pendingApprovalDigest {
			return fmt.Errorf("%w: waiting run %s has no matching pending approval", ErrOperationPlanChanged, request.Run.ID)
		}
		if pending.Request.Checkpoint == nil || request.InputDigest == "" ||
			request.InputDigest != pending.Request.Checkpoint.InputDigest {
			return fmt.Errorf("%w: waiting run %s input changed", ErrOperationPlanChanged, request.Run.ID)
		}
		if pending.Request.Operation.SessionID != request.Run.SessionID {
			return fmt.Errorf("%w: waiting run %s approval session changed", ErrOperationPlanChanged, request.Run.ID)
		}
		currentRevision := uint64(0)
		if existing, exists := s.sessions[request.Run.SessionID]; request.Run.SessionID != "" && exists {
			currentRevision = existing.Revision
		}
		if pending.Request.Checkpoint.ExpectedSessionRevision != currentRevision {
			return fmt.Errorf(
				"%w: waiting run %s expects session revision %d, current revision is %d",
				ErrOperationPlanChanged,
				request.Run.ID,
				pending.Request.Checkpoint.ExpectedSessionRevision,
				currentRevision,
			)
		}
		clonedPending, err := clonePendingApprovalCommitForTest(pending)
		if err != nil {
			return err
		}
		pendingApproval = &clonedPending
	}
	var session *SessionState
	sessionExists := false
	newBinding := false
	expiredActiveRunID := ""
	now := s.currentTime()
	if request.Run.SessionID != "" {
		if request.LeaseID == "" || request.LeaseTTL <= 0 {
			return errors.New("recordingStore: valid session lease is required")
		}
		existing, exists := s.sessions[request.Run.SessionID]
		sessionExists = exists
		if sessionExists && existing.SkillSetID != request.Run.SkillSetID {
			return fmt.Errorf("%w: session %s", ErrSkillSetMismatch, request.Run.SessionID)
		}
		if sessionExists && existing.OperationSetID != "" && existing.OperationSetID != request.Run.OperationSetID {
			return fmt.Errorf("%w: session %s operation set", ErrOperationPlanChanged, request.Run.SessionID)
		}
		if active, ok := s.leases[request.Run.SessionID]; ok {
			activeRunFound := false
			for i := range s.runs {
				if s.runs[i].ID != active.RunID {
					continue
				}
				activeRunFound = true
				if s.runs[i].SkillSetID != request.Run.SkillSetID {
					return fmt.Errorf("%w: active run %s", ErrSkillSetMismatch, active.RunID)
				}
				if s.runs[i].OperationSetID != request.Run.OperationSetID {
					return fmt.Errorf("%w: active run %s operation set", ErrOperationPlanChanged, active.RunID)
				}
				break
			}
			if !activeRunFound {
				return fmt.Errorf("%w: active run %s is missing", ErrSessionConflict, active.RunID)
			}
			if active.LeaseDeadline.After(now) {
				return fmt.Errorf("%w: run %s", ErrSessionBusy, active.RunID)
			}
			expiredActiveRunID = active.RunID
		}
		handle.LeaseID = request.LeaseID
		handle.LeaseGeneration = s.leaseGenerations[request.Run.SessionID] + 1
		handle.LeaseDeadline = now.Add(request.LeaseTTL)
		if sessionExists {
			handle.SessionRevision = existing.Revision
			cloned := existing
			cloned.Transcript = cloneModelInputItems(existing.Transcript)
			cloned.Checkpoint = cloneContextCheckpoint(existing.Checkpoint)
			cloned.SeenCallIDs = cloneStringsPreserveNil(existing.SeenCallIDs)
			session = &cloned
		} else if request.Run.SkillSetID != "" || request.Run.OperationSetID != "" {
			binding := SessionState{
				ID: request.Run.SessionID, SkillSetID: request.Run.SkillSetID, OperationSetID: request.Run.OperationSetID,
				CreatedAt: request.Run.CreatedAt, UpdatedAt: request.Run.UpdatedAt,
			}
			session = &binding
			newBinding = true
		}
	}
	start := RunStart{Handle: handle, Session: session}
	if resume {
		if err := acceptResume(ResumedRun{
			RunStart: start, PendingApprovalDigest: pendingApprovalDigest, PendingApproval: pendingApproval,
		}); err != nil {
			return err
		}
	} else if err := acceptCreate(start); err != nil {
		return err
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	commitNow := s.currentTime()
	if request.Run.SessionID != "" && !handle.LeaseDeadline.After(commitNow) {
		return fmt.Errorf("%w: proposed session lease expired before commit", ErrSessionConflict)
	}
	if request.Run.SessionID != "" {
		if expiredActiveRunID != "" {
			s.abandonExpiredRunLocked(expiredActiveRunID, now)
			delete(s.leases, request.Run.SessionID)
		}
		if s.leases == nil {
			s.leases = make(map[string]RunHandle)
		}
		if s.leaseGenerations == nil {
			s.leaseGenerations = make(map[string]uint64)
		}
		if newBinding {
			if s.sessions == nil {
				s.sessions = make(map[string]SessionState)
			}
			s.sessions[request.Run.SessionID] = *session
		}
		s.leaseGenerations[request.Run.SessionID] = handle.LeaseGeneration
		s.leases[request.Run.SessionID] = handle
	}
	if resume {
		request.Run.PendingApprovalDigest = pendingApprovalDigest
		s.runs[runIndex] = request.Run
	} else {
		s.runs = append(s.runs, request.Run)
	}
	return nil
}

func (s *recordingStore) RenewRunLease(ctx context.Context, request RenewRunLeaseRequest) (RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return RunHandle{}, err
	}
	if request.LeaseTTL <= 0 {
		return RunHandle{}, errors.New("recordingStore: session lease TTL must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.currentTime()
	active, ok := s.leases[request.Handle.SessionID]
	if !ok || !sameTestLeaseOwner(active, request.Handle) || !active.LeaseDeadline.After(now) {
		return RunHandle{}, ErrSessionLeaseLost
	}
	active.LeaseDeadline = now.Add(request.LeaseTTL)
	s.leases[active.SessionID] = active
	return active, nil
}

func (s *recordingStore) ValidateRunLease(ctx context.Context, handle RunHandle) (RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return RunHandle{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active, ok := s.leases[handle.SessionID]
	if !ok || !sameTestLeaseOwner(active, handle) || !active.LeaseDeadline.After(s.currentTime()) {
		return RunHandle{}, ErrSessionLeaseLost
	}
	return active, nil
}

func sameTestLeaseOwner(left, right RunHandle) bool {
	return left.RunID == right.RunID && left.SessionID == right.SessionID &&
		left.LeaseID == right.LeaseID && left.LeaseGeneration == right.LeaseGeneration &&
		left.SessionRevision == right.SessionRevision
}

func (s *recordingStore) abandonExpiredRunLocked(runID string, now time.Time) {
	for i := range s.runs {
		if s.runs[i].ID != runID || s.runs[i].Status != RunStatusRunning {
			continue
		}
		s.runs[i].Status = RunStatusFailed
		s.runs[i].Error = "agent: session lease expired before run completion"
		s.runs[i].UpdatedAt = now
		s.failed = append(s.failed, s.runs[i])
		return
	}
}

func (s *recordingStore) FinishRun(_ context.Context, request FinishRunRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := request.Run
	if run.Status != RunStatusWaitingUser && run.Status != RunStatusCompleted && run.Status != RunStatusInterrupted && run.Status != RunStatusCancelled && run.Status != RunStatusFailed {
		return fmt.Errorf("recordingStore: invalid terminal run status %q", run.Status)
	}
	if run.Status == RunStatusInterrupted || run.Status == RunStatusCancelled {
		request.Session = nil
	}
	storedRunIndex := -1
	for i := range s.runs {
		if s.runs[i].ID == run.ID {
			storedRunIndex = i
			break
		}
	}
	if storedRunIndex < 0 || s.runs[storedRunIndex].SessionID != run.SessionID {
		return ErrSessionConflict
	}
	if s.runs[storedRunIndex].SkillSetID != run.SkillSetID {
		return fmt.Errorf("%w: run %s", ErrSkillSetMismatch, run.ID)
	}
	if s.runs[storedRunIndex].OperationSetID != run.OperationSetID {
		return fmt.Errorf("%w: run %s operation set", ErrOperationPlanChanged, run.ID)
	}
	if run.SessionID != "" {
		existing, exists := s.sessions[run.SessionID]
		if exists && existing.SkillSetID != run.SkillSetID {
			return fmt.Errorf("%w: session %s", ErrSkillSetMismatch, run.SessionID)
		}
		if exists && existing.OperationSetID != "" && existing.OperationSetID != run.OperationSetID {
			return fmt.Errorf("%w: session %s operation set", ErrOperationPlanChanged, run.SessionID)
		}
		if (run.SkillSetID != "" || run.OperationSetID != "") && !exists {
			return fmt.Errorf("%w: session %s has no immutable runtime binding", ErrSessionConflict, run.SessionID)
		}
		if request.Session != nil && request.Session.SkillSetID != run.SkillSetID {
			return fmt.Errorf("%w: session %s snapshot", ErrSkillSetMismatch, run.SessionID)
		}
		if request.Session != nil && request.Session.OperationSetID != run.OperationSetID {
			return fmt.Errorf("%w: session %s snapshot operation set", ErrOperationPlanChanged, run.SessionID)
		}
	}
	var approvalAudit *ItemRecord
	var approvalCommit *PendingApprovalCommit
	if request.PendingApproval != nil {
		pending := request.PendingApproval
		if run.Status != RunStatusWaitingUser || !pending.Decision.Pending || pending.Decision.Approved ||
			pending.Request.Operation.RunID != run.ID || pending.Request.Operation.SessionID != run.SessionID ||
			pending.Audit.Type != ItemTypeApproval || pending.Audit.RunID != run.ID || pending.Audit.SessionID != run.SessionID {
			return fmt.Errorf("recordingStore: invalid pending approval for run %s", run.ID)
		}
		expected, err := json.Marshal(pending.Decision)
		if err != nil || !bytes.Equal(pending.Audit.Data, expected) {
			return fmt.Errorf("recordingStore: invalid pending approval audit for run %s", run.ID)
		}
		expectedDigest, err := pendingApprovalAuthorityDigest(*pending)
		if err != nil || pending.Digest == "" || pending.Digest != expectedDigest || run.PendingApprovalDigest != pending.Digest {
			return fmt.Errorf("recordingStore: invalid pending approval digest for run %s", run.ID)
		}
		committedRevision := uint64(0)
		if run.SessionID != "" {
			if request.Session != nil {
				committedRevision = request.Session.Revision
			} else if existing, exists := s.sessions[run.SessionID]; exists {
				committedRevision = existing.Revision
			}
		}
		if pending.Request.Checkpoint == nil || pending.Request.Checkpoint.ExpectedSessionRevision != committedRevision {
			return fmt.Errorf("recordingStore: pending approval session revision does not match waiting commit for run %s", run.ID)
		}
		commit, err := clonePendingApprovalCommitForTest(*pending)
		if err != nil {
			return err
		}
		approvalCommit = &commit
		cloned := pending.Audit
		cloned.Data = append(json.RawMessage(nil), pending.Audit.Data...)
		approvalAudit = &cloned
	}
	if approvalCommit == nil && run.Status == RunStatusWaitingUser && run.PendingApprovalDigest != "" {
		persisted, ok := s.pendingApprovals[run.ID]
		if !ok || persisted.Digest != run.PendingApprovalDigest {
			return fmt.Errorf("recordingStore: waiting run %s has no matching pending approval", run.ID)
		}
		currentRevision := uint64(0)
		if existing, exists := s.sessions[run.SessionID]; run.SessionID != "" && exists {
			currentRevision = existing.Revision
		}
		if persisted.Request.Checkpoint == nil || persisted.Request.Checkpoint.ExpectedSessionRevision != currentRevision {
			return fmt.Errorf("%w: waiting run %s approval session revision changed", ErrOperationPlanChanged, run.ID)
		}
	}
	if approvalAudit != nil {
		if err := s.requireAvailableItemIdentityLocked(approvalAudit.ID); err != nil {
			return err
		}
	}
	if run.SessionID != "" {
		active, ok := s.leases[run.SessionID]
		if !ok || !sameTestLeaseOwner(active, request.Handle) || !active.LeaseDeadline.After(s.currentTime()) {
			return ErrSessionConflict
		}
		existing, exists := s.sessions[run.SessionID]
		if (!exists && request.Handle.SessionRevision != 0) || (exists && existing.Revision != request.Handle.SessionRevision) {
			return ErrSessionConflict
		}
		if request.Session == nil {
			if run.Status != RunStatusFailed && run.Status != RunStatusWaitingUser &&
				run.Status != RunStatusInterrupted && run.Status != RunStatusCancelled {
				return ErrSessionConflict
			}
		} else if request.Session.ID != run.SessionID || request.Session.Revision != request.Handle.SessionRevision+1 {
			return ErrSessionConflict
		}
	}
	if request.Session != nil {
		if s.sessions == nil {
			s.sessions = make(map[string]SessionState)
		}
		cloned := *request.Session
		cloned.Transcript = cloneModelInputItems(request.Session.Transcript)
		cloned.Checkpoint = cloneContextCheckpoint(request.Session.Checkpoint)
		cloned.SeenCallIDs = cloneStringsPreserveNil(request.Session.SeenCallIDs)
		s.sessions[request.Session.ID] = cloned
	}
	if approvalAudit != nil {
		s.items = append(s.items, *approvalAudit)
	}
	if approvalCommit != nil {
		if s.pendingApprovals == nil {
			s.pendingApprovals = make(map[string]PendingApprovalCommit)
		}
		s.pendingApprovals[run.ID] = *approvalCommit
	} else if run.Status != RunStatusWaitingUser || run.PendingApprovalDigest == "" {
		delete(s.pendingApprovals, run.ID)
	}
	s.runs[storedRunIndex] = run
	delete(s.leases, run.SessionID)
	if run.Status == RunStatusCompleted {
		s.completed = append(s.completed, run)
	} else if run.Status == RunStatusFailed {
		s.failed = append(s.failed, run)
	}
	return nil
}

func cloneOperationPlanBatchForTest(batch OperationPlanBatch) OperationPlanBatch {
	batch.Steps = append([]OperationPlanStep(nil), batch.Steps...)
	for i := range batch.Steps {
		batch.Steps[i].Arguments = append(json.RawMessage(nil), batch.Steps[i].Arguments...)
	}
	return batch
}

func cloneOperationExecutionRecord(execution OperationExecutionRecord) OperationExecutionRecord {
	execution.Arguments = append(json.RawMessage(nil), execution.Arguments...)
	execution.Result = cloneOperationResult(execution.Result)
	execution.Verification = cloneVerificationPointerForTest(execution.Verification)
	return execution
}

func cloneOperationTransitionForTest(transition OperationExecutionTransition) OperationExecutionTransition {
	transition.Result = cloneOperationResult(transition.Result)
	transition.Verification = cloneVerificationPointerForTest(transition.Verification)
	transition.Evidence = append(json.RawMessage(nil), transition.Evidence...)
	return transition
}

func cloneVerificationPointerForTest(verification *VerificationResult) *VerificationResult {
	if verification == nil {
		return nil
	}
	out := *verification
	out.Evidence = append(json.RawMessage(nil), verification.Evidence...)
	return &out
}

func clonePendingApprovalCommitForTest(pending PendingApprovalCommit) (PendingApprovalCommit, error) {
	request := pending.Request
	input, err := cloneOperationInput(request.Operation.Input)
	if err != nil {
		return PendingApprovalCommit{}, err
	}
	request.Operation.Input = input
	request.Operation.Operation = cloneOperationSummaries([]OperationSummary{request.Operation.Operation})[0]
	request.Operation.Call.Input = append(json.RawMessage(nil), request.Operation.Call.Input...)
	arguments, err := json.Marshal(request.Operation.Arguments)
	if err != nil {
		return PendingApprovalCommit{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	request.Operation.Arguments = nil
	if err := decoder.Decode(&request.Operation.Arguments); err != nil {
		return PendingApprovalCommit{}, err
	}
	request.ModelOutput = cloneModelOutputItems(request.ModelOutput)
	request.Preview = append(json.RawMessage(nil), request.Preview...)
	request.Checkpoint = cloneApprovalCheckpoint(request.Checkpoint, true)
	audit := pending.Audit
	audit.Data = append(json.RawMessage(nil), pending.Audit.Data...)
	return PendingApprovalCommit{
		AuthorityVersion: pending.AuthorityVersion,
		Request:          request, Decision: pending.Decision, Audit: audit, Digest: pending.Digest,
	}, nil
}

func (s *recordingStore) pendingApprovalForTest(runID string) (*PendingApprovalCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.pendingApprovals[runID]
	if !ok {
		return nil, nil
	}
	cloned, err := clonePendingApprovalCommitForTest(pending)
	if err != nil {
		return nil, err
	}
	return &cloned, nil
}

func newTestRuntime(t *testing.T, model Model, ops *OperationRegistry, policy OperationPolicy, executor OperationExecutor, verifier ResultVerifier, approver Approver, store RunStore) *Runtime {
	return newTestRuntimeWithEventSink(t, model, ops, policy, executor, verifier, approver, store, nil)
}

var testRuntimeIdentitySequence atomic.Uint64

func newTestRuntimeWithEventSink(t *testing.T, model Model, ops *OperationRegistry, policy OperationPolicy, executor OperationExecutor, verifier ResultVerifier, approver Approver, store RunStore, eventSink EventSink) *Runtime {
	t.Helper()
	var executions ExecutionStore
	if value, ok := store.(ExecutionStore); ok {
		executions = value
	}
	var approvalResumer ApprovalResumer
	if value, ok := approver.(ApprovalResumer); ok {
		approvalResumer = value
	}
	config := RuntimeConfig{
		Model: model, Operations: ops, Policy: policy,
		Executor: executor, Verifier: verifier, Approver: approver, ApprovalResumer: approvalResumer,
		RunStore: store, Executions: executions,
		EventSink: eventSink,
		Now:       func() time.Time { return time.Unix(10, 0) },
	}
	if store != nil {
		config.NewID = func() string {
			return fmt.Sprintf("test-runtime-id-%d", testRuntimeIdentitySequence.Add(1))
		}
	}
	rt, err := NewRuntime(config)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt
}
