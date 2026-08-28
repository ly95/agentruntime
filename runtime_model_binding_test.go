package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type unboundModelBindingProbe struct {
	responses []*ModelResponse
	calls     int
}

func (model *unboundModelBindingProbe) Complete(context.Context, ModelRequest) (*ModelResponse, error) {
	model.calls++
	if len(model.responses) == 0 {
		return nil, errors.New("unboundModelBindingProbe: no response")
	}
	response := model.responses[0]
	model.responses = model.responses[1:]
	return response, nil
}

type unstableModelBindingProbe struct {
	Model
	bindings    [2]ModelBinding
	bindingCall int
}

func (model *unstableModelBindingProbe) Binding() ModelBinding {
	index := model.bindingCall
	if index >= len(model.bindings) {
		index = len(model.bindings) - 1
	}
	model.bindingCall++
	return model.bindings[index]
}

type driftingModelBindingProbe struct {
	binding         ModelBinding
	driftedBinding  ModelBinding
	driftOnComplete bool
	response        *ModelResponse
	calls           int
}

type mutableBoundModel struct {
	Model
	binding ModelBinding
}

func (model *mutableBoundModel) Binding() ModelBinding { return model.binding }

func (model *driftingModelBindingProbe) Binding() ModelBinding { return model.binding }

func (model *driftingModelBindingProbe) Complete(context.Context, ModelRequest) (*ModelResponse, error) {
	model.calls++
	if model.driftOnComplete {
		model.binding = model.driftedBinding
	}
	return model.response, nil
}

func TestNewRuntimeValidatesDurableModelBindingBeforeRegistryFreeze(t *testing.T) {
	changedBinding := defaultTestModelBinding
	changedBinding.Model += "-changed"
	invalidBinding := defaultTestModelBinding
	invalidBinding.Provider = ""

	tests := []struct {
		name       string
		model      func() Model
		wantDetail string
	}{
		{
			name: "unbound model",
			model: func() Model {
				return &unboundModelBindingProbe{}
			},
			wantDetail: "must implement BoundModel",
		},
		{
			name: "invalid binding",
			model: func() Model {
				return boundTestModel{Model: &unboundModelBindingProbe{}, binding: invalidBinding}
			},
			wantDetail: "invalid durable model binding",
		},
		{
			name: "unstable binding",
			model: func() Model {
				return &unstableModelBindingProbe{
					Model:    &unboundModelBindingProbe{},
					bindings: [2]ModelBinding{defaultTestModelBinding, changedBinding},
				}
			},
			wantDetail: "not stable during runtime construction",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := NewOperationRegistry()
			runtime, err := NewRuntime(RuntimeConfig{
				Model: test.model(), Operations: operations, RunStore: &recordingStore{},
			})
			if err == nil || runtime != nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("NewRuntime runtime=%v error=%v, want detail %q", runtime, err, test.wantDetail)
			}
			if err := operations.Register(operation("registry_remains_mutable", OperationEffectRead)); err != nil {
				t.Fatalf("binding validation froze registry before failing: %v", err)
			}
		})
	}
}

func TestDurableRuntimeRejectsModelBindingDriftAtEveryCallBoundary(t *testing.T) {
	drifted := defaultTestModelBinding
	drifted.Model += "-drifted"
	for _, test := range []struct {
		name            string
		driftBeforeRun  bool
		driftOnComplete bool
		wantModelCalls  int
		wantRequests    int
	}{
		{name: "before Complete", driftBeforeRun: true},
		{name: "during Complete", driftOnComplete: true, wantModelCalls: 1, wantRequests: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := &driftingModelBindingProbe{
				binding: defaultTestModelBinding, driftedBinding: drifted,
				driftOnComplete: test.driftOnComplete,
				response:        messageResponse("binding-drift-response", "must not commit"),
			}
			store := &recordingStore{}
			beforeStore := modelBindingRecordingStoreSnapshot(t, store)
			runtime, err := NewRuntime(RuntimeConfig{Model: model, RunStore: store})
			if err != nil {
				t.Fatalf("NewRuntime: %v", err)
			}
			if test.driftBeforeRun {
				model.binding = drifted
			}
			sessionID := "binding-drift-session-" + strings.ReplaceAll(test.name, " ", "-")
			_, err = runtime.Run(t.Context(), Input{User: "test binding drift", SessionID: sessionID})
			if !errors.Is(err, ErrModelBindingMismatch) {
				t.Fatalf("Run error=%v, want ErrModelBindingMismatch", err)
			}
			store.mu.Lock()
			requestAudits := 0
			responseAudits := 0
			for _, item := range store.items {
				switch item.Type {
				case ItemTypeModelRequest:
					requestAudits++
				case ItemTypeModelResponse:
					responseAudits++
				}
			}
			session := store.sessions[sessionID]
			store.mu.Unlock()
			if model.calls != test.wantModelCalls || requestAudits != test.wantRequests || responseAudits != 0 {
				t.Fatalf("model calls=%d request audits=%d response audits=%d", model.calls, requestAudits, responseAudits)
			}
			if session.LastResponseID != "" || len(session.Transcript) != 0 {
				t.Fatalf("binding drift advanced durable transcript: %+v", session)
			}
			if test.driftBeforeRun {
				afterStore := modelBindingRecordingStoreSnapshot(t, store)
				if !bytes.Equal(afterStore, beforeStore) {
					t.Fatalf("pre-run binding drift mutated the store\nbefore: %s\nafter:  %s", beforeStore, afterStore)
				}
			}
		})
	}
}

