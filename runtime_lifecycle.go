package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"
)

type runStartAcceptance[T any] struct {
	mu        sync.Mutex
	label     string
	route     *runStartRoute
	calls     int
	completed bool
	closed    bool
	value     T
	accepted  bool
	firstErr  error
	protocol  error
}

// runStartRoute joins the resume probe and explicit-ID create fallback into
// one protocol boundary. A callback that arrives after either store method has
// returned poisons the whole start attempt, so fallback cannot hide it.
type runStartRoute struct {
	mu       sync.Mutex
	protocol error
}

func (route *runStartRoute) record(err error) error {
	if route == nil || err == nil {
		return err
	}
	route.mu.Lock()
	defer route.mu.Unlock()
	if route.protocol == nil {
		route.protocol = err
	}
	return route.protocol
}

func (route *runStartRoute) current() error {
	if route == nil {
		return nil
	}
	route.mu.Lock()
	defer route.mu.Unlock()
	return route.protocol
}

func (acceptance *runStartAcceptance[T]) rejectProtocol(err error) error {
	if acceptance.protocol == nil {
		acceptance.protocol = err
	}
	return acceptance.route.record(acceptance.protocol)
}

func (acceptance *runStartAcceptance[T]) invoke(validate func() (T, error)) error {
	acceptance.mu.Lock()
	if acceptance.closed {
		err := fmt.Errorf("%w: run store invoked %s acceptance after method return", ErrRunStoreProtocol, acceptance.label)
		acceptance.rejectProtocol(err)
		acceptance.mu.Unlock()
		return err
	}
	acceptance.calls++
	if acceptance.calls != 1 {
		err := fmt.Errorf("%w: run store invoked %s acceptance more than once", ErrRunStoreProtocol, acceptance.label)
		acceptance.rejectProtocol(err)
		acceptance.mu.Unlock()
		return err
	}
	if err := acceptance.route.current(); err != nil {
		acceptance.rejectProtocol(err)
		acceptance.mu.Unlock()
		return err
	}
	acceptance.mu.Unlock()

	value, err := validate()
	acceptance.mu.Lock()
	defer acceptance.mu.Unlock()
	acceptance.completed = true
	acceptance.firstErr = err
	if acceptance.closed {
		acceptance.rejectProtocol(fmt.Errorf("%w: run store returned before %s acceptance completed", ErrRunStoreProtocol, acceptance.label))
		return acceptance.protocol
	}
	if err == nil {
		acceptance.value = value
		acceptance.accepted = true
	}
	return err
}

func (acceptance *runStartAcceptance[T]) finish(storeErr error) (T, error) {
	acceptance.mu.Lock()
	defer acceptance.mu.Unlock()
	acceptance.closed = true
	if routeErr := acceptance.route.current(); routeErr != nil && acceptance.protocol == nil {
		acceptance.protocol = routeErr
	}
	if acceptance.protocol != nil || acceptance.calls > 1 {
		if acceptance.protocol != nil {
			return *new(T), acceptance.protocol
		}
		return *new(T), fmt.Errorf("%w: run store invoked %s acceptance %d times", ErrRunStoreProtocol, acceptance.label, acceptance.calls)
	}
	if acceptance.calls == 1 && !acceptance.completed {
		err := fmt.Errorf("%w: run store returned before %s acceptance completed", ErrRunStoreProtocol, acceptance.label)
		acceptance.rejectProtocol(err)
		return *new(T), err
	}
	if acceptance.firstErr != nil {
		if storeErr != nil && !errors.Is(storeErr, acceptance.firstErr) {
			return *new(T), errors.Join(acceptance.firstErr, storeErr)
		}
		return *new(T), acceptance.firstErr
	}
	if storeErr != nil {
		return *new(T), storeErr
	}
	if acceptance.calls != 1 || !acceptance.completed || !acceptance.accepted {
		return *new(T), fmt.Errorf("%w: run store did not invoke %s acceptance exactly once", ErrRunStoreProtocol, acceptance.label)
	}
	return acceptance.value, nil
}

func (acceptance *runStartAcceptance[T]) wasInvoked() bool {
	acceptance.mu.Lock()
	defer acceptance.mu.Unlock()
	return acceptance.calls != 0
}

type createdRunStart struct {
	state *agentState
}

type resumedRunStart struct {
	state  *agentState
	digest string
}

type runStateSnapshot struct {
	lastResponseID      string
	transcript          []ModelInputItem
	checkpoint          *ContextCheckpoint
	seenCallIDs         map[string]struct{}
	seenResponseIDs     map[string]struct{}
	seenProviderItemIDs map[string]struct{}
	instructions        string
}

