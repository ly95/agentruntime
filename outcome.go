package agentruntime

import (
	"context"
	"errors"
)

// RunOutcome is the stable host-facing classification of a Run return. It
// deliberately separates retryability from reconciliation: an unknown write
// outcome must be reconciled, never blindly retried.
type RunOutcome struct {
	Status                 RunStatus `json:"status"`
	ErrorCode              string    `json:"error_code,omitempty"`
	Message                string    `json:"message"`
	Retryable              bool      `json:"retryable"`
	RequiresReconciliation bool      `json:"requires_reconciliation"`
}

// ClassifyRunOutcome maps Result/error pairs to HTTP, queue, and UI semantics.
func ClassifyRunOutcome(result *Result, err error) RunOutcome {
	if err == nil && result != nil {
		return RunOutcome{
			Status:  result.Status,
			Message: publicRunStatusMessage(result.Status),
		}
	}
	code := ErrorCode(err)
	outcome := RunOutcome{Status: RunStatusFailed, ErrorCode: code, Message: PublicErrorMessage(code)}
	switch {
	case errors.Is(err, ErrOperationOutcomeUnknown):
		outcome.Status = RunStatusInterrupted
		outcome.RequiresReconciliation = true
	case errors.Is(err, context.Canceled), errors.Is(err, ErrRunCancelled):
		outcome.Status = RunStatusCancelled
	case errors.Is(err, ErrRunInterrupted), errors.Is(err, ErrSessionLeaseLost),
		errors.Is(err, ErrProviderRateLimited), errors.Is(err, ErrProviderUnavailable),
		errors.Is(err, context.DeadlineExceeded):
		outcome.Status = RunStatusInterrupted
		outcome.Retryable = true
	}
	return outcome
}

// ErrorCode exposes Runtime's stable error classifier.
func ErrorCode(err error) string { return errorCode(err) }

// PublicErrorMessage returns non-sensitive product copy for a stable error code.
func PublicErrorMessage(code string) string {
	switch code {
	case "":
		return ""
	case "provider_rate_limited":
		return "The model provider is busy. Retry after backoff."
	case "provider_quota_exceeded":
		return "The model provider quota is unavailable."
	case "provider_authentication":
		return "The model provider credentials were rejected."
	case "provider_unavailable":
		return "The model provider is temporarily unavailable."
	case "operation_outcome_unknown":
		return "The operation outcome must be reconciled before retrying."
	case "approval_pending":
		return "Approval is pending."
	case "approval_denied":
		return "Approval was denied."
	case "run_cancelled":
		return "The run was cancelled."
	case "run_interrupted", "session_lease_lost":
		return "The run was interrupted and may be retried when safe."
	default:
		return "The run failed."
	}
}

func publicRunStatusMessage(status RunStatus) string {
	switch status {
	case RunStatusCompleted:
		return "The run completed."
	case RunStatusWaitingUser:
		return "The run is waiting for approval."
	case RunStatusCancelled:
		return "The run was cancelled."
	case RunStatusInterrupted:
		return "The run was interrupted."
	case RunStatusFailed:
		return "The run failed."
	case RunStatusRunning:
		return "The run is running."
	default:
		return "The run status is unknown."
	}
}
