package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type recordingStore struct {
	mu               sync.Mutex
	runs             []RunRecord
	items            []ItemRecord
	sessions         map[string]SessionState
	leases           map[string]RunHandle
	leaseGenerations map[string]uint64
	executions       map[string]OperationExecutionRecord
	transitions      map[string][]OperationExecutionTransition
	plans            map[string]map[uint64]OperationPlanBatch
	seals            map[string]OperationPlanSeal
	completed        []RunRecord
	failed           []RunRecord
	now              func() time.Time
}

func (s *recordingStore) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

type appendFailingStore struct {
	recordingStore
	failType ItemType
	err      error
}

type nthItemFailingStore struct {
	recordingStore
	failType ItemType
	failAt   int
	seen     int
	err      error
}

type mutatingAppendStore struct {
	recordingStore
}

type mutatingBeginStore struct {
	recordingStore
}

type mismatchedRunHandleStore struct {
	recordingStore
}

type hiddenBeginSessionStore struct {
	recordingStore
}

type completeFailingStore struct {
	recordingStore
	err error
}

type cancellationAwareStore struct {
	recordingStore
	unknownContextErr   error
	errorItemContextErr error
	finishContextErr    error
}

func (s *cancellationAwareStore) TransitionExecution(ctx context.Context, transition OperationExecutionTransition) (OperationExecutionRecord, error) {
	if transition.To == OperationExecutionUnknown {
		s.unknownContextErr = ctx.Err()
		if err := ctx.Err(); err != nil {
			return OperationExecutionRecord{}, err
		}
	}
	return s.recordingStore.TransitionExecution(ctx, transition)
}

func (s *cancellationAwareStore) AppendItem(ctx context.Context, item ItemRecord) error {
	if item.Type == ItemTypeError {
		s.errorItemContextErr = ctx.Err()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return s.recordingStore.AppendItem(ctx, item)
}

func (s *cancellationAwareStore) FinishRun(ctx context.Context, request FinishRunRequest) error {
	s.finishContextErr = ctx.Err()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.recordingStore.FinishRun(ctx, request)
}

type renewalSignalStore struct {
	recordingStore
	renewed chan struct{}
	once    sync.Once
}

type deadlineCrossingFinishStore struct {
	recordingStore

	finishMu          sync.Mutex
	finishRenewed     chan struct{}
	finishStatuses    []RunStatus
	failFirstComplete error
	completeFailed    bool
	clock             *leaseTestClock
}

type leaseTestClock struct {
	mu  sync.Mutex
	now time.Time
}

type renewalFailingAppendStore struct {
	recordingStore
	err error
}

type graceBlockingRenewStore struct {
	recordingStore
	mu             sync.Mutex
	renewals       int
	firstRenewed   chan struct{}
	secondStarted  chan struct{}
	secondCanceled chan struct{}
}

var ErrFinishRenewalNotObserved = errors.New("deadlineCrossingFinishStore: lease was not renewed during FinishRun")

func newDeadlineCrossingFinishStore(failFirstComplete error) *deadlineCrossingFinishStore {
	clock := &leaseTestClock{now: time.Unix(100, 0)}
	store := &deadlineCrossingFinishStore{clock: clock, failFirstComplete: failFirstComplete}
	store.recordingStore.now = clock.current
	return store
}

func (c *leaseTestClock) current() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *leaseTestClock) prepareRenewal(observedDeadline time.Time, ttl time.Duration) {
	c.mu.Lock()
	target := observedDeadline.Add(-ttl).Add(5 * time.Second)
	if c.now.Before(target) {
		c.now = target
	}
	c.mu.Unlock()
}

func (c *leaseTestClock) advancePast(deadline time.Time) {
	c.mu.Lock()
	if !c.now.After(deadline) {
		c.now = deadline.Add(time.Second)
	}
	c.mu.Unlock()
}

type rejectingLeaseStore struct {
	recordingStore
}

type rejectingAttemptStore struct {
	recordingStore
}