func captureRunState(state *agentState) runStateSnapshot {
	return runStateSnapshot{
		lastResponseID:      state.lastResponseID,
		transcript:          cloneModelInputItems(state.transcript),
		checkpoint:          cloneContextCheckpoint(state.checkpoint),
		seenCallIDs:         maps.Clone(state.seenCallIDs),
		seenResponseIDs:     maps.Clone(state.seenResponseIDs),
		seenProviderItemIDs: maps.Clone(state.seenProviderItemIDs),
		instructions:        state.instructions,
	}
}

func (snapshot runStateSnapshot) restore(state *agentState, _ *RunRecord) {
	state.lastResponseID = snapshot.lastResponseID
	state.transcript = cloneModelInputItems(snapshot.transcript)
	state.checkpoint = cloneContextCheckpoint(snapshot.checkpoint)
	state.seenCallIDs = maps.Clone(snapshot.seenCallIDs)
	state.seenResponseIDs = maps.Clone(snapshot.seenResponseIDs)
	state.seenProviderItemIDs = maps.Clone(snapshot.seenProviderItemIDs)
	state.instructions = snapshot.instructions
}

func normalizeRuntimeInput(input Input) (Input, error) {
	return normalizeRuntimeInputWithResolver(input, nil)
}

func normalizeRuntimeInputWithResolver(input Input, defaultResolver ImageAttachmentResolver) (Input, error) {
	if input.ImageAttachmentResolver != nil && isNilDependency(input.ImageAttachmentResolver) {
		return Input{}, errors.New("agent: input image attachment resolver is nil")
	}
	if input.ImageAttachmentResolver == nil {
		input.ImageAttachmentResolver = defaultResolver
	}
	boundary := input
	boundary.ImageAttachmentResolver = nil
	if err := validateUTF8Boundary("runtime input", boundary); err != nil {
		return Input{}, err
	}
	for _, identity := range []struct {
		value string
		kind  string
	}{
		{value: input.RunID, kind: "run id"},
		{value: input.SessionID, kind: "session id"},
		{value: input.IdempotencyKey, kind: "idempotency key"},
		{value: input.IdempotencyScope, kind: "idempotency scope"},
	} {
		if identity.value != "" {
			if err := validateRuntimeIdentity(identity.value, identity.kind); err != nil {
				return Input{}, err
			}
		}
	}
	input.TrustedContext = strings.TrimSpace(input.TrustedContext)
	if input.SessionID != "" {
		input.IdempotencyScope = ""
	}
	if input.TrustedContext != "" {
		trustedValue, err := decodeExactJSON(json.RawMessage(input.TrustedContext))
		if err != nil {
			return Input{}, fmt.Errorf("agent: trusted context must be unambiguous valid JSON: %w", err)
		}
		framed, err := json.Marshal(trustedValue)
		if err != nil {
			return Input{}, fmt.Errorf("agent: frame trusted context: %w", err)
		}
		// encoding/json escapes '<', '>', and '&' in strings, so trusted data
		// cannot synthesize or terminate the surrounding instruction tags.
		input.TrustedContext = string(framed)
	}
	input.Attachments = cloneModelInputAttachments(input.Attachments)
	if strings.TrimSpace(input.User) == "" {
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
) (RunRecord, *agentState, error) {
	if input.SessionID != "" && r.runStore == nil {
		return RunRecord{}, nil, ErrSessionStoreNeeded
	}
	now := r.now()
	acceptanceClockStarted := time.Now()
	// acceptanceTime measures wall-clock elapsed time on top of the injected
	// r.now() anchor. The two sources intentionally serve different roles: r.now()
	// keeps the run-start timestamps deterministic for hosts with a fake clock,
	// while time.Since provides a real monotonic guard for lease-deadline
	// liveness checks inside the store transaction. The returned value is only
	// ever compared against a store-owned deadline with a monotonic guard; it is
	// never persisted, so mixing the sources cannot leak into durable state.
	acceptanceTime := func() time.Time {
		return now.Add(time.Since(acceptanceClockStarted))
	}
	runID := input.RunID
	explicitRunID := runID != ""
	if runID == "" {
		var err error
		runID, err = r.nextGeneratedID(ctx, "run id")
		if err != nil {
			return RunRecord{}, nil, err
		}
	} else if err := r.reserveRunIdentity(ctx, runID); err != nil {
		return RunRecord{}, nil, err
	}
	input.RunID = runID
	run := RunRecord{
		ID:             runID,
		SessionID:      input.SessionID,
		ModelBindingID: r.modelBindingID,
		SkillSetID:     r.skillSetID,
		OperationSetID: r.operationSetID,
		Status:         RunStatusRunning,
		Input:          input,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := validateUTF8Boundary("new run", run); err != nil {
		return RunRecord{}, nil, err
	}
	handle := RunHandle{RunID: run.ID, SessionID: run.SessionID}
	if r.runStore == nil {
		state, err := r.stateFromSessionAt(run.ID, input.SessionID, handle.LeaseID, handle, nil, now, now)
		return run, state, err
	}
	startRoute := &runStartRoute{}
	storeRun := run
	var err error
	storeRun.Input, err = clonePersistentOperationInput(run.Input)
	if err != nil {
		return RunRecord{}, nil, err
	}
	leaseID := ""
	if run.SessionID != "" {
		leaseID, err = r.nextGeneratedID(ctx, "session lease id")
		if err != nil {
			return RunRecord{}, nil, err
		}
	}
	createRequest := CreateRunRequest{Run: storeRun, LeaseID: leaseID, LeaseTTL: r.sessionLeaseTTL}
	if err := createRequest.Validate(); err != nil {
		return RunRecord{}, nil, err
	}
	if err := validateUTF8Boundary("create run request", createRequest); err != nil {
		return RunRecord{}, nil, err
	}
	createAcceptance := runStartAcceptance[createdRunStart]{label: "create", route: startRoute}
	acceptCreate := func(start RunStart) error {
		return createAcceptance.invoke(func() (createdRunStart, error) {
			if cause := context.Cause(ctx); cause != nil {
				return createdRunStart{}, cause
			}
			if err := validateUTF8Boundary("run store start", start); err != nil {
				return createdRunStart{}, err
			}
			state, err := r.stateFromSessionAt(
				run.ID, input.SessionID, leaseID, start.Handle, start.Session, now, acceptanceTime(),
			)
			if err != nil {
				return createdRunStart{}, err
			}
			if err := validateRunHandle(run.ID, input.SessionID, leaseID, start.Handle, start.Session, acceptanceTime()); err != nil {
				return createdRunStart{}, err
			}
			if cause := context.Cause(ctx); cause != nil {
				return createdRunStart{}, cause
			}
			return createdRunStart{state: state}, nil
		})
	}
	create := func() (*agentState, error) {
		storeErr := validateUTF8Error("run store", r.runStore.CreateRunV4(ctx, createRequest, acceptCreate))
		accepted, err := createAcceptance.finish(storeErr)
		if err != nil {
			return nil, err
		}
		if err := r.validateAcceptedRunStartAfterStore(ctx, accepted.state, acceptanceTime); err != nil {
			return nil, err
		}
		if err := startRoute.current(); err != nil {
			return nil, err
		}
		return accepted.state, nil
	}
	if !explicitRunID {
		createState, err := create()
		if err != nil {
			return RunRecord{}, nil, err
		}
		return run, createState, nil
	}
	inputDigest, err := persistentOperationInputDigest(input)
	if err != nil {
		return RunRecord{}, nil, err
	}
	resumeRequest := ResumeRunRequest{
		Run: storeRun, LeaseID: leaseID, LeaseTTL: r.sessionLeaseTTL, InputDigest: inputDigest,
	}
	if err := resumeRequest.Validate(); err != nil {
		return RunRecord{}, nil, err
	}
	if err := validateUTF8Boundary("resume run request", resumeRequest); err != nil {
		return RunRecord{}, nil, err
	}
	resumeAcceptance := runStartAcceptance[resumedRunStart]{label: "resume", route: startRoute}
	storeErr := r.runStore.ResumeRunV4(ctx, resumeRequest, func(resumed ResumedRun) error {
		return resumeAcceptance.invoke(func() (resumedRunStart, error) {
			if cause := context.Cause(ctx); cause != nil {
				return resumedRunStart{}, cause
			}
			if err := validateUTF8Boundary("run store resumed run", resumed); err != nil {
				return resumedRunStart{}, err
			}
			pending, digest, err := r.validatePendingApprovalEnvelope(run, inputDigest, resumed)
			if err != nil {
				return resumedRunStart{}, err
			}
			state, err := r.stateFromSessionAt(
				run.ID, input.SessionID, leaseID, resumed.Handle, resumed.Session, now, acceptanceTime(),
			)
			if err != nil {
				return resumedRunStart{}, err
			}
			state.pendingApprovalDigest = digest
			state.resumedApproval = pending
			if err := r.validatePendingApprovalStartAuthority(state, pending); err != nil {
				return resumedRunStart{}, err
			}
			if err := validateRunHandle(run.ID, input.SessionID, leaseID, resumed.Handle, resumed.Session, acceptanceTime()); err != nil {
				return resumedRunStart{}, err
			}
			if cause := context.Cause(ctx); cause != nil {
				return resumedRunStart{}, cause
			}
			return resumedRunStart{state: state, digest: digest}, nil
		})
	})
	storeErr = validateUTF8Error("run store", storeErr)
	accepted, err := resumeAcceptance.finish(storeErr)
	if err != nil {
		if isUnambiguousRunNotFound(err) {
			if resumeAcceptance.wasInvoked() {
				return RunRecord{}, nil, fmt.Errorf("%w: absent resume invoked pre-commit acceptance", ErrRunStoreProtocol)
			}
			createState, createErr := create()
			if createErr != nil {
				return RunRecord{}, nil, createErr
			}
			return run, createState, nil
		}
		return RunRecord{}, nil, err
	}
	if err := r.validateAcceptedRunStartAfterStore(ctx, accepted.state, acceptanceTime); err != nil {
		return RunRecord{}, nil, err
	}
	if err := startRoute.current(); err != nil {
		return RunRecord{}, nil, err
	}
	run.PendingApprovalDigest = accepted.digest
	return run, accepted.state, nil
}

func (r *Runtime) validateAcceptedRunStartAfterStore(
	ctx context.Context,
	state *agentState,
	acceptedAt func() time.Time,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if state == nil || state.lease == nil || state.sessionID == "" {
		return nil
	}
	if _, err := state.lease.Validate(ctx); err != nil {
		return err
	}
	handle := state.lease.Handle()
	if !handle.LeaseDeadline.After(acceptedAt()) {
		return fmt.Errorf("%w: run %s lease expired before first work", ErrSessionLeaseLost, handle.RunID)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return nil
}

func (r *Runtime) prepareApprovalResume(
	ctx context.Context,
	run *RunRecord,
	state *agentState,
	input Input,
) (*ApprovalResume, error) {
	if state.pendingApprovalDigest == "" {
		return nil, nil
	}
	if r.approvalResumer == nil {
		return nil, fmt.Errorf("%w: waiting run %s requires a durable approval resumer", ErrApprovalRequired, run.ID)
	}
	if err := r.validateCurrentModelBinding(); err != nil {
		return nil, err
	}
	resume, err := r.approvalResumer.ResumeApproval(ctx, run.ID)
	if bindingErr := r.validateCurrentModelBinding(); bindingErr != nil {
		if err != nil {
			bindingErr = errors.Join(bindingErr, validateUTF8Error("approval resumer", err))
		}
		return nil, bindingErr
	}
	if err != nil {
		return nil, validateUTF8Error("approval resumer", err)
	}
	if resume == nil {
		if state.pendingApprovalDigest != "" {
			return nil, fmt.Errorf("%w: waiting run %s has no durable approval record", ErrApprovalRequired, run.ID)
		}
		return nil, nil
	}
	if err := validateUTF8Boundary("approval resume", resume); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
	}
	if err := r.validateApprovalResumeModelBinding(state, resume); err != nil {
		return nil, err
	}
	if err := validateApprovalResumeAuthority(state, resume); err != nil {
		return nil, err
	}
	checkpoint := resume.Checkpoint
	if checkpoint == nil || strings.TrimSpace(checkpoint.InputDigest) == "" {
		return nil, fmt.Errorf("%w: approval %s has no committed input identity", ErrOperationPlanChanged, resume.ID)
	}
	inputDigest, err := persistentOperationInputDigest(input)
	if err != nil {
		return nil, err
	}
	if inputDigest != checkpoint.InputDigest {
		return nil, fmt.Errorf("%w: approval %s input changed", ErrOperationPlanChanged, resume.ID)
	}
	if err := validateApprovalResumeSessionRevision(state, resume); err != nil {
		return nil, err
	}
	if resume.Pending {
		return resume, nil
	}
	if err := r.validateApprovalResume(state, resume); err != nil {
		return nil, err
	}
	state.pendingApprovalDigest = ""
	state.resumedApproval = nil
	run.PendingApprovalDigest = ""
	return resume, nil
}

func validateApprovalResumeSessionRevision(state *agentState, resume *ApprovalResume) error {
	if state == nil || state.lease == nil || resume == nil || resume.Checkpoint == nil {
		return fmt.Errorf("%w: approval resume has no session-revision authority", ErrOperationPlanChanged)
	}
	handle := state.lease.Handle()
	expected := resume.Checkpoint.ExpectedSessionRevision
	if state.sessionID == "" {
		if expected != 0 || handle.SessionRevision != 0 {
			return fmt.Errorf("%w: stateless approval %s contains a session revision", ErrOperationPlanChanged, resume.ID)
		}
		return nil
	}
	if handle.SessionRevision != expected {
		return fmt.Errorf(
			"%w: approval %s expects session revision %d, current revision is %d",
			ErrOperationPlanChanged,
			resume.ID,
			expected,
			handle.SessionRevision,
		)
	}
	return nil
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
	trailing, failure := active.runtime.failRun(active.baseContext, *active.run, active.state, cause)
	// Stop the lease before any observer delivery: a blocked event sink must
	// not keep the renewal loop or the lease alive past a failed run.
	active.stop()
	active.runtime.emit(trailing)
	return failure
}

func (active *activeRunLease) wait() (*Result, error) {
	active.prepareFinalization()
	result, trailing, err := active.runtime.waitRun(
		active.baseContext, *active.run, active.state,
	)
	if err != nil {
		return nil, active.fail(err)
	}
	// Deliver the waiting-user event after the lease stops for the same reason:
	// a blocked observer must not keep renewing the lease on a paused run.
	active.stop()
	active.runtime.emit(trailing)
	return result, nil
}

// ResumeApproval resumes or polls one durable waiting approval through the same
// fenced Run path. Input must repeat the original RunID, SessionID, user input,
// attachments, metadata, and trusted idempotency fields; ApprovalResumer remains
// the host authority for the current decision.
func (r *Runtime) ResumeApproval(ctx context.Context, input Input) (*Result, error) {
	if strings.TrimSpace(input.RunID) == "" {
		return nil, fmt.Errorf("%w: approval resume requires run id", ErrApprovalRequired)
	}
	return r.Run(ctx, input)
}

func (r *Runtime) Run(ctx context.Context, input Input) (*Result, error) {
	input, err := normalizeRuntimeInputWithResolver(input, r.imageAttachmentResolver)
	if err != nil {
		return nil, err
	}
	if err := r.validateCurrentModelBinding(); err != nil {
		return nil, err
	}
	ctx, identityScope := r.beginIdentityScope(ctx)
	defer r.releaseIdentityScope(identityScope)
	run, state, err := r.beginRuntimeRun(ctx, input)
	if err != nil {
		return nil, err
	}
	// Bind every downstream digest and persistence boundary to the one runtime-
	// selected identity, including when the caller omitted the optional RunID.
	input.RunID = run.ID
	state.pendingApprovalDigest = run.PendingApprovalDigest
	active := r.startActiveRunLease(ctx, &run, state)
	defer active.stop()
	r.emit(Event{Type: EventRunStarted, RunID: run.ID, SessionID: run.SessionID})
	stableState := captureRunState(state)
	approvalResume, err := r.prepareApprovalResume(active.runContext, &run, state, input)
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

	// Tool contracts and instructions are fixed for this Runtime.
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
	result, trailing, err := r.completeRun(ctx, run, state, out)
	if err != nil {
		stableState.restore(state, &run)
		return nil, active.fail(err)
	}
	active.stop()
	r.emit(trailing)
	return result, nil
}

func (r *Runtime) stateFromSession(runID, sessionID string, handle RunHandle, session *SessionState) (*agentState, error) {
	now := r.now()
	return r.stateFromSessionAt(runID, sessionID, handle.LeaseID, handle, session, now, now)
}

func (r *Runtime) stateFromSessionAt(
	runID, sessionID string,
	expectedLeaseID string,
	handle RunHandle,
	session *SessionState,
	createdAt time.Time,
	acceptedAt time.Time,
) (*agentState, error) {
	lease := newLeaseGuard(r.runStore, handle, r.sessionLeaseTTL, r.leaseRenewalInterval, r.cleanupTimeout)
	lease.onRenew = func(renewed RunHandle) {
		r.emit(Event{
			Type: EventSessionLeaseRenewed, RunID: runID, SessionID: sessionID,
			LeaseGeneration: renewed.LeaseGeneration, SessionRevision: renewed.SessionRevision,
		})
	}
	state := &agentState{
		sessionID:              sessionID,
		lease:                  lease,
		seenCallIDs:            make(map[string]struct{}),
		seenResponseIDs:        make(map[string]struct{}),
		seenProviderItemIDs:    make(map[string]struct{}),
		createdAt:              createdAt,
		instructions:           r.baseInstructions,
		persistentInstructions: r.baseInstructions,
	}
	if err := validateRunHandle(runID, sessionID, expectedLeaseID, handle, session, acceptedAt); err != nil {
		return state, err
	}
	if err := validateUTF8Boundary("persisted session", session); err != nil {
		return state, err
	}
	if session == nil {
		if sessionID != "" {
			return state, fmt.Errorf("%w: run store did not return session %q", ErrSessionConflict, sessionID)
		}
		state.sessionReady = true
		return state, nil
	}
	if session.ModelBindingID != r.modelBindingID {
		return state, fmt.Errorf(
			"%w: session %q uses model binding %q, current Runtime uses %q",
			ErrModelBindingMismatch, sessionID, session.ModelBindingID, r.modelBindingID,
		)
	}
	state.loadedOperationSetID = session.OperationSetID
	if session.SkillSetID != r.skillSetID {
		return state, fmt.Errorf(
			"%w: session %q uses SkillSet %q, current Runtime uses %q",
			ErrSkillSetMismatch, sessionID, session.SkillSetID, r.skillSetID,
		)
	}
	if session.OperationSetID != "" && session.OperationSetID != r.operationSetID {
		return state, fmt.Errorf("%w: session %q uses operation set %q, current Runtime uses %q", ErrOperationPlanChanged, sessionID, session.OperationSetID, r.operationSetID)
	}
	if err := validatePersistedModelInputItems(session.Transcript); err != nil {
		return state, fmt.Errorf("%w: session %q transcript is invalid: %v", ErrInvalidModelOutput, sessionID, err)
	}
	if err := validatePersistedSessionReplayShape(session); err != nil {
		return state, fmt.Errorf("%w: session %q replay state is invalid: %v", ErrInvalidModelOutput, sessionID, err)
	}
	state.lastResponseID = session.LastResponseID
	state.transcript = clonePersistentModelInputItems(session.Transcript)
	if err := restoreModelResponseIdentityLedger(state, session.LastResponseID, state.transcript); err != nil {
		return state, fmt.Errorf("%w: session %q response identity is invalid: %v", ErrInvalidModelOutput, sessionID, err)
	}
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
		state.createdAt = createdAt
	}
	state.sessionReady = true
	return state, nil
}

func validatePersistedSessionReplayShape(session *SessionState) error {
	if session == nil {
		return nil
	}
	if session.Revision == 0 && (len(session.Transcript) != 0 || session.Checkpoint != nil ||
		len(session.SeenCallIDs) != 0 || session.LastResponseID != "" || session.LastRunID != "" || session.LastError != "") {
		return errors.New("revision-zero session must contain immutable bindings only")
	}
	if len(session.Transcript) == 0 {
		return nil
	}
	if err := validateContextTranscriptToolSequences(session.Transcript); err != nil {
		return err
	}
	modernResponseAuthority := false
	lastAssistantResponseID := ""
	for _, item := range session.Transcript {
		if item.Type != ModelInputAssistantOutput {
			continue
		}
		lastAssistantResponseID = item.ResponseID
		if item.ResponseID != "" {
			modernResponseAuthority = true
		}
	}
	if modernResponseAuthority &&
		(lastAssistantResponseID == "" || session.LastResponseID != lastAssistantResponseID) {
		return errors.New("modern transcript final response does not match LastResponseID")
	}
	return nil
}

func (r *Runtime) validatePendingApprovalEnvelope(run RunRecord, inputDigest string, resumed ResumedRun) (*PendingApprovalCommit, string, error) {
	digest := resumed.PendingApprovalDigest
	if err := validateApprovalAuthorityDigest(digest); err != nil {
		return nil, "", fmt.Errorf("%w: resumed run has invalid durable approval authority: %v", ErrOperationPlanChanged, err)
	}
	pending := resumed.PendingApproval
	if pending == nil {
		return nil, "", fmt.Errorf("%w: resumed run omitted complete durable approval authority", ErrOperationPlanChanged)
	}
	if err := validateUTF8Boundary("pending approval authority", pending); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
	}
	if pending.AuthorityVersion != pendingApprovalAuthorityVersion {
		return nil, "", fmt.Errorf(
			"%w: pending approval authority version %d is not complete or supported",
			ErrOperationPlanChanged, pending.AuthorityVersion,
		)
	}
	if !pending.Decision.Pending || pending.Decision.Approved || pending.Decision.ID == "" ||
		pending.Decision.ID != strings.TrimSpace(pending.Decision.ID) {
		return nil, "", fmt.Errorf("%w: resumed run has invalid pending approval decision", ErrOperationPlanChanged)
	}
	request := pending.Request
	if request.Operation.RunID != run.ID || request.Operation.SessionID != run.SessionID {
		return nil, "", fmt.Errorf("%w: resumed approval targets another run or session", ErrOperationPlanChanged)
	}
	if request.Checkpoint == nil || request.Checkpoint.InputDigest == "" || request.Checkpoint.InputDigest != inputDigest {
		return nil, "", fmt.Errorf("%w: resumed approval input identity changed", ErrOperationPlanChanged)
	}
	if request.Checkpoint.ModelBindingID != r.modelBindingID || run.ModelBindingID != r.modelBindingID {
		return nil, "", fmt.Errorf("%w: resumed approval model authority changed", ErrModelBindingMismatch)
	}
	if err := validatePersistentApprovalOperationInput(run.Input, request.Operation.Input); err != nil {
		return nil, "", fmt.Errorf("%w: resumed approval operation input changed: %v", ErrOperationPlanChanged, err)
	}
	if err := r.validatePersistentApprovalOperation(request); err != nil {
		return nil, "", err
	}
	if pending.Audit.Type != ItemTypeApproval || pending.Audit.RunID != run.ID ||
		pending.Audit.SessionID != run.SessionID || pending.Audit.CallID != request.Operation.Call.ID ||
		pending.Audit.ExecutionID != request.Operation.ExecutionID || pending.Audit.Name != request.Operation.Operation.Name ||
		pending.Audit.ModelCallID != "" || pending.Audit.ResponseID != "" || pending.Audit.ProviderItemID != "" ||
		pending.Audit.RequestID != "" || pending.Audit.PlanBatch != 0 || pending.Audit.AttemptID != "" ||
		pending.Audit.Error != "" || pending.Audit.CreatedAt.IsZero() {
		return nil, "", fmt.Errorf("%w: resumed approval audit identity changed", ErrOperationPlanChanged)
	}
	if err := requireCanonicalIdentity(pending.Audit.ID, "pending approval audit item id"); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
	}
	expectedAudit, err := json.Marshal(pending.Decision)
	if err != nil || !bytes.Equal(expectedAudit, pending.Audit.Data) {
		return nil, "", fmt.Errorf("%w: resumed approval audit payload changed", ErrOperationPlanChanged)
	}
	expectedDigest, err := pendingApprovalAuthorityDigest(*pending)
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid resumed approval authority: %v", ErrOperationPlanChanged, err)
	}
	legacyDigest, legacyErr := legacyPendingApprovalAuthorityDigest(*pending)
	digestMatches := digest == expectedDigest || (legacyErr == nil && digest == legacyDigest)
	if pending.Digest != digest || !digestMatches {
		return nil, "", fmt.Errorf("%w: resumed approval digest does not match its complete authority", ErrOperationPlanChanged)
	}
	cloned, err := r.clonePersistentPendingApprovalCommit(*pending)
	if err != nil {
		return nil, "", fmt.Errorf("%w: clone resumed approval authority: %v", ErrOperationPlanChanged, err)
	}
	return &cloned, digest, nil
}