func TestNewRuntimeAllowsUnboundModelWithoutRunStore(t *testing.T) {
	model := &unboundModelBindingProbe{responses: []*ModelResponse{messageResponse("unbound-response", "done")}}
	runtime, err := NewRuntime(RuntimeConfig{Model: model})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	result, err := runtime.Run(t.Context(), Input{User: "ordinary stateless model"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != RunStatusCompleted || result.Output != "done" || model.calls != 1 {
		t.Fatalf("result=%+v model calls=%d", result, model.calls)
	}
}

func TestDurableRuntimePersistsOnlyModelBindingDigest(t *testing.T) {
	binding := ModelBinding{
		Provider:            "provider-persistence-marker",
		Model:               "model-persistence-marker",
		EndpointClass:       "endpoint-class-plaintext-must-not-persist",
		CredentialPrincipal: "credential-principal-plaintext-must-not-persist",
		AdapterVersion:      "adapter-persistence-marker-v1",
	}
	bindingID, err := binding.ID()
	if err != nil {
		t.Fatal(err)
	}
	model := &unboundModelBindingProbe{responses: []*ModelResponse{messageResponse("persisted-binding-response", "done")}}
	store := &recordingStore{}
	runtime := newTestRuntime(
		t,
		boundTestModel{Model: model, binding: binding},
		nil, nil, nil, nil, nil, store,
	)
	input := Input{
		RunID: "persisted-binding-run", SessionID: "persisted-binding-session", User: "persist digest only",
	}
	result, err := runtime.Run(t.Context(), input)
	if err != nil || result.Status != RunStatusCompleted {
		t.Fatalf("Run result=%+v error=%v", result, err)
	}

	store.mu.Lock()
	if len(store.runs) != 1 {
		store.mu.Unlock()
		t.Fatalf("stored runs=%d, want 1", len(store.runs))
	}
	run := store.runs[0]
	session, ok := store.sessions[input.SessionID]
	store.mu.Unlock()
	if !ok {
		t.Fatalf("session %q was not persisted", input.SessionID)
	}
	if run.ModelBindingID != bindingID || session.ModelBindingID != bindingID {
		t.Fatalf("persisted binding IDs: run=%q session=%q want=%q", run.ModelBindingID, session.ModelBindingID, bindingID)
	}

	payload, err := json.Marshal(struct {
		Run     RunRecord
		Session SessionState
	}{Run: run, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	for _, plaintext := range []string{binding.EndpointClass, binding.CredentialPrincipal} {
		if bytes.Contains(payload, []byte(plaintext)) {
			t.Fatalf("persisted RunRecord/SessionState leaked binding plaintext %q: %s", plaintext, payload)
		}
	}
	if !bytes.Contains(payload, []byte(bindingID)) {
		t.Fatalf("persisted records omit binding digest %q: %s", bindingID, payload)
	}
}

type modelBindingApprovalAuthority struct {
	inner       resumableApprover
	resumeCalls int
}

func (authority *modelBindingApprovalAuthority) RequestApproval(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	return authority.inner.RequestApproval(ctx, request)
}

func (authority *modelBindingApprovalAuthority) ResumeApproval(ctx context.Context, runID string) (*ApprovalResume, error) {
	authority.resumeCalls++
	return authority.inner.ResumeApproval(ctx, runID)
}

func (authority *modelBindingApprovalAuthority) resolve(approved bool, reason string) {
	authority.inner.resolve(approved, reason)
}

type modelBindingPhaseSnapshot struct {
	modelCalls       int
	policyCalls      int
	approvalRequests int
	approvalResumes  int
	executorCalls    int
}

type modelBindingApprovalFixture struct {
	model         *scriptedModel
	operations    *OperationRegistry
	policy        OperationPolicy
	executor      OperationExecutor
	authority     *modelBindingApprovalAuthority
	store         *recordingStore
	runtime       *Runtime
	input         Input
	policyCalls   int
	executorCalls int
}

func newModelBindingApprovalFixture(t *testing.T, terminal bool) *modelBindingApprovalFixture {
	t.Helper()
	fixture := &modelBindingApprovalFixture{
		model: &scriptedModel{responses: []*ModelResponse{
			callResponse("binding-approval-response", ToolCall{
				ID: "binding-approval-call", Name: "binding_approval_write", Input: json.RawMessage(`{}`),
			}),
		}},
		operations: NewOperationRegistry(),
		authority:  &modelBindingApprovalAuthority{},
		store:      &recordingStore{},
		input: Input{
			RunID: "binding-approval-run", SessionID: "binding-approval-session",
			User: "apply", IdempotencyKey: "binding-approval-key",
		},
	}
	write := operation("binding_approval_write", OperationEffectWrite)
	write.Terminal = terminal
	if err := fixture.operations.Register(write); err != nil {
		t.Fatal(err)
	}
	fixture.policy = OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		fixture.policyCalls++
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "confirm binding-bound write"}, nil
	})
	fixture.executor = OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		fixture.executorCalls++
		result := OperationResult{Output: json.RawMessage(`{"applied":true}`)}
		if terminal {
			result.FinalResponse = "applied"
		}
		return result, nil
	})
	fixture.runtime = fixture.newRuntime(t, defaultTestModelBinding, fixture.store)
	result, err := fixture.runtime.Run(t.Context(), fixture.input)
	if err != nil || result.Status != RunStatusWaitingUser {
		t.Fatalf("seed waiting approval result=%+v error=%v", result, err)
	}
	if got := fixture.phases(); got != (modelBindingPhaseSnapshot{
		modelCalls: 1, policyCalls: 1, approvalRequests: 1,
	}) {
		t.Fatalf("seed phase counts=%+v", got)
	}
	return fixture
}

