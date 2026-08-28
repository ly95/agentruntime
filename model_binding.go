package agentruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const modelBindingIdentityVersion = "agentruntime.model-binding.v1"

// ModelBinding identifies the complete provider adapter authority used for a
// model call. EndpointClass and CredentialPrincipal are stable, non-secret host
// identifiers; they must not contain an endpoint URL, API key, or access token.
// AdapterVersion identifies the adapter's mapping and replay contract rather
// than the agentruntime module version.
type ModelBinding struct {
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	EndpointClass       string `json:"endpoint_class"`
	CredentialPrincipal string `json:"credential_principal"`
	AdapterVersion      string `json:"adapter_version"`
}

// Validate checks that every binding component is explicit and canonical.
func (binding ModelBinding) Validate() error {
	if err := validateUTF8Boundary("model binding", binding); err != nil {
		return err
	}
	fields := []struct {
		name  string
		value string
	}{
		{name: "provider", value: binding.Provider},
		{name: "model", value: binding.Model},
		{name: "endpoint class", value: binding.EndpointClass},
		{name: "credential principal", value: binding.CredentialPrincipal},
		{name: "adapter version", value: binding.AdapterVersion},
	}
	for _, field := range fields {
		if err := validateModelBindingComponent(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

func validateModelBindingComponent(name, value string) error {
	if value == "" {
		return fmt.Errorf("agent: model binding %s is required", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("agent: model binding %s must be valid UTF-8", name)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("agent: model binding %s must be canonical without surrounding whitespace", name)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("agent: model binding %s cannot contain control characters", name)
	}
	return nil
}

// ID returns a versioned digest suitable for durable RunRecord, SessionState,
// and approval authority bindings. It intentionally does not expose endpoint or
// credential-principal text in persisted runtime records.
func (binding ModelBinding) ID() (string, error) {
	if err := binding.Validate(); err != nil {
		return "", err
	}
	digest := sha256.New()
	writeHashField(digest, []byte(modelBindingIdentityVersion))
	writeHashField(digest, []byte(binding.Provider))
	writeHashField(digest, []byte(binding.Model))
	writeHashField(digest, []byte(binding.EndpointClass))
	writeHashField(digest, []byte(binding.CredentialPrincipal))
	writeHashField(digest, []byte(binding.AdapterVersion))
	return "model_binding_" + hex.EncodeToString(digest.Sum(nil)), nil
}

// BoundModel exposes immutable provider and adapter identity. Runtime requires
// this interface whenever a RunStore is configured because adapter replay
// envelopes and approval checkpoints then cross durable run boundaries.
type BoundModel interface {
	Model
	// Binding may be called concurrently with Complete, must not wait for an active
	// Complete or its StreamSink, and must always return the same immutable value
	// for the lifetime of the model.
	Binding() ModelBinding
}

func (r *Runtime) validateCurrentModelBinding() error {
	if r == nil || r.modelBindingID == "" {
		return nil
	}
	bound, ok := r.model.(BoundModel)
	if !ok {
		return fmt.Errorf("%w: durable runtime model no longer implements BoundModel", ErrModelBindingMismatch)
	}
	current := bound.Binding()
	if err := current.Validate(); err != nil {
		return fmt.Errorf("%w: durable model binding became invalid: %v", ErrModelBindingMismatch, err)
	}
	if current != r.modelBinding {
		return fmt.Errorf("%w: durable model binding changed after runtime construction", ErrModelBindingMismatch)
	}
	return nil
}

func boundModelBinding(model Model) (ModelBinding, string, error) {
	bound, ok := model.(BoundModel)
	if !ok {
		return ModelBinding{}, "", errors.New("agent: durable runtime model must implement BoundModel")
	}
	binding := bound.Binding()
	id, err := binding.ID()
	if err != nil {
		return ModelBinding{}, "", fmt.Errorf("agent: invalid durable model binding: %w", err)
	}
	if repeated := bound.Binding(); repeated != binding {
		return ModelBinding{}, "", errors.New("agent: durable model binding is not stable during runtime construction")
	}
	return binding, id, nil
}
