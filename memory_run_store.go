package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (store *InMemoryStore) CreateRunV3(ctx context.Context, request CreateRunRequest, accept AcceptRunStart) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return store.beginRun(ctx, ResumeRunRequest{
		Run: request.Run, LeaseID: request.LeaseID, LeaseTTL: request.LeaseTTL,
	}, false, accept, nil)
}

func (store *InMemoryStore) ResumeRunV3(ctx context.Context, request ResumeRunRequest, accept AcceptResumedRun) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return store.beginRun(ctx, request, true, nil, accept)
}

func (store *InMemoryStore) beginRun(
	ctx context.Context,
	request ResumeRunRequest,
	resume bool,
	acceptCreate AcceptRunStart,
	acceptResume AcceptResumedRun,
) error {
	if (!resume && acceptCreate == nil) || (resume && acceptResume == nil) {
		return fmt.Errorf("%w: pre-commit acceptance callback is required", ErrRunStoreProtocol)
	}
	storedRequestRun, err := clonePersistentRunRecord(request.Run)
	if err != nil {
		return err
	}
	request.Run = storedRequestRun

	unlock, err := store.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	if _, exists := store.items[request.Run.ID]; exists {
		return fmt.Errorf("%w: run id %q is already assigned to an item", ErrIdentityConflict, request.Run.ID)
	}

	existingRun, runExists := store.runs[request.Run.ID]
	if !resume && runExists {
		return fmt.Errorf("%w: run id %q is already assigned", ErrIdentityConflict, request.Run.ID)
	}
	if resume && !runExists {
		return ErrRunNotFound
	}
	if resume {
		if existingRun.Status != RunStatusWaitingUser {
			return fmt.Errorf("%w: run id %q is not waiting", ErrIdentityConflict, request.Run.ID)
		}
		if existingRun.SkillSetID != request.Run.SkillSetID {
			return fmt.Errorf("%w: waiting run %s", ErrSkillSetMismatch, request.Run.ID)
		}
		if existingRun.OperationSetID != request.Run.OperationSetID {
			return fmt.Errorf("%w: waiting run %s operation set", ErrOperationPlanChanged, request.Run.ID)
		}
		inputMatches, inputErr := samePersistentRunInput(existingRun.Input, request.Run.Input)
		if inputErr != nil {
			return fmt.Errorf("%w: compare waiting run %s input: %v", ErrRunStoreProtocol, request.Run.ID, inputErr)
		}
		if !inputMatches {
			return fmt.Errorf("%w: waiting run %s input changed", ErrOperationPlanChanged, request.Run.ID)
		}
		if request.Run.UpdatedAt.Before(existingRun.UpdatedAt) {
			return fmt.Errorf("%w: waiting run %s timestamp moved backward", ErrRunStoreProtocol, request.Run.ID)
		}
		request.Run.CreatedAt = existingRun.CreatedAt
		request.Run.Input = existingRun.Input
	}

	var pendingApproval *PendingApprovalCommit
	pendingDigest := ""
	if resume {
		pendingDigest = existingRun.PendingApprovalDigest
		if err := validateApprovalAuthorityDigest(pendingDigest); err != nil {
			return fmt.Errorf("%w: waiting run %s has invalid durable approval authority: %v", ErrOperationPlanChanged, request.Run.ID, err)
		}
		pending, ok := store.pendingApprovals[request.Run.ID]
		if !ok || pending.Digest != pendingDigest {
			return fmt.Errorf("%w: waiting run %s has no matching pending approval", ErrOperationPlanChanged, request.Run.ID)
		}
		if pending.Request.Checkpoint == nil || request.InputDigest != pending.Request.Checkpoint.InputDigest {
			return fmt.Errorf("%w: waiting run %s input changed", ErrOperationPlanChanged, request.Run.ID)
		}
		if pending.Request.Operation.SessionID != request.Run.SessionID {
			return fmt.Errorf("%w: waiting run %s approval session changed", ErrOperationPlanChanged, request.Run.ID)
		}
		currentRevision := uint64(0)
		if session, exists := store.sessions[request.Run.SessionID]; request.Run.SessionID != "" && exists {
			currentRevision = session.Revision
		}
		if pending.Request.Checkpoint.ExpectedSessionRevision != currentRevision {
			return fmt.Errorf(
				"%w: waiting run %s expects session revision %d, current revision is %d",
				ErrOperationPlanChanged, request.Run.ID,
				pending.Request.Checkpoint.ExpectedSessionRevision, currentRevision,
			)
		}
		cloned, cloneErr := cloneStoredPendingApproval(pending)
		if cloneErr != nil {
			return cloneErr
		}
		pendingApproval = &cloned
	}

	handle := RunHandle{RunID: request.Run.ID, SessionID: request.Run.SessionID}
	var session *SessionState
	newBinding := false
	expiredRunID := ""
	now := store.currentTime()
	if request.Run.SessionID != "" {
		existingSession, sessionExists := store.sessions[request.Run.SessionID]
		if sessionExists && existingSession.SkillSetID != request.Run.SkillSetID {
			return fmt.Errorf("%w: session %s", ErrSkillSetMismatch, request.Run.SessionID)
		}
		if sessionExists && existingSession.OperationSetID != "" && existingSession.OperationSetID != request.Run.OperationSetID {
			return fmt.Errorf("%w: session %s operation set", ErrOperationPlanChanged, request.Run.SessionID)
		}
		if active, ok := store.leases[request.Run.SessionID]; ok {
			activeRun, ok := store.runs[active.RunID]
			if !ok {
				return fmt.Errorf("%w: active run %s is missing", ErrSessionConflict, active.RunID)
			}
			if activeRun.SkillSetID != request.Run.SkillSetID {
				return fmt.Errorf("%w: active run %s", ErrSkillSetMismatch, active.RunID)
			}
			if activeRun.OperationSetID != request.Run.OperationSetID {
				return fmt.Errorf("%w: active run %s operation set", ErrOperationPlanChanged, active.RunID)
			}
			if active.LeaseDeadline.After(now) {
				return fmt.Errorf("%w: run %s", ErrSessionBusy, active.RunID)
			}
			expiredRunID = active.RunID
		}

		handle.LeaseID = request.LeaseID
		handle.LeaseGeneration = store.leaseGenerations[request.Run.SessionID] + 1
		handle.LeaseDeadline = now.Add(request.LeaseTTL)
		if sessionExists {
			handle.SessionRevision = existingSession.Revision
			cloned := cloneStoredSessionState(existingSession)
			session = &cloned
		} else if request.Run.SkillSetID != "" || request.Run.OperationSetID != "" {
			binding := SessionState{
				ID: request.Run.SessionID, SkillSetID: request.Run.SkillSetID,
				OperationSetID: request.Run.OperationSetID,
				CreatedAt:      request.Run.CreatedAt, UpdatedAt: request.Run.UpdatedAt,
			}
			session = &binding
			newBinding = true
		}
	}

	start := RunStart{Handle: handle}
	if session != nil {
		cloned := cloneStoredSessionState(*session)
		start.Session = &cloned
	}
	if resume {
		if err := acceptResume(ResumedRun{
			RunStart: start, PendingApprovalDigest: pendingDigest, PendingApproval: pendingApproval,
		}); err != nil {
			return err
		}
	} else if err := acceptCreate(start); err != nil {
		return err
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	commitNow := store.currentTime()
	if request.Run.SessionID != "" && !handle.LeaseDeadline.After(commitNow) {
		return fmt.Errorf("%w: proposed session lease expired before commit", ErrSessionConflict)
	}

	if request.Run.SessionID != "" {
		if expiredRunID != "" {
			store.abandonExpiredRunLocked(expiredRunID, commitNow)
			delete(store.leases, request.Run.SessionID)
		}
		if newBinding {
			store.sessions[request.Run.SessionID] = cloneStoredSessionState(*session)
		}
		store.leaseGenerations[request.Run.SessionID] = handle.LeaseGeneration
		store.leases[request.Run.SessionID] = handle
	}
	if resume {
		request.Run.PendingApprovalDigest = pendingDigest
	}
	store.runs[request.Run.ID] = request.Run
	return nil
}

func (store *InMemoryStore) RenewRunLease(ctx context.Context, request RenewRunLeaseRequest) (RunHandle, error) {
	if request.LeaseTTL <= 0 {
		return RunHandle{}, fmt.Errorf("%w: session lease TTL must be positive", ErrRunStoreProtocol)
	}
	unlock, err := store.lock(ctx)
	if err != nil {
		return RunHandle{}, err
	}
	defer unlock()
	now := store.currentTime()
	active, ok := store.leases[request.Handle.SessionID]
	if !ok || !sameRunLeaseOwner(active, request.Handle) || !active.LeaseDeadline.After(now) {
		return RunHandle{}, ErrSessionLeaseLost
	}
	active.LeaseDeadline = now.Add(request.LeaseTTL)
	store.leases[active.SessionID] = active
	return active, nil
}

func (store *InMemoryStore) ValidateRunLease(ctx context.Context, handle RunHandle) (RunHandle, error) {
	unlock, err := store.lock(ctx)
	if err != nil {
		return RunHandle{}, err
	}
	defer unlock()
	active, ok := store.leases[handle.SessionID]
	if !ok || !sameRunLeaseOwner(active, handle) || !active.LeaseDeadline.After(store.currentTime()) {
		return RunHandle{}, ErrSessionLeaseLost
	}
	return active, nil
}

func sameRunLeaseOwner(left, right RunHandle) bool {
	return left.RunID == right.RunID && left.SessionID == right.SessionID &&
		left.LeaseID == right.LeaseID && left.LeaseGeneration == right.LeaseGeneration &&
		left.SessionRevision == right.SessionRevision
}

func (store *InMemoryStore) abandonExpiredRunLocked(runID string, now time.Time) {
	run, ok := store.runs[runID]
	if !ok || run.Status != RunStatusRunning {
		return
	}
	run.Status = RunStatusFailed
	run.ErrorCode = "session_lease_lost"
	run.Error = "agent: session lease expired before run completion"
	run.PendingApprovalDigest = ""
	run.FailureAuditStatus = FailureAuditMissing
	run.UpdatedAt = now
	store.runs[runID] = run
	delete(store.pendingApprovals, runID)
}

func (store *InMemoryStore) AppendItem(ctx context.Context, item ItemRecord) error {
	if err := validateStoredItem(item); err != nil {
		return err
	}
	unlock, err := store.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := store.requireAvailableItemIdentityLocked(item.ID); err != nil {
		return err
	}
	run, exists := store.runs[item.RunID]
	if !exists {
		return ErrRunNotFound
	}
	if run.SessionID != item.SessionID {
		return fmt.Errorf("%w: audit item session does not match run", ErrRunStoreProtocol)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	store.items[item.ID] = cloneStoredItemRecord(item)
	store.itemOrder[item.RunID] = append(store.itemOrder[item.RunID], item.ID)
	return nil
}

func validateStoredItem(item ItemRecord) error {
	if err := validateUTF8Boundary("audit item", item); err != nil {
		return err
	}
	if err := requireCanonicalIdentity(item.ID, "audit item id"); err != nil {
		return err
	}
	if err := requireCanonicalIdentity(item.RunID, "audit item run id"); err != nil {
		return err
	}
	if strings.TrimSpace(string(item.Type)) == "" || item.CreatedAt.IsZero() {
		return fmt.Errorf("%w: audit item type and timestamp are required", ErrRunStoreProtocol)
	}
	if len(item.Data) == 0 {
		return fmt.Errorf("%w: audit item data is required", ErrRunStoreProtocol)
	}
	if _, err := decodeExactJSON(item.Data); err != nil {
		return fmt.Errorf("%w: audit item data is ambiguous or invalid: %v", ErrRunStoreProtocol, err)
	}
	return nil
}

func (store *InMemoryStore) requireAvailableItemIdentityLocked(id string) error {
	if _, exists := store.runs[id]; exists {
		return fmt.Errorf("%w: item id %q is already assigned to a run", ErrIdentityConflict, id)
	}
	if _, exists := store.items[id]; exists {
		return fmt.Errorf("%w: item id %q is already assigned", ErrIdentityConflict, id)
	}
	return nil
}

func (store *InMemoryStore) FinishRun(ctx context.Context, request FinishRunRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	run, err := clonePersistentRunRecord(request.Run)
	if err != nil {
		return err
	}
	request.Run = run
	if request.Session != nil {
		cloned := cloneStoredSessionState(*request.Session)
		request.Session = &cloned
	}
	if request.PendingApproval != nil {
		cloned, cloneErr := cloneStoredPendingApproval(*request.PendingApproval)
		if cloneErr != nil {
			return cloneErr
		}
		request.PendingApproval = &cloned
	}
	if request.FailureItem != nil {
		cloned := cloneStoredItemRecord(*request.FailureItem)
		request.FailureItem = &cloned
	}

	unlock, err := store.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	existingRun, exists := store.runs[run.ID]
	if !exists {
		return ErrRunNotFound
	}
	if existingRun.Status != RunStatusRunning {
		return fmt.Errorf("%w: run %s is not running", ErrRunStoreProtocol, run.ID)
	}
	if existingRun.SessionID != run.SessionID {
		return ErrSessionConflict
	}
	if existingRun.SkillSetID != run.SkillSetID {
		return fmt.Errorf("%w: run %s", ErrSkillSetMismatch, run.ID)
	}
	if existingRun.OperationSetID != run.OperationSetID {
		return fmt.Errorf("%w: run %s operation set", ErrOperationPlanChanged, run.ID)
	}
	inputMatches, inputErr := samePersistentRunInput(existingRun.Input, run.Input)
	if inputErr != nil {
		return fmt.Errorf("%w: compare run %s input: %v", ErrRunStoreProtocol, run.ID, inputErr)
	}
	if !inputMatches {
		return fmt.Errorf("%w: run %s input changed", ErrRunStoreProtocol, run.ID)
	}
	if run.UpdatedAt.Before(existingRun.UpdatedAt) {
		return fmt.Errorf("%w: run %s timestamp moved backward", ErrRunStoreProtocol, run.ID)
	}
	run.CreatedAt = existingRun.CreatedAt
	run.Input = existingRun.Input

	if run.Status == RunStatusInterrupted || run.Status == RunStatusCancelled {
		request.Session = nil
	}
	if run.SessionID != "" {
		existingSession, sessionExists := store.sessions[run.SessionID]
		if sessionExists && existingSession.SkillSetID != run.SkillSetID {
			return fmt.Errorf("%w: session %s", ErrSkillSetMismatch, run.SessionID)
		}
		if sessionExists && existingSession.OperationSetID != "" && existingSession.OperationSetID != run.OperationSetID {
			return fmt.Errorf("%w: session %s operation set", ErrOperationPlanChanged, run.SessionID)
		}
		if (run.SkillSetID != "" || run.OperationSetID != "") && !sessionExists {
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
			return fmt.Errorf("%w: invalid pending approval for run %s", ErrRunStoreProtocol, run.ID)
		}
		expectedAudit, marshalErr := json.Marshal(pending.Decision)
		if marshalErr != nil || !bytes.Equal(pending.Audit.Data, expectedAudit) {
			return fmt.Errorf("%w: invalid pending approval audit for run %s", ErrRunStoreProtocol, run.ID)
		}
		expectedDigest, digestErr := pendingApprovalAuthorityDigest(*pending)
		if digestErr != nil || pending.Digest == "" || pending.Digest != expectedDigest || run.PendingApprovalDigest != pending.Digest {
			return fmt.Errorf("%w: invalid pending approval digest for run %s", ErrRunStoreProtocol, run.ID)
		}
		committedRevision := uint64(0)
		if run.SessionID != "" {
			if request.Session != nil {
				committedRevision = request.Session.Revision
			} else if session, ok := store.sessions[run.SessionID]; ok {
				committedRevision = session.Revision
			}
		}
		if pending.Request.Checkpoint == nil || pending.Request.Checkpoint.ExpectedSessionRevision != committedRevision {
			return fmt.Errorf("%w: pending approval session revision does not match waiting commit", ErrRunStoreProtocol)
		}
		commit := *pending
		approvalCommit = &commit
		audit := pending.Audit
		approvalAudit = &audit
	}
	if approvalCommit == nil && run.Status == RunStatusWaitingUser {
		persisted, ok := store.pendingApprovals[run.ID]
		if !ok || persisted.Digest != run.PendingApprovalDigest {
			return fmt.Errorf("%w: waiting run %s has no matching pending approval", ErrRunStoreProtocol, run.ID)
		}
		currentRevision := uint64(0)
		if session, exists := store.sessions[run.SessionID]; run.SessionID != "" && exists {
			currentRevision = session.Revision
		}
		if persisted.Request.Checkpoint == nil || persisted.Request.Checkpoint.ExpectedSessionRevision != currentRevision {
			return fmt.Errorf("%w: waiting run %s approval session revision changed", ErrOperationPlanChanged, run.ID)
		}
	}
	if approvalAudit != nil {
		if err := validateStoredItem(*approvalAudit); err != nil {
			return err
		}
		if err := store.requireAvailableItemIdentityLocked(approvalAudit.ID); err != nil {
			return err
		}
	}
	if request.FailureItem != nil {
		if err := store.requireAvailableItemIdentityLocked(request.FailureItem.ID); err != nil {
			return err
		}
	}

	if run.SessionID != "" {
		active, ok := store.leases[run.SessionID]
		if !ok || !sameRunLeaseOwner(active, request.Handle) || !active.LeaseDeadline.After(store.currentTime()) {
			return ErrSessionLeaseLost
		}
		existingSession, sessionExists := store.sessions[run.SessionID]
		if (!sessionExists && request.Handle.SessionRevision != 0) || (sessionExists && existingSession.Revision != request.Handle.SessionRevision) {
			return ErrSessionConflict
		}
		if request.Session == nil {
			if run.Status != RunStatusFailed && run.Status != RunStatusWaitingUser &&
				run.Status != RunStatusInterrupted && run.Status != RunStatusCancelled {
				return ErrSessionConflict
			}
		} else if request.Session.ID != run.SessionID || request.Session.Revision != request.Handle.SessionRevision+1 {
			return ErrSessionConflict
		} else {
			if sessionExists && !existingSession.CreatedAt.IsZero() && !request.Session.CreatedAt.Equal(existingSession.CreatedAt) {
				return fmt.Errorf("%w: session %s creation timestamp changed", ErrRunStoreProtocol, run.SessionID)
			}
			if sessionExists && !existingSession.CreatedAt.IsZero() {
				request.Session.CreatedAt = existingSession.CreatedAt
			}
			if sessionExists && !existingSession.UpdatedAt.IsZero() && request.Session.UpdatedAt.Before(existingSession.UpdatedAt) {
				return fmt.Errorf("%w: session %s timestamp moved backward", ErrRunStoreProtocol, run.SessionID)
			}
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}

	if request.Session != nil {
		store.sessions[request.Session.ID] = cloneStoredSessionState(*request.Session)
	}
	if approvalAudit != nil {
		store.items[approvalAudit.ID] = cloneStoredItemRecord(*approvalAudit)
		store.itemOrder[approvalAudit.RunID] = append(store.itemOrder[approvalAudit.RunID], approvalAudit.ID)
	}
	if request.FailureItem != nil {
		item := request.FailureItem
		store.items[item.ID] = cloneStoredItemRecord(*item)
		store.itemOrder[item.RunID] = append(store.itemOrder[item.RunID], item.ID)
	}
	if approvalCommit != nil {
		store.pendingApprovals[run.ID] = *approvalCommit
	} else if run.Status != RunStatusWaitingUser {
		delete(store.pendingApprovals, run.ID)
	}
	store.runs[run.ID] = run
	if run.SessionID != "" {
		delete(store.leases, run.SessionID)
	}
	return nil
}

var _ RunStore = (*InMemoryStore)(nil)

func samePersistentRunInput(left, right Input) (bool, error) {
	leftDigest, err := persistentOperationInputDigest(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := persistentOperationInputDigest(right)
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}
