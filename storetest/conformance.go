// Package storetest provides black-box conformance suites for agentruntime
// persistence adapters. Hosts call these functions from their adapter tests.
package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	agent "github.com/ly95/agentruntime"
)

// RunStoreFactory returns a fresh, empty RunStore for each subtest.
type RunStoreFactory func() agent.RunStore

// ExecutionStoreFactory returns a fresh, empty ExecutionStore for each subtest.
type ExecutionStoreFactory func() agent.ExecutionStore

// RunRunStoreConformance checks transaction callbacks, pending approval and
// model binding authority, identity, lease/session fencing, defensive copies,
// and terminal commits.
func RunRunStoreConformance(t *testing.T, factory RunStoreFactory) {
	t.Helper()
	if factory == nil {
		t.Fatal("storetest: RunStore factory is required")
	}

	t.Run("start_requests_require_canonical_model_binding", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			binding string
		}{
			{name: "empty", binding: ""},
			{name: "surrounding whitespace", binding: " model-binding-conformance-v1 "},
		} {
			t.Run(test.name, func(t *testing.T) {
				store := factory()
				request := createRequest("run-invalid-model-binding", "", time.Now())
				request.Run.ModelBindingID = test.binding
				callbacks := 0
				err := store.CreateRunV4(t.Context(), request, func(agent.RunStart) error {
					callbacks++
					return nil
				})
				if !errors.Is(err, agent.ErrRunStoreProtocol) || callbacks != 0 {
					t.Fatalf("CreateRunV4 error=%v callbacks=%d", err, callbacks)
				}
				request.Run.ModelBindingID = "model-binding-conformance-v1"
				if err := store.CreateRunV4(t.Context(), request, func(agent.RunStart) error {
					callbacks++
					return nil
				}); err != nil {
					t.Fatalf("valid create after rejected binding: %v", err)
				}
				if callbacks != 1 {
					t.Fatalf("callbacks=%d, want one valid callback", callbacks)
				}
			})
		}

		store := factory()
		create := createRequest("run-invalid-resume-binding", "session-invalid-resume-binding", time.Now())
		resume := agent.ResumeRunRequest{
			Run: create.Run, LeaseID: create.LeaseID, LeaseTTL: create.LeaseTTL,
			InputDigest: "resume-input-digest",
		}
		resume.Run.ModelBindingID = ""
		callbacks := 0
		err := store.ResumeRunV4(t.Context(), resume, func(agent.ResumedRun) error {
			callbacks++
			return nil
		})
		if !errors.Is(err, agent.ErrRunStoreProtocol) || callbacks != 0 {
			t.Fatalf("ResumeRunV4 invalid binding error=%v callbacks=%d", err, callbacks)
		}
		resume.Run.ModelBindingID = "model-binding-conformance-v1"
		if err := store.ResumeRunV4(t.Context(), resume, func(agent.ResumedRun) error {
			callbacks++
			return nil
		}); !errors.Is(err, agent.ErrRunNotFound) {
			t.Fatalf("canonical missing ResumeRunV4 error=%v", err)
		}
		if callbacks != 0 {
			t.Fatalf("missing resume callbacks=%d, want zero", callbacks)
		}
	})

	t.Run("create_model_binding_drift_is_atomic", func(t *testing.T) {
		store := factory()
		now := time.Now()
		first := createRequest("run-binding-owner", "session-binding-owner", now)
		var firstStart agent.RunStart
		if err := store.CreateRunV4(t.Context(), first, func(candidate agent.RunStart) error {
			firstStart = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		drifted := createRequest("run-binding-drift", first.Run.SessionID, now)
		drifted.Run.ModelBindingID = "model-binding-conformance-v2"
		callbacks := 0
		if err := store.CreateRunV4(t.Context(), drifted, func(agent.RunStart) error {
			callbacks++
			return nil
		}); !errors.Is(err, agent.ErrModelBindingMismatch) {
			t.Fatalf("CreateRunV4 binding drift error=%v", err)
		}
		if callbacks != 0 {
			t.Fatalf("binding drift callbacks=%d, want zero", callbacks)
		}
		if _, err := store.ValidateRunLease(t.Context(), firstStart.Handle); err != nil {
			t.Fatalf("owner lease after rejected binding drift: %v", err)
		}

		drifted.Run.ModelBindingID = first.Run.ModelBindingID
		if err := store.CreateRunV4(t.Context(), drifted, func(agent.RunStart) error {
			callbacks++
			return nil
		}); !errors.Is(err, agent.ErrSessionBusy) {
			t.Fatalf("matching create while owner is active error=%v", err)
		}
		if callbacks != 0 {
			t.Fatalf("busy create callbacks=%d, want zero", callbacks)
		}
	})

	t.Run("resume_model_binding_drift_is_atomic", func(t *testing.T) {
		store := factory()
		now := time.Now()
		create := createRequest("run-resume-binding", "session-resume-binding", now)
		var created agent.RunStart
		if err := store.CreateRunV4(t.Context(), create, func(candidate agent.RunStart) error {
			created = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		pending := conformancePendingApproval(t, create.Run, create.Run.ModelBindingID, created.Handle.SessionRevision, now.Add(time.Millisecond))
		waiting := create.Run
		waiting.Status = agent.RunStatusWaitingUser
		waiting.PendingApprovalDigest = pending.Digest
		waiting.UpdatedAt = now.Add(time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: created.Handle, Run: waiting, PendingApproval: &pending,
		}); err != nil {
			t.Fatal(err)
		}

		resume := agent.ResumeRunRequest{
			Run: create.Run, LeaseID: "lease-resume-binding", LeaseTTL: time.Minute,
			InputDigest: pending.Request.Checkpoint.InputDigest,
		}
		resume.Run.ModelBindingID = "model-binding-conformance-v2"
		resume.Run.UpdatedAt = now.Add(2 * time.Millisecond)
		callbacks := 0
		if err := store.ResumeRunV4(t.Context(), resume, func(agent.ResumedRun) error {
			callbacks++
			return nil
		}); !errors.Is(err, agent.ErrModelBindingMismatch) {
			t.Fatalf("ResumeRunV4 binding drift error=%v", err)
		}
		if callbacks != 0 {
			t.Fatalf("resume binding drift callbacks=%d, want zero", callbacks)
		}

		resume.Run.ModelBindingID = create.Run.ModelBindingID
		var resumed agent.ResumedRun
		if err := store.ResumeRunV4(t.Context(), resume, func(candidate agent.ResumedRun) error {
			callbacks++
			resumed = candidate
			return nil
		}); err != nil {
			t.Fatalf("valid ResumeRunV4 after rejected binding drift: %v", err)
		}
		if callbacks != 1 || resumed.Handle.LeaseGeneration != created.Handle.LeaseGeneration+1 ||
			resumed.Session == nil || resumed.Session.ModelBindingID != create.Run.ModelBindingID ||
			resumed.PendingApproval == nil || resumed.PendingApproval.Request.Checkpoint == nil ||
			resumed.PendingApproval.Request.Checkpoint.ModelBindingID != create.Run.ModelBindingID {
			t.Fatalf("callbacks=%d resumed=%+v", callbacks, resumed)
		}
	})

	t.Run("finish_model_binding_drift_is_atomic", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-finish-binding", "session-finish-binding", now)
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		completed := request.Run
		completed.Status = agent.RunStatusCompleted
		completed.Result = "done"
		completed.UpdatedAt = now.Add(time.Millisecond)
		session := &agent.SessionState{
			ID: request.Run.SessionID, ModelBindingID: request.Run.ModelBindingID,
			Revision: start.Handle.SessionRevision + 1, LastRunID: request.Run.ID,
			CreatedAt: now, UpdatedAt: completed.UpdatedAt,
		}

		driftedRun := completed
		driftedRun.ModelBindingID = "model-binding-conformance-v2"
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: driftedRun, Session: session,
		}); !errors.Is(err, agent.ErrModelBindingMismatch) {
			t.Fatalf("FinishRun run binding drift error=%v", err)
		}
		if _, err := store.ValidateRunLease(t.Context(), start.Handle); err != nil {
			t.Fatalf("lease after rejected run binding drift: %v", err)
		}

		driftedSession := *session
		driftedSession.ModelBindingID = "model-binding-conformance-v2"
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: completed, Session: &driftedSession,
		}); !errors.Is(err, agent.ErrModelBindingMismatch) {
			t.Fatalf("FinishRun session binding drift error=%v", err)
		}
		if _, err := store.ValidateRunLease(t.Context(), start.Handle); err != nil {
			t.Fatalf("lease after rejected session binding drift: %v", err)
		}
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: completed, Session: session,
		}); err != nil {
			t.Fatalf("valid FinishRun after rejected binding drift: %v", err)
		}
	})

	t.Run("finish_pending_approval_model_binding_drift_is_atomic", func(t *testing.T) {
		store := factory()
		now := time.Now()
		create := createRequest("run-finish-pending-binding", "session-finish-pending-binding", now)
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), create, func(candidate agent.RunStart) error {
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		pending := conformancePendingApproval(t, create.Run, "model-binding-conformance-v2", start.Handle.SessionRevision, now.Add(time.Millisecond))
		waiting := create.Run
		waiting.Status = agent.RunStatusWaitingUser
		waiting.PendingApprovalDigest = pending.Digest
		waiting.UpdatedAt = now.Add(time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: waiting, PendingApproval: &pending,
		}); !errors.Is(err, agent.ErrModelBindingMismatch) {
			t.Fatalf("FinishRun pending approval binding drift error=%v", err)
		}
		if _, err := store.ValidateRunLease(t.Context(), start.Handle); err != nil {
			t.Fatalf("lease after rejected pending approval binding drift: %v", err)
		}

		pending = conformancePendingApproval(t, create.Run, create.Run.ModelBindingID, start.Handle.SessionRevision, now.Add(time.Millisecond))
		waiting.PendingApprovalDigest = pending.Digest
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: waiting, PendingApproval: &pending,
		}); err != nil {
			t.Fatalf("valid waiting FinishRun after rejected binding drift: %v", err)
		}
	})

	t.Run("finish_rejects_malformed_pending_approval_atomically", func(t *testing.T) {
		tests := []struct {
			name   string
			id     string
			mutate func(*agent.PendingApprovalCommit)
		}{
			{
				name: "legacy authority version",
				id:   "legacy-version",
				mutate: func(pending *agent.PendingApprovalCommit) {
					pending.AuthorityVersion = agent.PendingApprovalAuthorityVersion - 1
				},
			},
			{
				name: "future authority version",
				id:   "future-version",
				mutate: func(pending *agent.PendingApprovalCommit) {
					pending.AuthorityVersion = agent.PendingApprovalAuthorityVersion + 1
				},
			},
			{
				name: "missing checkpoint",
				id:   "missing-checkpoint",
				mutate: func(pending *agent.PendingApprovalCommit) {
					pending.Request.Checkpoint = nil
				},
			},
			{
				name: "uncanonicalizable arguments",
				id:   "uncanonicalizable-arguments",
				mutate: func(pending *agent.PendingApprovalCommit) {
					pending.Request.Operation.Arguments = map[string]any{"invalid": math.NaN()}
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				store := factory()
				now := time.Now()
				create := createRequest("run-malformed-pending-"+test.id, "session-malformed-pending-"+test.id, now)
				var start agent.RunStart
				if err := store.CreateRunV4(t.Context(), create, func(candidate agent.RunStart) error {
					start = candidate
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				valid := conformancePendingApproval(
					t, create.Run, create.Run.ModelBindingID, start.Handle.SessionRevision, now.Add(time.Millisecond),
				)
				malformed := valid
				test.mutate(&malformed)
				waiting := create.Run
				waiting.Status = agent.RunStatusWaitingUser
				waiting.PendingApprovalDigest = malformed.Digest
				waiting.UpdatedAt = now.Add(time.Millisecond)
				if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
					Handle: start.Handle, Run: waiting, PendingApproval: &malformed,
				}); !errors.Is(err, agent.ErrOperationPlanChanged) {
					t.Fatalf("FinishRun malformed pending approval error=%v", err)
				}
				if _, err := store.ValidateRunLease(t.Context(), start.Handle); err != nil {
					t.Fatalf("lease after malformed pending approval rejection: %v", err)
				}

				waiting.PendingApprovalDigest = valid.Digest
				if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
					Handle: start.Handle, Run: waiting, PendingApproval: &valid,
				}); err != nil {
					t.Fatalf("valid waiting FinishRun after malformed rejection: %v", err)
				}
			})
		}
	})

	t.Run("create_callback_and_terminal_session", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-create", "session-create", now)
		callbacks := 0
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
			callbacks++
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if callbacks != 1 || start.Handle.RunID != request.Run.ID || start.Handle.SessionID != request.Run.SessionID ||
			start.Handle.LeaseID != request.LeaseID || start.Handle.LeaseGeneration == 0 || start.Handle.LeaseDeadline.IsZero() ||
			start.Session == nil || start.Session.ModelBindingID != request.Run.ModelBindingID || start.Session.Revision != 0 {
			t.Fatalf("callbacks=%d start=%+v", callbacks, start)
		}
		session := &agent.SessionState{
			ID: request.Run.SessionID, ModelBindingID: request.Run.ModelBindingID,
			Revision: start.Handle.SessionRevision + 1, LastRunID: request.Run.ID,
			CreatedAt: now, UpdatedAt: now.Add(time.Millisecond),
		}
		run := request.Run
		run.Status = agent.RunStatusCompleted
		run.Result = "done"
		run.UpdatedAt = session.UpdatedAt
		finish := agent.FinishRunRequest{Handle: start.Handle, Run: run, Session: session}
		if err := finish.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(t.Context(), finish); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ValidateRunLease(t.Context(), start.Handle); !errors.Is(err, agent.ErrSessionLeaseLost) {
			t.Fatalf("ValidateRunLease after finish error=%v", err)
		}
	})

	t.Run("callback_rejection_is_atomic", func(t *testing.T) {
		store := factory()
		request := createRequest("run-callback", "session-callback", time.Now())
		sentinel := errors.New("reject candidate")
		if err := store.CreateRunV4(t.Context(), request, func(agent.RunStart) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("first create error=%v", err)
		}
		callbacks := 0
		if err := store.CreateRunV4(t.Context(), request, func(agent.RunStart) error {
			callbacks++
			return nil
		}); err != nil {
			t.Fatalf("create after rejection: %v", err)
		}
		if callbacks != 1 {
			t.Fatalf("callbacks=%d, want one", callbacks)
		}
	})

	t.Run("create_run_input_write_is_a_defensive_copy", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-create-input-copy", "", now)
		request.Run.Input = conformanceAliasingInput(request.Run.ID, request.Run.SessionID)
		pristineInput := conformanceAliasingInput(request.Run.ID, request.Run.SessionID)
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		envelope := request.Run.Input.Metadata["request"].(map[string]any)
		metadata := envelope["metadata"].(map[string]any)
		metadata["owner"] = "mutated"
		metadata["labels"].([]any)[0] = "mutated"
		attachments := envelope["attachments"].([]any)
		attachments[0].(map[string]any)["metadata"].(map[string]any)["caption"] = "mutated"
		artifacts := envelope["artifacts"].([]any)
		artifacts[0].(map[string]any)["data"].(map[string]any)["state"] = "mutated"
		request.Run.Input.Attachments[0].Text = "mutated attachment"

		changed := request.Run
		changed.Status = agent.RunStatusCompleted
		changed.Result = "done"
		changed.UpdatedAt = now.Add(time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: changed,
		}); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("FinishRun with caller-mutated nested input error=%v", err)
		}

		completed := request.Run
		completed.Input = pristineInput
		completed.Status = agent.RunStatusCompleted
		completed.Result = "done"
		completed.UpdatedAt = changed.UpdatedAt
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: completed,
		}); err != nil {
			t.Fatalf("FinishRun with original nested input after caller mutation: %v", err)
		}
	})

	t.Run("finish_pending_approval_write_is_a_defensive_copy", func(t *testing.T) {
		store := factory()
		now := time.Now()
		create := createRequest("run-finish-pending-copy", "session-finish-pending-copy", now)
		var created agent.RunStart
		if err := store.CreateRunV4(t.Context(), create, func(candidate agent.RunStart) error {
			created = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		pending := conformancePendingApproval(
			t, create.Run, create.Run.ModelBindingID, created.Handle.SessionRevision, now.Add(time.Millisecond),
		)
		checkpoint := pending.Request.Checkpoint
		if checkpoint == nil || checkpoint.ContextCheckpoint == nil || len(checkpoint.SeenCallIDs) != 1 ||
			len(checkpoint.Transcript) != 1 || len(checkpoint.Transcript[0].Attachments) != 1 ||
			len(checkpoint.ContextCheckpoint.Summary.Facts) != 1 || len(pending.Request.ModelOutput) != 1 ||
			pending.Request.ModelOutput[0].Call == nil {
			t.Fatalf("incomplete pending approval fixture: %+v", pending)
		}

		wantAuthorityVersion := pending.AuthorityVersion
		wantDigest := pending.Digest
		wantCheckpointModelBindingID := checkpoint.ModelBindingID
		wantCheckpointInputDigest := checkpoint.InputDigest
		wantCheckpointSeenCallID := checkpoint.SeenCallIDs[0]
		wantCheckpointTranscriptText := checkpoint.Transcript[0].Text
		wantCheckpointAttachmentText := checkpoint.Transcript[0].Attachments[0].Text
		wantCheckpointSummary := checkpoint.ContextCheckpoint.Summary.Summary
		wantCheckpointFact := checkpoint.ContextCheckpoint.Summary.Facts[0]
		wantAuditData := append(json.RawMessage(nil), pending.Audit.Data...)
		wantOperationCallName := pending.Request.Operation.Call.Name
		wantOperationCallInput := append(json.RawMessage(nil), pending.Request.Operation.Call.Input...)
		wantArguments, err := json.Marshal(pending.Request.Operation.Arguments)
		if err != nil {
			t.Fatal(err)
		}
		wantPreview := append(json.RawMessage(nil), pending.Request.Preview...)
		wantModelOutputType := pending.Request.ModelOutput[0].Type
		wantModelCallName := pending.Request.ModelOutput[0].Call.Name
		wantModelCallInput := append(json.RawMessage(nil), pending.Request.ModelOutput[0].Call.Input...)
		wantModelRaw := append(json.RawMessage(nil), pending.Request.ModelOutput[0].Raw...)

		waiting := create.Run
		waiting.Status = agent.RunStatusWaitingUser
		waiting.PendingApprovalDigest = pending.Digest
		waiting.UpdatedAt = now.Add(time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: created.Handle, Run: waiting, PendingApproval: &pending,
		}); err != nil {
			t.Fatal(err)
		}

		pending.AuthorityVersion = 0
		pending.Digest = "approval_mutated"
		checkpoint.ModelBindingID = "model-binding-mutated"
		checkpoint.InputDigest = "input-digest-mutated"
		checkpoint.SeenCallIDs[0] = "call-mutated"
		checkpoint.Transcript[0].Text = "mutated checkpoint transcript"
		checkpoint.Transcript[0].Attachments[0].Text = "mutated checkpoint attachment"
		checkpoint.ContextCheckpoint.Summary.Summary = "mutated checkpoint summary"
		checkpoint.ContextCheckpoint.Summary.Facts[0] = "mutated checkpoint fact"
		pending.Audit.Data[0] = '['
		pending.Request.Operation.Call.Name = "mutated_operation"
		pending.Request.Operation.Call.Input[0] = '['
		arguments := pending.Request.Operation.Arguments.(map[string]any)
		arguments["value"] = "mutated"
		argumentArtifacts := arguments["artifacts"].([]any)
		argumentArtifacts[0].(map[string]any)["metadata"].(map[string]any)["state"] = "mutated"
		pending.Request.Preview[0] = '['
		pending.Request.ModelOutput[0].Type = agent.ModelOutputMessage
		pending.Request.ModelOutput[0].Call.Name = "mutated_operation"
		pending.Request.ModelOutput[0].Call.Input[0] = '['
		pending.Request.ModelOutput[0].Raw[0] = '['

		resume := agent.ResumeRunRequest{
			Run: create.Run, LeaseID: "lease-resume-pending-copy", LeaseTTL: time.Minute,
			InputDigest: wantCheckpointInputDigest,
		}
		resume.Run.UpdatedAt = now.Add(2 * time.Millisecond)
		callbacks := 0
		var resumed agent.ResumedRun
		var authorityErr error
		if err := store.ResumeRunV4(t.Context(), resume, func(candidate agent.ResumedRun) error {
			callbacks++
			if candidate.PendingApproval == nil {
				authorityErr = errors.New("resumed pending approval is nil")
				return authorityErr
			}
			authorityErr = candidate.PendingApproval.ValidateAuthority(create.Run.ModelBindingID)
			if authorityErr != nil {
				return authorityErr
			}
			resumed = candidate
			return nil
		}); err != nil {
			t.Fatalf("ResumeRunV4 after caller mutated committed pending approval: %v (authority validation: %v)", err, authorityErr)
		}
		if callbacks != 1 {
			t.Fatalf("resume callbacks=%d, want one", callbacks)
		}
		if resumed.PendingApproval == nil {
			t.Fatal("resumed pending approval is nil")
		}
		if resumed.PendingApprovalDigest != wantDigest || resumed.PendingApproval.Digest != wantDigest {
			t.Fatalf("resumed pending digests=%q/%q, want %q", resumed.PendingApprovalDigest, resumed.PendingApproval.Digest, wantDigest)
		}
		if resumed.PendingApproval.AuthorityVersion != wantAuthorityVersion {
			t.Fatalf("resumed authority version=%d, want %d", resumed.PendingApproval.AuthorityVersion, wantAuthorityVersion)
		}
		resumedCheckpoint := resumed.PendingApproval.Request.Checkpoint
		if resumedCheckpoint == nil || resumedCheckpoint.ContextCheckpoint == nil {
			t.Fatalf("resumed checkpoint=%+v", resumedCheckpoint)
		}
		if resumedCheckpoint.ModelBindingID != wantCheckpointModelBindingID ||
			resumedCheckpoint.InputDigest != wantCheckpointInputDigest {
			t.Fatalf("resumed checkpoint=%+v, want model binding %q input digest %q", resumedCheckpoint, wantCheckpointModelBindingID, wantCheckpointInputDigest)
		}
		if len(resumedCheckpoint.SeenCallIDs) != 1 || resumedCheckpoint.SeenCallIDs[0] != wantCheckpointSeenCallID {
			t.Fatalf("resumed checkpoint seen call IDs=%v, want [%q]", resumedCheckpoint.SeenCallIDs, wantCheckpointSeenCallID)
		}
		if len(resumedCheckpoint.Transcript) != 1 || resumedCheckpoint.Transcript[0].Text != wantCheckpointTranscriptText ||
			len(resumedCheckpoint.Transcript[0].Attachments) != 1 ||
			resumedCheckpoint.Transcript[0].Attachments[0].Text != wantCheckpointAttachmentText {
			t.Fatalf("resumed checkpoint transcript=%+v", resumedCheckpoint.Transcript)
		}
		if resumedCheckpoint.ContextCheckpoint.Summary.Summary != wantCheckpointSummary ||
			len(resumedCheckpoint.ContextCheckpoint.Summary.Facts) != 1 ||
			resumedCheckpoint.ContextCheckpoint.Summary.Facts[0] != wantCheckpointFact {
			t.Fatalf("resumed nested context checkpoint=%+v", resumedCheckpoint.ContextCheckpoint)
		}
		if !bytes.Equal(resumed.PendingApproval.Audit.Data, wantAuditData) {
			t.Fatalf("resumed audit data=%q, want %q", resumed.PendingApproval.Audit.Data, wantAuditData)
		}
		if resumed.PendingApproval.Request.Operation.Call.Name != wantOperationCallName ||
			!bytes.Equal(resumed.PendingApproval.Request.Operation.Call.Input, wantOperationCallInput) {
			t.Fatalf("resumed operation call=%+v", resumed.PendingApproval.Request.Operation.Call)
		}
		gotArguments, err := json.Marshal(resumed.PendingApproval.Request.Operation.Arguments)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotArguments, wantArguments) {
			t.Fatalf("resumed operation arguments=%s, want %s", gotArguments, wantArguments)
		}
		if !bytes.Equal(resumed.PendingApproval.Request.Preview, wantPreview) {
			t.Fatalf("resumed preview=%q, want %q", resumed.PendingApproval.Request.Preview, wantPreview)
		}
		if len(resumed.PendingApproval.Request.ModelOutput) != 1 || resumed.PendingApproval.Request.ModelOutput[0].Call == nil {
			t.Fatalf("resumed model output=%+v", resumed.PendingApproval.Request.ModelOutput)
		}
		resumedModelOutput := resumed.PendingApproval.Request.ModelOutput[0]
		if resumedModelOutput.Type != wantModelOutputType || resumedModelOutput.Call.Name != wantModelCallName ||
			!bytes.Equal(resumedModelOutput.Call.Input, wantModelCallInput) ||
			!bytes.Equal(resumedModelOutput.Raw, wantModelRaw) {
			t.Fatalf("resumed model output=%+v", resumedModelOutput)
		}
	})

	t.Run("resume_callback_rejection_is_atomic", func(t *testing.T) {
		store := factory()
		now := time.Now()
		create := createRequest("run-resume-callback", "session-resume-callback", now)
		var created agent.RunStart
		if err := store.CreateRunV4(t.Context(), create, func(candidate agent.RunStart) error {
			created = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		pending := conformancePendingApproval(
			t, create.Run, create.Run.ModelBindingID, created.Handle.SessionRevision, now.Add(time.Millisecond),
		)
		waiting := create.Run
		waiting.Status = agent.RunStatusWaitingUser
		waiting.PendingApprovalDigest = pending.Digest
		waiting.UpdatedAt = now.Add(time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: created.Handle, Run: waiting, PendingApproval: &pending,
		}); err != nil {
			t.Fatal(err)
		}

		resume := agent.ResumeRunRequest{
			Run: create.Run, LeaseID: "lease-resume-callback-retry", LeaseTTL: time.Minute,
			InputDigest: pending.Request.Checkpoint.InputDigest,
		}
		resume.Run.UpdatedAt = now.Add(2 * time.Millisecond)
		sentinel := errors.New("reject resumed candidate")
		callbacks := 0
		var wantAuthorityVersion uint32
		var wantPendingDigest, wantCheckpointModelBindingID, wantCheckpointInputDigest string
		var wantAuditData, wantOperationCallInput, wantModelCallInput, wantModelRaw json.RawMessage
		var wantSessionModelBindingID string
		if err := store.ResumeRunV4(t.Context(), resume, func(candidate agent.ResumedRun) error {
			callbacks++
			if candidate.PendingApproval == nil || candidate.PendingApproval.Request.Checkpoint == nil ||
				candidate.Session == nil || len(candidate.PendingApproval.Audit.Data) == 0 ||
				len(candidate.PendingApproval.Request.Operation.Call.Input) == 0 ||
				len(candidate.PendingApproval.Request.ModelOutput) != 1 ||
				candidate.PendingApproval.Request.ModelOutput[0].Call == nil ||
				len(candidate.PendingApproval.Request.ModelOutput[0].Call.Input) == 0 ||
				len(candidate.PendingApproval.Request.ModelOutput[0].Raw) == 0 {
				t.Fatalf("incomplete resumed candidate: %+v", candidate)
			}

			wantAuthorityVersion = candidate.PendingApproval.AuthorityVersion
			wantPendingDigest = candidate.PendingApproval.Digest
			wantCheckpointModelBindingID = candidate.PendingApproval.Request.Checkpoint.ModelBindingID
			wantCheckpointInputDigest = candidate.PendingApproval.Request.Checkpoint.InputDigest
			wantAuditData = append(json.RawMessage(nil), candidate.PendingApproval.Audit.Data...)
			wantOperationCallInput = append(json.RawMessage(nil), candidate.PendingApproval.Request.Operation.Call.Input...)
			wantModelCallInput = append(json.RawMessage(nil), candidate.PendingApproval.Request.ModelOutput[0].Call.Input...)
			wantModelRaw = append(json.RawMessage(nil), candidate.PendingApproval.Request.ModelOutput[0].Raw...)
			wantSessionModelBindingID = candidate.Session.ModelBindingID

			candidate.PendingApproval.AuthorityVersion = 0
			candidate.PendingApproval.Request.Checkpoint.ModelBindingID = "callback-mutated"
			candidate.PendingApproval.Audit.Data[0] ^= 0xff
			candidate.PendingApproval.Request.Operation.Call.Input[0] ^= 0xff
			candidate.PendingApproval.Request.ModelOutput[0].Call.Input[0] ^= 0xff
			candidate.PendingApproval.Request.ModelOutput[0].Raw[0] ^= 0xff
			candidate.Session.ModelBindingID = "callback-mutated"
			return sentinel
		}); !errors.Is(err, sentinel) {
			t.Fatalf("rejected ResumeRunV4 error=%v", err)
		}
		if callbacks != 1 {
			t.Fatalf("rejected resume callbacks=%d, want one", callbacks)
		}

		var resumed agent.ResumedRun
		if err := store.ResumeRunV4(t.Context(), resume, func(candidate agent.ResumedRun) error {
			callbacks++
			resumed = candidate
			return nil
		}); err != nil {
			t.Fatalf("ResumeRunV4 after callback rejection: %v", err)
		}
		if callbacks != 2 {
			t.Fatalf("resume callbacks=%d, want two", callbacks)
		}
		if resumed.Handle.LeaseGeneration != created.Handle.LeaseGeneration+1 {
			t.Fatalf("resumed lease generation=%d, want %d", resumed.Handle.LeaseGeneration, created.Handle.LeaseGeneration+1)
		}
		if resumed.PendingApproval == nil {
			t.Fatal("resumed pending approval is nil")
		}
		if resumed.PendingApprovalDigest != wantPendingDigest || resumed.PendingApproval.Digest != wantPendingDigest {
			t.Fatalf("resumed pending digests=%q/%q, want %q", resumed.PendingApprovalDigest, resumed.PendingApproval.Digest, wantPendingDigest)
		}
		if resumed.PendingApproval.AuthorityVersion != wantAuthorityVersion {
			t.Fatalf("resumed authority version=%d, want %d", resumed.PendingApproval.AuthorityVersion, wantAuthorityVersion)
		}
		if resumed.PendingApproval.Request.Checkpoint == nil {
			t.Fatal("resumed approval checkpoint is nil")
		}
		if resumed.PendingApproval.Request.Checkpoint.ModelBindingID != wantCheckpointModelBindingID {
			t.Fatalf("resumed checkpoint model binding=%q, want %q", resumed.PendingApproval.Request.Checkpoint.ModelBindingID, wantCheckpointModelBindingID)
		}
		if resumed.PendingApproval.Request.Checkpoint.InputDigest != wantCheckpointInputDigest {
			t.Fatalf("resumed checkpoint input digest=%q, want %q", resumed.PendingApproval.Request.Checkpoint.InputDigest, wantCheckpointInputDigest)
		}
		if !bytes.Equal(resumed.PendingApproval.Audit.Data, wantAuditData) {
			t.Fatalf("resumed audit data=%q, want %q", resumed.PendingApproval.Audit.Data, wantAuditData)
		}
		if !bytes.Equal(resumed.PendingApproval.Request.Operation.Call.Input, wantOperationCallInput) {
			t.Fatalf("resumed operation call input=%q, want %q", resumed.PendingApproval.Request.Operation.Call.Input, wantOperationCallInput)
		}
		if len(resumed.PendingApproval.Request.ModelOutput) != 1 || resumed.PendingApproval.Request.ModelOutput[0].Call == nil {
			t.Fatalf("resumed model output=%+v", resumed.PendingApproval.Request.ModelOutput)
		}
		if !bytes.Equal(resumed.PendingApproval.Request.ModelOutput[0].Call.Input, wantModelCallInput) {
			t.Fatalf("resumed model call input=%q, want %q", resumed.PendingApproval.Request.ModelOutput[0].Call.Input, wantModelCallInput)
		}
		if !bytes.Equal(resumed.PendingApproval.Request.ModelOutput[0].Raw, wantModelRaw) {
			t.Fatalf("resumed model raw=%q, want %q", resumed.PendingApproval.Request.ModelOutput[0].Raw, wantModelRaw)
		}
		if resumed.Session == nil || resumed.Session.ModelBindingID != wantSessionModelBindingID {
			t.Fatalf("resumed session=%+v, want model binding %q", resumed.Session, wantSessionModelBindingID)
		}
	})

	t.Run("finish_missing_run_is_not_found", func(t *testing.T) {
		store := factory()
		now := time.Now()
		run := createRequest("missing-run", "", now).Run
		run.Status = agent.RunStatusCompleted
		run.Result = "missing"
		request := agent.FinishRunRequest{
			Handle: agent.RunHandle{RunID: run.ID}, Run: run,
		}
		if err := store.FinishRun(t.Context(), request); !errors.Is(err, agent.ErrRunNotFound) {
			t.Fatalf("FinishRun missing error=%v", err)
		}
	})

	t.Run("stateless_finish_is_single_use", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-finish-once", "", now)
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		run := request.Run
		run.Status = agent.RunStatusCompleted
		run.Result = "done"
		run.UpdatedAt = now.Add(time.Millisecond)
		finish := agent.FinishRunRequest{Handle: start.Handle, Run: run}
		if err := store.FinishRun(t.Context(), finish); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(t.Context(), finish); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("duplicate FinishRun error=%v, want ErrRunStoreProtocol", err)
		}
	})

	t.Run("stateless_finish_rejects_session_authority", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-stateless-authority", "", now)
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		run := request.Run
		run.Status = agent.RunStatusCompleted
		run.Result = "done"
		run.UpdatedAt = now.Add(time.Second)
		injected := &agent.SessionState{
			ID: "injected-session", ModelBindingID: run.ModelBindingID,
			Revision: 1, LastRunID: run.ID, CreatedAt: now, UpdatedAt: run.UpdatedAt,
		}
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: run, Session: injected,
		}); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("stateless session injection error=%v", err)
		}
		forged := start.Handle
		forged.LeaseID = "forged-lease"
		forged.LeaseGeneration = 1
		forged.LeaseDeadline = now.Add(time.Minute)
		forged.SessionRevision = 1
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: forged, Run: run,
		}); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("stateless forged handle error=%v", err)
		}
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{Handle: start.Handle, Run: run}); err != nil {
			t.Fatalf("valid FinishRun after rejected stateless authority: %v", err)
		}
	})

	t.Run("finish_status_payload_is_consistent", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*agent.RunRecord)
		}{
			{name: "completed error", mutate: func(run *agent.RunRecord) {
				run.ErrorCode, run.Error = "internal_error", "unexpected"
			}},
			{name: "waiting result", mutate: func(run *agent.RunRecord) {
				run.Status, run.PendingApprovalDigest = agent.RunStatusWaitingUser, "approval-digest"
			}},
			{name: "failed missing error", mutate: func(run *agent.RunRecord) {
				run.Status, run.Result, run.FailureAuditStatus = agent.RunStatusFailed, "", agent.FailureAuditMissing
			}},
			{name: "failed result", mutate: func(run *agent.RunRecord) {
				run.Status, run.ErrorCode, run.Error = agent.RunStatusFailed, "internal_error", "failed"
				run.FailureAuditStatus = agent.FailureAuditMissing
			}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				store := factory()
				now := time.Now()
				request := createRequest("run-status-"+test.name, "", now)
				var start agent.RunStart
				if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
					start = candidate
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				invalid := request.Run
				invalid.Status = agent.RunStatusCompleted
				invalid.Result = "done"
				invalid.UpdatedAt = now.Add(time.Second)
				test.mutate(&invalid)
				if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
					Handle: start.Handle, Run: invalid,
				}); !errors.Is(err, agent.ErrRunStoreProtocol) {
					t.Fatalf("invalid status payload error=%v", err)
				}
				valid := request.Run
				valid.Status = agent.RunStatusCompleted
				valid.Result = "done"
				valid.UpdatedAt = now.Add(time.Second)
				if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
					Handle: start.Handle, Run: valid,
				}); err != nil {
					t.Fatalf("valid FinishRun after rejected status payload: %v", err)
				}
			})
		}
	})

	t.Run("finish_rejects_input_rewrite", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-input-immutable", "", now)
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		changed := request.Run
		changed.Status = agent.RunStatusCompleted
		changed.Result = "done"
		changed.Input.User = "rewritten"
		changed.UpdatedAt = now.Add(time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: changed,
		}); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("FinishRun changed input error=%v", err)
		}
		completed := request.Run
		completed.Status = agent.RunStatusCompleted
		completed.Result = "done"
		completed.UpdatedAt = now.Add(time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: start.Handle, Run: completed,
		}); err != nil {
			t.Fatalf("valid FinishRun after rejected rewrite: %v", err)
		}
	})

	t.Run("run_and_item_share_identity_namespace", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-identity", "", now)
		if err := store.CreateRunV4(t.Context(), request, func(agent.RunStart) error { return nil }); err != nil {
			t.Fatal(err)
		}
		item := agent.ItemRecord{
			ID: "item-identity", RunID: request.Run.ID, Type: agent.ItemTypeUserMessage,
			Data: json.RawMessage(`{"text":"hello"}`), CreatedAt: now,
		}
		if err := store.AppendItem(t.Context(), item); err != nil {
			t.Fatal(err)
		}
		collision := createRequest(item.ID, "", now)
		if err := store.CreateRunV4(t.Context(), collision, func(agent.RunStart) error { return nil }); !errors.Is(err, agent.ErrIdentityConflict) {
			t.Fatalf("run/item collision error=%v", err)
		}
		item.ID = request.Run.ID
		if err := store.AppendItem(t.Context(), item); !errors.Is(err, agent.ErrIdentityConflict) {
			t.Fatalf("item/run collision error=%v", err)
		}
	})

	t.Run("session_lease_is_exclusive", func(t *testing.T) {
		store := factory()
		now := time.Now()
		first := createRequest("run-first", "shared-session", now)
		if err := store.CreateRunV4(t.Context(), first, func(agent.RunStart) error { return nil }); err != nil {
			t.Fatal(err)
		}
		second := createRequest("run-second", "shared-session", now)
		second.LeaseID = "lease-second"
		if err := store.CreateRunV4(t.Context(), second, func(agent.RunStart) error { return nil }); !errors.Is(err, agent.ErrSessionBusy) {
			t.Fatalf("concurrent session create error=%v", err)
		}
	})

	t.Run("lease_renewal_preserves_owner_and_fences_stale_generation", func(t *testing.T) {
		store := factory()
		now := time.Now()
		request := createRequest("run-renew", "session-renew", now)
		var start agent.RunStart
		if err := store.CreateRunV4(t.Context(), request, func(candidate agent.RunStart) error {
			start = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		renewed, err := store.RenewRunLease(t.Context(), agent.RenewRunLeaseRequest{
			Handle: start.Handle, LeaseTTL: 2 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		if renewed.LeaseID != start.Handle.LeaseID || renewed.LeaseGeneration != start.Handle.LeaseGeneration ||
			!renewed.LeaseDeadline.After(start.Handle.LeaseDeadline) {
			t.Fatalf("renewed handle=%+v start=%+v", renewed, start.Handle)
		}
		validated, err := store.ValidateRunLease(t.Context(), start.Handle)
		if err != nil || validated != renewed {
			t.Fatalf("ValidateRunLease result=%+v err=%v, want renewed=%+v", validated, err, renewed)
		}
		stale := renewed
		stale.LeaseGeneration++
		if _, err := store.ValidateRunLease(t.Context(), stale); !errors.Is(err, agent.ErrSessionLeaseLost) {
			t.Fatalf("stale lease validation error=%v", err)
		}
		session := &agent.SessionState{
			ID: request.Run.SessionID, ModelBindingID: request.Run.ModelBindingID,
			Revision: renewed.SessionRevision + 1, LastRunID: request.Run.ID,
			CreatedAt: now, UpdatedAt: now.Add(time.Millisecond),
		}
		run := request.Run
		run.Status = agent.RunStatusCompleted
		run.Result = "renewed"
		run.UpdatedAt = session.UpdatedAt
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: renewed, Run: run, Session: session,
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("session_identity_and_timestamps_do_not_drift", func(t *testing.T) {
		store := factory()
		now := time.Now()
		first := createRequest("run-session-first", "session-history", now)
		var firstStart agent.RunStart
		if err := store.CreateRunV4(t.Context(), first, func(candidate agent.RunStart) error {
			firstStart = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		firstSession := &agent.SessionState{
			ID: first.Run.SessionID, ModelBindingID: first.Run.ModelBindingID,
			Revision: firstStart.Handle.SessionRevision + 1, LastRunID: first.Run.ID,
			CreatedAt: now, UpdatedAt: now.Add(time.Second),
		}
		firstRun := first.Run
		firstRun.Status = agent.RunStatusCompleted
		firstRun.Result = "first"
		firstRun.UpdatedAt = firstSession.UpdatedAt
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: firstStart.Handle, Run: firstRun, Session: firstSession,
		}); err != nil {
			t.Fatal(err)
		}

		second := createRequest("run-session-second", first.Run.SessionID, now.Add(2*time.Second))
		var secondStart agent.RunStart
		if err := store.CreateRunV4(t.Context(), second, func(candidate agent.RunStart) error {
			secondStart = candidate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if secondStart.Session == nil || secondStart.Session.Revision != firstSession.Revision {
			t.Fatalf("second start session=%+v", secondStart.Session)
		}
		secondRun := second.Run
		secondRun.Status = agent.RunStatusCompleted
		secondRun.Result = "second"
		secondRun.UpdatedAt = now.Add(4 * time.Second)
		next := *secondStart.Session
		next.Revision = secondStart.Handle.SessionRevision + 1
		next.LastRunID = second.Run.ID
		next.UpdatedAt = secondRun.UpdatedAt

		wrongRun := next
		wrongRun.LastRunID = first.Run.ID
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: secondStart.Handle, Run: secondRun, Session: &wrongRun,
		}); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("session LastRunID rewrite error=%v", err)
		}
		changedCreation := next
		changedCreation.CreatedAt = now.Add(500 * time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: secondStart.Handle, Run: secondRun, Session: &changedCreation,
		}); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("session CreatedAt rewrite error=%v", err)
		}
		regressedUpdate := next
		regressedUpdate.UpdatedAt = now.Add(500 * time.Millisecond)
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: secondStart.Handle, Run: secondRun, Session: &regressedUpdate,
		}); !errors.Is(err, agent.ErrRunStoreProtocol) {
			t.Fatalf("session UpdatedAt regression error=%v", err)
		}
		if err := store.FinishRun(t.Context(), agent.FinishRunRequest{
			Handle: secondStart.Handle, Run: secondRun, Session: &next,
		}); err != nil {
			t.Fatalf("valid FinishRun after rejected session drift: %v", err)
		}
	})
}

