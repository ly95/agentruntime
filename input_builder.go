package agentruntime

import (
	"errors"
	"fmt"
	"strings"
)

// TrustedInputFields contains values that must come from authenticated host
// state rather than request JSON or model output.
type TrustedInputFields struct {
	RunID                   string
	IdempotencyScope        string
	TrustedContext          string
	ImageAttachmentResolver ImageAttachmentResolver
}

// ApplyTrustedInput attaches host-authored fields to a decoded Input and
// validates them before Runtime or a queue is invoked. It rejects an Input that
// already contains trusted fields so transport code cannot accidentally accept
// a programmatically smuggled value.
func ApplyTrustedInput(input Input, trusted TrustedInputFields) (Input, error) {
	if input.RunID != "" || input.IdempotencyScope != "" || input.TrustedContext != "" || input.ImageAttachmentResolver != nil {
		return Input{}, errors.New("agent: decoded input already contains trusted host fields")
	}
	if trusted.ImageAttachmentResolver != nil && isNilDependency(trusted.ImageAttachmentResolver) {
		return Input{}, errors.New("agent: trusted image attachment resolver is nil")
	}
	input.RunID = strings.TrimSpace(trusted.RunID)
	input.IdempotencyScope = strings.TrimSpace(trusted.IdempotencyScope)
	input.TrustedContext = strings.TrimSpace(trusted.TrustedContext)
	input.ImageAttachmentResolver = trusted.ImageAttachmentResolver
	for _, identity := range []struct {
		value string
		name  string
	}{
		{value: input.RunID, name: "run id"},
		{value: input.IdempotencyScope, name: "idempotency scope"},
	} {
		if identity.value != "" {
			if err := validateRuntimeIdentity(identity.value, identity.name); err != nil {
				return Input{}, err
			}
		}
	}
	if input.TrustedContext != "" {
		if _, err := decodeExactJSON([]byte(input.TrustedContext)); err != nil {
			return Input{}, fmt.Errorf("agent: trusted context must be unambiguous valid JSON: %w", err)
		}
	}
	return input, nil
}

// ValidateWriteInput lets a host reject a known write request before invoking
// a model. Session writes require an idempotency key; stateless writes require
// both an idempotency key and a trusted idempotency scope.
func ValidateWriteInput(input Input) error {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return ErrIdempotencyKeyRequired
	}
	if strings.TrimSpace(input.SessionID) == "" && strings.TrimSpace(input.IdempotencyScope) == "" {
		return ErrIdempotencyScopeRequired
	}
	return nil
}
