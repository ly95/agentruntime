package agentruntime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

func (r *Runtime) validateApprovalResume(state *agentState, resume *ApprovalResume) error {
	if resume == nil || resume.Pending || strings.TrimSpace(resume.ID) == "" || strings.TrimSpace(resume.ExecutionID) == "" ||
		strings.TrimSpace(resume.Operation) == "" || strings.TrimSpace(resume.ResponseID) == "" {
		return fmt.Errorf("%w: incomplete approval resume state", ErrInvalidModelOutput)
	}
	if _, err := appendModelOutputItems(nil, resume.ModelOutput); err != nil {
		return fmt.Errorf("%w: approval %s has invalid model output: %v", ErrOperationPlanChanged, resume.ID, err)
	}
	calls, err := responseToolCalls(&ModelResponse{Items: cloneModelOutputItems(resume.ModelOutput)}, state.seenCallIDs)
	if err != nil {
		return fmt.Errorf("%w: approval %s has invalid function call: %v", ErrOperationPlanChanged, resume.ID, err)
	}
	if len(calls) != 1 || calls[0].ID != resume.Call.ID || calls[0].Name != resume.Operation ||
		!jsonSemanticallyEqual(calls[0].Input, resume.Call.Input) {
		return fmt.Errorf("%w: approval %s does not match one persisted function call", ErrOperationPlanChanged, resume.ID)
	}
	operation, ok := r.operations.Get(calls[0].Name)
	if !ok || operation.Effect != OperationEffectWrite {
		return fmt.Errorf("%w: approval %s targets unavailable or non-write operation %q", ErrOperationPlanChanged, resume.ID, calls[0].Name)
	}
	return nil
}

type leaseGuard struct {
	store           RunStore
	handleMu        sync.RWMutex
	handle          RunHandle
	ttl             time.Duration
	renewalInterval time.Duration
	cancelGrace     time.Duration
	finalizeGrace   time.Duration
}

func newLeaseGuard(store RunStore, handle RunHandle, ttl, renewalInterval, cancelGrace time.Duration) *leaseGuard {
	const maxDuration = time.Duration(1<<63 - 1)
	finalizeGrace := maxDuration
	if cancelGrace <= maxDuration/3 {
		finalizeGrace = 3 * cancelGrace
	}
	return &leaseGuard{
		store: store, handle: handle, ttl: ttl,
		renewalInterval: renewalInterval, cancelGrace: cancelGrace, finalizeGrace: finalizeGrace,
	}
}

func (guard *leaseGuard) Handle() RunHandle {
	guard.handleMu.RLock()
	defer guard.handleMu.RUnlock()
	return guard.handle
}

func (guard *leaseGuard) Fence() SessionLeaseFence {
	return sessionLeaseFence(guard.Handle())
}

func (guard *leaseGuard) replace(next RunHandle) error {
	guard.handleMu.Lock()
	defer guard.handleMu.Unlock()
	current := guard.handle
	if current.RunID != next.RunID || current.SessionID != next.SessionID ||
		current.LeaseID != next.LeaseID || current.LeaseGeneration != next.LeaseGeneration ||
		current.SessionRevision != next.SessionRevision || next.LeaseDeadline.IsZero() {
		return fmt.Errorf("%w: run %s lease fence changed during renewal", ErrSessionLeaseLost, current.RunID)
	}
	if next.LeaseDeadline.Before(current.LeaseDeadline) {
		// A concurrent validation can return the store snapshot from just before
		// a newer heartbeat; never regress the locally observed deadline.
		next.LeaseDeadline = current.LeaseDeadline
	}
	guard.handle = next
	return nil
}

func (guard *leaseGuard) Start(ctx context.Context) (context.Context, func(), func() error) {
	runCtx, cancelRun := context.WithCancelCause(ctx)
	if guard.Handle().SessionID == "" {
		var once sync.Once
		return runCtx, func() {}, func() error {
			once.Do(func() { cancelRun(nil) })
			return nil
		}
	}
	// Keep renewal independent from run cancellation. Cancellation drives the
	// control loop into detached failure cleanup, which still needs a live lease
	// while it flushes audit events and commits FinishRun. The grace timer keeps
	// a dependency that ignores cancellation from retaining the lease forever.
	renewCtx, cancelRenew := context.WithCancel(context.WithoutCancel(ctx))
	stopGrace := make(chan struct{})
	refreshGrace := make(chan time.Duration, 1)
	graceDone := guard.startCancellationGrace(runCtx, cancelRenew, refreshGrace, stopGrace)
	done := make(chan struct{})
	failure := make(chan error, 1)
	go guard.renewLoop(renewCtx, cancelRun, done, failure)
	prepareFinalization := func() {
		refreshLeaseGrace(refreshGrace, graceDone, guard.finalizeGrace)
	}
	return runCtx, prepareFinalization, guard.renewalStopper(
		cancelRun, cancelRenew, stopGrace, done, graceDone, failure,
	)
}