func validatePersistentApprovalOperationInput(current, persisted Input) error {
	if !isNilDependency(persisted.ImageAttachmentResolver) || persisted.TrustedContext != "" {
		return errors.New("persistent input contains transient host authority")
	}
	for index, attachment := range persisted.Attachments {
		if attachment.URL != "" || attachment.CurrentRun {
			return fmt.Errorf("persistent input attachment %d contains transient authority", index)
		}
	}
	left, err := clonePersistentOperationInput(current)
	if err != nil {
		return err
	}
	right, err := clonePersistentOperationInput(persisted)
	if err != nil {
		return err
	}
	if left.RunID != right.RunID || left.IdempotencyScope != right.IdempotencyScope {
		return errors.New("persistent input identity fields differ")
	}
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return err
	}
	rightJSON, err := json.Marshal(right)
	if err != nil || !jsonSemanticallyEqual(leftJSON, rightJSON) {
		return errors.New("persistent input payload differs")
	}
	return nil
}

func (r *Runtime) validatePersistentApprovalOperation(request ApprovalRequest) error {
	operationRequest := request.Operation
	if operationRequest.Input.RunID != operationRequest.RunID ||
		operationRequest.Input.SessionID != operationRequest.SessionID || operationRequest.AttemptID != "" ||
		operationRequest.Call.Name != operationRequest.Operation.Name ||
		request.Reason != strings.TrimSpace(request.Reason) {
		return fmt.Errorf("%w: resumed approval request identity changed", ErrOperationPlanChanged)
	}
	if operationRequest.SessionID == "" {
		if operationRequest.SessionLease != (SessionLeaseFence{}) {
			return fmt.Errorf("%w: stateless approval contains session lease authority", ErrOperationPlanChanged)
		}
	} else {
		lease := operationRequest.SessionLease
		if lease.RunID != operationRequest.RunID || lease.SessionID != operationRequest.SessionID ||
			lease.LeaseID == "" || lease.Generation == 0 || lease.Deadline.IsZero() ||
			request.Checkpoint == nil || lease.SessionRevision > request.Checkpoint.ExpectedSessionRevision {
			return fmt.Errorf("%w: resumed approval contains invalid originating lease authority", ErrOperationPlanChanged)
		}
		if err := requireCanonicalIdentity(lease.LeaseID, "pending approval originating lease id"); err != nil {
			return fmt.Errorf("%w: %v", ErrOperationPlanChanged, err)
		}
	}
	registered, ok := r.operations.Get(operationRequest.Operation.Name)
	if !ok || !equalApprovalOperationSummary(operationRequest.Operation, operationSummary(registered)) {
		return fmt.Errorf("%w: resumed approval operation contract changed", ErrOperationPlanChanged)
	}
	normalizedArguments, err := normalizeExactJSONHostValue(
		"pending approval normalized arguments", operationRequest.Arguments,
	)
	if err != nil {
		return fmt.Errorf("%w: resumed approval arguments are not persistent: %v", ErrOperationPlanChanged, err)
	}
	arguments, err := json.Marshal(normalizedArguments)
	if err != nil {
		return fmt.Errorf("%w: resumed approval arguments are not persistent: %v", ErrOperationPlanChanged, err)
	}
	decoded, err := r.operations.DecodeInput(operationRequest.Call.Name, arguments)
	if err != nil {
		return fmt.Errorf("%w: resumed approval arguments no longer validate: %v", ErrOperationPlanChanged, err)
	}
	decodedJSON, err := json.Marshal(decoded)
	if err != nil || !jsonSemanticallyEqual(arguments, decodedJSON) {
		return fmt.Errorf("%w: resumed approval normalized arguments changed", ErrOperationPlanChanged)
	}
	return nil
}