func createRequest(runID, sessionID string, now time.Time) agent.CreateRunRequest {
	leaseID := ""
	if sessionID != "" {
		leaseID = "lease-" + runID
	}
	return agent.CreateRunRequest{
		Run: agent.RunRecord{
			ID: runID, SessionID: sessionID, ModelBindingID: "model-binding-conformance-v1",
			Status:    agent.RunStatusRunning,
			Input:     agent.Input{RunID: runID, SessionID: sessionID, User: "conformance"},
			CreatedAt: now, UpdatedAt: now,
		},
		LeaseID: leaseID, LeaseTTL: time.Minute,
	}
}

func conformanceAliasingInput(runID, sessionID string) agent.Input {
	return agent.Input{
		RunID: runID, SessionID: sessionID, User: "conformance",
		Attachments: []agent.ModelInputAttachment{{
			Kind: agent.ModelInputAttachmentText, ID: "input-attachment", Filename: "input.txt",
			MIMEType: "text/plain", Text: "original attachment",
		}},
		Metadata: map[string]any{
			"request": map[string]any{
				"metadata": map[string]any{
					"owner":  "original",
					"labels": []any{"original", map[string]any{"priority": "high"}},
				},
				"attachments": []any{map[string]any{
					"id":       "metadata-attachment",
					"metadata": map[string]any{"caption": "original"},
				}},
				"artifacts": []any{map[string]any{
					"type": "draft",
					"data": map[string]any{"state": "original"},
				}},
			},
		},
	}
}

