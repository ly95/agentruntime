package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	skillspkg "github.com/ly95/agentruntime/skills"
)

type runtimeSkillSource struct {
	id        string
	artifacts []skillspkg.Artifact
}

type pendingOnlyApprovalResumer struct {
	runID string
}

type finishRequestCapturingStore struct {
	recordingStore
	requests []FinishRunRequest
}

func (store *finishRequestCapturingStore) FinishRun(ctx context.Context, request FinishRunRequest) error {
	store.requests = append(store.requests, request)
	return store.recordingStore.FinishRun(ctx, request)
}

func (resumer pendingOnlyApprovalResumer) ResumeApproval(_ context.Context, runID string) (*ApprovalResume, error) {
	if runID != resumer.runID {
		return nil, nil
	}
	return &ApprovalResume{ID: "legacy-approval", Pending: true}, nil
}

func (source runtimeSkillSource) ID() string { return source.id }

func (source runtimeSkillSource) Resolve(context.Context) ([]skillspkg.Artifact, error) {
	return source.artifacts, nil
}

func runtimeSkillSet(t *testing.T, entries ...struct {
	name        string
	description string
	body        string
	extras      fstest.MapFS
}) (*skillspkg.SkillSet, []fstest.MapFS) {
	t.Helper()
	artifacts := make([]skillspkg.Artifact, 0, len(entries))
	filesystems := make([]fstest.MapFS, 0, len(entries))
	for index, entry := range entries {
		filesystem := fstest.MapFS{
			"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: " + entry.name + "\ndescription: " + entry.description + "\n---\n\n" + entry.body + "\n")},
		}
		for path, file := range entry.extras {
			cloned := *file
			cloned.Data = append([]byte(nil), file.Data...)
			filesystem[path] = &cloned
		}
		filesystems = append(filesystems, filesystem)
		artifacts = append(artifacts, skillspkg.Artifact{
			SourceID: "runtime-test", Locator: "skill-" + string(rune('a'+index)), FS: filesystem,
		})
	}
	set, err := skillspkg.LoadSet(t.Context(), runtimeSkillSource{id: "runtime-test", artifacts: artifacts})
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return set, filesystems
}

func oneRuntimeSkill(t *testing.T, name, body string) (*skillspkg.SkillSet, fstest.MapFS) {
	t.Helper()
	set, filesystems := runtimeSkillSet(t, struct {
		name        string
		description string
		body        string
		extras      fstest.MapFS
	}{name: name, description: "Use " + name + " for matching tasks.", body: body})
	return set, filesystems[0]
}

func TestRuntimeAddsFrozenSkillInstructionsOutsideMCP(t *testing.T) {
	set, filesystem := oneRuntimeSkill(t, "release-notes", "FROZEN_SKILL_SENTINEL: summarize user-visible changes.")
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("skill-response", "done")}}
	runtime, err := NewRuntime(RuntimeConfig{Model: model, Skills: set, MCPInstructions: "MCP_ONLY_SENTINEL"})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	filesystem["SKILL.md"].Data = []byte("---\nname: mutated\ndescription: Mutated.\n---\nMUTATED_SOURCE_SENTINEL")
	if _, err := runtime.Run(t.Context(), Input{User: "write release notes"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests=%d", len(model.requests))
	}
	instructions := model.requests[0].Instructions
	for _, wanted := range []string{"release-notes", "Use release-notes for matching tasks.", "FROZEN_SKILL_SENTINEL", "only when", "MCP_ONLY_SENTINEL"} {
		if !strings.Contains(instructions, wanted) {
			t.Fatalf("instructions missing %q: %s", wanted, instructions)
		}
	}
	if strings.Contains(instructions, "MUTATED_SOURCE_SENTINEL") {
		t.Fatal("runtime reread a mutable Skill source")
	}
	if strings.Contains(runtime.mcp.Instructions(), "FROZEN_SKILL_SENTINEL") {
		t.Fatal("Skill instructions were disguised as MCP instructions")
	}
}

