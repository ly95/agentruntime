package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
)

type runStateSnapshot struct {
	lastResponseID string
	transcript     []ModelInputItem
	checkpoint     *ContextCheckpoint
	seenCallIDs    map[string]struct{}
	instructions   string
}

func captureRunState(state *agentState) runStateSnapshot {
	return runStateSnapshot{
		lastResponseID: state.lastResponseID,
		transcript:     cloneModelInputItems(state.transcript),
		checkpoint:     cloneContextCheckpoint(state.checkpoint),
		seenCallIDs:    maps.Clone(state.seenCallIDs),
		instructions:   state.instructions,
	}
}

func (snapshot runStateSnapshot) restore(state *agentState, _ *RunRecord) {
	state.lastResponseID = snapshot.lastResponseID
	state.transcript = cloneModelInputItems(snapshot.transcript)
	state.checkpoint = cloneContextCheckpoint(snapshot.checkpoint)
	state.seenCallIDs = maps.Clone(snapshot.seenCallIDs)
	state.instructions = snapshot.instructions
}

func normalizeRuntimeInput(input Input) (Input, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	input.User = strings.TrimSpace(input.User)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.IdempotencyScope = strings.TrimSpace(input.IdempotencyScope)
	input.Attachments = cloneModelInputAttachments(input.Attachments)
	if input.User == "" {
		return Input{}, errors.New("agent: input user text is required")
	}
	for index := range input.Attachments {
		input.Attachments[index] = NormalizeModelInputAttachment(input.Attachments[index])
		if err := ValidateModelInputAttachment(input.Attachments[index]); err != nil {
			return Input{}, fmt.Errorf("agent: input attachment %d: %w", index, err)
		}
		if input.SessionID != "" && input.Attachments[index].Kind == ModelInputAttachmentImage {
			if input.Attachments[index].StorageKey == "" || input.Attachments[index].ExpiresAt.IsZero() {
				return Input{}, fmt.Errorf("agent: input attachment %d: session image attachment requires stable storage metadata", index)
			}
			if isNilDependency(input.ImageAttachmentResolver) {
				return Input{}, fmt.Errorf("agent: input attachment %d: session image attachment requires a resolver", index)
			}
		}
		input.Attachments[index].CurrentRun = true
	}
	clonedInput, err := cloneOperationInput(input)
	if err != nil {
		return Input{}, err
	}
	return clonedInput, nil
}