func (guard *leaseGuard) startCancellationGrace(
	runCtx context.Context,
	cancelRenew context.CancelFunc,
	refreshGrace <-chan time.Duration,
	stopGrace <-chan struct{},
) <-chan struct{} {
	graceDone := make(chan struct{})
	go func() {
		defer close(graceDone)
		grace := guard.cancelGrace
		for {
			select {
			case grace = <-refreshGrace:
				continue
			case <-runCtx.Done():
				timer := time.NewTimer(grace)
				defer timer.Stop()
				for {
					select {
					case next := <-refreshGrace:
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						timer.Reset(next)
					case <-timer.C:
						cancelRenew()
						return
					case <-stopGrace:
						return
					}
				}
			case <-stopGrace:
				return
			}
		}
	}()
	return graceDone
}

func (guard *leaseGuard) renewLoop(
	renewCtx context.Context,
	cancelRun context.CancelCauseFunc,
	done chan<- struct{},
	failure chan<- error,
) {
	defer close(done)
	ticker := time.NewTicker(guard.renewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-renewCtx.Done():
			return
		case <-ticker.C:
			if err := guard.renewLease(renewCtx); err != nil {
				if renewCtx.Err() != nil {
					return
				}
				failure <- err
				cancelRun(err)
				return
			}
		}
	}
}

func (guard *leaseGuard) renewLease(ctx context.Context) error {
	handle := guard.Handle()
	renewed, err := guard.store.RenewRunLease(ctx, RenewRunLeaseRequest{
		Handle: handle, LeaseTTL: guard.ttl,
	})
	if err == nil && !renewed.LeaseDeadline.After(handle.LeaseDeadline) {
		err = fmt.Errorf("agent: run store did not extend lease deadline")
	}
	if err == nil {
		err = guard.replace(renewed)
	}
	if err != nil {
		return fmt.Errorf("%w: renew session %s: %w", ErrSessionLeaseLost, handle.SessionID, err)
	}
	return nil
}

func refreshLeaseGrace(refresh chan time.Duration, done <-chan struct{}, grace time.Duration) {
	select {
	case refresh <- grace:
		return
	default:
	}
	select {
	case <-refresh:
	default:
	}
	select {
	case refresh <- grace:
	case <-done:
	}
}

func (guard *leaseGuard) renewalStopper(
	cancelRun context.CancelCauseFunc,
	cancelRenew context.CancelFunc,
	stopGrace chan struct{},
	done <-chan struct{},
	graceDone <-chan struct{},
	failure <-chan error,
) func() error {
	var once sync.Once
	var renewalErr error
	return func() error {
		once.Do(func() {
			close(stopGrace)
			cancelRenew()
			<-done
			<-graceDone
			select {
			case renewalErr = <-failure:
			default:
			}
			cancelRun(nil)
		})
		return renewalErr
	}
}

func (guard *leaseGuard) Validate(ctx context.Context) (SessionLeaseFence, error) {
	handle := guard.Handle()
	if handle.SessionID == "" {
		return SessionLeaseFence{}, nil
	}
	validated, err := guard.store.ValidateRunLease(ctx, handle)
	if err != nil {
		return SessionLeaseFence{}, fmt.Errorf("%w: validate session %s: %w", ErrSessionLeaseLost, handle.SessionID, err)
	}
	if err := guard.replace(validated); err != nil {
		return SessionLeaseFence{}, err
	}
	return guard.Fence(), nil
}

func (r *Runtime) runAgent(ctx context.Context, run *RunRecord, input Input, state *agentState, approvalResume *ApprovalResume) (string, error) {
	if trusted := strings.TrimSpace(input.TrustedContext); trusted != "" {
		state.instructions = appendTrustedHostContext(state.instructions, trusted)
	}
	transcript, terminalResponse, err := r.initializeAgentTranscript(
		ctx, run, input, state, approvalResume,
	)
	if err != nil || terminalResponse != "" {
		return terminalResponse, err
	}
	lastModelCallID := ""
	for iteration := 0; iteration < r.maxIterations; iteration++ {
		resp, modelCallID, compactedTranscript, err := r.completeAgentModel(
			ctx, run, state, transcript,
		)
		if err != nil {
			return "", err
		}
		transcript = compactedTranscript
		lastModelCallID = modelCallID
		var done bool
		transcript, terminalResponse, done, err = r.handleAgentResponse(
			ctx, run, input, state, transcript, resp, modelCallID,
		)
		if err != nil {
			return "", err
		}
		if done {
			return terminalResponse, nil
		}
	}
	return "", correlateModelCallError(lastModelCallID, fmt.Errorf("%w: %d", ErrMaxIterations, r.maxIterations))
}