func (s *rejectingLeaseStore) ValidateRunLease(context.Context, RunHandle) (RunHandle, error) {
	return RunHandle{}, ErrSessionLeaseLost
}

func (s *rejectingAttemptStore) ValidateExecutionAttempt(context.Context, string, string) error {
	return ErrOperationAttemptLost
}

func (s *renewalSignalStore) RenewRunLease(ctx context.Context, request RenewRunLeaseRequest) (RunHandle, error) {
	handle, err := s.recordingStore.RenewRunLease(ctx, request)
	if err == nil {
		s.once.Do(func() { close(s.renewed) })
	}
	return handle, err
}

func (s *deadlineCrossingFinishStore) RenewRunLease(ctx context.Context, request RenewRunLeaseRequest) (RunHandle, error) {
	s.clock.prepareRenewal(request.Handle.LeaseDeadline, request.LeaseTTL)
	handle, err := s.recordingStore.RenewRunLease(ctx, request)
	if err != nil {
		return RunHandle{}, err
	}
	s.finishMu.Lock()
	if s.finishRenewed != nil {
		select {
		case <-s.finishRenewed:
		default:
			close(s.finishRenewed)
		}
	}
	s.finishMu.Unlock()
	return handle, nil
}

func (s *deadlineCrossingFinishStore) FinishRun(ctx context.Context, request FinishRunRequest) error {
	renewed := make(chan struct{})
	s.finishMu.Lock()
	s.finishRenewed = renewed
	s.finishStatuses = append(s.finishStatuses, request.Run.Status)
	s.finishMu.Unlock()
	defer func() {
		s.finishMu.Lock()
		if s.finishRenewed == renewed {
			s.finishRenewed = nil
		}
		s.finishMu.Unlock()
	}()

	select {
	case <-renewed:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return ErrFinishRenewalNotObserved
	}
	// Cross the deadline observed in the FinishRun request without sleeping.
	// The renewal handshake above has already moved the store-owned deadline
	// five seconds farther, so the live lease remains valid.
	s.clock.advancePast(request.Handle.LeaseDeadline)

	s.finishMu.Lock()
	shouldFail := request.Run.Status == RunStatusCompleted && s.failFirstComplete != nil && !s.completeFailed
	if shouldFail {
		s.completeFailed = true
	}
	s.finishMu.Unlock()
	if shouldFail {
		return s.failFirstComplete
	}
	return s.recordingStore.FinishRun(ctx, request)
}

func (s *deadlineCrossingFinishStore) statuses() []RunStatus {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	return append([]RunStatus(nil), s.finishStatuses...)
}

func (s *renewalFailingAppendStore) RenewRunLease(context.Context, RenewRunLeaseRequest) (RunHandle, error) {
	return RunHandle{}, s.err
}

func (s *renewalFailingAppendStore) AppendItem(ctx context.Context, item ItemRecord) error {
	if item.Type == ItemTypeUserMessage {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.recordingStore.AppendItem(ctx, item)
}

func (s *graceBlockingRenewStore) RenewRunLease(ctx context.Context, request RenewRunLeaseRequest) (RunHandle, error) {
	s.mu.Lock()
	s.renewals++
	renewal := s.renewals
	s.mu.Unlock()
	switch renewal {
	case 1:
		handle, err := s.recordingStore.RenewRunLease(ctx, request)
		if err == nil {
			close(s.firstRenewed)
		}
		return handle, err
	case 2:
		close(s.secondStarted)
		<-ctx.Done()
		close(s.secondCanceled)
		return RunHandle{}, ctx.Err()
	default:
		return s.recordingStore.RenewRunLease(ctx, request)
	}
}

type blockingModel struct {
	release <-chan struct{}
}

type cancelingModel struct {
	cancel context.CancelFunc
}

func (m blockingModel) Complete(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.release:
		return messageResponse("resp-blocked", "done"), nil
	}
}

func (m cancelingModel) Complete(ctx context.Context, _ ModelRequest) (*ModelResponse, error) {
	m.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *completeFailingStore) FinishRun(ctx context.Context, request FinishRunRequest) error {
	if request.Run.Status == RunStatusCompleted {
		return s.err
	}
	return s.recordingStore.FinishRun(ctx, request)
}

