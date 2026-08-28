package agentruntime

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/openai/openai-go/v3/responses"
)

// E. Event dispatcher
// ---------------------------------------------------------------------------

func validateOpenAIStreamEventFields(event responses.ResponseStreamEventUnion) error {
	label := "stream event " + event.Type
	raw := json.RawMessage(event.RawJSON())

	variant := event.AsAny()
	if variant == nil {
		return fmt.Errorf("%w: OpenAI stream event %s has no supported schema", ErrInvalidModelOutput, event.Type)
	}
	closedRaw := openAIStripResponseTools(raw)
	if err := validateOpenAIClosedSchema(closedRaw, label, reflect.TypeOf(variant)); err != nil {
		return err
	}
	if err := openAIStreamRequire(event.JSON.SequenceNumber.Valid(), event.Type, "sequence_number"); err != nil {
		return err
	}

	switch event.Type {
	case "response.created":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Response.Valid(), "response"},
			{event.Response.JSON.ID.Valid(), "response.id"},
			{event.Response.JSON.Object.Valid(), "response.object"},
			{event.Response.JSON.CreatedAt.Valid(), "response.created_at"},
			{event.Response.JSON.Model.Valid(), "response.model"},
			{event.Response.JSON.Output.Valid(), "response.output"},
			{event.Response.JSON.Status.Valid(), "response.status"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		if err := validateOpenAIResponseEventStatus(event.Type, event.Response.Status); err != nil {
			return err
		}
	case "response.in_progress", "response.completed", "response.failed", "response.incomplete":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Response.Valid(), "response"},
			{event.Response.JSON.ID.Valid(), "response.id"},
			{event.Response.JSON.Output.Valid(), "response.output"},
			{event.Response.JSON.Status.Valid(), "response.status"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		if err := validateOpenAIResponseEventStatus(event.Type, event.Response.Status); err != nil {
			return err
		}
	case "response.output_item.added", "response.output_item.done":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Item.Valid(), "item"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		var expected responses.ResponseStatus
		if event.Type == "response.output_item.done" {
			expected = responses.ResponseStatusCompleted
		} else {
			expected = responses.ResponseStatusInProgress
		}
		if err := validateOpenAIStreamItem(event.Item, expected, event.Type); err != nil {
			return err
		}
	case "response.function_call_arguments.done":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Arguments.Valid(), "arguments"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.Name.Valid(), "name"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
	case "response.function_call_arguments.delta":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Delta.Valid(), "delta"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
	case "response.output_text.delta":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Delta.Valid(), "delta"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
			{event.JSON.ContentIndex.Valid(), "content_index"},
			{event.JSON.Logprobs.Valid(), "logprobs"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		if err := validateOpenAIStreamEventLogprobs(event, label+".logprobs", false); err != nil {
			return err
		}
	case "response.output_text.done":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Text.Valid(), "text"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
			{event.JSON.ContentIndex.Valid(), "content_index"},
			{event.JSON.Logprobs.Valid(), "logprobs"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		if err := validateOpenAIStreamEventLogprobs(event, label+".logprobs", false); err != nil {
			return err
		}
	case "response.output_text.annotation.added":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Annotation.Valid(), "annotation"},
			{event.JSON.AnnotationIndex.Valid(), "annotation_index"},
			{event.JSON.ContentIndex.Valid(), "content_index"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		if raw, err := openAIStreamEventField(event, "annotation"); err == nil && len(raw) > 0 {
			var annotation responses.ResponseOutputTextAnnotationUnion
			if err := json.Unmarshal(raw, &annotation); err != nil {
				return fmt.Errorf("%w: OpenAI %s.annotation is invalid: %v", ErrInvalidModelOutput, label, err)
			}
			if err := validateOpenAIAnnotation(annotation, label+".annotation"); err != nil {
				return err
			}
		}
	case "response.refusal.delta":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Delta.Valid(), "delta"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
			{event.JSON.ContentIndex.Valid(), "content_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
	case "response.refusal.done":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Refusal.Valid(), "refusal"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
			{event.JSON.ContentIndex.Valid(), "content_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Delta.Valid(), "delta"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		if event.Type == "response.reasoning_summary_text.delta" {
			if err := openAIStreamRequire(event.JSON.SummaryIndex.Valid(), event.Type, "summary_index"); err != nil {
				return err
			}
		} else {
			if err := openAIStreamRequire(event.JSON.ContentIndex.Valid(), event.Type, "content_index"); err != nil {
				return err
			}
		}
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Text.Valid(), "text"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
		if event.Type == "response.reasoning_summary_text.done" {
			if err := openAIStreamRequire(event.JSON.SummaryIndex.Valid(), event.Type, "summary_index"); err != nil {
				return err
			}
		} else {
			if err := openAIStreamRequire(event.JSON.ContentIndex.Valid(), event.Type, "content_index"); err != nil {
				return err
			}
		}
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Part.Valid(), "part"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
			{event.JSON.SummaryIndex.Valid(), "summary_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
	case "response.content_part.added", "response.content_part.done":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Part.Valid(), "part"},
			{event.JSON.ItemID.Valid(), "item_id"},
			{event.JSON.OutputIndex.Valid(), "output_index"},
			{event.JSON.ContentIndex.Valid(), "content_index"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
	case "error":
		for _, check := range []struct {
			valid bool
			name  string
		}{
			{event.JSON.Code.Valid(), "code"},
			{event.JSON.Message.Valid(), "message"},
			{event.JSON.Param.Valid(), "param"},
		} {
			if err := openAIStreamRequire(check.valid, event.Type, check.name); err != nil {
				return err
			}
		}
	}

	// Response-level deep checks on every response-bearing event.
	if event.JSON.Response.Valid() {
		responseFields, err := decodeOpenAIRawObject(json.RawMessage(event.Response.RawJSON()), label+" response")
		if err != nil {
			return err
		}
		if err := validateOpenAIImmutableResponseFieldTypes(event.Type, event.Response, responseFields); err != nil {
			return err
		}
		if err := validateOpenAIResponseScalarValues(event.Type, responseFields, event.Response); err != nil {
			return err
		}
		if event.Type == "response.completed" {
			if err := validateOpenAIResponseTerminalAuthority(event.Type, event.Response, responseFields); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIResponseEventStatus(eventType string, status responses.ResponseStatus) error {
	var expected string
	switch eventType {
	case "response.created", "response.in_progress":
		expected = "in_progress"
	case "response.completed":
		expected = "completed"
	case "response.failed":
		expected = "failed"
	case "response.incomplete":
		expected = "incomplete"
	default:
		return nil
	}
	if string(status) != expected {
		return fmt.Errorf("%w: OpenAI stream event %s response.status contradicts its event type", ErrInvalidModelOutput, eventType)
	}
	return nil
}

func openAIStreamRequire(valid bool, eventType, name string) error {
	if !valid {
		return fmt.Errorf("%w: OpenAI stream event %s is missing %s", ErrInvalidModelOutput, eventType, name)
	}
	return nil
}

func validateOpenAIStreamEventLogprobs(event responses.ResponseStreamEventUnion, label string, withBytes bool) error {
	raw, err := openAIStreamEventField(event, "logprobs")
	if err != nil {
		return err
	}
	if !openAINonNullRaw(raw) {
		return nil
	}
	return validateOpenAIRawLogprobs(raw, label, withBytes)
}

// openAIStripResponseTools removes the response.tools array from a stream event
// payload before the structural closed-schema pass. The `tools` field is a flat
// union whose variants carry heterogeneous types for the same JSON name (for
// example `environment` is a string for computer_use_preview but an object for
// shell); validateOpenAIResponseTools owns that field's full validation, so the
// closed schema must not descend into it.
func openAIStripResponseTools(raw json.RawMessage) json.RawMessage {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return raw
	}
	object, ok := value.(map[string]any)
	if !ok {
		return raw
	}
	responseValue, ok := object["response"]
	if !ok {
		return raw
	}
	responseObject, ok := responseValue.(map[string]any)
	if !ok {
		return raw
	}
	if _, exists := responseObject["tools"]; !exists {
		return raw
	}
	stripped := make(map[string]any, len(responseObject))
	for name, item := range responseObject {
		if name == "tools" {
			continue
		}
		stripped[name] = item
	}
	object["response"] = stripped
	marshaled, err := json.Marshal(object)
	if err != nil {
		return raw
	}
	return json.RawMessage(marshaled)
}
