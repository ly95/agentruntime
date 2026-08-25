package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
)

// ProviderErrorCategory is a business-neutral failure class that hosts can map
// to retry policy, HTTP status, queue behavior, and user-facing copy.
type ProviderErrorCategory string

const (
	ProviderErrorRateLimit      ProviderErrorCategory = "rate_limit"
	ProviderErrorQuota          ProviderErrorCategory = "quota"
	ProviderErrorAuthentication ProviderErrorCategory = "authentication"
	ProviderErrorTransient      ProviderErrorCategory = "transient"
	ProviderErrorRejected       ProviderErrorCategory = "rejected"
)

// ProviderError carries stable provider failure metadata without exposing a
// provider response body through Error. The original error remains available
// through errors.As/errors.Unwrap for trusted diagnostics.
type ProviderError struct {
	Provider   string
	Category   ProviderErrorCategory
	Code       string
	StatusCode int
	RequestID  string
	cause      error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "agent: provider error"
	}
	provider := strings.TrimSpace(e.Provider)
	if provider == "" {
		provider = "model"
	}
	detail := string(e.Category)
	if detail == "" {
		detail = "error"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("agent: %s provider %s (HTTP %d)", provider, detail, e.StatusCode)
	}
	return fmt.Sprintf("agent: %s provider %s", provider, detail)
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *ProviderError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch e.Category {
	case ProviderErrorRateLimit:
		return target == ErrProviderRateLimited
	case ProviderErrorQuota:
		return target == ErrProviderQuotaExceeded || target == ErrInsufficientCredits
	case ProviderErrorAuthentication:
		return target == ErrProviderAuthentication
	case ProviderErrorTransient:
		return target == ErrProviderUnavailable
	case ProviderErrorRejected:
		return target == ErrProviderRequestRejected
	default:
		return false
	}
}

// NewProviderError constructs a stable provider failure. Category must be one
// of the exported ProviderError constants.
func NewProviderError(provider string, category ProviderErrorCategory, code string, statusCode int, requestID string, cause error) (*ProviderError, error) {
	switch category {
	case ProviderErrorRateLimit, ProviderErrorQuota, ProviderErrorAuthentication, ProviderErrorTransient, ProviderErrorRejected:
	default:
		return nil, fmt.Errorf("agent: unsupported provider error category %q", category)
	}
	if statusCode < 0 || statusCode > 999 {
		return nil, fmt.Errorf("agent: invalid provider HTTP status %d", statusCode)
	}
	if err := validateUTF8Boundary("provider error", struct {
		Provider  string
		Code      string
		RequestID string
	}{Provider: provider, Code: code, RequestID: requestID}); err != nil {
		return nil, err
	}
	return &ProviderError{
		Provider: strings.TrimSpace(provider), Category: category,
		Code: strings.TrimSpace(code), StatusCode: statusCode,
		RequestID: strings.TrimSpace(requestID), cause: cause,
	}, nil
}

// ProviderErrorCategoryOf returns the stable category of err.
func ProviderErrorCategoryOf(err error) (ProviderErrorCategory, bool) {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && providerErr != nil && providerErr.Category != "" {
		return providerErr.Category, true
	}
	switch {
	case errors.Is(err, ErrProviderRateLimited):
		return ProviderErrorRateLimit, true
	case errors.Is(err, ErrProviderQuotaExceeded):
		return ProviderErrorQuota, true
	case errors.Is(err, ErrProviderAuthentication):
		return ProviderErrorAuthentication, true
	case errors.Is(err, ErrProviderUnavailable):
		return ProviderErrorTransient, true
	case errors.Is(err, ErrProviderRequestRejected):
		return ProviderErrorRejected, true
	default:
		return "", false
	}
}

// IsRetryableProviderError reports whether the stable provider category is safe
// to retry subject to the host's idempotency and backoff policy.
func IsRetryableProviderError(err error) bool {
	category, ok := ProviderErrorCategoryOf(err)
	return ok && (category == ProviderErrorRateLimit || category == ProviderErrorTransient)
}

func classifyOpenAIError(err error) error {
	if err == nil {
		return nil
	}
	var already *ProviderError
	if errors.As(err, &already) {
		return err
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr != nil {
		category := openAIProviderCategory(apiErr.StatusCode, apiErr.Code, apiErr.Type)
		requestID := ""
		if apiErr.Response != nil {
			requestID = apiErr.Response.Header.Get("x-request-id")
		}
		classified, buildErr := NewProviderError("openai", category, apiErr.Code, apiErr.StatusCode, requestID, err)
		if buildErr != nil {
			return errors.Join(err, buildErr)
		}
		return classified
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		var netErr net.Error
		if errors.As(err, &netErr) {
			classified, buildErr := NewProviderError("openai", ProviderErrorTransient, "transport_error", 0, "", err)
			if buildErr == nil {
				return classified
			}
		}
	}
	return err
}

func openAIProviderCategory(status int, values ...string) ProviderErrorCategory {
	joined := strings.ToLower(strings.Join(values, " "))
	if strings.Contains(joined, "insufficient_quota") || strings.Contains(joined, "quota_exceeded") ||
		strings.Contains(joined, "billing_hard_limit") {
		return ProviderErrorQuota
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ProviderErrorAuthentication
	case http.StatusTooManyRequests:
		return ProviderErrorRateLimit
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly:
		return ProviderErrorTransient
	}
	if status >= 500 {
		return ProviderErrorTransient
	}
	return ProviderErrorRejected
}