func (s *appendFailingStore) AppendItem(ctx context.Context, item ItemRecord) error {
	if item.Type == s.failType {
		return s.err
	}
	return s.recordingStore.AppendItem(ctx, item)
}

func (s *nthItemFailingStore) AppendItem(ctx context.Context, item ItemRecord) error {
	if item.Type == s.failType {
		s.seen++
		if s.seen == s.failAt {
			return s.err
		}
	}
	return s.recordingStore.AppendItem(ctx, item)
}

func (s *mutatingAppendStore) AppendItem(ctx context.Context, item ItemRecord) error {
	if item.Type == ItemTypeOperationResult && len(item.Data) > 0 {
		item.Data[0] = '['
	}
	return s.recordingStore.AppendItem(ctx, item)
}

func (s *mutatingBeginStore) BeginRun(ctx context.Context, request BeginRunRequest) (BeginRunResult, error) {
	if nested, ok := request.Run.Input.Metadata["nested"].(map[string]any); ok {
		nested["value"] = "store-mutated"
	}
	return s.recordingStore.BeginRun(ctx, request)
}

func (s *mismatchedRunHandleStore) BeginRun(ctx context.Context, request BeginRunRequest) (BeginRunResult, error) {
	result, err := s.recordingStore.BeginRun(ctx, request)
	if err == nil {
		result.Handle.RunID = "run-other"
	}
	return result, err
}

func (s *hiddenBeginSessionStore) BeginRun(ctx context.Context, request BeginRunRequest) (BeginRunResult, error) {
	result, err := s.recordingStore.BeginRun(ctx, request)
	if err == nil {
		result.Session = nil
	}
	return result, err
}

func (s *recordingStore) AppendItem(_ context.Context, item ItemRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
	return nil
}

func (s *recordingStore) ReservePlanBatch(_ context.Context, batch OperationPlanBatch) (PlanBatchReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plans == nil {
		s.plans = make(map[string]map[uint64]OperationPlanBatch)
	}
	batches := s.plans[batch.RequestID]
	if existing, ok := batches[batch.Index]; ok {
		return PlanBatchReservation{Batch: cloneOperationPlanBatchForTest(existing)}, nil
	}
	if _, sealed := s.seals[batch.RequestID]; sealed {
		return PlanBatchReservation{}, fmt.Errorf("%w: request %s is sealed", ErrOperationPlanChanged, batch.RequestID)
	}
	if batch.Index != uint64(len(batches)) {
		return PlanBatchReservation{}, ErrOperationPlanChanged
	}
	if batches == nil {
		batches = make(map[uint64]OperationPlanBatch)
		s.plans[batch.RequestID] = batches
	}
	batch = cloneOperationPlanBatchForTest(batch)
	batches[batch.Index] = batch
	return PlanBatchReservation{Batch: cloneOperationPlanBatchForTest(batch), Created: true}, nil
}

func (s *recordingStore) SealPlan(_ context.Context, seal OperationPlanSeal) (PlanSealResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.seals[seal.RequestID]; ok {
		return PlanSealResult{Seal: existing}, nil
	}
	batches := s.plans[seal.RequestID]
	if uint64(len(batches)) != seal.BatchCount {
		return PlanSealResult{}, fmt.Errorf("%w: request %s recorded %d batch(es), observed %d", ErrOperationPlanChanged, seal.RequestID, len(batches), seal.BatchCount)
	}
	for index := uint64(0); index < seal.BatchCount; index++ {
		if _, ok := batches[index]; !ok {
			return PlanSealResult{}, fmt.Errorf("%w: missing batch %d", ErrOperationPlanChanged, index)
		}
	}
	if s.seals == nil {
		s.seals = make(map[string]OperationPlanSeal)
	}
	s.seals[seal.RequestID] = seal
	return PlanSealResult{Seal: seal, Created: true}, nil
}