func TestRuntimeTreatsZeroValueSkillSetAsEmpty(t *testing.T) {
	nilRuntime, err := NewRuntime(RuntimeConfig{Model: &scriptedModel{}, MCPInstructions: "MCP_EMPTY_COMPATIBILITY"})
	if err != nil {
		t.Fatal(err)
	}
	emptyRuntime, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{}, Skills: &skillspkg.SkillSet{}, MCPInstructions: "MCP_EMPTY_COMPATIBILITY",
	})
	if err != nil {
		t.Fatalf("NewRuntime with empty SkillSet: %v", err)
	}
	if emptyRuntime.skillSetID != "" || emptyRuntime.baseInstructions != nilRuntime.baseInstructions {
		t.Fatalf("empty SkillSet changed runtime: id=%q\nnil=%q\nempty=%q", emptyRuntime.skillSetID, nilRuntime.baseInstructions, emptyRuntime.baseInstructions)
	}
}

func TestRuntimeSkillInstructionsUseStableSkillOrder(t *testing.T) {
	set, _ := runtimeSkillSet(t,
		struct {
			name        string
			description string
			body        string
			extras      fstest.MapFS
		}{name: "zeta", description: "Zeta tasks.", body: "ZETA_BODY"},
		struct {
			name        string
			description string
			body        string
			extras      fstest.MapFS
		}{name: "alpha", description: "Alpha tasks.", body: "ALPHA_BODY"},
	)
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("ordered", "done")}}
	runtime, err := NewRuntime(RuntimeConfig{Model: model, Skills: set})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "use a skill"}); err != nil {
		t.Fatal(err)
	}
	instructions := model.requests[0].Instructions
	if alpha, zeta := strings.Index(instructions, "ALPHA_BODY"), strings.Index(instructions, "ZETA_BODY"); alpha < 0 || zeta < 0 || alpha > zeta {
		t.Fatalf("unstable skill order: %s", instructions)
	}
}

func TestRuntimeWithoutSkillsPreservesExistingInstructionsExactly(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("no-skills", "done")}}
	runtime, err := NewRuntime(RuntimeConfig{Model: model, MCPInstructions: "domain instructions"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "plain request"}); err != nil {
		t.Fatal(err)
	}
	want := "You are a general-purpose agent. Solve the user's task by reasoning and by calling tools discovered from the connected MCP server when execution is needed.\n" +
		"Before each tool call, send a brief user-visible commentary update describing the immediate next action and why. Keep it factual and concise; do not reveal private chain-of-thought. Skip commentary for trivial direct answers.\n" +
		"Return a normal final response when the task is complete. Do not invent tools or claim a tool succeeded without its result.\n\n" +
		"<mcp_server_instructions>\n" + runtime.mcp.Instructions() + "\n</mcp_server_instructions>"
	if got := model.requests[0].Instructions; got != want {
		t.Fatalf("instructions changed without skills:\nwant=%q\ngot=%q", want, got)
	}
}

func TestRuntimeDoesNotExecuteOrInjectSkillScripts(t *testing.T) {
	set, _ := runtimeSkillSet(t, struct {
		name        string
		description string
		body        string
		extras      fstest.MapFS
	}{
		name: "inert-script", description: "Use for inert script tests.", body: "BODY_ONLY_SENTINEL",
		extras: fstest.MapFS{"scripts/run.sh": &fstest.MapFile{Data: []byte("SCRIPT_MUST_NOT_ENTER_MODEL_OR_EXECUTE")}},
	})
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("inert", "done")}}
	runtime, err := NewRuntime(RuntimeConfig{Model: model, Skills: set})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "test"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.requests[0].Instructions, "BODY_ONLY_SENTINEL") || strings.Contains(model.requests[0].Instructions, "SCRIPT_MUST_NOT_ENTER_MODEL_OR_EXECUTE") {
		t.Fatalf("instructions=%q", model.requests[0].Instructions)
	}
	if len(model.requests[0].Tools) != 0 || len(runtime.operations.Summaries()) != 0 {
		t.Fatalf("Skill files created executable operations: tools=%+v operations=%+v", model.requests[0].Tools, runtime.operations.Summaries())
	}
}