func equalApprovalOperationSummary(left, right OperationSummary) bool {
	if left.Name != right.Name || left.ContractID != right.ContractID || left.Description != right.Description ||
		left.Effect != right.Effect || left.Confirmation != right.Confirmation || left.Terminal != right.Terminal ||
		left.TerminalBatchLimit != right.TerminalBatchLimit || !equalStringSlices(left.PreviousNames, right.PreviousNames) ||
		!equalStringSlices(left.Capabilities, right.Capabilities) {
		return false
	}
	return jsonSemanticallyEqual(left.InputSchema, right.InputSchema) &&
		jsonSemanticallyEqual(left.OutputSchema, right.OutputSchema)
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (r *Runtime) validatePendingApprovalStartAuthority(state *agentState, pending *PendingApprovalCommit) error {
	if state == nil || pending == nil {
		return fmt.Errorf("%w: resumed run has no complete approval authority", ErrOperationPlanChanged)
	}
	resume := approvalResumeFromPending(pending)
	if _, err := r.validateApprovalResumeImmutablePayload(state, resume); err != nil {
		return err
	}
	if err := validateApprovalResumeSessionRevision(state, resume); err != nil {
		return err
	}
	return nil
}

func approvalResumeFromPending(pending *PendingApprovalCommit) *ApprovalResume {
	if pending == nil {
		return nil
	}
	request := pending.Request
	return &ApprovalResume{
		ID: pending.Decision.ID, ExecutionID: request.Operation.ExecutionID,
		Operation: request.Operation.Operation.Name, ContractID: request.Operation.Operation.ContractID,
		Call: request.Operation.Call, ResponseID: request.ResponseID,
		ModelOutput: cloneModelOutputItems(request.ModelOutput),
		Preview:     append(json.RawMessage(nil), request.Preview...),
		Checkpoint:  cloneApprovalCheckpoint(request.Checkpoint, true),
		Pending:     true, Reason: pending.Decision.Reason,
	}
}

func isUnambiguousRunNotFound(err error) bool {
	// Creation authority is intentionally narrower than errors.Is matching:
	// joined, custom-Is, wrapped cancellation/conflict, invalid-UTF8 wrappers,
	// and cleanup failures must never be downgraded to read-only absence.
	return err == ErrRunNotFound
}

func validateRunHandle(
	runID, sessionID, expectedLeaseID string,
	handle RunHandle,
	session *SessionState,
	acceptedAt time.Time,
) error {
	if handle.RunID != runID {
		return fmt.Errorf("agent: run store returned handle for run %q, want %q", handle.RunID, runID)
	}
	if handle.SessionID != sessionID {
		return fmt.Errorf("agent: run store returned handle for session %q, want %q", handle.SessionID, sessionID)
	}
	if sessionID == "" {
		if expectedLeaseID != "" || handle.LeaseID != "" || handle.LeaseGeneration != 0 ||
			!handle.LeaseDeadline.IsZero() || handle.SessionRevision != 0 {
			return errors.New("agent: stateless run store returned session lease authority")
		}
		if session != nil {
			return errors.New("agent: stateless run store returned session state")
		}
		return nil
	}
	if err := requireCanonicalIdentity(handle.LeaseID, "run store session lease id"); err != nil {
		return err
	}
	if handle.LeaseID != expectedLeaseID {
		return fmt.Errorf("agent: run store returned lease %q, want %q", handle.LeaseID, expectedLeaseID)
	}
	if handle.LeaseGeneration == 0 || handle.LeaseDeadline.IsZero() || !handle.LeaseDeadline.After(acceptedAt) {
		return errors.New("agent: run store returned an invalid or expired session lease fence")
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
	if session != nil && handle.SessionRevision == ^uint64(0) {
		return errors.New("agent: run store returned a session revision that cannot advance")
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
	retainedCallIDs := transcriptCallIDs(state.transcript)
	if hasSavedCallIDs {
		for callID := range retainedCallIDs {
			if _, exists := state.seenCallIDs[callID]; !exists {
				return fmt.Errorf("%w: saved function call ids omit transcript call id %q", ErrInvalidModelOutput, callID)
			}
		}
	}
	resultCallIDs := make(map[string]struct{})
	for _, item := range state.transcript {
		if item.Type != ModelInputToolResult {
			continue
		}
		callID := item.CallID
		if err := requireCanonicalIdentity(callID, "session transcript function call id"); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidModelOutput, err)
		}
		if _, duplicate := resultCallIDs[callID]; duplicate {
			return fmt.Errorf("%w: session transcript contains duplicate function call id %q", ErrInvalidModelOutput, callID)
		}
		resultCallIDs[callID] = struct{}{}
	}
	// The retained assistant calls are authoritative. Saved supersets from
	// legacy snapshots are pruned so duplicate tracking cannot grow beyond the
	// replay context, while saved omissions remain an explicit corruption error.
	state.seenCallIDs = retainedCallIDs
	return nil
}