func (fixture *modelBindingApprovalFixture) newRuntime(t *testing.T, binding ModelBinding, store RunStore) *Runtime {
	t.Helper()
	return newTestRuntime(
		t,
		boundTestModel{Model: fixture.model, binding: binding},
		fixture.operations, fixture.policy, fixture.executor, confirmingVerifier(), fixture.authority, store,
	)
}

func (fixture *modelBindingApprovalFixture) phases() modelBindingPhaseSnapshot {
	return modelBindingPhaseSnapshot{
		modelCalls:       len(fixture.model.requests),
		policyCalls:      fixture.policyCalls,
		approvalRequests: fixture.authority.inner.calls,
		approvalResumes:  fixture.authority.resumeCalls,
		executorCalls:    fixture.executorCalls,
	}
}

func modelBindingRecordingStoreSnapshot(t *testing.T, store *recordingStore) []byte {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	payload, err := json.Marshal(struct {
		Runs             []RunRecord
		Items            []ItemRecord
		Sessions         map[string]SessionState
		Leases           map[string]RunHandle
		LeaseGenerations map[string]uint64
		Executions       map[string]OperationExecutionRecord
		Transitions      map[string][]OperationExecutionTransition
		Plans            map[string]map[uint64]OperationPlanBatch
		Seals            map[string]OperationPlanSeal
		PendingApprovals map[string]PendingApprovalCommit
		Completed        []RunRecord
		Failed           []RunRecord
	}{
		Runs: store.runs, Items: store.items, Sessions: store.sessions,
		Leases: store.leases, LeaseGenerations: store.leaseGenerations,
		Executions: store.executions, Transitions: store.transitions,
		Plans: store.plans, Seals: store.seals, PendingApprovals: store.pendingApprovals,
		Completed: store.completed, Failed: store.failed,
	})
	if err != nil {
		t.Fatalf("snapshot recordingStore: %v", err)
	}
	return payload
}

