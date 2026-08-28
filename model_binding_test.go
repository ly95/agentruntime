package agentruntime

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestModelBindingRequiresCanonicalUTF8Components(t *testing.T) {
	if err := defaultTestModelBinding.Validate(); err != nil {
		t.Fatalf("default binding is invalid: %v", err)
	}

	fields := []struct {
		name string
		get  func(ModelBinding) string
		set  func(*ModelBinding, string)
	}{
		{
			name: "provider",
			get:  func(binding ModelBinding) string { return binding.Provider },
			set:  func(binding *ModelBinding, value string) { binding.Provider = value },
		},
		{
			name: "model",
			get:  func(binding ModelBinding) string { return binding.Model },
			set:  func(binding *ModelBinding, value string) { binding.Model = value },
		},
		{
			name: "endpoint_class",
			get:  func(binding ModelBinding) string { return binding.EndpointClass },
			set:  func(binding *ModelBinding, value string) { binding.EndpointClass = value },
		},
		{
			name: "credential_principal",
			get:  func(binding ModelBinding) string { return binding.CredentialPrincipal },
			set:  func(binding *ModelBinding, value string) { binding.CredentialPrincipal = value },
		},
		{
			name: "adapter_version",
			get:  func(binding ModelBinding) string { return binding.AdapterVersion },
			set:  func(binding *ModelBinding, value string) { binding.AdapterVersion = value },
		},
	}
	invalidValues := []struct {
		name       string
		value      func(string) string
		wantDetail string
	}{
		{name: "missing", value: func(string) string { return "" }, wantDetail: "required"},
		{name: "surrounding_whitespace", value: func(value string) string { return " " + value + "\u2003" }, wantDetail: "surrounding whitespace"},
		{name: "control_character", value: func(value string) string { return value + "\x00" }, wantDetail: "control characters"},
		{name: "invalid_utf8", value: func(value string) string { return value + string([]byte{0xff}) }, wantDetail: "invalid UTF-8"},
	}

	for _, field := range fields {
		for _, invalid := range invalidValues {
			t.Run(field.name+"/"+invalid.name, func(t *testing.T) {
				binding := defaultTestModelBinding
				field.set(&binding, invalid.value(field.get(binding)))

				err := binding.Validate()
				if err == nil || !strings.Contains(err.Error(), invalid.wantDetail) {
					t.Fatalf("Validate error=%v, want detail %q", err, invalid.wantDetail)
				}
				id, idErr := binding.ID()
				if idErr == nil || id != "" {
					t.Fatalf("ID=%q error=%v, want empty ID and validation error", id, idErr)
				}
			})
		}
	}
}

func TestModelBindingIDIsStableVersionedAndFieldSensitive(t *testing.T) {
	const want = "model_binding_8c140eef5683c112bf0bdba6d49365293150fbe8d31e365d04a11aaaef08f0e2"

	first, err := defaultTestModelBinding.ID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := defaultTestModelBinding.ID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != want {
		t.Fatalf("stable ID mismatch: first=%q second=%q want=%q", first, second, want)
	}
	const prefix = "model_binding_"
	if !strings.HasPrefix(first, prefix) {
		t.Fatalf("ID %q has no explicit %q prefix", first, prefix)
	}
	encodedDigest := strings.TrimPrefix(first, prefix)
	if len(encodedDigest) != 64 {
		t.Fatalf("digest length=%d, want 64 hex characters", len(encodedDigest))
	}
	if _, err := hex.DecodeString(encodedDigest); err != nil {
		t.Fatalf("digest %q is not hexadecimal: %v", encodedDigest, err)
	}

	changes := []struct {
		name   string
		mutate func(*ModelBinding)
	}{
		{name: "provider", mutate: func(binding *ModelBinding) { binding.Provider += "-changed" }},
		{name: "model", mutate: func(binding *ModelBinding) { binding.Model += "-changed" }},
		{name: "endpoint_class", mutate: func(binding *ModelBinding) { binding.EndpointClass += "-changed" }},
		{name: "credential_principal", mutate: func(binding *ModelBinding) { binding.CredentialPrincipal += "-changed" }},
		{name: "adapter_version", mutate: func(binding *ModelBinding) { binding.AdapterVersion += "-changed" }},
	}
	seen := map[string]string{first: "unchanged"}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			binding := defaultTestModelBinding
			change.mutate(&binding)
			changed, err := binding.ID()
			if err != nil {
				t.Fatal(err)
			}
			if changed == first {
				t.Fatalf("changing %s did not change ID %q", change.name, changed)
			}
			if prior, exists := seen[changed]; exists {
				t.Fatalf("changing %s collided with %s at %q", change.name, prior, changed)
			}
			seen[changed] = change.name
		})
	}
}

func TestPendingApprovalValidateAuthorityRejectsInvalidExpectedModelBinding(t *testing.T) {
	for _, modelBindingID := range []string{"", " model-binding-v1 "} {
		t.Run(modelBindingID, func(t *testing.T) {
			run := RunRecord{
				ID: "authority-binding-run", ModelBindingID: modelBindingID,
				Input: Input{RunID: "authority-binding-run", User: "validate authority"},
			}
			pending := memoryStorePendingApproval(t, run, modelBindingID, 0, time.Unix(1, 0).UTC())
			if err := pending.ValidateAuthority(modelBindingID); !errors.Is(err, ErrModelBindingMismatch) {
				t.Fatalf("ValidateAuthority(%q) error=%v, want ErrModelBindingMismatch", modelBindingID, err)
			}
		})
	}
}

func TestModelBindingIDFramesComponents(t *testing.T) {
	left := defaultTestModelBinding
	left.Provider = "ab"
	left.Model = "c"
	right := defaultTestModelBinding
	right.Provider = "a"
	right.Model = "bc"

	simpleConcat := func(binding ModelBinding) string {
		return binding.Provider + binding.Model + binding.EndpointClass +
			binding.CredentialPrincipal + binding.AdapterVersion
	}
	if simpleConcat(left) != simpleConcat(right) {
		t.Fatal("test fixture does not collide under simple concatenation")
	}

	leftID, err := left.ID()
	if err != nil {
		t.Fatal(err)
	}
	rightID, err := right.ID()
	if err != nil {
		t.Fatal(err)
	}
	if leftID == rightID {
		t.Fatalf("framed binding IDs collided: %q", leftID)
	}
}
