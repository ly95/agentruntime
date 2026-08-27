package mcpadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"

	"github.com/ly95/agentruntime"
)

// Execute dispatches one allowlisted read after validating the frozen operation
// contract and current transport binding. It performs exactly one tools/call and
// never retries, refreshes discovery, requests approval, or follows MRTR input.
func (snapshot *Snapshot) Execute(ctx context.Context, request agentruntime.OperationRequest) (agentruntime.OperationResult, error) {
	if snapshot == nil {
		return agentruntime.OperationResult{}, errors.New("mcpadapter: nil snapshot")
	}
	if ctx == nil {
		return agentruntime.OperationResult{}, errors.New("mcpadapter: context is required")
	}
	selected, exists := snapshot.byName[request.Call.Name]
	if !exists {
		return agentruntime.OperationResult{}, errors.New("mcpadapter: operation is not part of the snapshot")
	}
	if err := validateOperationRequest(request, selected); err != nil {
		return agentruntime.OperationResult{}, err
	}
	normalizedArguments, err := selected.normalizer.normalize(request.Arguments)
	if err != nil {
		return agentruntime.OperationResult{}, errors.New("mcpadapter: operation arguments violate the frozen input contract")
	}
	arguments, err := marshalNativeJSON(normalizedArguments)
	if err != nil {
		return agentruntime.OperationResult{}, errors.New("mcpadapter: operation arguments are not exact native JSON")
	}
	if err := snapshot.validator.ValidateInput(selected.operation.Name, arguments); err != nil {
		return agentruntime.OperationResult{}, errors.New("mcpadapter: operation arguments violate the frozen input contract")
	}
	parameterHeaders, err := extractParameterHeaders(normalizedArguments, selected.headers)
	if err != nil {
		return agentruntime.OperationResult{}, err
	}
	params, err := requestParams(map[string]any{
		"name":      selected.remoteName,
		"arguments": json.RawMessage(arguments),
	})
	if err != nil {
		return agentruntime.OperationResult{}, err
	}
	result, err := roundTrip(
		ctx,
		snapshot.transport,
		snapshot.bindingID,
		snapshot.limits,
		"tools/call",
		selected.remoteName,
		params,
		parameterHeaders,
		Correlation{
			RunID:       request.RunID,
			CallID:      request.Call.ID,
			ExecutionID: request.ExecutionID,
			AttemptID:   request.AttemptID,
		},
	)
	if err != nil {
		return agentruntime.OperationResult{}, err
	}
	if err := requireCompleteResult(result); err != nil {
		return agentruntime.OperationResult{}, err
	}
	output, err := structuredToolOutput(result)
	if err != nil {
		return agentruntime.OperationResult{}, err
	}
	if err := snapshot.validator.ValidateOutput(selected.operation.Name, output); err != nil {
		return agentruntime.OperationResult{}, errors.New("mcpadapter: remote output violates the frozen output contract")
	}
	return agentruntime.OperationResult{Output: output}, nil
}

func validateOperationRequest(request agentruntime.OperationRequest, selected snapshotOperation) error {
	if request.RunID == "" || request.Call.ID == "" {
		return errors.New("mcpadapter: runtime run and call identities are required")
	}
	if request.Call.Name != selected.operation.Name || request.Operation.Name != selected.operation.Name {
		return errors.New("mcpadapter: operation request name does not match the frozen mapping")
	}
	if !equalOperationSummary(request.Operation, selected.summary) {
		return errors.New("mcpadapter: operation request contract does not match the frozen snapshot")
	}
	return nil
}

