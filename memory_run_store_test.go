package agentruntime

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInMemoryStoreFinishRunRejectsInputRewrite(t *testing.T) {
	now := time.Date(2026, time.August, 25, 8, 0, 0, 0, time.UTC)
	store := NewInMemoryStore()
	request := CreateRunRequest{
		Run: RunRecord{
			ID: "immutable-input-run", ModelBindingID: "model-binding-v1", Status: RunStatusRunning,
			Input: Input{
				RunID: "immutable-input-run", User: "original",
				IdempotencyKey: "original-key", IdempotencyScope: "tenant",
			},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseTTL: time.Minute,
	}
	var start RunStart
	if err := store.CreateRunV4(t.Context(), request, func(candidate RunStart) error {
		start = candidate
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed := request.Run
	changed.Status = RunStatusCompleted
	changed.Result = "done"
	changed.Input.User = "rewritten"
	changed.Input.IdempotencyKey = "rewritten-key"
	changed.UpdatedAt = now.Add(time.Second)
	if err := store.FinishRun(t.Context(), FinishRunRequest{Handle: start.Handle, Run: changed}); !errors.Is(err, ErrRunStoreProtocol) {
		t.Fatalf("FinishRun changed input error=%v", err)
	}
	stored, err := store.GetRun(t.Context(), request.Run.ID)
	if err != nil || stored.Status != RunStatusRunning || stored.Input.User != "original" || stored.Input.IdempotencyKey != "original-key" {
		t.Fatalf("stored run after rejected finish=%+v err=%v", stored, err)
	}

	completed := request.Run
	completed.Status = RunStatusCompleted
	completed.Result = "done"
	completed.UpdatedAt = now.Add(time.Second)
	if err := store.FinishRun(t.Context(), FinishRunRequest{Handle: start.Handle, Run: completed}); err != nil {
		t.Fatal(err)
	}
}

func TestInMemoryStoreFinishRunRejectsStatelessSessionInjectionWithoutMutation(t *testing.T) {
	now := time.Date(2026, time.August, 25, 9, 0, 0, 0, time.UTC)
	store := NewInMemoryStore()
	request := CreateRunRequest{
		Run: RunRecord{
			ID: "stateless-session-injection", ModelBindingID: "model-binding-v1", Status: RunStatusRunning,
			Input:     Input{RunID: "stateless-session-injection", User: "run"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseTTL: time.Minute,
	}
	var start RunStart
	if err := store.CreateRunV4(t.Context(), request, func(candidate RunStart) error {
		start = candidate
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	completed := request.Run
	completed.Status = RunStatusCompleted
	completed.Result = "done"
	completed.UpdatedAt = now.Add(time.Second)
	injected := &SessionState{
		ID: "unrelated-session", Revision: 1, LastRunID: completed.ID,
		CreatedAt: now, UpdatedAt: completed.UpdatedAt,
	}
	if err := store.FinishRun(t.Context(), FinishRunRequest{
		Handle: start.Handle, Run: completed, Session: injected,
	}); !errors.Is(err, ErrRunStoreProtocol) {
		t.Fatalf("FinishRun injection error=%v", err)
	}
	if _, err := store.GetSession(t.Context(), injected.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("injected session lookup error=%v", err)
	}
	stored, err := store.GetRun(t.Context(), completed.ID)
	if err != nil || stored.Status != RunStatusRunning {
		t.Fatalf("stored run after rejected injection=%+v err=%v", stored, err)
	}
	if err := store.FinishRun(t.Context(), FinishRunRequest{Handle: start.Handle, Run: completed}); err != nil {
		t.Fatalf("valid FinishRun after rejected injection: %v", err)
	}
}

func TestRuntimeSessionCreationTimeUsesPersistentClockAnchor(t *testing.T) {
	store := &recordingStore{}
	runtime := newTestRuntime(
		t,
		&scriptedModel{responses: []*ModelResponse{messageResponse("session-time-response", "done")}},
		nil, nil, nil, nil, nil, store,
	)
	const sessionID = "session-time-anchor"
	if _, err := runtime.Run(t.Context(), Input{User: "run", SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(10, 0)
	session := store.sessions[sessionID]
	if !session.CreatedAt.Equal(want) || !session.UpdatedAt.Equal(want) {
		t.Fatalf("session timestamps=%s/%s, want persistent clock anchor %s", session.CreatedAt, session.UpdatedAt, want)
	}
}

func TestInMemoryStoreExpiredLeaseTakeoverMarksMissingFailureAudit(t *testing.T) {
	now := time.Date(2026, time.August, 25, 10, 0, 0, 0, time.UTC)
	store, err := NewInMemoryStoreWithConfig(InMemoryStoreConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first := CreateRunRequest{
		Run: RunRecord{
			ID: "expired-run", SessionID: "shared-session", ModelBindingID: "model-binding-v1", Status: RunStatusRunning,
			Input:     Input{RunID: "expired-run", SessionID: "shared-session", User: "first"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "expired-lease", LeaseTTL: time.Second,
	}
	if err := store.CreateRunV4(t.Context(), first, func(RunStart) error { return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	second := CreateRunRequest{
		Run: RunRecord{
			ID: "replacement-run", SessionID: "shared-session", ModelBindingID: "model-binding-v1", Status: RunStatusRunning,
			Input:     Input{RunID: "replacement-run", SessionID: "shared-session", User: "second"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "replacement-lease", LeaseTTL: time.Second,
	}
	if err := store.CreateRunV4(t.Context(), second, func(RunStart) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRun(t.Context(), first.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunStatusFailed || got.ErrorCode != "session_lease_lost" ||
		got.FailureAuditStatus != FailureAuditMissing || got.PendingApprovalDigest != "" {
		t.Fatalf("expired run=%+v", got)
	}
}

func TestInMemoryStoreCallbackCannotMutateCommittedSessionBinding(t *testing.T) {
	now := time.Date(2026, time.August, 25, 11, 0, 0, 0, time.UTC)
	store, err := NewInMemoryStoreWithConfig(InMemoryStoreConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := CreateRunRequest{
		Run: RunRecord{
			ID: "binding-run", SessionID: "binding-session", ModelBindingID: "model-binding-v1", Status: RunStatusRunning,
			SkillSetID: "skill-set-v1", OperationSetID: "operation-set-v1",
			Input:     Input{RunID: "binding-run", SessionID: "binding-session", User: "bind"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "binding-lease", LeaseTTL: time.Minute,
	}
	if err := store.CreateRunV4(t.Context(), request, func(start RunStart) error {
		if start.Session == nil {
			t.Fatal("callback session is nil")
		}
		start.Session.ID = "callback-mutated"
		start.Session.ModelBindingID = "callback-mutated"
		start.Session.SkillSetID = "callback-mutated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession(t.Context(), request.Run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != request.Run.SessionID || session.ModelBindingID != request.Run.ModelBindingID ||
		session.SkillSetID != request.Run.SkillSetID {
		t.Fatalf("committed session was mutated through callback: %+v", session)
	}
}

func TestInMemoryStoreResumeRunV4RejectsExhaustedLeaseGenerationAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 11, 30, 0, 0, time.UTC)
	store, resume := waitingApprovalMemoryStore(t, now, "generation-exhausted")
	store.mu.Lock()
	store.leaseGenerations[resume.Run.SessionID] = math.MaxUint64
	store.mu.Unlock()
	before := snapshotInMemoryRunStore(t, store)
	callbacks := 0
	err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error {
		callbacks++
		return nil
	})
	if !errors.Is(err, ErrRunStoreProtocol) || err == nil ||
		!strings.Contains(err.Error(), "lease generation exhausted") || callbacks != 0 {
		t.Fatalf("ResumeRunV4 generation exhaustion error=%v callbacks=%d", err, callbacks)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreCreateRunV4CreatesBindingOnlySessionRevisionZero(t *testing.T) {
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	store := NewInMemoryStore()
	request := memoryStoreCreateRequest("binding-only-run", "binding-only-session", now)
	var start RunStart
	if err := store.CreateRunV4(t.Context(), request, func(candidate RunStart) error {
		start = candidate
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if start.Session == nil || start.Session.ID != request.Run.SessionID ||
		start.Session.ModelBindingID != request.Run.ModelBindingID || start.Session.Revision != 0 ||
		start.Session.SkillSetID != "" || start.Session.OperationSetID != "" {
		t.Fatalf("binding-only start session=%+v", start.Session)
	}
	stored, err := store.GetSession(t.Context(), request.Run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, *start.Session) {
		t.Fatalf("stored binding-only session=%+v, start session=%+v", stored, *start.Session)
	}
}

func TestInMemoryStoreCreateRunV4RejectsLegacyEmptySessionBindingBeforeCallback(t *testing.T) {
	now := time.Date(2026, time.August, 25, 13, 0, 0, 0, time.UTC)
	store := NewInMemoryStore()
	request := memoryStoreCreateRequest("legacy-session-run", "legacy-empty-session", now)
	store.sessions[request.Run.SessionID] = SessionState{
		ID: request.Run.SessionID, CreatedAt: now, UpdatedAt: now,
	}
	before := snapshotInMemoryRunStore(t, store)
	callbacks := 0
	err := store.CreateRunV4(t.Context(), request, func(RunStart) error {
		callbacks++
		return nil
	})
	if !errors.Is(err, ErrModelBindingMismatch) || callbacks != 0 {
		t.Fatalf("CreateRunV4 error=%v callbacks=%d", err, callbacks)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreExpiredFenceRejectsActiveRunModelBindingDriftAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 14, 0, 0, 0, time.UTC)
	store, err := NewInMemoryStoreWithConfig(InMemoryStoreConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first := memoryStoreCreateRequest("expired-binding-run", "expired-binding-session", now)
	first.LeaseTTL = time.Second
	if err := store.CreateRunV4(t.Context(), first, func(RunStart) error { return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	store.mu.Lock()
	drifted := store.runs[first.Run.ID]
	drifted.ModelBindingID = ""
	store.runs[first.Run.ID] = drifted
	store.mu.Unlock()
	before := snapshotInMemoryRunStore(t, store)

	replacement := memoryStoreCreateRequest("expired-binding-replacement", first.Run.SessionID, now)
	replacement.LeaseID = "lease-expired-binding-replacement"
	callbacks := 0
	err = store.CreateRunV4(t.Context(), replacement, func(RunStart) error {
		callbacks++
		return nil
	})
	if !errors.Is(err, ErrModelBindingMismatch) || callbacks != 0 {
		t.Fatalf("expired fence error=%v callbacks=%d", err, callbacks)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
	stored, err := store.GetRun(t.Context(), first.Run.ID)
	if err != nil || stored.Status != RunStatusRunning || stored.FailureAuditStatus != "" {
		t.Fatalf("expired owner after rejected fence=%+v err=%v", stored, err)
	}
}

func TestInMemoryStoreExpiredFenceRejectsDurableApprovalModelBindingDriftAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 14, 30, 0, 0, time.UTC)
	store, resume := waitingApprovalMemoryStore(t, now, "expired-approval-binding")
	var resumed ResumedRun
	if err := store.ResumeRunV4(t.Context(), resume, func(candidate ResumedRun) error {
		resumed = candidate
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	active := store.leases[resume.Run.SessionID]
	active.LeaseDeadline = now.Add(-time.Second)
	store.leases[resume.Run.SessionID] = active
	pending := store.pendingApprovals[resume.Run.ID]
	checkpoint := *pending.Request.Checkpoint
	checkpoint.ModelBindingID = ""
	pending.Request.Checkpoint = &checkpoint
	store.pendingApprovals[resume.Run.ID] = pending
	store.mu.Unlock()
	before := snapshotInMemoryRunStore(t, store)

	replacement := memoryStoreCreateRequest("expired-approval-replacement", resume.Run.SessionID, now.Add(time.Second))
	callbacks := 0
	err := store.CreateRunV4(t.Context(), replacement, func(RunStart) error {
		callbacks++
		return nil
	})
	if !errors.Is(err, ErrModelBindingMismatch) || callbacks != 0 {
		t.Fatalf("expired approval fence error=%v callbacks=%d resumed=%+v", err, callbacks, resumed)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreResumeRunV4RejectsModelBindingDriftAtomically(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		mutate func(*InMemoryStore, *ResumeRunRequest)
	}{
		{
			name: "request run",
			id:   "request-run",
			mutate: func(_ *InMemoryStore, request *ResumeRunRequest) {
				request.Run.ModelBindingID = "model-binding-v2"
			},
		},
		{
			name: "legacy waiting run",
			id:   "legacy-waiting-run",
			mutate: func(store *InMemoryStore, request *ResumeRunRequest) {
				store.mu.Lock()
				run := store.runs[request.Run.ID]
				run.ModelBindingID = ""
				store.runs[request.Run.ID] = run
				store.mu.Unlock()
			},
		},
		{
			name: "legacy session",
			id:   "legacy-session",
			mutate: func(store *InMemoryStore, request *ResumeRunRequest) {
				store.mu.Lock()
				session := store.sessions[request.Run.SessionID]
				session.ModelBindingID = ""
				store.sessions[request.Run.SessionID] = session
				store.mu.Unlock()
			},
		},
		{
			name: "legacy approval checkpoint",
			id:   "legacy-approval-checkpoint",
			mutate: func(store *InMemoryStore, request *ResumeRunRequest) {
				store.mu.Lock()
				pending := store.pendingApprovals[request.Run.ID]
				checkpoint := *pending.Request.Checkpoint
				checkpoint.ModelBindingID = ""
				pending.Request.Checkpoint = &checkpoint
				store.pendingApprovals[request.Run.ID] = pending
				store.mu.Unlock()
			},
		},
		{
			name: "different approval checkpoint",
			id:   "different-approval-checkpoint",
			mutate: func(store *InMemoryStore, request *ResumeRunRequest) {
				store.mu.Lock()
				pending := store.pendingApprovals[request.Run.ID]
				checkpoint := *pending.Request.Checkpoint
				checkpoint.ModelBindingID = "model-binding-v2"
				pending.Request.Checkpoint = &checkpoint
				store.pendingApprovals[request.Run.ID] = pending
				store.mu.Unlock()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 25, 15, 0, 0, 0, time.UTC)
			store, resume := waitingApprovalMemoryStore(t, now, "resume-binding-"+test.id)
			test.mutate(store, &resume)
			before := snapshotInMemoryRunStore(t, store)
			callbacks := 0
			err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error {
				callbacks++
				return nil
			})
			if !errors.Is(err, ErrModelBindingMismatch) || callbacks != 0 {
				t.Fatalf("ResumeRunV4 error=%v callbacks=%d", err, callbacks)
			}
			assertInMemoryRunStoreSnapshot(t, store, before)
		})
	}
}

func TestInMemoryStoreFinishRunRejectsModelBindingDriftAtomically(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		mutate func(*InMemoryStore, *FinishRunRequest)
	}{
		{
			name: "request run",
			id:   "request-run",
			mutate: func(_ *InMemoryStore, request *FinishRunRequest) {
				request.Run.ModelBindingID = "model-binding-v2"
			},
		},
		{
			name: "legacy stored run",
			id:   "legacy-stored-run",
			mutate: func(store *InMemoryStore, request *FinishRunRequest) {
				store.mu.Lock()
				run := store.runs[request.Run.ID]
				run.ModelBindingID = ""
				store.runs[request.Run.ID] = run
				store.mu.Unlock()
			},
		},
		{
			name: "legacy stored session",
			id:   "legacy-stored-session",
			mutate: func(store *InMemoryStore, request *FinishRunRequest) {
				store.mu.Lock()
				session := store.sessions[request.Run.SessionID]
				session.ModelBindingID = ""
				store.sessions[request.Run.SessionID] = session
				store.mu.Unlock()
			},
		},
		{
			name: "request session",
			id:   "request-session",
			mutate: func(_ *InMemoryStore, request *FinishRunRequest) {
				request.Session.ModelBindingID = "model-binding-v2"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 25, 16, 0, 0, 0, time.UTC)
			store, request := completedMemoryStoreFinishRequest(t, now, "finish-binding-"+test.id)
			test.mutate(store, &request)
			before := snapshotInMemoryRunStore(t, store)
			err := store.FinishRun(t.Context(), request)
			if !errors.Is(err, ErrModelBindingMismatch) {
				t.Fatalf("FinishRun error=%v", err)
			}
			assertInMemoryRunStoreSnapshot(t, store, before)
		})
	}
}

func TestInMemoryStoreFinishRunRejectsPendingApprovalModelBindingDriftAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 17, 0, 0, 0, time.UTC)
	store, create, start := runningMemoryStore(t, now, "finish-pending-binding")
	pending := memoryStorePendingApproval(t, create.Run, "", start.Handle.SessionRevision, now.Add(time.Second))
	waiting := create.Run
	waiting.Status = RunStatusWaitingUser
	waiting.PendingApprovalDigest = pending.Digest
	waiting.UpdatedAt = now.Add(time.Second)
	request := FinishRunRequest{Handle: start.Handle, Run: waiting, PendingApproval: &pending}
	before := snapshotInMemoryRunStore(t, store)
	if err := store.FinishRun(t.Context(), request); !errors.Is(err, ErrModelBindingMismatch) {
		t.Fatalf("FinishRun pending approval binding drift error=%v", err)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)

	pending = memoryStorePendingApproval(t, create.Run, create.Run.ModelBindingID, start.Handle.SessionRevision, now.Add(time.Second))
	waiting.PendingApprovalDigest = pending.Digest
	request.Run = waiting
	request.PendingApproval = &pending
	if err := store.FinishRun(t.Context(), request); err != nil {
		t.Fatalf("valid waiting FinishRun after rejected binding drift: %v", err)
	}
}

func TestInMemoryStoreFinishRunClassifiesUncloneablePendingAuthority(t *testing.T) {
	now := time.Date(2026, time.August, 25, 17, 30, 0, 0, time.UTC)
	store, create, start := runningMemoryStore(t, now, "finish-uncloneable-pending")
	pending := memoryStorePendingApproval(
		t, create.Run, create.Run.ModelBindingID, start.Handle.SessionRevision, now.Add(time.Second),
	)
	pending.Request.Operation.Arguments = map[string]any{"invalid": math.NaN()}
	waiting := create.Run
	waiting.Status = RunStatusWaitingUser
	waiting.PendingApprovalDigest = pending.Digest
	waiting.UpdatedAt = now.Add(time.Second)
	before := snapshotInMemoryRunStore(t, store)

	err := store.FinishRun(t.Context(), FinishRunRequest{
		Handle: start.Handle, Run: waiting, PendingApproval: &pending,
	})
	if !errors.Is(err, ErrOperationPlanChanged) {
		t.Fatalf("FinishRun error=%v, want ErrOperationPlanChanged", err)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreFinishRunRejectsDurableApprovalModelBindingDriftAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 18, 0, 0, 0, time.UTC)
	store, resume := waitingApprovalMemoryStore(t, now, "finish-durable-binding")
	var resumed ResumedRun
	if err := store.ResumeRunV4(t.Context(), resume, func(candidate ResumedRun) error {
		resumed = candidate
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	pending := store.pendingApprovals[resume.Run.ID]
	checkpoint := *pending.Request.Checkpoint
	checkpoint.ModelBindingID = ""
	pending.Request.Checkpoint = &checkpoint
	store.pendingApprovals[resume.Run.ID] = pending
	store.mu.Unlock()

	completed := resume.Run
	completed.Status = RunStatusCompleted
	completed.Result = "done"
	completed.UpdatedAt = now.Add(3 * time.Second)
	session := cloneStoredSessionState(*resumed.Session)
	session.Revision = resumed.Handle.SessionRevision + 1
	session.LastRunID = completed.ID
	session.UpdatedAt = completed.UpdatedAt
	request := FinishRunRequest{Handle: resumed.Handle, Run: completed, Session: &session}
	before := snapshotInMemoryRunStore(t, store)
	if err := store.FinishRun(t.Context(), request); !errors.Is(err, ErrModelBindingMismatch) {
		t.Fatalf("FinishRun durable approval binding drift error=%v", err)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreResumeRunV4RejectsMalformedDurableApprovalAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PendingApprovalCommit)
	}{
		{
			name: "legacy authority version",
			mutate: func(pending *PendingApprovalCommit) {
				pending.AuthorityVersion = PendingApprovalAuthorityVersion - 1
			},
		},
		{
			name: "future authority version",
			mutate: func(pending *PendingApprovalCommit) {
				pending.AuthorityVersion = PendingApprovalAuthorityVersion + 1
			},
		},
		{
			name: "missing checkpoint",
			mutate: func(pending *PendingApprovalCommit) {
				pending.Request.Checkpoint = nil
			},
		},
		{
			name: "digest-covered audit drift",
			mutate: func(pending *PendingApprovalCommit) {
				pending.Audit.Data = json.RawMessage(`{"pending":false}`)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 25, 18, 30, 0, 0, time.UTC)
			store, resume := waitingApprovalMemoryStore(t, now, "resume-malformed-"+test.name)
			store.mu.Lock()
			pending := store.pendingApprovals[resume.Run.ID]
			test.mutate(&pending)
			store.pendingApprovals[resume.Run.ID] = pending
			store.mu.Unlock()
			before := snapshotInMemoryRunStore(t, store)
			callbacks := 0
			err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error {
				callbacks++
				return nil
			})
			if !errors.Is(err, ErrOperationPlanChanged) || callbacks != 0 {
				t.Fatalf("ResumeRunV4 error=%v callbacks=%d", err, callbacks)
			}
			assertInMemoryRunStoreSnapshot(t, store, before)
		})
	}
}

func TestInMemoryStoreResumeNonWaitingRunPrecedesBindingDrift(t *testing.T) {
	now := time.Date(2026, time.August, 25, 18, 40, 0, 0, time.UTC)
	store, finish := completedMemoryStoreFinishRequest(t, now, "resume-priority")
	if err := store.FinishRun(t.Context(), finish); err != nil {
		t.Fatal(err)
	}
	resumeRun := finish.Run
	resumeRun.Status = RunStatusRunning
	resumeRun.Result = ""
	resumeRun.ModelBindingID = "model-binding-v2"
	resumeRun.UpdatedAt = now.Add(2 * time.Second)
	resume := ResumeRunRequest{
		Run: resumeRun, LeaseID: "resume-priority-lease", LeaseTTL: time.Minute,
		InputDigest: "resume-priority-input",
	}
	before := snapshotInMemoryRunStore(t, store)
	callbacks := 0
	err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error {
		callbacks++
		return nil
	})
	if !errors.Is(err, ErrIdentityConflict) || errors.Is(err, ErrModelBindingMismatch) || callbacks != 0 {
		t.Fatalf("ResumeRunV4 error=%v callbacks=%d, want identity conflict before binding drift", err, callbacks)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreResumeRunV4RejectsMissingDurableSessionAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 18, 45, 0, 0, time.UTC)
	store, resume := waitingApprovalMemoryStore(t, now, "missing-session")
	store.mu.Lock()
	delete(store.sessions, resume.Run.SessionID)
	store.mu.Unlock()
	before := snapshotInMemoryRunStore(t, store)
	callbacks := 0
	err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error {
		callbacks++
		return nil
	})
	if !errors.Is(err, ErrSessionNotFound) || callbacks != 0 {
		t.Fatalf("ResumeRunV4 missing session error=%v callbacks=%d", err, callbacks)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreRejectsMissingDurableApprovalAtomically(t *testing.T) {
	t.Run("resume", func(t *testing.T) {
		now := time.Date(2026, time.August, 25, 18, 50, 0, 0, time.UTC)
		store, resume := waitingApprovalMemoryStore(t, now, "missing-approval-resume")
		store.mu.Lock()
		delete(store.pendingApprovals, resume.Run.ID)
		store.mu.Unlock()
		before := snapshotInMemoryRunStore(t, store)
		callbacks := 0
		err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error {
			callbacks++
			return nil
		})
		if !errors.Is(err, ErrOperationPlanChanged) || callbacks != 0 {
			t.Fatalf("ResumeRunV4 missing approval error=%v callbacks=%d", err, callbacks)
		}
		assertInMemoryRunStoreSnapshot(t, store, before)
	})

	t.Run("expired takeover", func(t *testing.T) {
		now := time.Date(2026, time.August, 25, 18, 55, 0, 0, time.UTC)
		store, resume := waitingApprovalMemoryStore(t, now, "missing-approval-takeover")
		if err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error { return nil }); err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		active := store.leases[resume.Run.SessionID]
		active.LeaseDeadline = now.Add(-time.Second)
		store.leases[resume.Run.SessionID] = active
		delete(store.pendingApprovals, resume.Run.ID)
		store.mu.Unlock()
		before := snapshotInMemoryRunStore(t, store)
		replacement := memoryStoreCreateRequest("missing-approval-replacement", resume.Run.SessionID, now.Add(time.Second))
		callbacks := 0
		err := store.CreateRunV4(t.Context(), replacement, func(RunStart) error {
			callbacks++
			return nil
		})
		if !errors.Is(err, ErrOperationPlanChanged) || callbacks != 0 {
			t.Fatalf("expired takeover missing approval error=%v callbacks=%d", err, callbacks)
		}
		assertInMemoryRunStoreSnapshot(t, store, before)
	})

	t.Run("finish", func(t *testing.T) {
		now := time.Date(2026, time.August, 25, 18, 58, 0, 0, time.UTC)
		store, resume := waitingApprovalMemoryStore(t, now, "missing-approval-finish")
		var resumed ResumedRun
		if err := store.ResumeRunV4(t.Context(), resume, func(candidate ResumedRun) error {
			resumed = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		delete(store.pendingApprovals, resume.Run.ID)
		store.mu.Unlock()
		completed := resume.Run
		completed.Status = RunStatusCompleted
		completed.Result = "done"
		completed.UpdatedAt = now.Add(3 * time.Second)
		session := cloneStoredSessionState(*resumed.Session)
		session.Revision = resumed.Handle.SessionRevision + 1
		session.LastRunID = completed.ID
		session.UpdatedAt = completed.UpdatedAt
		before := snapshotInMemoryRunStore(t, store)
		err := store.FinishRun(t.Context(), FinishRunRequest{Handle: resumed.Handle, Run: completed, Session: &session})
		if !errors.Is(err, ErrOperationPlanChanged) {
			t.Fatalf("FinishRun missing approval error=%v", err)
		}
		assertInMemoryRunStoreSnapshot(t, store, before)
	})
}

func TestInMemoryStoreExpiredFenceRejectsSessionModelBindingDriftAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 19, 0, 0, 0, time.UTC)
	store, err := NewInMemoryStoreWithConfig(InMemoryStoreConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first := memoryStoreCreateRequest("expired-session-run", "expired-session", now)
	first.LeaseTTL = time.Second
	if err := store.CreateRunV4(t.Context(), first, func(RunStart) error { return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	store.mu.Lock()
	session := store.sessions[first.Run.SessionID]
	session.ModelBindingID = ""
	store.sessions[first.Run.SessionID] = session
	store.mu.Unlock()
	before := snapshotInMemoryRunStore(t, store)

	replacement := memoryStoreCreateRequest("expired-session-replacement", first.Run.SessionID, now)
	replacement.LeaseID = "lease-expired-session-replacement"
	callbacks := 0
	err = store.CreateRunV4(t.Context(), replacement, func(RunStart) error {
		callbacks++
		return nil
	})
	if !errors.Is(err, ErrModelBindingMismatch) || callbacks != 0 {
		t.Fatalf("expired session fence error=%v callbacks=%d", err, callbacks)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

func TestInMemoryStoreExpiredFenceRejectsMalformedDurableApprovalAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PendingApprovalCommit)
		want   error
	}{
		{
			name: "legacy authority version",
			mutate: func(pending *PendingApprovalCommit) {
				pending.AuthorityVersion = PendingApprovalAuthorityVersion - 1
			},
			want: ErrOperationPlanChanged,
		},
		{
			name: "missing checkpoint",
			mutate: func(pending *PendingApprovalCommit) {
				pending.Request.Checkpoint = nil
			},
			want: ErrOperationPlanChanged,
		},
		{
			name: "different checkpoint model binding",
			mutate: func(pending *PendingApprovalCommit) {
				checkpoint := *pending.Request.Checkpoint
				checkpoint.ModelBindingID = "model-binding-v2"
				pending.Request.Checkpoint = &checkpoint
			},
			want: ErrModelBindingMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 25, 19, 30, 0, 0, time.UTC)
			store, resume := waitingApprovalMemoryStore(t, now, "expired-malformed-"+test.name)
			if err := store.ResumeRunV4(t.Context(), resume, func(ResumedRun) error { return nil }); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			active := store.leases[resume.Run.SessionID]
			active.LeaseDeadline = now.Add(-time.Second)
			store.leases[resume.Run.SessionID] = active
			pending := store.pendingApprovals[resume.Run.ID]
			test.mutate(&pending)
			store.pendingApprovals[resume.Run.ID] = pending
			store.mu.Unlock()
			before := snapshotInMemoryRunStore(t, store)

			replacement := memoryStoreCreateRequest("replacement-"+test.name, resume.Run.SessionID, now.Add(time.Second))
			callbacks := 0
			err := store.CreateRunV4(t.Context(), replacement, func(RunStart) error {
				callbacks++
				return nil
			})
			if !errors.Is(err, test.want) || callbacks != 0 {
				t.Fatalf("expired malformed approval fence error=%v want=%v callbacks=%d", err, test.want, callbacks)
			}
			assertInMemoryRunStoreSnapshot(t, store, before)
		})
	}
}

func TestInMemoryStoreFinishRunRejectsMalformedDurableApprovalAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PendingApprovalCommit)
		want   error
	}{
		{
			name: "legacy authority version",
			mutate: func(pending *PendingApprovalCommit) {
				pending.AuthorityVersion = PendingApprovalAuthorityVersion - 1
			},
			want: ErrOperationPlanChanged,
		},
		{
			name: "missing checkpoint",
			mutate: func(pending *PendingApprovalCommit) {
				pending.Request.Checkpoint = nil
			},
			want: ErrOperationPlanChanged,
		},
		{
			name: "different checkpoint model binding",
			mutate: func(pending *PendingApprovalCommit) {
				checkpoint := *pending.Request.Checkpoint
				checkpoint.ModelBindingID = "model-binding-v2"
				pending.Request.Checkpoint = &checkpoint
			},
			want: ErrModelBindingMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 25, 20, 0, 0, 0, time.UTC)
			store, resume := waitingApprovalMemoryStore(t, now, "finish-malformed-"+test.name)
			var resumed ResumedRun
			if err := store.ResumeRunV4(t.Context(), resume, func(candidate ResumedRun) error {
				resumed = candidate
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			pending := store.pendingApprovals[resume.Run.ID]
			test.mutate(&pending)
			store.pendingApprovals[resume.Run.ID] = pending
			store.mu.Unlock()

			completed := resume.Run
			completed.Status = RunStatusCompleted
			completed.Result = "done"
			completed.UpdatedAt = now.Add(3 * time.Second)
			session := cloneStoredSessionState(*resumed.Session)
			session.Revision = resumed.Handle.SessionRevision + 1
			session.LastRunID = completed.ID
			session.UpdatedAt = completed.UpdatedAt
			before := snapshotInMemoryRunStore(t, store)
			err := store.FinishRun(t.Context(), FinishRunRequest{
				Handle: resumed.Handle, Run: completed, Session: &session,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("FinishRun malformed durable approval error=%v want=%v", err, test.want)
			}
			assertInMemoryRunStoreSnapshot(t, store, before)
		})
	}
}

func TestInMemoryStoreFinishRunRejectsActiveLeaseRunModelBindingDriftAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 25, 20, 30, 0, 0, time.UTC)
	store, request := completedMemoryStoreFinishRequest(t, now, "active-lease-binding")
	store.mu.Lock()
	active := store.leases[request.Run.SessionID]
	foreign := store.runs[request.Run.ID]
	foreign.ID = "foreign-active-run"
	foreign.ModelBindingID = ""
	store.runs[foreign.ID] = foreign
	active.RunID = foreign.ID
	store.leases[request.Run.SessionID] = active
	store.mu.Unlock()
	before := snapshotInMemoryRunStore(t, store)

	if err := store.FinishRun(t.Context(), request); !errors.Is(err, ErrModelBindingMismatch) {
		t.Fatalf("FinishRun active lease run binding drift error=%v", err)
	}
	assertInMemoryRunStoreSnapshot(t, store, before)
}

const memoryStoreModelBindingID = "model-binding-v1"

func memoryStoreCreateRequest(runID, sessionID string, now time.Time) CreateRunRequest {
	leaseID := ""
	if sessionID != "" {
		leaseID = "lease-" + runID
	}
	return CreateRunRequest{
		Run: RunRecord{
			ID: runID, SessionID: sessionID, ModelBindingID: memoryStoreModelBindingID,
			Status:    RunStatusRunning,
			Input:     Input{RunID: runID, SessionID: sessionID, User: "run"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: leaseID, LeaseTTL: time.Minute,
	}
}

func runningMemoryStore(t *testing.T, now time.Time, id string) (*InMemoryStore, CreateRunRequest, RunStart) {
	t.Helper()
	store, err := NewInMemoryStoreWithConfig(InMemoryStoreConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	create := memoryStoreCreateRequest("run-"+id, "session-"+id, now)
	var start RunStart
	if err := store.CreateRunV4(t.Context(), create, func(candidate RunStart) error {
		start = candidate
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store, create, start
}

func completedMemoryStoreFinishRequest(t *testing.T, now time.Time, id string) (*InMemoryStore, FinishRunRequest) {
	t.Helper()
	store, create, start := runningMemoryStore(t, now, id)
	completed := create.Run
	completed.Status = RunStatusCompleted
	completed.Result = "done"
	completed.UpdatedAt = now.Add(time.Second)
	session := cloneStoredSessionState(*start.Session)
	session.Revision = start.Handle.SessionRevision + 1
	session.LastRunID = completed.ID
	session.UpdatedAt = completed.UpdatedAt
	return store, FinishRunRequest{Handle: start.Handle, Run: completed, Session: &session}
}

func waitingApprovalMemoryStore(t *testing.T, now time.Time, id string) (*InMemoryStore, ResumeRunRequest) {
	t.Helper()
	store, create, start := runningMemoryStore(t, now, id)
	pending := memoryStorePendingApproval(t, create.Run, create.Run.ModelBindingID, start.Handle.SessionRevision, now.Add(time.Second))
	waiting := create.Run
	waiting.Status = RunStatusWaitingUser
	waiting.PendingApprovalDigest = pending.Digest
	waiting.UpdatedAt = now.Add(time.Second)
	if err := store.FinishRun(t.Context(), FinishRunRequest{
		Handle: start.Handle, Run: waiting, PendingApproval: &pending,
	}); err != nil {
		t.Fatal(err)
	}
	resume := ResumeRunRequest{
		Run: create.Run, LeaseID: "resume-lease-" + id, LeaseTTL: time.Minute,
		InputDigest: pending.Request.Checkpoint.InputDigest,
	}
	resume.Run.UpdatedAt = now.Add(2 * time.Second)
	return store, resume
}

func memoryStorePendingApproval(
	t *testing.T,
	run RunRecord,
	modelBindingID string,
	expectedSessionRevision uint64,
	now time.Time,
) PendingApprovalCommit {
	t.Helper()
	decision := ApprovalDecision{ID: "approval-" + run.ID, Pending: true, Reason: "waiting"}
	auditData, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingApprovalCommit{
		AuthorityVersion: PendingApprovalAuthorityVersion,
		Request: ApprovalRequest{
			Operation: OperationRequest{
				RunID: run.ID, SessionID: run.SessionID, Input: run.Input,
			},
			Reason: "waiting",
			Checkpoint: &ApprovalCheckpoint{
				ModelBindingID: modelBindingID, InputDigest: "input-digest-" + run.ID,
				ExpectedSessionRevision: expectedSessionRevision,
			},
		},
		Decision: decision,
		Audit: ItemRecord{
			ID: "approval-audit-" + run.ID, RunID: run.ID, SessionID: run.SessionID,
			Type: ItemTypeApproval, Data: auditData, CreatedAt: now,
		},
	}
	pending.Digest, err = pendingApprovalAuthorityDigest(pending)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

type inMemoryRunStoreSnapshot struct {
	runs             map[string]RunRecord
	items            map[string]ItemRecord
	itemOrder        map[string][]string
	sessions         map[string]SessionState
	leases           map[string]RunHandle
	leaseGenerations map[string]uint64
	pendingApprovals map[string]PendingApprovalCommit
}

func snapshotInMemoryRunStore(t *testing.T, store *InMemoryStore) inMemoryRunStoreSnapshot {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot := inMemoryRunStoreSnapshot{
		runs:             make(map[string]RunRecord, len(store.runs)),
		items:            make(map[string]ItemRecord, len(store.items)),
		itemOrder:        make(map[string][]string, len(store.itemOrder)),
		sessions:         make(map[string]SessionState, len(store.sessions)),
		leases:           make(map[string]RunHandle, len(store.leases)),
		leaseGenerations: make(map[string]uint64, len(store.leaseGenerations)),
		pendingApprovals: make(map[string]PendingApprovalCommit, len(store.pendingApprovals)),
	}
	for id, run := range store.runs {
		cloned, err := clonePersistentRunRecord(run)
		if err != nil {
			t.Fatalf("clone run %s: %v", id, err)
		}
		snapshot.runs[id] = cloned
	}
	for id, item := range store.items {
		snapshot.items[id] = cloneStoredItemRecord(item)
	}
	for runID, ids := range store.itemOrder {
		snapshot.itemOrder[runID] = append([]string(nil), ids...)
	}
	for id, session := range store.sessions {
		snapshot.sessions[id] = cloneStoredSessionState(session)
	}
	for id, lease := range store.leases {
		snapshot.leases[id] = lease
	}
	for id, generation := range store.leaseGenerations {
		snapshot.leaseGenerations[id] = generation
	}
	for id, pending := range store.pendingApprovals {
		cloned, err := cloneStoredPendingApproval(pending)
		if err != nil {
			t.Fatalf("clone pending approval %s: %v", id, err)
		}
		snapshot.pendingApprovals[id] = cloned
	}
	return snapshot
}

func assertInMemoryRunStoreSnapshot(t *testing.T, store *InMemoryStore, want inMemoryRunStoreSnapshot) {
	t.Helper()
	got := snapshotInMemoryRunStore(t, store)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("in-memory RunStore mutated after rejected transaction\ngot:  %+v\nwant: %+v", got, want)
	}
}