func TestRuntimeSkillInstructionsSurviveContextCompaction(t *testing.T) {
	set, _ := oneRuntimeSkill(t, "compaction", "COMPACTION_SKILL_SENTINEL")
	counter := &testTokenCounter{
		countRequest: func(request ModelRequest) (int, error) {
			if !strings.Contains(request.Instructions, "COMPACTION_SKILL_SENTINEL") {
				t.Fatalf("token counter request lost skill instructions: %q", request.Instructions)
			}
			if requestHasCheckpoint(request) {
				return 40, nil
			}
			return 100, nil
		},
		countText: func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(request ContextCompactionRequest) (ContextSummary, error) {
		for _, item := range request.Items {
			if strings.Contains(item.Text, "COMPACTION_SKILL_SENTINEL") {
				t.Fatal("skill instructions leaked into compactable transcript")
			}
		}
		return ContextSummary{Summary: "compacted history"}, nil
	}}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("compacted", "done")}}
	store := &recordingStore{}
	seed := seedContextSession(store, "skill-compaction", []ModelInputItem{
		{Type: ModelInputUserMessage, Text: "old"},
		{Type: ModelInputAssistantOutput, OutputType: ModelOutputMessage, Text: "answer", Raw: json.RawMessage(`{"id":"answer"}`)},
	}, nil)
	seed.SkillSetID = set.ID()
	store.sessions[seed.ID] = seed
	operations := NewOperationRegistry()
	if err := operations.Register(operation("read_context", OperationEffectRead)); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, Skills: set, RunStore: store, Operations: operations,
		Policy: allowPolicy(), Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{Output: json.RawMessage(`{}`)}, nil
		}),
		ContextWindow: contextWindowForTest(counter, compactor),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(t.Context(), Input{User: "current", SessionID: seed.ID}); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 || !requestHasCheckpoint(model.requests[0]) || !strings.Contains(model.requests[0].Instructions, "COMPACTION_SKILL_SENTINEL") {
		t.Fatalf("model requests=%+v", model.requests)
	}
}