func equalOperationSummary(left, right agentruntime.OperationSummary) bool {
	if left.Name != right.Name ||
		left.ContractID != right.ContractID ||
		left.Description != right.Description ||
		left.Effect != right.Effect ||
		left.Confirmation != right.Confirmation ||
		left.Terminal != right.Terminal ||
		left.TerminalBatchLimit != right.TerminalBatchLimit ||
		!bytes.Equal(left.InputSchema, right.InputSchema) ||
		!bytes.Equal(left.OutputSchema, right.OutputSchema) ||
		!equalStrings(left.PreviousNames, right.PreviousNames) ||
		!equalStrings(left.Capabilities, right.Capabilities) {
		return false
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func structuredToolOutput(result map[string]any) (json.RawMessage, error) {
	content, exists := result["content"].([]any)
	if !exists {
		return nil, errors.New("mcpadapter: tools/call content array is required")
	}
	if err := validateContentBlocks(content); err != nil {
		return nil, err
	}
	if metadata, exists := result["_meta"]; exists {
		if _, ok := metadata.(map[string]any); !ok {
			return nil, errors.New("mcpadapter: tools/call result metadata must be an object")
		}
	}
	if errorValue, exists := result["isError"]; exists {
		isError, ok := errorValue.(bool)
		if !ok {
			return nil, errors.New("mcpadapter: tools/call isError must be a boolean")
		}
		if isError {
			return nil, errors.New("mcpadapter: remote tool reported an execution error")
		}
	}
	structured, exists := result["structuredContent"]
	if !exists {
		return nil, errors.New("mcpadapter: remote tool did not return structuredContent")
	}
	output, err := marshalNativeJSON(structured)
	if err != nil {
		return nil, errors.New("mcpadapter: encode remote structuredContent")
	}
	return output, nil
}

func validateContentBlocks(content []any) error {
	for _, value := range content {
		block, ok := value.(map[string]any)
		if !ok || block == nil {
			return errors.New("mcpadapter: tools/call content block is invalid")
		}
		kind, ok := block["type"].(string)
		if !ok {
			return errors.New("mcpadapter: tools/call content block type is invalid")
		}
		switch kind {
		case "text":
			if _, ok := block["text"].(string); !ok {
				return errors.New("mcpadapter: tools/call text content is invalid")
			}
		case "image", "audio":
			data, ok := block["data"].(string)
			if !ok || !validBase64(data) {
				return errors.New("mcpadapter: tools/call media content is invalid")
			}
			if _, ok := block["mimeType"].(string); !ok {
				return errors.New("mcpadapter: tools/call media content is invalid")
			}
		case "resource_link":
			if _, ok := block["uri"].(string); !ok {
				return errors.New("mcpadapter: tools/call resource link is invalid")
			}
			if _, ok := block["name"].(string); !ok {
				return errors.New("mcpadapter: tools/call resource link is invalid")
			}
			if err := validateOptionalStrings(block, "title", "description", "mimeType"); err != nil {
				return err
			}
			if value, exists := block["size"]; exists {
				number, ok := value.(json.Number)
				if !ok {
					return errors.New("mcpadapter: tools/call resource link size is invalid")
				}
				if _, ok := parseJSONInt64(number); !ok {
					return errors.New("mcpadapter: tools/call resource link size is invalid")
				}
			}
			if value, exists := block["icons"]; exists {
				if err := validateContentIcons(value); err != nil {
					return err
				}
			}
		case "resource":
			resource, ok := block["resource"].(map[string]any)
			if !ok {
				return errors.New("mcpadapter: tools/call embedded resource is invalid")
			}
			if _, ok := resource["uri"].(string); !ok {
				return errors.New("mcpadapter: tools/call embedded resource is invalid")
			}
			_, hasText := resource["text"].(string)
			blob, hasBlob := resource["blob"].(string)
			if !hasText && !hasBlob {
				return errors.New("mcpadapter: tools/call embedded resource is invalid")
			}
			if hasBlob && !validBase64(blob) {
				return errors.New("mcpadapter: tools/call embedded resource is invalid")
			}
			if err := validateOptionalStrings(resource, "mimeType"); err != nil {
				return err
			}
			if metadata, exists := resource["_meta"]; exists {
				if _, ok := metadata.(map[string]any); !ok {
					return errors.New("mcpadapter: tools/call resource metadata is invalid")
				}
			}
		default:
			return errors.New("mcpadapter: tools/call content block type is unsupported")
		}
		if annotations, exists := block["annotations"]; exists {
			if err := validateContentAnnotations(annotations); err != nil {
				return err
			}
		}
		if metadata, exists := block["_meta"]; exists {
			if _, ok := metadata.(map[string]any); !ok {
				return errors.New("mcpadapter: tools/call content metadata is invalid")
			}
		}
	}
	return nil
}

func validBase64(value string) bool {
	_, err := base64.StdEncoding.DecodeString(value)
	return err == nil
}

func validateOptionalStrings(object map[string]any, keys ...string) error {
	for _, key := range keys {
		if value, exists := object[key]; exists {
			if _, ok := value.(string); !ok {
				return errors.New("mcpadapter: tools/call optional content field is invalid")
			}
		}
	}
	return nil
}

func validateContentAnnotations(value any) error {
	annotations, ok := value.(map[string]any)
	if !ok {
		return errors.New("mcpadapter: tools/call content annotations are invalid")
	}
	if audienceValue, exists := annotations["audience"]; exists {
		audience, ok := audienceValue.([]any)
		if !ok {
			return errors.New("mcpadapter: tools/call content audience is invalid")
		}
		for _, roleValue := range audience {
			role, ok := roleValue.(string)
			if !ok || (role != "user" && role != "assistant") {
				return errors.New("mcpadapter: tools/call content audience is invalid")
			}
		}
	}
	if priorityValue, exists := annotations["priority"]; exists {
		priority, ok := priorityValue.(json.Number)
		if !ok {
			return errors.New("mcpadapter: tools/call content priority is invalid")
		}
		if _, ok := parseBoundedJSONRational(priority, new(big.Rat), big.NewRat(1, 1)); !ok {
			return errors.New("mcpadapter: tools/call content priority is invalid")
		}
	}
	if modified, exists := annotations["lastModified"]; exists {
		if _, ok := modified.(string); !ok {
			return errors.New("mcpadapter: tools/call content lastModified is invalid")
		}
	}
	return nil
}

func validateContentIcons(value any) error {
	icons, ok := value.([]any)
	if !ok {
		return errors.New("mcpadapter: tools/call resource icons are invalid")
	}
	for _, iconValue := range icons {
		icon, ok := iconValue.(map[string]any)
		if !ok {
			return errors.New("mcpadapter: tools/call resource icon is invalid")
		}
		if _, ok := icon["src"].(string); !ok {
			return errors.New("mcpadapter: tools/call resource icon is invalid")
		}
		if err := validateOptionalStrings(icon, "mimeType"); err != nil {
			return err
		}
		if sizesValue, exists := icon["sizes"]; exists {
			sizes, ok := sizesValue.([]any)
			if !ok {
				return errors.New("mcpadapter: tools/call resource icon sizes are invalid")
			}
			for _, size := range sizes {
				if _, ok := size.(string); !ok {
					return errors.New("mcpadapter: tools/call resource icon sizes are invalid")
				}
			}
		}
		if themeValue, exists := icon["theme"]; exists {
			theme, ok := themeValue.(string)
			if !ok || (theme != "light" && theme != "dark") {
				return errors.New("mcpadapter: tools/call resource icon theme is invalid")
			}
		}
	}
	return nil
}