func conformancePendingApproval(
	t *testing.T,
	run agent.RunRecord,
	modelBindingID string,
	expectedSessionRevision uint64,
	now time.Time,
) agent.PendingApprovalCommit {
	t.Helper()
	decision := agent.ApprovalDecision{ID: "approval-" + run.ID, Pending: true, Reason: "waiting"}
	auditData, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	operationCall := agent.ToolCall{
		ID: "pending-call", Name: "pending_operation",
		Input: json.RawMessage(`{"artifacts":[{"metadata":{"state":"original"},"type":"draft"}],"value":"original"}`),
	}
	modelCall := operationCall
	modelCall.Input = append(json.RawMessage(nil), operationCall.Input...)
	pending := agent.PendingApprovalCommit{
		AuthorityVersion: agent.PendingApprovalAuthorityVersion,
		Request: agent.ApprovalRequest{
			Operation: agent.OperationRequest{
				RunID: run.ID, SessionID: run.SessionID, Input: run.Input, Call: operationCall,
				Arguments: map[string]any{
					"artifacts": []any{map[string]any{
						"metadata": map[string]any{"state": "original"}, "type": "draft",
					}},
					"value": "original",
				},
			},
			Reason:  "waiting",
			Preview: json.RawMessage(`{"artifacts":[{"state":"original"}]}`),
			ModelOutput: []agent.ModelOutputItem{{
				ID: "pending-output", Type: agent.ModelOutputFunctionCall, Call: &modelCall,
				Raw: json.RawMessage(`{"arguments":"{\"artifacts\":[{\"metadata\":{\"state\":\"original\"},\"type\":\"draft\"}],\"value\":\"original\"}","call_id":"pending-call","id":"pending-output","name":"pending_operation","status":"completed","type":"function_call"}`),
			}},
			Checkpoint: &agent.ApprovalCheckpoint{
				ModelBindingID: modelBindingID,
				Transcript: []agent.ModelInputItem{{
					Type: agent.ModelInputUserMessage, Text: "original checkpoint transcript",
					Attachments: []agent.ModelInputAttachment{{
						Kind: agent.ModelInputAttachmentText, ID: "checkpoint-attachment", Filename: "checkpoint.txt",
						MIMEType: "text/plain", Text: "original checkpoint attachment",
					}},
				}},
				ContextCheckpoint: &agent.ContextCheckpoint{
					Version: 1,
					Summary: agent.ContextSummary{
						Summary: "original checkpoint summary", Facts: []string{"original checkpoint fact"},
					},
					CompactedItemCount: 1, SourceSessionRevision: expectedSessionRevision, UpdatedAt: now,
				},
				SeenCallIDs:             []string{operationCall.ID},
				OperationBatchCount:     1,
				InputDigest:             "input-digest-" + run.ID,
				ExpectedSessionRevision: expectedSessionRevision,
			},
		},
		Decision: decision,
		Audit: agent.ItemRecord{
			ID: "approval-audit-" + run.ID, RunID: run.ID, SessionID: run.SessionID,
			Type: agent.ItemTypeApproval, Data: auditData, CreatedAt: now,
		},
	}
	pending.Digest, err = conformancePendingApprovalDigest(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.ValidateAuthority(modelBindingID); err != nil {
		t.Fatalf("validate conformance pending approval authority: %v", err)
	}
	return pending
}

func conformancePendingApprovalDigest(pending agent.PendingApprovalCommit) (string, error) {
	return pending.AuthorityDigest()
}

// RunExecutionStoreConformance checks plan sealing, acquisition, replay,
// transition fencing, global transition identities, and defensive copies.
func RunExecutionStoreConformance(t *testing.T, factory ExecutionStoreFactory) {
	t.Helper()
	if factory == nil {
		t.Fatal("storetest: ExecutionStore factory is required")
	}

	t.Run("reserved_plan_can_execute_before_final_seal", func(t *testing.T) {
		store := factory()
		now := time.Now()
		batch, seal, acquisition := executionFixture(now, "execution-before-seal")
		if _, err := store.ReservePlanBatch(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		acquired, err := store.AcquireExecution(t.Context(), acquisition)
		if err != nil || acquired.Disposition != agent.ExecutionAcquired {
			t.Fatalf("AcquireExecution result=%+v err=%v", acquired, err)
		}
		// Runtime seals only after the model has produced a terminal response;
		// current reserved batches must remain sealable after their execution.
		if _, err := store.SealPlan(t.Context(), seal); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("sealed_plan_execute_and_replay", func(t *testing.T) {
		store := factory()
		now := time.Now()
		batch, seal, acquisition := executionFixture(now, "execution-replay")
		reserved, err := store.ReservePlanBatch(t.Context(), batch)
		if err != nil || !reserved.Created {
			t.Fatalf("ReservePlanBatch result=%+v err=%v", reserved, err)
		}
		sealed, err := store.SealPlan(t.Context(), seal)
		if err != nil || !sealed.Created {
			t.Fatalf("SealPlan result=%+v err=%v", sealed, err)
		}
		acquired, err := store.AcquireExecution(t.Context(), acquisition)
		if err != nil || acquired.Disposition != agent.ExecutionAcquired {
			t.Fatalf("AcquireExecution result=%+v err=%v", acquired, err)
		}
		transition := agent.OperationExecutionTransition{
			ID: "transition-executed", ExecutionID: acquisition.Execution.ID,
			AttemptID: acquisition.Execution.AttemptID, RunID: acquisition.Execution.RunID,
			CallID: acquisition.Execution.CallID, Actor: "executor", Message: "committed",
			From: agent.OperationExecutionStarted, To: agent.OperationExecutionExecuted,
			Result:    agent.OperationResult{Output: json.RawMessage(`{"applied":true}`)},
			CreatedAt: now.Add(time.Second),
		}
		if _, err := store.TransitionExecution(t.Context(), transition); err != nil {
			t.Fatal(err)
		}
		replayRequest := acquisition
		replayRequest.Execution.AttemptID = "attempt-replay"
		replayRequest.Execution.RunID = "run-replay"
		replayRequest.Execution.CallID = "call-replay"
		replayRequest.Execution.UpdatedAt = now.Add(2 * time.Second)
		replayRequest.Transition.ID = "transition-replay"
		replayRequest.Transition.AttemptID = replayRequest.Execution.AttemptID
		replayRequest.Transition.RunID = replayRequest.Execution.RunID
		replayRequest.Transition.CallID = replayRequest.Execution.CallID
		replayRequest.Transition.CreatedAt = replayRequest.Execution.UpdatedAt
		replayed, err := store.AcquireExecution(t.Context(), replayRequest)
		if err != nil || replayed.Disposition != agent.ExecutionReplay || replayed.Execution.Status != agent.OperationExecutionExecuted {
			t.Fatalf("replay result=%+v err=%v", replayed, err)
		}
	})

	t.Run("plan_replay_retains_first_timestamps", func(t *testing.T) {
		store := factory()
		now := time.Now()
		batch, seal, _ := executionFixture(now, "execution-seal-replay")
		if _, err := store.ReservePlanBatch(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		batchReplay := batch
		batchReplay.CreatedAt = now.Add(30 * time.Minute)
		reservedAgain, err := store.ReservePlanBatch(t.Context(), batchReplay)
		if err != nil || reservedAgain.Created || !reservedAgain.Batch.CreatedAt.Equal(batch.CreatedAt) {
			t.Fatalf("replayed ReservePlanBatch result=%+v err=%v", reservedAgain, err)
		}
		first, err := store.SealPlan(t.Context(), seal)
		if err != nil || !first.Created {
			t.Fatalf("first SealPlan result=%+v err=%v", first, err)
		}
		replay := seal
		replay.SealedAt = now.Add(time.Hour)
		again, err := store.SealPlan(t.Context(), replay)
		if err != nil || again.Created || !again.Seal.SealedAt.Equal(seal.SealedAt) {
			t.Fatalf("replayed SealPlan result=%+v err=%v", again, err)
		}
	})

	t.Run("plan_execution_ids_are_global", func(t *testing.T) {
		store := factory()
		now := time.Now()
		firstBatch, firstSeal, firstAcquire := executionFixture(now, "execution-plan-global")
		secondBatch, _, _ := executionFixture(now, "execution-plan-collision")
		secondBatch.Steps[0].ExecutionID = firstBatch.Steps[0].ExecutionID
		if _, err := store.ReservePlanBatch(t.Context(), firstBatch); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReservePlanBatch(t.Context(), secondBatch); !errors.Is(err, agent.ErrIdentityConflict) {
			t.Fatalf("duplicate plan execution id error=%v", err)
		}
		if _, err := store.SealPlan(t.Context(), firstSeal); err != nil {
			t.Fatalf("SealPlan after rejected collision: %v", err)
		}
		acquired, err := store.AcquireExecution(t.Context(), firstAcquire)
		if err != nil || acquired.Disposition != agent.ExecutionAcquired {
			t.Fatalf("AcquireExecution after rejected collision result=%+v err=%v", acquired, err)
		}
	})

	t.Run("plan_timestamps_do_not_regress", func(t *testing.T) {
		store := factory()
		now := time.Now()
		first, seal, _ := executionFixture(now, "execution-plan-time-first")
		if _, err := store.ReservePlanBatch(t.Context(), first); err != nil {
			t.Fatal(err)
		}
		second := first
		second.Index = 1
		second.Steps = []agent.OperationPlanStep{{
			ExecutionID: "execution-plan-time-second", Name: first.Steps[0].Name,
			ContractID: first.Steps[0].ContractID,
			Arguments:  append(json.RawMessage(nil), first.Steps[0].Arguments...),
		}}
		second.CreatedAt = now.Add(-time.Second)
		if _, err := store.ReservePlanBatch(t.Context(), second); !errors.Is(err, agent.ErrOperationPlanChanged) {
			t.Fatalf("regressed plan batch timestamp error=%v", err)
		}
		second.CreatedAt = now.Add(time.Second)
		if _, err := store.ReservePlanBatch(t.Context(), second); err != nil {
			t.Fatalf("valid batch after rejected timestamp: %v", err)
		}
		seal.BatchCount = 2
		seal.SealedAt = now.Add(500 * time.Millisecond)
		if _, err := store.SealPlan(t.Context(), seal); !errors.Is(err, agent.ErrOperationPlanChanged) {
			t.Fatalf("regressed seal timestamp error=%v", err)
		}
		seal.SealedAt = now.Add(2 * time.Second)
		if _, err := store.SealPlan(t.Context(), seal); err != nil {
			t.Fatalf("valid seal after rejected timestamp: %v", err)
		}
	})

	t.Run("unknown_retry_fences_old_attempt", func(t *testing.T) {
		store := factory()
		now := time.Now()
		batch, seal, acquisition := executionFixture(now, "execution-retry")
		if _, err := store.ReservePlanBatch(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SealPlan(t.Context(), seal); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcquireExecution(t.Context(), acquisition); err != nil {
			t.Fatal(err)
		}
		unknown := agent.OperationExecutionTransition{
			ID: "transition-unknown", ExecutionID: acquisition.Execution.ID,
			AttemptID: acquisition.Execution.AttemptID, RunID: acquisition.Execution.RunID,
			CallID: acquisition.Execution.CallID, Actor: "executor", Message: "outcome unknown",
			From: agent.OperationExecutionStarted, To: agent.OperationExecutionUnknown,
			CreatedAt: now.Add(time.Second),
		}
		if _, err := store.TransitionExecution(t.Context(), unknown); err != nil {
			t.Fatal(err)
		}
		retryable := unknown
		retryable.ID = "transition-retryable"
		retryable.Actor = "reconciler"
		retryable.Message = "evidence proves not applied"
		retryable.From = agent.OperationExecutionUnknown
		retryable.To = agent.OperationExecutionRetryable
		retryable.Evidence = json.RawMessage(`{"kind":"executor_log","not_applied":true}`)
		retryable.CreatedAt = now.Add(2 * time.Second)
		if _, err := store.TransitionExecution(t.Context(), retryable); err != nil {
			t.Fatal(err)
		}
		retry := acquisition
		retry.Execution.RunID = "run-retry-2"
		retry.Execution.CallID = "call-retry-2"
		retry.Execution.AttemptID = "attempt-retry-2"
		retry.Execution.UpdatedAt = now.Add(3 * time.Second)
		retry.Transition.ID = "transition-acquire-2"
		retry.Transition.RunID = retry.Execution.RunID
		retry.Transition.CallID = retry.Execution.CallID
		retry.Transition.AttemptID = retry.Execution.AttemptID
		retry.Transition.CreatedAt = retry.Execution.UpdatedAt
		if _, err := store.AcquireExecution(t.Context(), retry); err != nil {
			t.Fatal(err)
		}
		if err := store.ValidateExecutionAttempt(t.Context(), acquisition.Execution.ID, acquisition.Execution.AttemptID); !errors.Is(err, agent.ErrOperationAttemptLost) {
			t.Fatalf("old attempt validation error=%v", err)
		}
	})

	t.Run("execution_timestamps_do_not_regress", func(t *testing.T) {
		store := factory()
		now := time.Now()
		batch, seal, acquisition := executionFixture(now, "execution-monotonic-time")
		if _, err := store.ReservePlanBatch(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SealPlan(t.Context(), seal); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcquireExecution(t.Context(), acquisition); err != nil {
			t.Fatal(err)
		}
		unknown := agent.OperationExecutionTransition{
			ID: "transition-monotonic-unknown", ExecutionID: acquisition.Execution.ID,
			AttemptID: acquisition.Execution.AttemptID, RunID: acquisition.Execution.RunID,
			CallID: acquisition.Execution.CallID, Actor: "executor", Message: "outcome unknown",
			From: agent.OperationExecutionStarted, To: agent.OperationExecutionUnknown,
			CreatedAt: now.Add(2 * time.Second),
		}
		if _, err := store.TransitionExecution(t.Context(), unknown); err != nil {
			t.Fatal(err)
		}

		staleTransition := unknown
		staleTransition.ID = "transition-monotonic-stale"
		staleTransition.Actor = "reconciler"
		staleTransition.Message = "evidence proves not applied"
		staleTransition.From = agent.OperationExecutionUnknown
		staleTransition.To = agent.OperationExecutionRetryable
		staleTransition.Evidence = json.RawMessage(`{"kind":"executor_log","not_applied":true}`)
		staleTransition.CreatedAt = now.Add(time.Second)
		if _, err := store.TransitionExecution(t.Context(), staleTransition); !errors.Is(err, agent.ErrInvalidExecutionTransition) {
			t.Fatalf("stale transition error=%v", err)
		}
		stored, err := store.GetExecution(t.Context(), acquisition.Execution.ID)
		if err != nil || stored.Status != agent.OperationExecutionUnknown || !stored.UpdatedAt.Equal(unknown.CreatedAt) {
			t.Fatalf("execution after stale transition=%+v err=%v", stored, err)
		}
		history, err := store.ListExecutionTransitions(t.Context(), acquisition.Execution.ID)
		if err != nil || len(history) != 2 {
			t.Fatalf("history after stale transition=%+v err=%v", history, err)
		}

		retryable := staleTransition
		retryable.ID = "transition-monotonic-retryable"
		retryable.CreatedAt = now.Add(3 * time.Second)
		if _, err := store.TransitionExecution(t.Context(), retryable); err != nil {
			t.Fatal(err)
		}
		staleRetry := acquisition
		staleRetry.Execution.RunID = "run-monotonic-retry"
		staleRetry.Execution.CallID = "call-monotonic-retry"
		staleRetry.Execution.AttemptID = "attempt-monotonic-retry"
		staleRetry.Execution.UpdatedAt = now.Add(2500 * time.Millisecond)
		staleRetry.Transition.ID = "transition-monotonic-acquire"
		staleRetry.Transition.RunID = staleRetry.Execution.RunID
		staleRetry.Transition.CallID = staleRetry.Execution.CallID
		staleRetry.Transition.AttemptID = staleRetry.Execution.AttemptID
		staleRetry.Transition.CreatedAt = staleRetry.Execution.UpdatedAt
		if _, err := store.AcquireExecution(t.Context(), staleRetry); !errors.Is(err, agent.ErrInvalidExecutionTransition) {
			t.Fatalf("stale retry acquisition error=%v", err)
		}
		stored, err = store.GetExecution(t.Context(), acquisition.Execution.ID)
		if err != nil || stored.Status != agent.OperationExecutionRetryable ||
			stored.AttemptID != acquisition.Execution.AttemptID || !stored.UpdatedAt.Equal(retryable.CreatedAt) {
			t.Fatalf("execution after stale retry acquisition=%+v err=%v", stored, err)
		}
		history, err = store.ListExecutionTransitions(t.Context(), acquisition.Execution.ID)
		if err != nil || len(history) != 3 {
			t.Fatalf("history after stale retry acquisition=%+v err=%v", history, err)
		}
	})

	t.Run("plan_and_results_are_defensive_copies", func(t *testing.T) {
		store := factory()
		now := time.Now()
		batch, seal, acquisition := executionFixture(now, "execution-copy")
		originalArguments := append(json.RawMessage(nil), batch.Steps[0].Arguments...)
		reserved, err := store.ReservePlanBatch(t.Context(), batch)
		if err != nil {
			t.Fatal(err)
		}
		batch.Steps[0].Arguments[0] = '['
		reserved.Batch.Steps[0].Arguments[0] = '['
		batch.Steps[0].Arguments = originalArguments
		acquisition.Execution.Arguments = append(json.RawMessage(nil), originalArguments...)
		again, err := store.ReservePlanBatch(t.Context(), batch)
		if err != nil || string(again.Batch.Steps[0].Arguments) != string(originalArguments) {
			t.Fatalf("second reservation=%+v err=%v", again, err)
		}
		if _, err := store.SealPlan(t.Context(), seal); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcquireExecution(t.Context(), acquisition); err != nil {
			t.Fatal(err)
		}
		transition := agent.OperationExecutionTransition{
			ID: "transition-copy-executed", ExecutionID: acquisition.Execution.ID,
			AttemptID: acquisition.Execution.AttemptID, RunID: acquisition.Execution.RunID,
			CallID: acquisition.Execution.CallID, Actor: "executor", Message: "committed",
			From: agent.OperationExecutionStarted, To: agent.OperationExecutionExecuted,
			Result:    agent.OperationResult{Output: json.RawMessage(`{"applied":true}`)},
			CreatedAt: now.Add(time.Second),
		}
		acknowledged, err := store.TransitionExecution(t.Context(), transition)
		if err != nil {
			t.Fatal(err)
		}
		transition.Result.Output[0] = '['
		acknowledged.Result.Output[0] = '['
		stored, err := store.GetExecution(t.Context(), acquisition.Execution.ID)
		if err != nil || string(stored.Result.Output) != `{"applied":true}` {
			t.Fatalf("stored execution=%+v err=%v", stored, err)
		}
		history, err := store.ListExecutionTransitions(t.Context(), acquisition.Execution.ID)
		if err != nil || len(history) != 2 {
			t.Fatalf("history=%+v err=%v", history, err)
		}
		history[1].Result.Output[0] = '['
		historyAgain, err := store.ListExecutionTransitions(t.Context(), acquisition.Execution.ID)
		if err != nil || string(historyAgain[1].Result.Output) != `{"applied":true}` {
			t.Fatalf("second history=%+v err=%v", historyAgain, err)
		}
	})

	t.Run("transition_ids_are_global", func(t *testing.T) {
		store := factory()
		now := time.Now()
		firstBatch, firstSeal, firstAcquire := executionFixture(now, "execution-global-a")
		secondBatch, secondSeal, secondAcquire := executionFixture(now, "execution-global-b")
		for _, fixture := range []struct {
			batch   agent.OperationPlanBatch
			seal    agent.OperationPlanSeal
			acquire agent.AcquireExecutionRequest
		}{{firstBatch, firstSeal, firstAcquire}, {secondBatch, secondSeal, secondAcquire}} {
			if _, err := store.ReservePlanBatch(t.Context(), fixture.batch); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SealPlan(t.Context(), fixture.seal); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AcquireExecution(t.Context(), fixture.acquire); err != nil {
				t.Fatal(err)
			}
		}
		transition := func(acquisition agent.AcquireExecutionRequest) agent.OperationExecutionTransition {
			return agent.OperationExecutionTransition{
				ID: "globally-shared-transition", ExecutionID: acquisition.Execution.ID,
				AttemptID: acquisition.Execution.AttemptID, RunID: acquisition.Execution.RunID,
				CallID: acquisition.Execution.CallID, Actor: "executor", Message: "committed",
				From: agent.OperationExecutionStarted, To: agent.OperationExecutionExecuted,
				Result:    agent.OperationResult{Output: json.RawMessage(`{"applied":true}`)},
				CreatedAt: now.Add(time.Second),
			}
		}
		if _, err := store.TransitionExecution(t.Context(), transition(firstAcquire)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.TransitionExecution(t.Context(), transition(secondAcquire)); !errors.Is(err, agent.ErrIdentityConflict) {
			t.Fatalf("duplicate global transition id error=%v", err)
		}
	})
}

func executionFixture(now time.Time, executionID string) (agent.OperationPlanBatch, agent.OperationPlanSeal, agent.AcquireExecutionRequest) {
	arguments := json.RawMessage(`{"value":1}`)
	batch := agent.OperationPlanBatch{
		RequestID: "request-" + executionID, IdempotencyKey: "key-" + executionID,
		IdempotencyScope: "tenant", CreatedAt: now,
		Steps: []agent.OperationPlanStep{{
			ExecutionID: executionID, Name: "apply_change", ContractID: "contract-v1", Arguments: arguments,
		}},
	}
	seal := agent.OperationPlanSeal{
		RequestID: batch.RequestID, IdempotencyKey: batch.IdempotencyKey,
		IdempotencyScope: batch.IdempotencyScope, BatchCount: 1, SealedAt: now,
	}
	execution := agent.OperationExecutionRecord{
		ID: executionID, IdempotencyKey: batch.IdempotencyKey, IdempotencyScope: batch.IdempotencyScope,
		RunID: "run-" + executionID, CallID: "call-" + executionID,
		AttemptID: "attempt-" + executionID, Name: "apply_change", ContractID: "contract-v1",
		Arguments: arguments, Status: agent.OperationExecutionStarted,
		CreatedAt: now, UpdatedAt: now,
	}
	transition := agent.OperationExecutionTransition{
		ID: "transition-" + executionID, ExecutionID: execution.ID, AttemptID: execution.AttemptID,
		RunID: execution.RunID, CallID: execution.CallID, Actor: "runtime", Message: "acquired",
		To: agent.OperationExecutionStarted, CreatedAt: now,
	}
	return batch, seal, agent.AcquireExecutionRequest{Execution: execution, Transition: transition}
}
