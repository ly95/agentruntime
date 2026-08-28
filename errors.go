package agentruntime

import (
	"context"
	"errors"
)

var (
	ErrInvalidModelOutput         = errors.New("agent: invalid model output")
	ErrOperationNotFound          = errors.New("agent: operation not found")
	ErrOperationDenied            = errors.New("agent: operation denied")
	ErrExecutionStoreRequired     = errors.New("agent: execution store is required")
	ErrOperationExecutionNotFound = errors.New("agent: operation execution not found")
	ErrOperationPlanChanged       = errors.New("agent: operation plan changed during retry")
	ErrOperationAttemptLost       = errors.New("agent: operation execution attempt lost ownership")
	ErrInvalidExecutionTransition = errors.New("agent: invalid operation execution transition")
	ErrInvalidReconciliation      = errors.New("agent: invalid operation reconciliation")
	ErrIdempotencyKeyRequired     = errors.New("agent: idempotency key is required")
	ErrIdempotencyScopeRequired   = errors.New("agent: idempotency scope is required for stateless writes")
	ErrOperationNotApplied        = errors.New("agent: operation was definitely not applied")
	ErrOperationOutcomeUnknown    = errors.New("agent: operation outcome is unknown")
	ErrApprovalRequired           = errors.New("agent: operation approval required")
	ErrApprovalPending            = errors.New("agent: operation approval pending")
	ErrApprovalDenied             = errors.New("agent: operation approval denied")
	ErrVerifierRequired           = errors.New("agent: result verifier required")
	ErrVerificationFailed         = errors.New("agent: result verification failed")
	ErrMaxIterations              = errors.New("agent: max iterations reached")
	ErrSessionNotFound            = errors.New("agent: session not found")
	ErrRunNotFound                = errors.New("agent: run not found")
	ErrSessionConflict            = errors.New("agent: session revision conflict")
	ErrSessionBusy                = errors.New("agent: session already has an active run")
	ErrIdentityConflict           = errors.New("agent: runtime identity conflict")
	ErrRunStoreProtocol           = errors.New("agent: run store protocol violation")
	ErrSessionLeaseLost           = errors.New("agent: session lease ownership lost")
	ErrSessionStoreNeeded         = errors.New("agent: session store is required")
	ErrSkillSetMismatch           = errors.New("agent: session SkillSet mismatch")
	ErrModelBindingMismatch       = errors.New("agent: model binding mismatch")
	ErrContextLimitExceeded       = errors.New("agent: context limit exceeded")
	ErrContextCompactionFailed    = errors.New("agent: context compaction failed")
	ErrImageAttachmentUnavailable = errors.New("agent: image attachment unavailable")
	ErrProviderRateLimited        = errors.New("agent: provider rate limited")
	ErrProviderQuotaExceeded      = errors.New("agent: provider quota exceeded")
	ErrProviderAuthentication     = errors.New("agent: provider authentication failed")
	ErrProviderUnavailable        = errors.New("agent: provider temporarily unavailable")
	ErrProviderRequestRejected    = errors.New("agent: provider request rejected")
	// ErrInsufficientCredits is retained as a compatibility alias. Runtime does
	// not own billing semantics; use ErrProviderQuotaExceeded instead.
	// Deprecated: use ErrProviderQuotaExceeded.
	ErrInsufficientCredits = ErrProviderQuotaExceeded
	ErrRunInterrupted      = errors.New("agent: run interrupted")
	ErrRunCancelled        = errors.New("agent: run cancelled")
)

// MarkOperationNotApplied lets a write executor prove that it failed before
// reaching its side-effect commit boundary. Runtime may safely make that
// execution retryable without entering ambiguous-outcome recovery.
func MarkOperationNotApplied(cause error) error {
	if cause == nil {
		return ErrOperationNotApplied
	}
	return errors.Join(ErrOperationNotApplied, cause)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrInvalidModelOutput):
		return "invalid_model_output"
	case errors.Is(err, ErrOperationNotFound):
		return "operation_not_found"
	case errors.Is(err, ErrOperationDenied):
		return "operation_denied"
	case errors.Is(err, ErrExecutionStoreRequired):
		return "execution_store_required"
	case errors.Is(err, ErrOperationExecutionNotFound):
		return "operation_execution_not_found"
	case errors.Is(err, ErrOperationPlanChanged):
		return "operation_plan_changed"
	case errors.Is(err, ErrOperationAttemptLost):
		return "operation_attempt_lost"
	case errors.Is(err, ErrInvalidExecutionTransition):
		return "invalid_execution_transition"
	case errors.Is(err, ErrInvalidReconciliation):
		return "invalid_operation_reconciliation"
	case errors.Is(err, ErrIdempotencyKeyRequired):
		return "idempotency_key_required"
	case errors.Is(err, ErrIdempotencyScopeRequired):
		return "idempotency_scope_required"
	case errors.Is(err, ErrOperationNotApplied):
		return "operation_not_applied"
	case errors.Is(err, ErrOperationOutcomeUnknown):
		return "operation_outcome_unknown"
	case errors.Is(err, ErrApprovalRequired):
		return "approval_required"
	case errors.Is(err, ErrApprovalPending):
		return "approval_pending"
	case errors.Is(err, ErrApprovalDenied):
		return "approval_denied"
	case errors.Is(err, ErrVerifierRequired):
		return "verifier_required"
	case errors.Is(err, ErrVerificationFailed):
		return "verification_failed"
	case errors.Is(err, ErrMaxIterations):
		return "max_iterations"
	case errors.Is(err, ErrSessionNotFound):
		return "session_not_found"
	case errors.Is(err, ErrRunNotFound):
		return "run_not_found"
	case errors.Is(err, ErrSessionConflict):
		return "session_conflict"
	case errors.Is(err, ErrSessionBusy):
		return "session_busy"
	case errors.Is(err, ErrIdentityConflict):
		return "identity_conflict"
	case errors.Is(err, ErrRunStoreProtocol):
		return "run_store_protocol"
	case errors.Is(err, ErrSessionLeaseLost):
		return "session_lease_lost"
	case errors.Is(err, ErrSessionStoreNeeded):
		return "session_store_required"
	case errors.Is(err, ErrSkillSetMismatch):
		return "skill_set_mismatch"
	case errors.Is(err, ErrModelBindingMismatch):
		return "model_binding_mismatch"
	case errors.Is(err, ErrContextLimitExceeded):
		return "context_limit_exceeded"
	case errors.Is(err, ErrContextCompactionFailed):
		return "context_compaction_failed"
	case errors.Is(err, ErrImageAttachmentUnavailable):
		return "image_attachment_unavailable"
	case errors.Is(err, ErrProviderRateLimited):
		return "provider_rate_limited"
	case errors.Is(err, ErrProviderQuotaExceeded):
		return "provider_quota_exceeded"
	case errors.Is(err, ErrProviderAuthentication):
		return "provider_authentication"
	case errors.Is(err, ErrProviderUnavailable):
		return "provider_unavailable"
	case errors.Is(err, ErrProviderRequestRejected):
		return "provider_request_rejected"
	case errors.Is(err, ErrRunInterrupted):
		return "run_interrupted"
	case errors.Is(err, ErrRunCancelled):
		return "run_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "run_interrupted"
	case errors.Is(err, context.Canceled):
		return "run_cancelled"
	default:
		return "internal_error"
	}
}
