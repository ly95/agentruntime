// Command approval demonstrates the durable write-operation path without any
// network dependency. It is deliberately executable documentation: the same
// scenarios are asserted by main_test.go.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/ly95/agentruntime"
)

const changeOperation = "apply_change"

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	completed, err := runApprovalScenario(context.Background())
	if err != nil {
		return err
	}
	retryable, err := runNotAppliedScenario(context.Background())
	if err != nil {
		return err
	}
	reconciled, err := runReconciliationScenario(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("approval=%s not_applied=%s reconciled=%s\n", completed, retryable, reconciled)
	return nil
}

type scriptedModel struct {
	mu        sync.Mutex
	responses []*agentruntime.ModelResponse
}

func (model *scriptedModel) Complete(context.Context, agentruntime.ModelRequest) (*agentruntime.ModelResponse, error) {
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.responses) == 0 {
		return nil, errors.New("approval example: scripted model is exhausted")
	}
	response := model.responses[0]
	model.responses = model.responses[1:]
	return response, nil
}

type approvalGateway struct {
	store    *agentruntime.InMemoryStore
	mu       sync.Mutex
	approved map[string]string
}

func (gateway *approvalGateway) RequestApproval(_ context.Context, request agentruntime.ApprovalRequest) (agentruntime.ApprovalDecision, error) {
	return agentruntime.ApprovalDecision{
		ID:      "approval-" + request.Operation.ExecutionID,
		Pending: true,
		Reason:  "waiting for an authenticated operator",
	}, nil
}

func (gateway *approvalGateway) approve(runID, reason string) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.approved == nil {
		gateway.approved = make(map[string]string)
	}
	gateway.approved[runID] = reason
}

func (gateway *approvalGateway) ResumeApproval(ctx context.Context, runID string) (*agentruntime.ApprovalResume, error) {
	pending, err := gateway.store.GetPendingApproval(ctx, runID)
	if err != nil {
		return nil, err
	}
	gateway.mu.Lock()
	reason, approved := gateway.approved[runID]
	gateway.mu.Unlock()
	request := pending.Request
	resume := &agentruntime.ApprovalResume{
		ID: pending.Decision.ID, ExecutionID: request.Operation.ExecutionID,
		Operation:  request.Operation.Operation.Name,
		ContractID: request.Operation.Operation.ContractID,
		Call:       request.Operation.Call, ResponseID: request.ResponseID,
		ModelOutput: request.ModelOutput, Preview: request.Preview,
		Checkpoint: request.Checkpoint, Reason: reason,
	}
	if approved {
		resume.Approved = true
	} else {
		resume.Pending = true
	}
	return resume, nil
}

type changeArguments struct {
	Value string `json:"value"`
}