func (r *Runtime) initializeAgentTranscript(
	ctx context.Context,
	run *RunRecord,
	input Input,
	state *agentState,
	approvalResume *ApprovalResume,
) ([]ModelInputItem, string, error) {
	transcript := cloneModelInputItems(state.transcript)
	if approvalResume == nil {
		transcript = append(transcript, ModelInputItem{
			Type: ModelInputUserMessage, Text: input.User,
			Attachments: cloneModelInputAttachments(input.Attachments),
		})
		return transcript, "", nil
	}
	transcript, terminalResponse, err := r.resumeApprovedOperation(
		ctx, run, input, state, transcript, approvalResume,
	)
	if err != nil || terminalResponse == "" {
		return transcript, terminalResponse, err
	}
	if err := r.sealOperationPlan(ctx, run, input, state); err != nil {
		return nil, "", err
	}
	state.transcript = cloneModelInputItems(transcript)
	return transcript, terminalResponse, nil
}

func (r *Runtime) completeAgentModel(
	ctx context.Context,
	run *RunRecord,
	state *agentState,
	transcript []ModelInputItem,
) (*ModelResponse, string, []ModelInputItem, error) {
	request, transcript, err := r.prepareModelRequest(
		ctx, run, state, transcript, modelRequestOptions{},
	)
	if err != nil {
		return nil, "", nil, err
	}
	response, modelCallID, err := r.completeModel(ctx, run.ID, run.SessionID, request)
	if err != nil || !reasoningOnlyResponse(response) {
		return response, modelCallID, transcript, err
	}
	retryRequest, transcript, err := r.prepareModelRequest(ctx, run, state, transcript, modelRequestOptions{
		instructionsSuffix: reasoningOnlyCorrection,
		disableReasoning:   true,
	})
	if err != nil {
		return nil, "", nil, err
	}
	response, modelCallID, err = r.completeModel(ctx, run.ID, run.SessionID, retryRequest)
	return response, modelCallID, transcript, err
}

func (r *Runtime) handleAgentResponse(
	ctx context.Context,
	run *RunRecord,
	input Input,
	state *agentState,
	transcript []ModelInputItem,
	response *ModelResponse,
	modelCallID string,
) ([]ModelInputItem, string, bool, error) {
	if strings.TrimSpace(response.ID) == "" {
		err := fmt.Errorf("%w: model response id is required for continuation", ErrInvalidModelOutput)
		return nil, "", false, correlateModelCallError(modelCallID, err)
	}
	state.lastResponseID = response.ID
	transcript, err := appendModelOutputItems(transcript, response.Items)
	if err != nil {
		return nil, "", false, correlateModelCallError(modelCallID, err)
	}
	calls, err := responseToolCalls(response, state.seenCallIDs)
	if err != nil {
		return nil, "", false, correlateModelCallError(modelCallID, err)
	}
	for _, call := range calls {
		state.seenCallIDs[call.ID] = struct{}{}
	}
	if len(calls) == 0 {
		output, err := modelResponseText(response)
		if err == nil {
			err = r.sealOperationPlan(ctx, run, input, state)
		}
		if err != nil {
			return nil, "", false, correlateModelCallError(modelCallID, err)
		}
		state.transcript = cloneModelInputItems(transcript)
		return transcript, output, true, nil
	}
	results, terminalResponse, err := r.executeCalls(
		ctx, run, input, state, calls, response.ID, response.Items,
	)
	if err != nil {
		return nil, "", false, correlateModelCallError(modelCallID, err)
	}
	transcript = append(transcript, results...)
	if terminalResponse == "" {
		return transcript, "", false, nil
	}
	if err := r.sealOperationPlan(ctx, run, input, state); err != nil {
		return nil, "", false, correlateModelCallError(modelCallID, err)
	}
	state.transcript = cloneModelInputItems(transcript)
	return transcript, terminalResponse, true, nil
}

func modelResponseText(response *ModelResponse) (string, error) {
	output := strings.TrimSpace(response.OutputText)
	if output == "" {
		output = strings.TrimSpace(response.Refusal)
	}
	if output == "" {
		return "", fmt.Errorf(
			"%w: response has neither output text, refusal, nor function calls (finish_reason=%q)",
			ErrInvalidModelOutput, response.FinishReason,
		)
	}
	return output, nil
}

func appendTrustedHostContext(instructions, trusted string) string {
	var out strings.Builder
	out.Grow(len(instructions) + len(trusted) + 256)
	out.WriteString(instructions)
	out.WriteString("\n\n<trusted_host_context>\n")
	out.WriteString("The following JSON is authoritative current host state, not user-authored text. Terminal modification-proposal states override earlier conversational assumptions. Temporary attachment expiry timestamps are authoritative; never claim an expired attachment is still usable. Never ask the user to apply an already terminal proposal. Internal protocol labels and operation names must never appear in user-facing text; refer to reviewable writes as modification proposals (修改方案).\n")
	out.WriteString(trusted)
	out.WriteString("\n</trusted_host_context>")
	return out.String()
}
