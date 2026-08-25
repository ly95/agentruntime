package agentruntime

import (
	"context"
	"errors"
	"sync"
	"time"
)

// InMemoryStore is a concurrency-safe reference implementation of RunStore and
// ExecutionStore. It is intended for examples, tests, local processes, and as
// executable protocol documentation. State is process-local and is lost when
// the process exits; production hosts still need a durable transactional store.
type InMemoryStore struct {
	mu  sync.Mutex
	now func() time.Time

	runs             map[string]RunRecord
	items            map[string]ItemRecord
	itemOrder        map[string][]string
	sessions         map[string]SessionState
	leases           map[string]RunHandle
	leaseGenerations map[string]uint64
	pendingApprovals map[string]PendingApprovalCommit

	plans         map[string]map[uint64]OperationPlanBatch
	seals         map[string]OperationPlanSeal
	executions    map[string]OperationExecutionRecord
	transitions   map[string][]OperationExecutionTransition
	transitionIDs map[string]string
}

// InMemoryRunStore and InMemoryExecutionStore name the two protocol views of
// InMemoryStore for discoverability in host configuration.
type InMemoryRunStore = InMemoryStore
type InMemoryExecutionStore = InMemoryStore

// InMemoryStoreConfig configures deterministic time for tests and examples.
// A nil Now function uses time.Now.
type InMemoryStoreConfig struct {
	Now func() time.Time
}

// NewInMemoryStore constructs a store implementing both persistence ports.
func NewInMemoryStore() *InMemoryStore {
	store, _ := NewInMemoryStoreWithConfig(InMemoryStoreConfig{})
	return store
}

// NewInMemoryRunStore constructs the RunStore reference implementation.
func NewInMemoryRunStore() *InMemoryRunStore { return NewInMemoryStore() }

// NewInMemoryExecutionStore constructs the ExecutionStore reference
// implementation.
func NewInMemoryExecutionStore() *InMemoryExecutionStore { return NewInMemoryStore() }

// NewInMemoryStoreWithConfig constructs a store with an injected clock.
func NewInMemoryStoreWithConfig(config InMemoryStoreConfig) (*InMemoryStore, error) {
	if config.Now != nil && isNilDependency(config.Now) {
		return nil, errors.New("agent: in-memory store clock is nil")
	}
	store := &InMemoryStore{now: config.Now}
	store.initializeLocked()
	return store, nil
}

func (store *InMemoryStore) initializeLocked() {
	if store.runs == nil {
		store.runs = make(map[string]RunRecord)
	}
	if store.items == nil {
		store.items = make(map[string]ItemRecord)
	}
	if store.itemOrder == nil {
		store.itemOrder = make(map[string][]string)
	}
	if store.sessions == nil {
		store.sessions = make(map[string]SessionState)
	}
	if store.leases == nil {
		store.leases = make(map[string]RunHandle)
	}
	if store.leaseGenerations == nil {
		store.leaseGenerations = make(map[string]uint64)
	}
	if store.pendingApprovals == nil {
		store.pendingApprovals = make(map[string]PendingApprovalCommit)
	}
	if store.plans == nil {
		store.plans = make(map[string]map[uint64]OperationPlanBatch)
	}
	if store.seals == nil {
		store.seals = make(map[string]OperationPlanSeal)
	}
	if store.executions == nil {
		store.executions = make(map[string]OperationExecutionRecord)
	}
	if store.transitions == nil {
		store.transitions = make(map[string][]OperationExecutionTransition)
	}
	if store.transitionIDs == nil {
		store.transitionIDs = make(map[string]string)
	}
}

func (store *InMemoryStore) currentTime() time.Time {
	if store.now != nil {
		return store.now()
	}
	return time.Now()
}

func (store *InMemoryStore) lock(ctx context.Context) (func(), error) {
	if store == nil {
		return nil, errors.New("agent: in-memory store is nil")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	store.mu.Lock()
	store.initializeLocked()
	if err := context.Cause(ctx); err != nil {
		store.mu.Unlock()
		return nil, err
	}
	return store.mu.Unlock, nil
}

// GetRun returns a defensive copy of one stored run.
func (store *InMemoryStore) GetRun(ctx context.Context, runID string) (RunRecord, error) {
	unlock, err := store.lock(ctx)
	if err != nil {
		return RunRecord{}, err
	}
	defer unlock()
	run, ok := store.runs[runID]
	if !ok {
		return RunRecord{}, ErrRunNotFound
	}
	return clonePersistentRunRecord(run)
}

// GetSession returns a defensive copy of one stored session.
func (store *InMemoryStore) GetSession(ctx context.Context, sessionID string) (SessionState, error) {
	unlock, err := store.lock(ctx)
	if err != nil {
		return SessionState{}, err
	}
	defer unlock()
	session, ok := store.sessions[sessionID]
	if !ok {
		return SessionState{}, ErrSessionNotFound
	}
	return cloneStoredSessionState(session), nil
}

// ListItems returns defensive copies in append order.
func (store *InMemoryStore) ListItems(ctx context.Context, runID string) ([]ItemRecord, error) {
	unlock, err := store.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, ok := store.runs[runID]; !ok {
		return nil, ErrRunNotFound
	}
	ids := store.itemOrder[runID]
	items := make([]ItemRecord, 0, len(ids))
	for _, id := range ids {
		items = append(items, cloneStoredItemRecord(store.items[id]))
	}
	return items, nil
}

// GetPendingApproval returns the complete durable approval authority for a
// waiting run. ErrApprovalRequired means the run has no pending authority.
func (store *InMemoryStore) GetPendingApproval(ctx context.Context, runID string) (PendingApprovalCommit, error) {
	unlock, err := store.lock(ctx)
	if err != nil {
		return PendingApprovalCommit{}, err
	}
	defer unlock()
	if _, ok := store.runs[runID]; !ok {
		return PendingApprovalCommit{}, ErrRunNotFound
	}
	pending, ok := store.pendingApprovals[runID]
	if !ok {
		return PendingApprovalCommit{}, ErrApprovalRequired
	}
	return cloneStoredPendingApproval(pending)
}
