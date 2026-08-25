package agentruntime

import (
	"errors"
	"testing"
	"time"
)

func TestInMemoryStoreFinishRunRejectsInputRewrite(t *testing.T) {
	now := time.Date(2026, time.August, 25, 8, 0, 0, 0, time.UTC)
	store := NewInMemoryStore()
	request := CreateRunRequest{
		Run: RunRecord{
			ID: "immutable-input-run", Status: RunStatusRunning,
			Input: Input{
				RunID: "immutable-input-run", User: "original",
				IdempotencyKey: "original-key", IdempotencyScope: "tenant",
			},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseTTL: time.Minute,
	}
	var start RunStart
	if err := store.CreateRunV3(t.Context(), request, func(candidate RunStart) error {
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
			ID: "stateless-session-injection", Status: RunStatusRunning,
			Input:     Input{RunID: "stateless-session-injection", User: "run"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseTTL: time.Minute,
	}
	var start RunStart
	if err := store.CreateRunV3(t.Context(), request, func(candidate RunStart) error {
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
			ID: "expired-run", SessionID: "shared-session", Status: RunStatusRunning,
			Input:     Input{RunID: "expired-run", SessionID: "shared-session", User: "first"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "expired-lease", LeaseTTL: time.Second,
	}
	if err := store.CreateRunV3(t.Context(), first, func(RunStart) error { return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	second := CreateRunRequest{
		Run: RunRecord{
			ID: "replacement-run", SessionID: "shared-session", Status: RunStatusRunning,
			Input:     Input{RunID: "replacement-run", SessionID: "shared-session", User: "second"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "replacement-lease", LeaseTTL: time.Second,
	}
	if err := store.CreateRunV3(t.Context(), second, func(RunStart) error { return nil }); err != nil {
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
			ID: "binding-run", SessionID: "binding-session", Status: RunStatusRunning,
			SkillSetID: "skill-set-v1", OperationSetID: "operation-set-v1",
			Input:     Input{RunID: "binding-run", SessionID: "binding-session", User: "bind"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: "binding-lease", LeaseTTL: time.Minute,
	}
	if err := store.CreateRunV3(t.Context(), request, func(start RunStart) error {
		if start.Session == nil {
			t.Fatal("callback session is nil")
		}
		start.Session.ID = "callback-mutated"
		start.Session.SkillSetID = "callback-mutated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession(t.Context(), request.Run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != request.Run.SessionID || session.SkillSetID != request.Run.SkillSetID {
		t.Fatalf("committed session was mutated through callback: %+v", session)
	}
}
