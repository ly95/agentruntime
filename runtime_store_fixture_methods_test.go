package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			existing.SessionID != execution.SessionID || existing.Name != execution.Name || !bytes.Equal(existing.Arguments, execution.Arguments) {
			return AcquireExecutionResult{}, ErrOperationPlanChanged
		}
		if existing.Status == OperationExecutionRetryable {
			if existing.AttemptID == execution.AttemptID {
				return AcquireExecutionResult{}, ErrInvalidExecutionTransition
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
	if transition.To == OperationExecutionStarted {
		return OperationExecutionRecord{}, ErrInvalidExecutionTransition
	}
	execution.Status = transition.To
	execution.UpdatedAt = transition.CreatedAt
	switch transition.To {
	case OperationExecutionUnknown, OperationExecutionRetryable:
		execution.Error = transition.Message
		execution.Result = OperationResult{}
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

func (s *recordingStore) BeginRun(ctx context.Context, request BeginRunRequest) (BeginRunResult, error) {
	if err := ctx.Err(); err != nil {
		return BeginRunResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Run.Status != RunStatusRunning {
		return BeginRunResult{}, errors.New("recordingStore: run must be running")
	}
	handle := RunHandle{RunID: request.Run.ID, SessionID: request.Run.SessionID}
	runIndex := -1
	for i := range s.runs {
		if s.runs[i].ID == request.Run.ID {
			if s.runs[i].Status != RunStatusWaitingUser {
				return BeginRunResult{}, fmt.Errorf("recordingStore: run %s cannot resume from %q", request.Run.ID, s.runs[i].Status)
			}
			if s.runs[i].SkillSetID != request.Run.SkillSetID {
				return BeginRunResult{}, fmt.Errorf("%w: waiting run %s", ErrSkillSetMismatch, request.Run.ID)
			}
			runIndex = i
			break
		}
	}
	var session *SessionState
	if request.Run.SessionID != "" {
		if request.LeaseID == "" || request.LeaseTTL <= 0 {
			return BeginRunResult{}, errors.New("recordingStore: valid session lease is required")
		}
		existing, sessionExists := s.sessions[request.Run.SessionID]
		if sessionExists && existing.SkillSetID != request.Run.SkillSetID {
			return BeginRunResult{}, fmt.Errorf("%w: session %s", ErrSkillSetMismatch, request.Run.SessionID)
		}
		now := s.currentTime()
		if active, ok := s.leases[request.Run.SessionID]; ok {
			activeRunFound := false
			for i := range s.runs {
				if s.runs[i].ID != active.RunID {
					continue
				}
				activeRunFound = true
				if s.runs[i].SkillSetID != request.Run.SkillSetID {
					return BeginRunResult{}, fmt.Errorf("%w: active run %s", ErrSkillSetMismatch, active.RunID)
				}
				break
			}
			if !activeRunFound {
				return BeginRunResult{}, fmt.Errorf("%w: active run %s is missing", ErrSessionConflict, active.RunID)
			}
			if active.LeaseDeadline.After(now) {
				return BeginRunResult{}, fmt.Errorf("%w: run %s", ErrSessionBusy, active.RunID)
			}
			s.abandonExpiredRunLocked(active.RunID, now)
			delete(s.leases, request.Run.SessionID)
		}
		if s.leases == nil {
			s.leases = make(map[string]RunHandle)
		}
		if s.leaseGenerations == nil {
			s.leaseGenerations = make(map[string]uint64)
		}
		handle.LeaseID = request.LeaseID
		s.leaseGenerations[request.Run.SessionID]++
		handle.LeaseGeneration = s.leaseGenerations[request.Run.SessionID]
		handle.LeaseDeadline = now.Add(request.LeaseTTL)
		if sessionExists {
			handle.SessionRevision = existing.Revision
			cloned := existing
			cloned.Transcript = cloneModelInputItems(existing.Transcript)
			cloned.Checkpoint = cloneContextCheckpoint(existing.Checkpoint)
			cloned.SeenCallIDs = cloneStringsPreserveNil(existing.SeenCallIDs)
			session = &cloned
		} else if request.Run.SkillSetID != "" {
			binding := SessionState{
				ID: request.Run.SessionID, SkillSetID: request.Run.SkillSetID,
				CreatedAt: request.Run.CreatedAt, UpdatedAt: request.Run.UpdatedAt,
			}
			if s.sessions == nil {
				s.sessions = make(map[string]SessionState)
			}
			s.sessions[request.Run.SessionID] = binding
			cloned := binding
			session = &cloned
		}
		s.leases[request.Run.SessionID] = handle
	}
	if runIndex >= 0 {
		s.runs[runIndex] = request.Run
	} else {
		s.runs = append(s.runs, request.Run)
	}
	return BeginRunResult{Handle: handle, Session: session}, nil
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
	if run.SessionID != "" {
		existing, exists := s.sessions[run.SessionID]
		if exists && existing.SkillSetID != run.SkillSetID {
			return fmt.Errorf("%w: session %s", ErrSkillSetMismatch, run.SessionID)
		}
		if run.SkillSetID != "" && !exists {
			return fmt.Errorf("%w: session %s has no SkillSet binding", ErrSessionConflict, run.SessionID)
		}
		if request.Session != nil && request.Session.SkillSetID != run.SkillSetID {
			return fmt.Errorf("%w: session %s snapshot", ErrSkillSetMismatch, run.SessionID)
		}
	}
	var approvalAudit *ItemRecord
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
		cloned := pending.Audit
		cloned.Data = append(json.RawMessage(nil), pending.Audit.Data...)
		approvalAudit = &cloned
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

func newTestRuntime(t *testing.T, model Model, ops *OperationRegistry, policy OperationPolicy, executor OperationExecutor, verifier ResultVerifier, approver Approver, store RunStore) *Runtime {
	return newTestRuntimeWithEventSink(t, model, ops, policy, executor, verifier, approver, store, nil)
}

func newTestRuntimeWithEventSink(t *testing.T, model Model, ops *OperationRegistry, policy OperationPolicy, executor OperationExecutor, verifier ResultVerifier, approver Approver, store RunStore, eventSink EventSink) *Runtime {
	t.Helper()
	nextID := 0
	var executions ExecutionStore
	if value, ok := store.(ExecutionStore); ok {
		executions = value
	}
	var approvalResumer ApprovalResumer
	if value, ok := approver.(ApprovalResumer); ok {
		approvalResumer = value
	}
	rt, err := NewRuntime(RuntimeConfig{
		Model: model, Operations: ops, Policy: policy,
		Executor: executor, Verifier: verifier, Approver: approver, ApprovalResumer: approvalResumer,
		RunStore: store, Executions: executions,
		EventSink: eventSink,
		Now:       func() time.Time { return time.Unix(10, 0) },
		NewID:     func() string { nextID++; return fmt.Sprintf("id-%d", nextID) },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt
}