func (r *Runtime) beginRuntimeRun(
	ctx context.Context,
	input Input,
) (RunRecord, RunHandle, *SessionState, error) {
	if input.SessionID != "" && r.runStore == nil {
		return RunRecord{}, RunHandle{}, nil, ErrSessionStoreNeeded
	}
	now := r.now()
	runID := input.RunID
	if runID == "" {
		runID = r.newID()
	}
	run := RunRecord{
		ID:         runID,
		SessionID:  input.SessionID,
		SkillSetID: r.skillSetID,
		Status:     RunStatusRunning,
		Input:      input,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	handle := RunHandle{RunID: run.ID, SessionID: run.SessionID}
	var session *SessionState
	if r.runStore != nil {
		storeRun := run
		var err error
		storeRun.Input, err = cloneOperationInput(run.Input)
		if err != nil {
			return RunRecord{}, RunHandle{}, nil, err
		}
		leaseID := ""
		if run.SessionID != "" {
			leaseID = r.newID()
		}
		begun, err := r.runStore.BeginRun(ctx, BeginRunRequest{Run: storeRun, LeaseID: leaseID, LeaseTTL: r.sessionLeaseTTL})
		if err != nil {
			return RunRecord{}, RunHandle{}, nil, err
		}
		handle = begun.Handle
		session = begun.Session
	}
	return run, handle, session, nil
}

func (r *Runtime) prepareApprovalResume(
	ctx context.Context,
	run *RunRecord,
	state *agentState,
) (*ApprovalResume, error) {
	if r.approvalResumer == nil {
		return nil, nil
	}
	resume, err := r.approvalResumer.ResumeApproval(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if resume == nil {
		return nil, nil
	}
	if resume.Pending {
		return resume, nil
	}
	if err := r.validateApprovalResume(state, resume); err != nil {
		return nil, err
	}
	return resume, nil
}

type activeRunLease struct {
	runtime             *Runtime
	baseContext         context.Context
	runContext          context.Context
	run                 *RunRecord
	state               *agentState
	prepareFinalization func()
	stopRenewal         func() error
}

func (r *Runtime) startActiveRunLease(
	ctx context.Context,
	run *RunRecord,
	state *agentState,
) *activeRunLease {
	runCtx, prepareFinalization, stopRenewal := state.lease.Start(ctx)
	return &activeRunLease{
		runtime: r, baseContext: ctx, runContext: runCtx, run: run, state: state,
		prepareFinalization: prepareFinalization, stopRenewal: stopRenewal,
	}
}

func (active *activeRunLease) stop() {
	_ = active.stopRenewal()
}

func (active *activeRunLease) fail(cause error) error {
	active.prepareFinalization()
	runCause := context.Cause(active.runContext)
	if errors.Is(runCause, ErrSessionLeaseLost) && !errors.Is(cause, ErrSessionLeaseLost) {
		cause = errors.Join(runCause, cause)
	}
	failure := active.runtime.failRun(active.baseContext, *active.run, active.state, cause)
	active.stop()
	return failure
}

func (active *activeRunLease) wait() (*Result, error) {
	active.prepareFinalization()
	result, err := active.runtime.waitRun(
		active.baseContext, *active.run, active.state,
	)
	if err != nil {
		return nil, active.fail(err)
	}
	active.stop()
	return result, nil
}

func (r *Runtime) Run(ctx context.Context, input Input) (*Result, error) {
	input, err := normalizeRuntimeInput(input)
	if err != nil {
		return nil, err
	}
	run, handle, session, err := r.beginRuntimeRun(ctx, input)
	if err != nil {
		return nil, err
	}
	r.emit(Event{Type: EventRunStarted, RunID: run.ID, SessionID: run.SessionID})
	mcpInfo := r.mcp.ServerInfo()
	r.emit(Event{
		Type: EventMCPConnected, RunID: run.ID, SessionID: run.SessionID,
		MCPServer: mcpInfo.Name, MCPVersion: mcpInfo.Version,
		MCPProtocol: mcpInfo.ProtocolVersion, MCPToolCount: len(r.toolSnapshot),
	})
	state, err := r.stateFromSession(run.ID, input.SessionID, handle, session)
	if err != nil {
		return nil, r.failRun(ctx, run, state, err)
	}
	active := r.startActiveRunLease(ctx, &run, state)
	defer active.stop()
	stableState := captureRunState(state)
	approvalResume, err := r.prepareApprovalResume(active.runContext, &run, state)
	if err != nil {
		stableState.restore(state, &run)
		return nil, active.fail(err)
	}
	if approvalResume != nil && approvalResume.Pending {
		if runCause := context.Cause(active.runContext); runCause != nil {
			stableState.restore(state, &run)
			return nil, active.fail(runCause)
		}
		return active.wait()
	}

	// MCP negotiation and its server instructions are fixed for this Runtime.
	// Later failures roll back only per-run model and transcript mutations.
	stableState = captureRunState(state)
	if approvalResume == nil {
		if err := r.appendUserItem(active.runContext, run.ID, input); err != nil {
			stableState.restore(state, &run)
			return nil, active.fail(err)
		}
	}
	out, err := r.runAgent(active.runContext, &run, input, state, approvalResume)
	if err != nil {
		if ctxCause := context.Cause(active.runContext); ctxCause != nil && !errors.Is(err, ErrOperationOutcomeUnknown) {
			err = ctxCause
		}
		// A failed run may end on an unresolved function call. Keep that response
		// in the audit log, but resume the next user turn from the last stable run.
		stableState.restore(state, &run)
		if errors.Is(err, ErrApprovalPending) {
			if runCause := context.Cause(active.runContext); runCause != nil {
				return nil, active.fail(runCause)
			}
			return active.wait()
		}
		if errors.Is(err, ErrContextLimitExceeded) || errors.Is(err, ErrContextCompactionFailed) {
			state.sessionReady = false
		}
		return nil, active.fail(err)
	}
	if runCause := context.Cause(active.runContext); runCause != nil {
		stableState.restore(state, &run)
		return nil, active.fail(runCause)
	}
	active.prepareFinalization()
	result, err := r.completeRun(ctx, run, state, out)
	if err != nil {
		stableState.restore(state, &run)
		return nil, active.fail(err)
	}
	active.stop()
	return result, nil
}

func (r *Runtime) stateFromSession(runID, sessionID string, handle RunHandle, session *SessionState) (*agentState, error) {
	state := &agentState{
		sessionID:    sessionID,
		lease:        newLeaseGuard(r.runStore, handle, r.sessionLeaseTTL, r.leaseRenewalInterval, r.cleanupTimeout),
		seenCallIDs:  make(map[string]struct{}),
		createdAt:    r.now(),
		instructions: r.baseInstructions,
	}
	if err := validateRunHandle(runID, sessionID, handle, session); err != nil {
		return state, err
	}
	if session == nil {
		if sessionID != "" && r.skillSetID != "" {
			return state, fmt.Errorf("%w: run store did not return the SkillSet binding for session %q", ErrSessionConflict, sessionID)
		}
		state.sessionReady = true
		return state, nil
	}
	if session.SkillSetID != r.skillSetID {
		return state, fmt.Errorf(
			"%w: session %q uses SkillSet %q, current Runtime uses %q",
			ErrSkillSetMismatch, sessionID, session.SkillSetID, r.skillSetID,
		)
	}
	state.lastResponseID = session.LastResponseID
	state.transcript = clonePersistentModelInputItems(session.Transcript)
	state.checkpoint = cloneContextCheckpoint(session.Checkpoint)
	if err := r.validateSessionCheckpoint(state.checkpoint, session.Revision); err != nil {
		return state, err
	}
	if err := restoreSavedCallIDs(state, session.SeenCallIDs); err != nil {
		return state, err
	}
	if err := restoreTranscriptCallIDs(state, session.SeenCallIDs != nil); err != nil {
		return state, err
	}
	state.createdAt = session.CreatedAt
	if state.createdAt.IsZero() {
		state.createdAt = r.now()
	}
	state.sessionReady = true
	return state, nil
}

func validateRunHandle(runID, sessionID string, handle RunHandle, session *SessionState) error {
	if handle.RunID != runID {
		return fmt.Errorf("agent: run store returned handle for run %q, want %q", handle.RunID, runID)
	}
	if handle.SessionID != sessionID {
		return fmt.Errorf("agent: run store returned handle for session %q, want %q", handle.SessionID, sessionID)
	}
	if sessionID != "" && handle.LeaseID == "" {
		return errors.New("agent: run store returned an empty session lease")
	}
	if sessionID != "" && (handle.LeaseGeneration == 0 || handle.LeaseDeadline.IsZero()) {
		return errors.New("agent: run store returned an invalid session lease fence")
	}
	if session == nil && handle.SessionRevision != 0 {
		return fmt.Errorf("agent: run store returned revision %d without a session", handle.SessionRevision)
	}
	if session != nil && session.ID != sessionID {
		return fmt.Errorf("agent: run store returned session %q for %q", session.ID, sessionID)
	}
	if session != nil && session.Revision != handle.SessionRevision {
		return fmt.Errorf("agent: run store returned session revision %d with handle revision %d", session.Revision, handle.SessionRevision)
	}
	return nil
}

func (r *Runtime) validateSessionCheckpoint(checkpoint *ContextCheckpoint, revision uint64) error {
	if checkpoint == nil {
		return nil
	}
	if r.contextWindow == nil {
		return fmt.Errorf("%w: session contains a checkpoint but context compaction is disabled", ErrContextCompactionFailed)
	}
	if err := validateContextCheckpoint(checkpoint); err != nil {
		return fmt.Errorf("%w: invalid session checkpoint: %v", ErrContextCompactionFailed, err)
	}
	if checkpoint.SourceSessionRevision >= revision {
		return fmt.Errorf(
			"%w: checkpoint source revision %d is not older than session revision %d",
			ErrContextCompactionFailed, checkpoint.SourceSessionRevision, revision,
		)
	}
	return nil
}

func restoreSavedCallIDs(state *agentState, savedCallIDs []string) error {
	previousCallID := ""
	for _, savedCallID := range savedCallIDs {
		callID := strings.TrimSpace(savedCallID)
		if callID == "" || callID != savedCallID {
			return fmt.Errorf("%w: session contains an empty or unnormalized saved function call id", ErrInvalidModelOutput)
		}
		if _, exists := state.seenCallIDs[callID]; exists {
			return fmt.Errorf("%w: session contains duplicate saved function call id %q", ErrInvalidModelOutput, callID)
		}
		if previousCallID != "" && callID < previousCallID {
			return fmt.Errorf("%w: saved function call ids are not sorted", ErrInvalidModelOutput)
		}
		state.seenCallIDs[callID] = struct{}{}
		previousCallID = callID
	}
	return nil
}

func restoreTranscriptCallIDs(state *agentState, hasSavedCallIDs bool) error {
	transcriptCallIDs := make(map[string]struct{})
	for _, item := range state.transcript {
		if item.Type != ModelInputToolResult {
			continue
		}
		callID := strings.TrimSpace(item.CallID)
		if callID == "" {
			return fmt.Errorf("%w: session transcript contains an empty function call id", ErrInvalidModelOutput)
		}
		if _, duplicate := transcriptCallIDs[callID]; duplicate {
			return fmt.Errorf("%w: session transcript contains duplicate function call id %q", ErrInvalidModelOutput, callID)
		}
		transcriptCallIDs[callID] = struct{}{}
		_, exists := state.seenCallIDs[callID]
		if hasSavedCallIDs && !exists {
			return fmt.Errorf("%w: saved function call ids omit transcript call id %q", ErrInvalidModelOutput, callID)
		}
		if !hasSavedCallIDs && exists {
			return fmt.Errorf("%w: session transcript contains duplicate function call id %q", ErrInvalidModelOutput, callID)
		}
		state.seenCallIDs[callID] = struct{}{}
	}
	return nil
}