func TestDurableRuntimeRejectsResumeBindingDriftBeforeDependenciesWithoutStoreMutation(t *testing.T) {
	driftedBinding := defaultTestModelBinding
	driftedBinding.EndpointClass += "-drifted"
	driftedBindingID, err := driftedBinding.ID()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		runtimeBinding ModelBinding
		resumeStore    func(*modelBindingApprovalFixture) RunStore
	}{
		{
			name:           "session state",
			runtimeBinding: defaultTestModelBinding,
			resumeStore: func(fixture *modelBindingApprovalFixture) RunStore {
				return &corruptingResumeStartAdapter{
					recordingStore: fixture.store,
					corrupt: func(resumed *ResumedRun) {
						if resumed.Session == nil {
							resumed.Session = &SessionState{ModelBindingID: driftedBindingID}
							return
						}
						session := *resumed.Session
						session.ModelBindingID = driftedBindingID
						resumed.Session = &session
					},
				}
			},
		},
		{
			name:           "waiting run",
			runtimeBinding: driftedBinding,
			resumeStore: func(fixture *modelBindingApprovalFixture) RunStore {
				return fixture.store
			},
		},
		{
			name:           "approval checkpoint",
			runtimeBinding: defaultTestModelBinding,
			resumeStore: func(fixture *modelBindingApprovalFixture) RunStore {
				return &corruptingResumeStartAdapter{
					recordingStore: fixture.store,
					corrupt: func(resumed *ResumedRun) {
						if resumed.PendingApproval == nil {
							resumed.PendingApproval = &PendingApprovalCommit{Request: ApprovalRequest{
								Checkpoint: &ApprovalCheckpoint{ModelBindingID: driftedBindingID},
							}}
							return
						}
						pending := *resumed.PendingApproval
						request := pending.Request
						if request.Checkpoint == nil {
							request.Checkpoint = &ApprovalCheckpoint{ModelBindingID: driftedBindingID}
						} else {
							checkpoint := *request.Checkpoint
							checkpoint.ModelBindingID = driftedBindingID
							request.Checkpoint = &checkpoint
						}
						pending.Request = request
						resumed.PendingApproval = &pending
					},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newModelBindingApprovalFixture(t, false)
			resumeRuntime := fixture.newRuntime(t, test.runtimeBinding, test.resumeStore(fixture))
			beforeStore := modelBindingRecordingStoreSnapshot(t, fixture.store)
			beforePhases := fixture.phases()

			result, err := resumeRuntime.ResumeApproval(t.Context(), fixture.input)
			if !errors.Is(err, ErrModelBindingMismatch) || result != nil {
				t.Fatalf("ResumeApproval result=%+v error=%v, want ErrModelBindingMismatch", result, err)
			}
			if got := fixture.phases(); got != beforePhases {
				t.Fatalf("binding drift reached model/policy/approval/executor: before=%+v after=%+v", beforePhases, got)
			}
			afterStore := modelBindingRecordingStoreSnapshot(t, fixture.store)
			if !bytes.Equal(afterStore, beforeStore) {
				t.Fatalf("store mutated after rejected binding drift\nbefore: %s\nafter:  %s", beforeStore, afterStore)
			}
		})
	}
}

func TestRuntimeRejectsCurrentModelBindingDriftFromApprovalResumerBeforeExecution(t *testing.T) {
	fixture := newModelBindingApprovalFixture(t, true)
	model := &mutableBoundModel{Model: fixture.model, binding: defaultTestModelBinding}
	resumeRuntime := newTestRuntime(
		t,
		model,
		fixture.operations, fixture.policy, fixture.executor,
		confirmingVerifier(), fixture.authority, fixture.store,
	)
	driftedBinding := defaultTestModelBinding
	driftedBinding.CredentialPrincipal += "-drifted"
	fixture.authority.resolve(true, "approved")
	fixture.authority.inner.mutateResume = func(*ApprovalResume) {
		model.binding = driftedBinding
	}
	before := fixture.phases()

	result, err := resumeRuntime.ResumeApproval(t.Context(), fixture.input)
	if !errors.Is(err, ErrModelBindingMismatch) {
		t.Errorf("ResumeApproval error=%v, want ErrModelBindingMismatch", err)
	}
	if result != nil {
		t.Errorf("ResumeApproval result=%+v, want nil", result)
	}
	want := before
	want.approvalResumes++
	if got := fixture.phases(); got != want {
		t.Fatalf("model binding drift crossed a side-effect boundary: got=%+v want=%+v", got, want)
	}
}

func TestRuntimeRejectsTerminalApprovalResumeBindingDriftBeforeExecution(t *testing.T) {
	fixture := newModelBindingApprovalFixture(t, true)
	driftedBinding := defaultTestModelBinding
	driftedBinding.CredentialPrincipal += "-drifted"
	driftedBindingID, err := driftedBinding.ID()
	if err != nil {
		t.Fatal(err)
	}
	fixture.authority.resolve(true, "approved")
	fixture.authority.inner.mutateResume = func(resume *ApprovalResume) {
		checkpoint := *resume.Checkpoint
		checkpoint.ModelBindingID = driftedBindingID
		resume.Checkpoint = &checkpoint
	}
	before := fixture.phases()

	result, err := fixture.runtime.ResumeApproval(t.Context(), fixture.input)
	if !errors.Is(err, ErrModelBindingMismatch) {
		t.Errorf("ResumeApproval error=%v, want ErrModelBindingMismatch", err)
	}
	if result != nil {
		t.Errorf("ResumeApproval result=%+v, want nil", result)
	}
	want := before
	want.approvalResumes++
	if got := fixture.phases(); got != want {
		t.Fatalf("terminal resume binding drift crossed a side-effect boundary: got=%+v want=%+v", got, want)
	}
}
