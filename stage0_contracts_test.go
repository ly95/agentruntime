package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

type failingEntropyReader struct{ err error }

func (reader failingEntropyReader) Read([]byte) (int, error) { return 0, reader.err }

func TestRandomIDReturnsEntropyFailure(t *testing.T) {
	sentinel := errors.New("entropy unavailable")
	if id, err := randomIDFrom(failingEntropyReader{err: sentinel}); id != "" || !errors.Is(err, sentinel) {
		t.Fatalf("randomIDFrom() id=%q err=%v, want explicit entropy failure", id, err)
	}
}

func TestRuntimeReturnsIdentityFactoryFailure(t *testing.T) {
	sentinel := errors.New("entropy unavailable")
	model := &scriptedModel{responses: []*ModelResponse{messageResponse("must-not-run", "unexpected")}}
	runtime, err := NewRuntime(RuntimeConfig{
		Model: model, RunStore: &recordingStore{},
		IDFactory: func() (string, error) { return "", sentinel },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(t.Context(), Input{User: "hello"})
	if result != nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "generate run id") {
		t.Fatalf("Run() result=%+v err=%v, want identity factory failure", result, err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests=%d, want zero", len(model.requests))
	}
}

func TestOperationReconcilerReturnsIdentityFactoryFailure(t *testing.T) {
	operations := NewOperationRegistry()
	store := &recordingStore{}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	sentinel := errors.New("entropy unavailable")
	reconciler, err := NewOperationReconcilerWithConfig(OperationReconcilerConfig{
		Operations: operations, Executions: store,
		IDFactory: func() (string, error) { return "", sentinel },
		Now:       func() time.Time { return time.Unix(200, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = reconciler.ReconcileOperation(t.Context(), startedCompletionRequest(execution))
	if !errors.Is(err, sentinel) {
		t.Fatalf("ReconcileOperation() error=%v, want identity factory failure", err)
	}
	history, historyErr := store.ListExecutionTransitions(t.Context(), execution.ID)
	if historyErr != nil {
		t.Fatal(historyErr)
	}
	if len(history) != 1 {
		t.Fatalf("transition count=%d, want acquisition only", len(history))
	}
}

func TestOperationReconcilerEmitsReconciliationLifecycle(t *testing.T) {
	operations := NewOperationRegistry()
	store := &recordingStore{}
	execution := seedStartedReconciliationExecution(t, operations, store, false)
	var events []Event
	reconciler, err := NewOperationReconcilerWithConfig(OperationReconcilerConfig{
		Operations: operations, Executions: store,
		IDFactory: func() (string, error) { return "reconciliation-event-transition", nil },
		Now:       func() time.Time { return time.Unix(300, 0) },
		EventSink: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := startedCompletionRequest(execution)
	if err := reconciler.ReconcileOperation(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != EventReconciliationStarted ||
		events[1].Type != EventReconciliationCompleted {
		t.Fatalf("events=%+v", events)
	}
	for _, event := range events {
		if event.ExecutionID != execution.ID || event.AttemptID != execution.AttemptID ||
			event.Reconciliation != string(request.Action) {
			t.Fatalf("event=%+v request=%+v", event, request)
		}
	}
}

func TestWaitingResultAndEventCarrySameApprovalSummary(t *testing.T) {
	model := &scriptedModel{responses: []*ModelResponse{
		callResponse("resp-pending", ToolCall{ID: "call-pending", Name: "apply_change", Input: json.RawMessage(`{}`)}),
	}}
	operations := NewOperationRegistry()
	if err := operations.Register(operation("apply_change", OperationEffectWrite)); err != nil {
		t.Fatal(err)
	}
	policy := OperationPolicyFunc(func(context.Context, OperationRequest) (PolicyDecision, error) {
		return PolicyDecision{Action: PolicyRequireApproval, Reason: "review the proposed change"}, nil
	})
	store := &recordingStore{}
	var waiting Event
	runtime := newTestRuntimeWithEventSink(t, model, operations, policy,
		OperationExecutorFunc(func(context.Context, OperationRequest) (OperationResult, error) {
			t.Fatal("pending operation executed")
			return OperationResult{}, nil
		}), confirmingVerifier(), &resumableApprover{}, store, func(event Event) {
			if event.Type == EventRunWaitingUser {
				waiting = event
			}
		})

	result, err := runtime.Run(t.Context(), Input{
		RunID: "run-pending-summary", SessionID: "session-pending-summary",
		User: "apply", IdempotencyKey: "pending-summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunStatusWaitingUser || result.PendingApproval == nil {
		t.Fatalf("result=%+v, want waiting approval summary", result)
	}
	pending := result.PendingApproval
	if pending.ID != "approval-1" || pending.Digest == "" || pending.Operation != "apply_change" ||
		pending.ExecutionID == "" || pending.Reason != "review the proposed change" ||
		string(pending.Preview) != `{"change":"test"}` {
		t.Fatalf("pending summary=%+v", pending)
	}
	if waiting.ApprovalID != pending.ID || waiting.ApprovalDigest != pending.Digest ||
		waiting.ApprovalReason != pending.Reason || string(waiting.ApprovalPreview) != string(pending.Preview) {
		t.Fatalf("waiting event=%+v pending=%+v", waiting, pending)
	}
}

func TestOpenAIProviderErrorClassification(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		status    int
		code      string
		want      error
		category  ProviderErrorCategory
		retryable bool
		errorCode string
	}{
		{name: "rate limit", status: http.StatusTooManyRequests, code: "rate_limit_exceeded", want: ErrProviderRateLimited, category: ProviderErrorRateLimit, retryable: true, errorCode: "provider_rate_limited"},
		{name: "quota", status: http.StatusTooManyRequests, code: "insufficient_quota", want: ErrProviderQuotaExceeded, category: ProviderErrorQuota, errorCode: "provider_quota_exceeded"},
		{name: "authentication", status: http.StatusUnauthorized, code: "invalid_api_key", want: ErrProviderAuthentication, category: ProviderErrorAuthentication, errorCode: "provider_authentication"},
		{name: "transient", status: http.StatusServiceUnavailable, code: "server_error", want: ErrProviderUnavailable, category: ProviderErrorTransient, retryable: true, errorCode: "provider_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: make(http.Header), Request: request}
			response.Header.Set("x-request-id", "req-test")
			classified := classifyOpenAIError(&openai.Error{
				Code: test.code, Type: test.code, StatusCode: test.status,
				Request: request, Response: response,
			})
			if !errors.Is(classified, test.want) {
				t.Fatalf("classified error=%v, want %v", classified, test.want)
			}
			category, ok := ProviderErrorCategoryOf(classified)
			if !ok || category != test.category || IsRetryableProviderError(classified) != test.retryable {
				t.Fatalf("category=%q ok=%v retryable=%v", category, ok, IsRetryableProviderError(classified))
			}
			if code := errorCode(classified); code != test.errorCode {
				t.Fatalf("errorCode=%q, want %q", code, test.errorCode)
			}
			var providerErr *ProviderError
			if !errors.As(classified, &providerErr) || providerErr.RequestID != "req-test" {
				t.Fatalf("ProviderError=%+v", providerErr)
			}
		})
	}
}