func TestRuntimePersistsAndValidatesSkillSetID(t *testing.T) {
	setA, _ := oneRuntimeSkill(t, "set-a", "SET_A_BODY")
	setB, _ := oneRuntimeSkill(t, "set-b", "SET_B_BODY")
	store := &recordingStore{}
	firstModel := &scriptedModel{responses: []*ModelResponse{messageResponse("first", "done")}}
	first, err := NewRuntime(RuntimeConfig{Model: firstModel, Skills: setA, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(t.Context(), Input{User: "first", SessionID: "skill-session"}); err != nil {
		t.Fatal(err)
	}
	if got := store.sessions["skill-session"].SkillSetID; got != setA.ID() {
		t.Fatalf("persisted SkillSetID=%q, want %q", got, setA.ID())
	}

	sameModel := &scriptedModel{responses: []*ModelResponse{messageResponse("same", "done")}}
	same, err := NewRuntime(RuntimeConfig{Model: sameModel, Skills: setA, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := same.Run(t.Context(), Input{User: "same", SessionID: "skill-session"}); err != nil {
		t.Fatalf("same SkillSet resume: %v", err)
	}

	before := store.sessions["skill-session"]
	differentModel := &scriptedModel{}
	different, err := NewRuntime(RuntimeConfig{Model: differentModel, Skills: setB, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = different.Run(t.Context(), Input{User: "different", SessionID: "skill-session"})
	if !errors.Is(err, ErrSkillSetMismatch) || len(differentModel.requests) != 0 || errorCode(err) != "skill_set_mismatch" {
		t.Fatalf("error=%v requests=%d code=%q", err, len(differentModel.requests), errorCode(err))
	}
	after := store.sessions["skill-session"]
	if after.SkillSetID != before.SkillSetID || after.Revision != before.Revision || len(after.Transcript) != len(before.Transcript) {
		t.Fatalf("mismatch changed session: before=%+v after=%+v", before, after)
	}
}

func TestRuntimeBindsSkillSetBeforeFirstSessionSnapshot(t *testing.T) {
	setA, _ := oneRuntimeSkill(t, "first-binding-a", "FIRST_BINDING_A")
	setB, _ := oneRuntimeSkill(t, "first-binding-b", "FIRST_BINDING_B")
	store := &recordingStore{}
	counter := &testTokenCounter{
		countRequest: func(ModelRequest) (int, error) { return 101, nil },
		countText:    func(string) (int, error) { return 10, nil },
	}
	compactor := &testContextCompactor{compact: func(ContextCompactionRequest) (ContextSummary, error) {
		t.Fatal("a first-turn context overflow must fail before compaction")
		return ContextSummary{}, nil
	}}
	firstModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	first, err := NewRuntime(RuntimeConfig{
		Model: firstModel, Skills: setA, RunStore: store,
		ContextWindow: contextWindowForTest(counter, compactor),
	})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "first-binding-session"
	if _, err := first.Run(t.Context(), Input{User: "too large", SessionID: sessionID}); !errors.Is(err, ErrContextLimitExceeded) {
		t.Fatalf("first run error=%v", err)
	}
	if len(firstModel.requests) != 0 {
		t.Fatalf("first model requests=%d", len(firstModel.requests))
	}
	binding, exists := store.sessions[sessionID]
	if !exists || binding.SkillSetID != setA.ID() || binding.Revision != 0 || len(binding.Transcript) != 0 {
		t.Fatalf("binding after failed first run=%+v exists=%v", binding, exists)
	}
	beforeRuns := len(store.runs)
	beforeGeneration := store.leaseGenerations[sessionID]

	mismatchModel := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	mismatch, err := NewRuntime(RuntimeConfig{Model: mismatchModel, Skills: setB, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatch.Run(t.Context(), Input{User: "switch", SessionID: sessionID}); !errors.Is(err, ErrSkillSetMismatch) {
		t.Fatalf("mismatch error=%v", err)
	}
	if len(mismatchModel.requests) != 0 || len(store.runs) != beforeRuns || store.leaseGenerations[sessionID] != beforeGeneration {
		t.Fatalf("mismatch mutated store: requests=%d runs=%d generation=%d", len(mismatchModel.requests), len(store.runs), store.leaseGenerations[sessionID])
	}
	if after := store.sessions[sessionID]; after.SkillSetID != binding.SkillSetID || after.Revision != binding.Revision {
		t.Fatalf("mismatch changed binding: before=%+v after=%+v", binding, after)
	}

	recoveryModel := &scriptedModel{responses: []*ModelResponse{messageResponse("recovered", "done")}}
	recovery, err := NewRuntime(RuntimeConfig{Model: recoveryModel, Skills: setA, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovery.Run(t.Context(), Input{User: "retry", SessionID: sessionID})
	if err != nil || result.Status != RunStatusCompleted || len(recoveryModel.requests) != 1 {
		t.Fatalf("recovery result=%+v error=%v requests=%d", result, err, len(recoveryModel.requests))
	}
}

func TestRuntimeRejectsMissingBeginRunSkillBinding(t *testing.T) {
	set, _ := oneRuntimeSkill(t, "required-binding", "REQUIRED_BINDING")
	store := &hiddenBeginSessionStore{}
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "bad")}}
	runtime, err := NewRuntime(RuntimeConfig{Model: model, Skills: set, RunStore: store})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Run(t.Context(), Input{User: "run", SessionID: "hidden-binding-session"})
	if !errors.Is(err, ErrSessionConflict) || len(model.requests) != 0 {
		t.Fatalf("error=%v requests=%d", err, len(model.requests))
	}
	if len(store.runs) != 0 || len(store.sessions) != 0 || len(store.leases) != 0 || len(store.leaseGenerations) != 0 {
		t.Fatalf("rejected pre-commit binding mutated store: runs=%+v sessions=%+v leases=%+v generations=%+v", store.runs, store.sessions, store.leases, store.leaseGenerations)
	}
}

func TestRunStoreRejectsFinishRunSkillBindingRewrite(t *testing.T) {
	store := &recordingStore{sessions: map[string]SessionState{
		"binding-session": {ID: "binding-session", SkillSetID: "set-a"},
	}}
	run := RunRecord{ID: "binding-run", SessionID: "binding-session", SkillSetID: "set-a", Status: RunStatusRunning}
	begun, err := createRunForTest(t.Context(), store, CreateRunRequest{Run: run, LeaseID: "binding-lease", LeaseTTL: defaultSessionLeaseTTL})
	if err != nil {
		t.Fatal(err)
	}
	rewritten := run
	rewritten.SkillSetID = "set-b"
	rewritten.Status = RunStatusCompleted
	err = store.FinishRun(t.Context(), FinishRunRequest{
		Handle: begun.Handle, Run: rewritten,
		Session: &SessionState{ID: run.SessionID, SkillSetID: "set-b", Revision: 1},
	})
	if !errors.Is(err, ErrSkillSetMismatch) {
		t.Fatalf("FinishRun error=%v", err)
	}
	if binding := store.sessions[run.SessionID]; binding.SkillSetID != "set-a" || binding.Revision != 0 {
		t.Fatalf("binding changed: %+v", binding)
	}
	if len(store.runs) != 1 || store.runs[0].Status != RunStatusRunning || store.runs[0].SkillSetID != "set-a" {
		t.Fatalf("runs changed: %+v", store.runs)
	}
}

func TestRuntimeRejectsSkillAdditionAndRemovalFromExistingSession(t *testing.T) {
	set, _ := oneRuntimeSkill(t, "bound", "BOUND_BODY")
	tests := []struct {
		name   string
		first  *skillspkg.SkillSet
		second *skillspkg.SkillSet
	}{
		{name: "addition", first: nil, second: set},
		{name: "removal", first: set, second: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &recordingStore{}
			first, err := NewRuntime(RuntimeConfig{Model: &scriptedModel{responses: []*ModelResponse{messageResponse("first", "done")}}, Skills: test.first, RunStore: store})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.Run(t.Context(), Input{User: "first", SessionID: "session"}); err != nil {
				t.Fatal(err)
			}
			model := &scriptedModel{}
			second, err := NewRuntime(RuntimeConfig{Model: model, Skills: test.second, RunStore: store})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := second.Run(t.Context(), Input{User: "second", SessionID: "session"}); !errors.Is(err, ErrSkillSetMismatch) || len(model.requests) != 0 {
				t.Fatalf("error=%v requests=%d", err, len(model.requests))
			}
		})
	}
}

func TestRuntimeAnchorsSkillSetAcrossApprovalPauseAndResume(t *testing.T) {
	setA, _ := oneRuntimeSkill(t, "approval-a", "APPROVAL_A_BODY")
	setB, _ := oneRuntimeSkill(t, "approval-b", "APPROVAL_B_BODY")
	ops := NewOperationRegistry()
	if err := ops.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
	})
	approver := &resumableApprover{}
	store := &recordingStore{}
	executor := OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{Output: json.RawMessage(`{"applied":true}`)}, nil
	})
	firstModel := &scriptedModel{responses: []*ModelResponse{
		callResponse("approval-call", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	first, err := NewRuntime(RuntimeConfig{
		Model: firstModel, Skills: setA, Operations: ops, Policy: policy, Executor: executor,
		Verifier: confirmingVerifier(), Approver: approver, ApprovalResumer: approver, RunStore: store, Executions: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := Input{RunID: "skill-approval-run", SessionID: "skill-approval-session", User: "apply", IdempotencyKey: "skill-approval"}
	result, err := first.Run(t.Context(), input)
	if err != nil || result.Status != RunStatusWaitingUser {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if got := store.sessions[input.SessionID].SkillSetID; got != setA.ID() {
		t.Fatalf("waiting session SkillSetID=%q, want %q", got, setA.ID())
	}
	approver.resolve(true, "approved")

	mismatchModel := &scriptedModel{}
	mismatch, err := NewRuntime(RuntimeConfig{
		Model: mismatchModel, Skills: setB, Operations: ops, Policy: policy, Executor: executor,
		Verifier: confirmingVerifier(), Approver: approver, ApprovalResumer: approver, RunStore: store, Executions: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatch.Run(t.Context(), input); !errors.Is(err, ErrSkillSetMismatch) || len(mismatchModel.requests) != 0 {
		t.Fatalf("error=%v requests=%d", err, len(mismatchModel.requests))
	}
	store.mu.Lock()
	statusAfterMismatch := store.runs[0].Status
	store.mu.Unlock()
	if statusAfterMismatch != RunStatusWaitingUser {
		t.Fatalf("mismatched resume consumed waiting run: status=%q", statusAfterMismatch)
	}

	correctModel := &scriptedModel{responses: []*ModelResponse{messageResponse("approval-complete", "done")}}
	correct, err := NewRuntime(RuntimeConfig{
		Model: correctModel, Skills: setA, Operations: ops, Policy: policy, Executor: executor,
		Verifier: confirmingVerifier(), Approver: approver, ApprovalResumer: approver, RunStore: store, Executions: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = correct.Run(t.Context(), input)
	if err != nil || result.Status != RunStatusCompleted || len(correctModel.requests) != 1 {
		t.Fatalf("correct resume result=%+v error=%v requests=%d", result, err, len(correctModel.requests))
	}
}

func TestRuntimeRejectsUnanchoredLegacyApprovalBeforeBindingClaim(t *testing.T) {
	set, _ := oneRuntimeSkill(t, "new-skill", "NEW_SKILL_BODY")
	input := Input{RunID: "legacy-waiting-run", SessionID: "legacy-waiting-session", User: "poll approval"}
	store := &recordingStore{runs: []RunRecord{{
		ID: input.RunID, SessionID: input.SessionID, Status: RunStatusWaitingUser, Input: input,
	}}}
	resumer := pendingOnlyApprovalResumer{runID: input.RunID}
	mismatched, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{}, Skills: set, RunStore: store, ApprovalResumer: resumer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatched.Run(t.Context(), input); !errors.Is(err, ErrSkillSetMismatch) {
		t.Fatalf("legacy mismatch error=%v", err)
	}
	store.mu.Lock()
	statusAfterMismatch := store.runs[0].Status
	_, rebound := store.sessions[input.SessionID]
	store.mu.Unlock()
	if statusAfterMismatch != RunStatusWaitingUser || rebound {
		t.Fatalf("legacy mismatch changed waiting state: status=%q rebound=%v", statusAfterMismatch, rebound)
	}

	correct, err := NewRuntime(RuntimeConfig{
		Model: &scriptedModel{}, RunStore: store, ApprovalResumer: resumer,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := correct.Run(t.Context(), input)
	if !errors.Is(err, ErrOperationPlanChanged) || result != nil {
		t.Fatalf("unanchored legacy approval result=%+v error=%v", result, err)
	}
	store.mu.Lock()
	after := store.runs[0]
	_, leased := store.leases[input.SessionID]
	generation := store.leaseGenerations[input.SessionID]
	_, rebound = store.sessions[input.SessionID]
	store.mu.Unlock()
	if after.Status != RunStatusWaitingUser || after.PendingApprovalDigest != "" || leased || generation != 0 || rebound {
		t.Fatalf("unanchored legacy resume mutated state: run=%+v leased=%v generation=%d rebound=%v", after, leased, generation, rebound)
	}
}

func TestRuntimeWithoutSkillsPersistsOperationBindingWhileWaiting(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("no-skill-approval", ToolCall{ID: "call-1", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	operations := NewOperationRegistry()
	if err := operations.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	approver := &resumableApprover{}
	store := &finishRequestCapturingStore{}
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, Operations: operations,
		Policy: OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
			return PolicyDecision{Action: PolicyRequireApproval, Reason: "write operation"}, nil
		}),
		Executor: OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("pending operation must not execute")
			return OperationResult{}, nil
		}),
		Verifier: confirmingVerifier(), Approver: approver, ApprovalResumer: approver,
		RunStore: store, Executions: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := Input{RunID: "no-skill-wait-run", SessionID: "no-skill-wait-session", User: "apply", IdempotencyKey: "no-skill-wait"}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := runtime.Run(t.Context(), input)
		if err != nil || result.Status != RunStatusWaitingUser {
			t.Fatalf("attempt=%d result=%+v error=%v", attempt, result, err)
		}
	}
	if len(store.requests) != 2 {
		t.Fatalf("FinishRun requests=%d, want 2", len(store.requests))
	}
	if first := store.requests[0].Session; first == nil || first.SkillSetID != "" || first.OperationSetID == "" {
		t.Fatalf("initial FinishRun operation binding=%+v", first)
	}
	if polled := store.requests[1].Session; polled != nil {
		t.Fatalf("pending poll advanced the approval-bound session: %+v", polled)
	}
	store.mu.Lock()
	persistedSession, persisted := store.sessions[input.SessionID]
	pending := store.pendingApprovals[input.RunID]
	store.mu.Unlock()
	if !persisted {
		t.Fatal("approval polling did not preserve the operation-set binding")
	}
	if pending.Request.Checkpoint == nil || pending.Request.Checkpoint.ExpectedSessionRevision != persistedSession.Revision {
		t.Fatalf("pending revision=%+v session revision=%d", pending.Request.Checkpoint, persistedSession.Revision)
	}
}

func TestSkillSetIDJSONOmitsEmptyCompatibilityBindings(t *testing.T) {
	for _, test := range []struct {
		name  string
		empty any
		bound any
	}{
		{name: "run", empty: RunRecord{}, bound: RunRecord{SkillSetID: "skill-set"}},
		{name: "session", empty: SessionState{}, bound: SessionState{SkillSetID: "skill-set"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			emptyJSON, err := json.Marshal(test.empty)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(emptyJSON), "SkillSetID") {
				t.Fatalf("empty binding changed JSON shape: %s", emptyJSON)
			}
			boundJSON, err := json.Marshal(test.bound)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(boundJSON), `"SkillSetID":"skill-set"`) {
				t.Fatalf("active binding missing from JSON: %s", boundJSON)
			}
		})
	}
}

var _ fs.FS = fstest.MapFS{}
