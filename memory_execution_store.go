package agentruntime

import (
	"context"
	"fmt"
)

func (store *InMemoryStore) ReservePlanBatch(ctx context.Context, batch OperationPlanBatch) (PlanBatchReservation, error) {
	if err := batch.Validate(); err != nil {
		return PlanBatchReservation{}, err
	}
	batch = cloneStoredOperationPlanBatch(batch)
	unlock, err := store.lock(ctx)
	if err != nil {
		return PlanBatchReservation{}, err
	}
	defer unlock()
	batches := store.plans[batch.RequestID]
	if existing, ok := batches[batch.Index]; ok {
		if !equalOperationPlanBatch(existing, batch) {
			return PlanBatchReservation{}, fmt.Errorf("%w: request %s batch %d changed", ErrOperationPlanChanged, batch.RequestID, batch.Index)
		}
		return PlanBatchReservation{Batch: cloneStoredOperationPlanBatch(existing)}, nil
	}
	if _, sealed := store.seals[batch.RequestID]; sealed {
		return PlanBatchReservation{}, fmt.Errorf("%w: request %s is sealed", ErrOperationPlanChanged, batch.RequestID)
	}
	if batch.Index != uint64(len(batches)) {
		return PlanBatchReservation{}, fmt.Errorf("%w: request %s batch index %d is not contiguous", ErrOperationPlanChanged, batch.RequestID, batch.Index)
	}
	for _, existing := range batches {
		if !samePlanAuthority(existing.SessionID, existing.IdempotencyKey, existing.IdempotencyScope,
			batch.SessionID, batch.IdempotencyKey, batch.IdempotencyScope) {
			return PlanBatchReservation{}, fmt.Errorf("%w: request %s authority changed", ErrOperationPlanChanged, batch.RequestID)
		}
		if batch.CreatedAt.Before(existing.CreatedAt) {
			return PlanBatchReservation{}, fmt.Errorf("%w: request %s batch timestamp moved backward", ErrOperationPlanChanged, batch.RequestID)
		}
	}
	for requestID, existingBatches := range store.plans {
		for index, existing := range existingBatches {
			for _, existingStep := range existing.Steps {
				for _, step := range batch.Steps {
					if existingStep.ExecutionID == step.ExecutionID {
						return PlanBatchReservation{}, fmt.Errorf(
							"%w: execution id %q is already assigned to request %s batch %d",
							ErrIdentityConflict, step.ExecutionID, requestID, index,
						)
					}
				}
			}
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return PlanBatchReservation{}, cause
	}
	if batches == nil {
		batches = make(map[uint64]OperationPlanBatch)
		store.plans[batch.RequestID] = batches
	}
	batches[batch.Index] = batch
	return PlanBatchReservation{Batch: cloneStoredOperationPlanBatch(batch), Created: true}, nil
}

func (store *InMemoryStore) SealPlan(ctx context.Context, seal OperationPlanSeal) (PlanSealResult, error) {
	if err := seal.Validate(); err != nil {
		return PlanSealResult{}, err
	}
	unlock, err := store.lock(ctx)
	if err != nil {
		return PlanSealResult{}, err
	}
	defer unlock()
	if existing, ok := store.seals[seal.RequestID]; ok {
		if !equalOperationPlanSeal(existing, seal) {
			return PlanSealResult{}, fmt.Errorf("%w: request %s seal changed", ErrOperationPlanChanged, seal.RequestID)
		}
		return PlanSealResult{Seal: existing}, nil
	}
	batches := store.plans[seal.RequestID]
	if uint64(len(batches)) != seal.BatchCount {
		return PlanSealResult{}, fmt.Errorf("%w: request %s recorded %d batch(es), observed %d", ErrOperationPlanChanged, seal.RequestID, len(batches), seal.BatchCount)
	}
	for index := uint64(0); index < seal.BatchCount; index++ {
		batch, ok := batches[index]
		if !ok {
			return PlanSealResult{}, fmt.Errorf("%w: request %s is missing batch %d", ErrOperationPlanChanged, seal.RequestID, index)
		}
		if !samePlanAuthority(batch.SessionID, batch.IdempotencyKey, batch.IdempotencyScope,
			seal.SessionID, seal.IdempotencyKey, seal.IdempotencyScope) {
			return PlanSealResult{}, fmt.Errorf("%w: request %s seal authority changed", ErrOperationPlanChanged, seal.RequestID)
		}
		if seal.SealedAt.Before(batch.CreatedAt) {
			return PlanSealResult{}, fmt.Errorf("%w: request %s seal timestamp precedes batch %d", ErrOperationPlanChanged, seal.RequestID, index)
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return PlanSealResult{}, cause
	}
	store.seals[seal.RequestID] = seal
	return PlanSealResult{Seal: seal, Created: true}, nil
}

func samePlanAuthority(leftSession, leftKey, leftScope, rightSession, rightKey, rightScope string) bool {
	return leftSession == rightSession && leftKey == rightKey && leftScope == rightScope
}

func (store *InMemoryStore) AcquireExecution(ctx context.Context, request AcquireExecutionRequest) (AcquireExecutionResult, error) {
	if err := request.Validate(); err != nil {
		return AcquireExecutionResult{}, err
	}
	request.Execution = cloneStoredOperationExecution(request.Execution)
	request.Transition = cloneStoredOperationTransition(request.Transition)
	unlock, err := store.lock(ctx)
	if err != nil {
		return AcquireExecutionResult{}, err
	}
	defer unlock()
	if err := store.validateExecutionPlanLocked(request.Execution); err != nil {
		return AcquireExecutionResult{}, err
	}
	execution := request.Execution
	if existing, ok := store.executions[execution.ID]; ok {
		if existing.IdempotencyKey != execution.IdempotencyKey || existing.IdempotencyScope != execution.IdempotencyScope ||
			existing.SessionID != execution.SessionID || existing.Name != execution.Name ||
			existing.ContractID != execution.ContractID || existing.VerificationRequired != execution.VerificationRequired ||
			!jsonSemanticallyEqual(existing.Arguments, execution.Arguments) {
			return AcquireExecutionResult{}, ErrOperationPlanChanged
		}
		if existing.Status == OperationExecutionRetryable {
			if store.executionAttemptIdentityExistsLocked(execution.ID, execution.AttemptID) {
				return AcquireExecutionResult{}, fmt.Errorf("%w: attempt id %q is already assigned to execution %q", ErrIdentityConflict, execution.AttemptID, execution.ID)
			}
			if _, exists := store.transitionIDs[request.Transition.ID]; exists {
				return AcquireExecutionResult{}, fmt.Errorf("%w: transition id %q is already assigned", ErrIdentityConflict, request.Transition.ID)
			}
			transition := request.Transition
			transition.From = OperationExecutionRetryable
			transition.To = OperationExecutionStarted
			if err := transition.Validate(); err != nil {
				return AcquireExecutionResult{}, err
			}
			if execution.UpdatedAt.Before(existing.UpdatedAt) {
				return AcquireExecutionResult{}, fmt.Errorf(
					"%w: retry acquisition timestamp precedes current execution state",
					ErrInvalidExecutionTransition,
				)
			}
			existing.Status = OperationExecutionStarted
			existing.RunID = execution.RunID
			existing.CallID = execution.CallID
			existing.AttemptID = execution.AttemptID
			existing.Error = ""
			existing.Result = OperationResult{}
			existing.Verification = nil
			existing.UpdatedAt = execution.UpdatedAt
			if cause := context.Cause(ctx); cause != nil {
				return AcquireExecutionResult{}, cause
			}
			store.executions[execution.ID] = existing
			store.transitions[execution.ID] = append(store.transitions[execution.ID], transition)
			store.transitionIDs[transition.ID] = execution.ID
			return AcquireExecutionResult{Execution: cloneStoredOperationExecution(existing), Disposition: ExecutionAcquired}, nil
		}
		disposition := ExecutionBlocked
		if existing.Status == OperationExecutionExecuted || existing.Status == OperationExecutionCompleted {
			disposition = ExecutionReplay
		}
		return AcquireExecutionResult{Execution: cloneStoredOperationExecution(existing), Disposition: disposition}, nil
	}
	if _, exists := store.transitionIDs[request.Transition.ID]; exists {
		return AcquireExecutionResult{}, fmt.Errorf("%w: transition id %q is already assigned", ErrIdentityConflict, request.Transition.ID)
	}
	if cause := context.Cause(ctx); cause != nil {
		return AcquireExecutionResult{}, cause
	}
	store.executions[execution.ID] = execution
	store.transitions[execution.ID] = append(store.transitions[execution.ID], request.Transition)
	store.transitionIDs[request.Transition.ID] = execution.ID
	return AcquireExecutionResult{Execution: cloneStoredOperationExecution(execution), Disposition: ExecutionAcquired}, nil
}

func (store *InMemoryStore) validateExecutionPlanLocked(execution OperationExecutionRecord) error {
	found := false
	for requestID, batches := range store.plans {
		seal, sealed := store.seals[requestID]
		for _, batch := range batches {
			for _, step := range batch.Steps {
				if step.ExecutionID != execution.ID {
					continue
				}
				if found {
					return fmt.Errorf("%w: execution %s appears in multiple plans", ErrOperationPlanChanged, execution.ID)
				}
				found = true
				sealAuthorityMatches := !sealed ||
					(seal.SessionID == batch.SessionID && seal.IdempotencyKey == batch.IdempotencyKey &&
						seal.IdempotencyScope == batch.IdempotencyScope)
				if !samePlanAuthority(batch.SessionID, batch.IdempotencyKey, batch.IdempotencyScope,
					execution.SessionID, execution.IdempotencyKey, execution.IdempotencyScope) ||
					!sealAuthorityMatches || step.Name != execution.Name ||
					step.ContractID != execution.ContractID || !jsonSemanticallyEqual(step.Arguments, execution.Arguments) {
					return fmt.Errorf(
						"%w: execution %s differs from its reserved plan (authority=%t seal_authority=%t name=%t contract=%t arguments=%t)",
						ErrOperationPlanChanged, execution.ID,
						samePlanAuthority(batch.SessionID, batch.IdempotencyKey, batch.IdempotencyScope,
							execution.SessionID, execution.IdempotencyKey, execution.IdempotencyScope),
						sealAuthorityMatches,
						step.Name == execution.Name, step.ContractID == execution.ContractID,
						jsonSemanticallyEqual(step.Arguments, execution.Arguments),
					)
				}
			}
		}
	}
	if !found {
		return fmt.Errorf("%w: execution %s is not present in a reserved plan", ErrOperationPlanChanged, execution.ID)
	}
	return nil
}

func (store *InMemoryStore) ValidateExecutionAttempt(ctx context.Context, executionID, attemptID string) error {
	unlock, err := store.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	execution, ok := store.executions[executionID]
	if !ok {
		return ErrOperationExecutionNotFound
	}
	if execution.Status != OperationExecutionStarted || execution.AttemptID != attemptID {
		return ErrOperationAttemptLost
	}
	return nil
}

func (store *InMemoryStore) TransitionExecution(ctx context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error) {
	if err := transition.Validate(); err != nil {
		return OperationExecutionRecord{}, err
	}
	transition = cloneStoredOperationTransition(transition)
	unlock, err := store.lock(ctx)
	if err != nil {
		return OperationExecutionRecord{}, err
	}
	defer unlock()
	execution, ok := store.executions[transition.ExecutionID]
	if !ok {
		return OperationExecutionRecord{}, ErrOperationExecutionNotFound
	}
	if execution.AttemptID != transition.AttemptID || execution.Status != transition.From {
		return OperationExecutionRecord{}, ErrOperationAttemptLost
	}
	if execution.RunID != transition.RunID || execution.CallID != transition.CallID ||
		execution.VerificationRequired != transition.VerificationRequired || transition.To == OperationExecutionStarted {
		return OperationExecutionRecord{}, ErrInvalidExecutionTransition
	}
	if transition.CreatedAt.Before(execution.UpdatedAt) {
		return OperationExecutionRecord{}, fmt.Errorf(
			"%w: transition timestamp precedes current execution state",
			ErrInvalidExecutionTransition,
		)
	}
	if _, exists := store.transitionIDs[transition.ID]; exists {
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
		if transition.Verification != nil {
			verification := cloneVerificationResult(*transition.Verification)
			execution.Verification = &verification
		} else {
			execution.Verification = nil
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return OperationExecutionRecord{}, cause
	}
	store.executions[execution.ID] = execution
	store.transitions[execution.ID] = append(store.transitions[execution.ID], transition)
	store.transitionIDs[transition.ID] = execution.ID
	return cloneStoredOperationExecution(execution), nil
}

func (store *InMemoryStore) GetExecution(ctx context.Context, executionID string) (OperationExecutionRecord, error) {
	unlock, err := store.lock(ctx)
	if err != nil {
		return OperationExecutionRecord{}, err
	}
	defer unlock()
	execution, ok := store.executions[executionID]
	if !ok {
		return OperationExecutionRecord{}, ErrOperationExecutionNotFound
	}
	return cloneStoredOperationExecution(execution), nil
}

func (store *InMemoryStore) ListExecutionTransitions(ctx context.Context, executionID string) ([]OperationExecutionTransition, error) {
	unlock, err := store.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, ok := store.executions[executionID]; !ok {
		return nil, ErrOperationExecutionNotFound
	}
	history := store.transitions[executionID]
	out := make([]OperationExecutionTransition, len(history))
	for index := range history {
		out[index] = cloneStoredOperationTransition(history[index])
	}
	return out, nil
}

func (store *InMemoryStore) executionAttemptIdentityExistsLocked(executionID, attemptID string) bool {
	for _, transition := range store.transitions[executionID] {
		if transition.AttemptID == attemptID {
			return true
		}
	}
	return false
}

var _ ExecutionStore = (*InMemoryStore)(nil)