func changeOperations(confirmation agentruntime.ConfirmationMode) (*agentruntime.OperationRegistry, error) {
	registry := agentruntime.NewOperationRegistry()
	description := ""
	var preview func(any) (json.RawMessage, error)
	if confirmation == agentruntime.ConfirmationRequired {
		description = "Confirm the exact value before committing it."
		preview = func(arguments any) (json.RawMessage, error) {
			typed, err := agentruntime.DecodeArguments[changeArguments](arguments)
			if err != nil {
				return nil, err
			}
			return json.Marshal(struct {
				Change string `json:"change"`
			}{Change: typed.Value})
		}
	}
	err := registry.Register(agentruntime.Operation{
		Name: changeOperation, ContractVersion: "example-v1",
		Description: "Apply one durable example value.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"value":{"type":"string","minLength":1,"maxLength":64}},
			"required":["value"],
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"applied":{"type":"boolean"}},
			"required":["applied"],
			"additionalProperties":false
		}`),
		Effect: agentruntime.OperationEffectWrite,
		Confirmation: agentruntime.ConfirmationSpec{
			Mode: confirmation, Description: description,
		},
		ApprovalPreview: preview,
	})
	if err != nil {
		return nil, fmt.Errorf("register change operation: %w", err)
	}
	return registry, nil
}

func requireApprovalPolicy(_ context.Context, request agentruntime.OperationRequest) (agentruntime.PolicyDecision, error) {
	if request.Operation.Confirmation.Mode == agentruntime.ConfirmationRequired {
		return agentruntime.PolicyDecision{
			Action: agentruntime.PolicyRequireApproval,
			Reason: "durable write requires operator approval",
		}, nil
	}
	return agentruntime.PolicyDecision{Action: agentruntime.PolicyAllow}, nil
}

func positiveVerifier(context.Context, agentruntime.VerificationRequest) (agentruntime.VerificationResult, error) {
	return agentruntime.VerificationResult{
		Confirmed: true, Message: "the durable value matches",
		Evidence: json.RawMessage(`{"observed":true}`),
	}, nil
}

func runApprovalScenario(ctx context.Context) (agentruntime.RunStatus, error) {
	operations, err := changeOperations(agentruntime.ConfirmationRequired)
	if err != nil {
		return "", err
	}
	store := agentruntime.NewInMemoryStore()
	gateway := &approvalGateway{store: store}
	model := &scriptedModel{responses: []*agentruntime.ModelResponse{
		functionResponse("response-approval-call", "call-approval", `{"value":"enabled"}`),
		messageResponse("response-approval-done", "The approved change is complete."),
	}}
	runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
		Model: model, Operations: operations, Policy: agentruntime.OperationPolicyFunc(requireApprovalPolicy),
		Executor: agentruntime.OperationExecutorFunc(func(_ context.Context, request agentruntime.OperationRequest) (agentruntime.OperationResult, error) {
			if _, err := agentruntime.DecodeArguments[changeArguments](request.Arguments); err != nil {
				return agentruntime.OperationResult{}, err
			}
			return agentruntime.OperationResult{
				Output:  json.RawMessage(`{"applied":true}`),
				Receipt: json.RawMessage(`{"commit":"example"}`),
			}, nil
		}),
		Verifier: agentruntime.ResultVerifierFunc(positiveVerifier),
		Approver: gateway, ApprovalResumer: gateway,
		RunStore: store, Executions: store,
	})
	if err != nil {
		return "", fmt.Errorf("create approval runtime: %w", err)
	}
	input := agentruntime.Input{
		RunID: "run-approval-example", SessionID: "session-approval-example",
		User: "Apply the example change.", IdempotencyKey: "change-request-1",
	}
	waiting, err := runtime.Run(ctx, input)
	if err != nil {
		return "", fmt.Errorf("start approval run: %w", err)
	}
	if waiting.Status != agentruntime.RunStatusWaitingUser || waiting.PendingApproval == nil {
		return "", fmt.Errorf("approval example: got status %q without pending approval", waiting.Status)
	}
	gateway.approve(input.RunID, "approved by example operator")
	completed, err := runtime.ResumeApproval(ctx, input)
	if err != nil {
		return "", fmt.Errorf("resume approval run: %w", err)
	}
	if completed.Status != agentruntime.RunStatusCompleted {
		return "", fmt.Errorf("approval example: resumed status is %q", completed.Status)
	}
	return completed.Status, nil
}

func runNotAppliedScenario(ctx context.Context) (agentruntime.OperationExecutionStatus, error) {
	operations, err := changeOperations(agentruntime.ConfirmationNone)
	if err != nil {
		return "", err
	}
	store := agentruntime.NewInMemoryStore()
	var executionID string
	runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
		Model: &scriptedModel{responses: []*agentruntime.ModelResponse{
			functionResponse("response-not-applied", "call-not-applied", `{"value":"safe"}`),
		}},
		Operations: operations, Policy: agentruntime.OperationPolicyFunc(requireApprovalPolicy),
		Executor: agentruntime.OperationExecutorFunc(func(_ context.Context, request agentruntime.OperationRequest) (agentruntime.OperationResult, error) {
			executionID = request.ExecutionID
			return agentruntime.OperationResult{}, agentruntime.MarkOperationNotApplied(errors.New("precondition rejected before commit"))
		}),
		RunStore: store, Executions: store,
	})
	if err != nil {
		return "", err
	}
	_, runErr := runtime.Run(ctx, agentruntime.Input{
		RunID: "run-not-applied-example", User: "Apply safely.",
		IdempotencyKey: "not-applied-request", IdempotencyScope: "example-tenant",
	})
	if !errors.Is(runErr, agentruntime.ErrOperationNotApplied) {
		return "", fmt.Errorf("not-applied example: got error %v", runErr)
	}
	record, err := store.GetExecution(ctx, executionID)
	if err != nil {
		return "", err
	}
	if record.Status != agentruntime.OperationExecutionRetryable {
		return "", fmt.Errorf("not-applied example: execution status is %q", record.Status)
	}
	return record.Status, nil
}

func runReconciliationScenario(ctx context.Context) (agentruntime.OperationExecutionStatus, error) {
	operations, err := changeOperations(agentruntime.ConfirmationNone)
	if err != nil {
		return "", err
	}
	store := agentruntime.NewInMemoryStore()
	var executionID, attemptID string
	runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
		Model: &scriptedModel{responses: []*agentruntime.ModelResponse{
			functionResponse("response-unknown", "call-unknown", `{"value":"uncertain"}`),
		}},
		Operations: operations, Policy: agentruntime.OperationPolicyFunc(requireApprovalPolicy),
		Executor: agentruntime.OperationExecutorFunc(func(_ context.Context, request agentruntime.OperationRequest) (agentruntime.OperationResult, error) {
			executionID, attemptID = request.ExecutionID, request.AttemptID
			return agentruntime.OperationResult{}, errors.Join(
				agentruntime.ErrOperationOutcomeUnknown,
				errors.New("connection ended after the commit boundary"),
			)
		}),
		RunStore: store, Executions: store,
	})
	if err != nil {
		return "", err
	}
	_, runErr := runtime.Run(ctx, agentruntime.Input{
		RunID: "run-reconcile-example", User: "Apply an uncertain write.",
		IdempotencyKey: "unknown-request", IdempotencyScope: "example-tenant",
	})
	if !errors.Is(runErr, agentruntime.ErrOperationOutcomeUnknown) {
		return "", fmt.Errorf("reconciliation example: got error %v", runErr)
	}
	reconciler, err := agentruntime.NewOperationReconciler(operations, store)
	if err != nil {
		return "", err
	}
	err = reconciler.ReconcileOperation(ctx, agentruntime.ReconcileOperationRequest{
		ExecutionID: executionID, ExpectedAttemptID: attemptID,
		Action: agentruntime.OperationReconciliationComplete,
		Result: agentruntime.OperationResult{Output: json.RawMessage(`{"applied":true}`)},
		Actor:  "example-operator", Message: "commit verified in the authoritative system",
		Evidence: json.RawMessage(`{"source":"authoritative-read","commit_present":true}`),
	})
	if err != nil {
		return "", fmt.Errorf("reconcile unknown execution: %w", err)
	}
	record, err := store.GetExecution(ctx, executionID)
	if err != nil {
		return "", err
	}
	if record.Status != agentruntime.OperationExecutionCompleted {
		return "", fmt.Errorf("reconciliation example: execution status is %q", record.Status)
	}
	return record.Status, nil
}

func functionResponse(responseID, callID, arguments string) *agentruntime.ModelResponse {
	itemID := responseID + "-item"
	call := &agentruntime.ToolCall{ID: callID, Name: changeOperation, Input: json.RawMessage(arguments)}
	raw, _ := json.Marshal(map[string]any{
		"id": itemID, "type": "function_call", "status": "completed",
		"call_id": callID, "name": changeOperation, "arguments": arguments,
	})
	return &agentruntime.ModelResponse{ID: responseID, Items: []agentruntime.ModelOutputItem{{
		ID: itemID, Type: agentruntime.ModelOutputFunctionCall, Call: call, Raw: raw,
	}}}
}

func messageResponse(responseID, text string) *agentruntime.ModelResponse {
	itemID := responseID + "-item"
	raw, _ := json.Marshal(map[string]any{
		"id": itemID, "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		}},
	})
	return &agentruntime.ModelResponse{
		ID: responseID, OutputText: text,
		Items: []agentruntime.ModelOutputItem{{
			ID: itemID, Type: agentruntime.ModelOutputMessage, Text: text, Raw: raw,
		}},
	}
}
