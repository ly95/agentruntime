package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"
)

// This file owns the OpenAI Responses-API stream state machine. It tracks the
// lifecycle, identity, and evidence of every streamed output item so that the
// completed response can be reconciled against what the stream actually
// proved. Per-payload closed-schema and deep per-type validation lives in
// openai_validate.go / openai_validate_deep.go; this file never re-decodes JSON
// for validation and instead delegates to those validators.

type openAIStreamState struct {
	parsed                *ModelResponse
	responseID            string
	responseCreated       bool
	terminal              bool
	sequenceNumber        int64
	hasSequence           bool
	sink                  ModelStreamSink
	messagePhases         map[string]responses.ResponseOutputMessagePhase
	callIDs               map[string]string
	functionCalls         map[string]openAIStreamFunctionCall
	functionArgumentText  map[string]string
	itemIndexes           map[string]int64
	indexItems            map[int64]string
	itemTypes             map[string]string
	addedItems            map[string]struct{}
	argumentsDone         map[string]struct{}
	doneItems             map[string]struct{}
	doneItemJSON          map[string]json.RawMessage
	responseFields        map[string]json.RawMessage
	responseEnvelopeBound bool
	initialText           map[openAIStreamTextKey]string
	partAddedText         map[openAIStreamTextKey]string
	textEvidence          map[openAIStreamTextKey]string
	finalText             map[openAIStreamTextKey]string
	textDone              map[openAIStreamTextKey]struct{}
	partsAdded            map[openAIStreamTextKey]struct{}
	partsDone             map[openAIStreamTextKey]struct{}
	partDoneJSON          map[openAIStreamTextKey]json.RawMessage
	partAnnotations       map[openAIStreamAnnotationKey]json.RawMessage
	partAnnotationCounts  map[openAIStreamTextKey]int64
	annotations           map[openAIStreamAnnotationKey]json.RawMessage
	terminalError         error
	lastRawJSON           json.RawMessage
}

type openAIStreamTextKey struct {
	ItemID string
	Kind   string
	Index  int64
}

type openAIStreamAnnotationKey struct {
	ItemID          string
	ContentIndex    int64
	AnnotationIndex int64
}

type openAIStreamFunctionCall struct {
	CallID               string
	Name                 string
	Arguments            string
	ObservedArguments    string
	HasObservedArguments bool
	Finalized            bool
}

func newOpenAIStreamState(sink ModelStreamSink) *openAIStreamState {
	return &openAIStreamState{
		sink:                 sink,
		messagePhases:        make(map[string]responses.ResponseOutputMessagePhase),
		callIDs:              make(map[string]string),
		functionCalls:        make(map[string]openAIStreamFunctionCall),
		functionArgumentText: make(map[string]string),
		itemIndexes:          make(map[string]int64),
		indexItems:           make(map[int64]string),
		itemTypes:            make(map[string]string),
		addedItems:           make(map[string]struct{}),
		argumentsDone:        make(map[string]struct{}),
		doneItems:            make(map[string]struct{}),
		doneItemJSON:         make(map[string]json.RawMessage),
		responseFields:       make(map[string]json.RawMessage),
		initialText:          make(map[openAIStreamTextKey]string),
		partAddedText:        make(map[openAIStreamTextKey]string),
		textEvidence:         make(map[openAIStreamTextKey]string),
		finalText:            make(map[openAIStreamTextKey]string),
		textDone:             make(map[openAIStreamTextKey]struct{}),
		partsAdded:           make(map[openAIStreamTextKey]struct{}),
		partsDone:            make(map[openAIStreamTextKey]struct{}),
		partDoneJSON:         make(map[openAIStreamTextKey]json.RawMessage),
		partAnnotations:      make(map[openAIStreamAnnotationKey]json.RawMessage),
		partAnnotationCounts: make(map[openAIStreamTextKey]int64),
		annotations:          make(map[openAIStreamAnnotationKey]json.RawMessage),
	}
}

func (state *openAIStreamState) emit(chunk ModelStreamEvent) {
	if state.sink == nil {
		return
	}
	if chunk.ResponseID == "" {
		chunk.ResponseID = state.responseID
	}
	if chunk.CallID == "" && chunk.ItemID != "" {
		chunk.CallID = state.callIDs[chunk.ItemID]
	}
	state.sink(chunk)
}

func (state *openAIStreamState) accept(event responses.ResponseStreamEventUnion) error {
	if !event.JSON.Type.Valid() || strings.TrimSpace(event.Type) == "" {
		return fmt.Errorf("%w: OpenAI stream event is missing type", ErrInvalidModelOutput)
	}
	if state.terminal {
		return fmt.Errorf("%w: OpenAI stream event %s followed a terminal response event", ErrInvalidModelOutput, event.Type)
	}
	if !event.JSON.SequenceNumber.Valid() {
		return fmt.Errorf("%w: OpenAI stream event %s is missing sequence_number", ErrInvalidModelOutput, event.Type)
	}
	if event.SequenceNumber < 0 {
		return fmt.Errorf("%w: OpenAI stream event %s has negative sequence_number", ErrInvalidModelOutput, event.Type)
	}
	if state.hasSequence && event.SequenceNumber <= state.sequenceNumber {
		return fmt.Errorf("%w: OpenAI stream event %s has nonincreasing sequence_number %d after %d", ErrInvalidModelOutput, event.Type, event.SequenceNumber, state.sequenceNumber)
	}
	if err := validateOpenAIStreamEventFields(event); err != nil {
		return err
	}
	state.sequenceNumber = event.SequenceNumber
	state.hasSequence = true
	state.lastRawJSON = json.RawMessage(event.RawJSON())
	switch event.Type {
	case "response.created":
		return state.acceptResponseCreated(event)
	case "response.in_progress":
		return state.acceptResponseInProgress(event)
	case "response.completed":
		return state.acceptResponseCompleted(event)
	case "response.failed", "response.incomplete":
		return state.acceptResponseFailure(event)
	case "response.output_item.added":
		return state.acceptOutputItemAdded(event)
	case "response.output_item.done":
		return state.acceptOutputItemDone(event)
	case "response.function_call_arguments.delta":
		return state.acceptFunctionCallArgumentsDelta(event)
	case "response.function_call_arguments.done":
		return state.acceptFunctionCallArgumentsDone(event)
	case "response.output_text.delta", "response.refusal.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return state.acceptDeltaEvent(event)
	case "response.output_text.done", "response.reasoning_summary_text.done",
		"response.reasoning_text.done", "response.refusal.done":
		return state.acceptTextDone(event)
	case "response.content_part.added", "response.reasoning_summary_part.added":
		return state.acceptContentPartAdded(event)
	case "response.content_part.done", "response.reasoning_summary_part.done":
		return state.acceptContentPartDone(event)
	case "response.output_text.annotation.added":
		return state.acceptAnnotationAdded(event)
	case "error":
		return state.acceptErrorEvent(event)
	default:
		return openAIUnsupportedStreamEventError(event.Type)
	}
}

// openAIUnofferedToolEventPrefixes lists provider tool-call lifecycles that can
// never be legal here: the transport only ever offers function tools derived
// from registered operations, so a lifecycle for any other tool class is
// provider drift and must fail explicitly instead of being silently ignored.
// The names mirror the SDK's documented stream event type families.
var openAIUnofferedToolEventPrefixes = []string{
	"response.audio.",
	"response.code_interpreter_call.",
	"response.code_interpreter_call_code.",
	"response.file_search_call.",
	"response.web_search_call.",
	"response.image_generation_call.",
	"response.mcp_call.",
	"response.mcp_call_arguments.",
	"response.mcp_list_tools.",
	"response.custom_tool_call_input.",
}

func openAIUnsupportedStreamEventError(eventType string) error {
	if eventType == "response.queued" {
		return fmt.Errorf("%w: OpenAI stream event %q is a queueing lifecycle this runtime never enables", ErrInvalidModelOutput, eventType)
	}
	for _, prefix := range openAIUnofferedToolEventPrefixes {
		if strings.HasPrefix(eventType, prefix) {
			return fmt.Errorf("%w: OpenAI stream event %q is a tool-call lifecycle for a tool class this runtime never offers", ErrInvalidModelOutput, eventType)
		}
	}
	return fmt.Errorf("%w: unsupported OpenAI stream event type %q", ErrInvalidModelOutput, eventType)
}

func (state *openAIStreamState) bindResponseID(eventType, responseID string, created bool) error {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return fmt.Errorf("%w: OpenAI %s is missing response id", ErrInvalidModelOutput, eventType)
	}
	if created {
		if state.responseCreated {
			return fmt.Errorf("%w: OpenAI %s repeated response creation for %q", ErrInvalidModelOutput, eventType, responseID)
		}
		state.responseCreated = true
		state.responseID = responseID
		return nil
	}
	if !state.responseCreated {
		return fmt.Errorf("%w: OpenAI %s arrived before response.created", ErrInvalidModelOutput, eventType)
	}
	if state.responseID != responseID {
		return fmt.Errorf("%w: OpenAI stream response id changed from %q to %q at %s", ErrInvalidModelOutput, state.responseID, responseID, eventType)
	}
	return nil
}

func openAIStreamResponseError(status string, response responses.Response) error {
	code := strings.TrimSpace(string(response.Error.Code))
	message := strings.TrimSpace(response.Error.Message)
	if status == "incomplete" && message == "" {
		message = strings.TrimSpace(response.IncompleteDetails.Reason)
	}
	if code == "" {
		code = "unknown_error"
	}
	if message == "" {
		message = "no error details"
	}
	return fmt.Errorf("agent: OpenAI response %s: %s: %s", status, code, message)
}

func supportedOpenAIStreamItemType(itemType string) bool {
	switch itemType {
	case string(ModelOutputMessage), string(ModelOutputReasoning), string(ModelOutputFunctionCall):
		return true
	default:
		return false
	}
}

func openAIStreamPartText(part responses.ResponseStreamEventUnionPart) string {
	if part.Type == "output_text" {
		return part.Text
	}
	if part.Type == "refusal" {
		return part.Refusal
	}
	return ""
}

func newOpenAIStreamChunk(event responses.ResponseStreamEventUnion, eventType ModelStreamEventType) ModelStreamEvent {
	chunk := ModelStreamEvent{
		Type:         eventType,
		ProviderType: event.Type,
		ItemID:       event.ItemID,
		Delta:        event.Delta,
		RawJSON:      event.RawJSON(),
	}
	if event.JSON.SequenceNumber.Valid() {
		sequenceNumber := event.SequenceNumber
		chunk.SequenceNumber = &sequenceNumber
	}
	if event.JSON.OutputIndex.Valid() {
		outputIndex := event.OutputIndex
		chunk.OutputIndex = &outputIndex
	}
	return chunk
}

func (state *openAIStreamState) finish(streamErr error) (*ModelResponse, error) {
	if streamErr != nil {
		state.emit(ModelStreamEvent{
			Type:         ModelStreamError,
			ProviderType: "transport.error",
			ErrorCode:    "transport_error",
			ErrorMessage: streamErr.Error(),
		})
		transportErr := fmt.Errorf("agent: OpenAI stream failed: %w", streamErr)
		if state.terminalError != nil {
			return nil, errors.Join(state.terminalError, transportErr)
		}
		return nil, transportErr
	}
	if state.parsed != nil {
		return state.parsed, nil
	}
	if state.terminalError != nil {
		return nil, state.terminalError
	}
	state.emit(ModelStreamEvent{
		Type:         ModelStreamError,
		ProviderType: "stream.ended",
		ErrorCode:    "incomplete_stream",
		ErrorMessage: "stream ended without response.completed",
		RawJSON:      string(state.lastRawJSON),
	})
	return nil, errors.New("agent: OpenAI stream ended without response.completed")
}

func (state *openAIStreamState) requireActiveResponse(eventType string) error {
	if !state.responseCreated {
		return fmt.Errorf("%w: OpenAI %s arrived before response.created", ErrInvalidModelOutput, eventType)
	}
	return nil
}

func (state *openAIStreamState) requireActiveItem(eventType, itemID string, outputIndex int64) error {
	expectedIndex, exists := state.itemIndexes[itemID]
	if !exists {
		return fmt.Errorf("%w: OpenAI %s targets output item %q before it was added", ErrInvalidModelOutput, eventType, itemID)
	}
	if expectedIndex != outputIndex {
		return fmt.Errorf(
			"%w: OpenAI %s output index %d does not match item %q index %d",
			ErrInvalidModelOutput, eventType, outputIndex, itemID, expectedIndex,
		)
	}
	if _, done := state.doneItems[itemID]; done {
		return fmt.Errorf("%w: OpenAI %s arrived after output item %q was done", ErrInvalidModelOutput, eventType, itemID)
	}
	return nil
}

func openAIImmutableResponseField(name string) bool {
	for _, field := range openAIImmutableResponseFields {
		if field == name {
			return true
		}
	}
	return false
}

// bindResponseEnvelope captures the response object's immutable fields and
// rejects drift. Immutable fields that were absent at response.created cannot
// appear later, and present immutable fields cannot change value.
func (state *openAIStreamState) bindResponseEnvelope(eventType string, response responses.Response) error {
	raw := json.RawMessage(response.RawJSON())
	fields, err := decodeOpenAIRawObject(raw, eventType+" response")
	if err != nil {
		return err
	}
	if err := validateOpenAIImmutableResponseFieldTypes(eventType, response, fields); err != nil {
		return err
	}
	for name, fieldRaw := range fields {
		if !openAIImmutableResponseField(name) {
			continue
		}
		if !state.responseEnvelopeBound {
			state.responseFields[name] = fieldRaw
			continue
		}
		if existing, exists := state.responseFields[name]; exists {
			if !jsonSemanticallyEqual(existing, fieldRaw) {
				return fmt.Errorf("%w: OpenAI %s changed immutable response field %q", ErrInvalidModelOutput, eventType, name)
			}
		} else {
			return fmt.Errorf("%w: OpenAI %s introduced immutable response field %q after response.created", ErrInvalidModelOutput, eventType, name)
		}
	}
	state.responseEnvelopeBound = true
	return nil
}

func (state *openAIStreamState) observeItem(itemID string, outputIndex int64, itemType string, added, done bool) error {
	if added {
		if err := requireCanonicalIdentity(itemID, "output item id"); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidModelOutput, err)
		}
		if !supportedOpenAIStreamItemType(itemType) {
			return fmt.Errorf("%w: unsupported OpenAI output item type %q", ErrInvalidModelOutput, itemType)
		}
		if outputIndex < 0 {
			return fmt.Errorf("%w: OpenAI output item %q has negative output index", ErrInvalidModelOutput, itemID)
		}
		if _, exists := state.addedItems[itemID]; exists {
			return fmt.Errorf("%w: OpenAI stream repeats output item id %q", ErrInvalidModelOutput, itemID)
		}
		if _, exists := state.indexItems[outputIndex]; exists {
			return fmt.Errorf("%w: OpenAI stream repeats output index %d", ErrInvalidModelOutput, outputIndex)
		}
		state.itemIndexes[itemID] = outputIndex
		state.indexItems[outputIndex] = itemID
		state.itemTypes[itemID] = itemType
		state.addedItems[itemID] = struct{}{}
		return nil
	}
	if !done {
		return fmt.Errorf("%w: OpenAI stream item observation requires added or done", ErrInvalidModelOutput)
	}
	existingType, exists := state.itemTypes[itemID]
	if !exists {
		return fmt.Errorf("%w: OpenAI stream ends output item %q before it was added", ErrInvalidModelOutput, itemID)
	}
	if existingType != itemType {
		return fmt.Errorf("%w: OpenAI stream output item %q changed type from %q to %q", ErrInvalidModelOutput, itemID, existingType, itemType)
	}
	if index, ok := state.itemIndexes[itemID]; !ok || index != outputIndex {
		return fmt.Errorf("%w: OpenAI stream output item %q changed output index", ErrInvalidModelOutput, itemID)
	}
	if _, exists := state.doneItems[itemID]; exists {
		return fmt.Errorf("%w: OpenAI stream repeats output item id %q", ErrInvalidModelOutput, itemID)
	}
	state.doneItems[itemID] = struct{}{}
	return nil
}

// acceptItemEvent records type-specific item metadata after observeItem. For
// added messages it validates and records the phase plus initial content and
// annotation evidence; for function calls it records identity and observed
// arguments. For done items it enforces phase drift and reconciles evidence.
func (state *openAIStreamState) acceptItemEvent(event responses.ResponseStreamEventUnion) error {
	item := event.Item
	itemID := item.ID
	_, isDone := state.doneItems[itemID]
	if isDone {
		if err := state.validateItemTextEvidenceDomain(item); err != nil {
			return err
		}
	}
	switch item.Type {
	case string(ModelOutputMessage):
		if !validOpenAIStreamMessagePhase(item.Phase) {
			return fmt.Errorf("%w: OpenAI output message %q has unsupported phase %q", ErrInvalidModelOutput, itemID, item.Phase)
		}
		if !isDone {
			state.messagePhases[itemID] = item.Phase
			if err := state.recordInitialTextEvidence(item); err != nil {
				return err
			}
		} else {
			if addedPhase, ok := state.messagePhases[itemID]; ok {
				if openAIStreamMessagePhase(addedPhase) != openAIStreamMessagePhase(item.Phase) {
					return fmt.Errorf("%w: OpenAI output message %q changed phase", ErrInvalidModelOutput, itemID)
				}
			}
			if err := state.validateTextEvidenceAgainstItem(item); err != nil {
				return err
			}
			if err := state.validateAnnotationsAgainstItem(item); err != nil {
				return err
			}
		}
	case string(ModelOutputReasoning):
		if !isDone {
			if err := state.recordInitialTextEvidence(item); err != nil {
				return err
			}
		} else {
			if err := state.validateTextEvidenceAgainstItem(item); err != nil {
				return err
			}
		}
	case string(ModelOutputFunctionCall):
		if err := state.recordFunctionCall(itemID, item); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unsupported OpenAI output item type %q", ErrInvalidModelOutput, item.Type)
	}
	return nil
}

func validOpenAIStreamMessagePhase(phase responses.ResponseOutputMessagePhase) bool {
	switch phase {
	case "", responses.ResponseOutputMessagePhaseCommentary, responses.ResponseOutputMessagePhaseFinalAnswer:
		return true
	default:
		return false
	}
}

func openAIStreamMessagePhase(phase responses.ResponseOutputMessagePhase) responses.ResponseOutputMessagePhase {
	switch phase {
	case "", responses.ResponseOutputMessagePhaseFinalAnswer:
		return responses.ResponseOutputMessagePhaseFinalAnswer
	default:
		return phase
	}
}

func openAIStreamContentText(content responses.ResponseOutputMessageContentUnion) string {
	switch content.Type {
	case "output_text":
		return content.Text
	case "refusal":
		return content.Refusal
	default:
		return ""
	}
}

// recordInitialTextEvidence seeds initial text and annotation evidence from an
// added item's content so that later content_part events and deltas can be
// reconciled against what the item already declared.
func (state *openAIStreamState) recordInitialTextEvidence(item responses.ResponseOutputItemUnion) error {
	itemID := item.ID
	switch item.Type {
	case string(ModelOutputMessage):
		for index, content := range item.Content {
			kind := content.Type
			if kind != "output_text" && kind != "refusal" {
				continue
			}
			key := openAIStreamTextKey{ItemID: itemID, Kind: kind, Index: int64(index)}
			text := openAIStreamContentText(content)
			if _, exists := state.initialText[key]; !exists {
				state.initialText[key] = text
			}
			if _, exists := state.partAddedText[key]; !exists {
				state.partAddedText[key] = text
			}
			if err := state.recordAnnotationBase(key, content.Annotations); err != nil {
				return err
			}
		}
	case string(ModelOutputReasoning):
		for index, summary := range item.Summary {
			key := openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_summary", Index: int64(index)}
			if _, exists := state.initialText[key]; !exists {
				state.initialText[key] = summary.Text
			}
			if _, exists := state.partAddedText[key]; !exists {
				state.partAddedText[key] = summary.Text
			}
		}
		for index, content := range item.Content {
			key := openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_text", Index: int64(index)}
			if _, exists := state.initialText[key]; !exists {
				state.initialText[key] = content.Text
			}
			if _, exists := state.partAddedText[key]; !exists {
				state.partAddedText[key] = content.Text
			}
		}
	}
	return nil
}

func (state *openAIStreamState) recordAnnotationBase(key openAIStreamTextKey, annotations []responses.ResponseOutputTextAnnotationUnion) error {
	for index, annotation := range annotations {
		annotationKey := openAIStreamAnnotationKey{
			ItemID:          key.ItemID,
			ContentIndex:    key.Index,
			AnnotationIndex: int64(index),
		}
		raw := json.RawMessage(annotation.RawJSON())
		if existing, exists := state.partAnnotations[annotationKey]; exists {
			if !jsonSemanticallyEqual(existing, raw) {
				return fmt.Errorf("%w: OpenAI output item annotation is replaced by part added", ErrInvalidModelOutput)
			}
			continue
		}
		state.partAnnotations[annotationKey] = raw
		state.annotations[annotationKey] = raw
	}
	return nil
}

func (state *openAIStreamState) appendFunctionArgumentEvidence(itemID, fragment string) {
	state.functionArgumentText[itemID] += fragment
}

func (state *openAIStreamState) validateFunctionArgumentEvidence(itemID, finalized string) error {
	if accumulated, saw := state.functionArgumentText[itemID]; saw && accumulated != "" {
		if !jsonSemanticallyEqual(json.RawMessage(accumulated), json.RawMessage(finalized)) {
			return fmt.Errorf("%w: OpenAI function call %q argument deltas contradict finalization", ErrInvalidModelOutput, itemID)
		}
	}
	if fc, ok := state.functionCalls[itemID]; ok && fc.HasObservedArguments && fc.ObservedArguments != "" {
		if !jsonSemanticallyEqual(json.RawMessage(fc.ObservedArguments), json.RawMessage(finalized)) {
			return fmt.Errorf("%w: OpenAI function call %q added arguments contradict finalization", ErrInvalidModelOutput, itemID)
		}
	}
	return nil
}

func (state *openAIStreamState) recordFunctionCall(itemID string, item responses.ResponseOutputItemUnion) error {
	call := item.AsFunctionCall()
	if call.CallID != "" {
		state.callIDs[itemID] = call.CallID
	}
	existing, ok := state.functionCalls[itemID]
	if !ok {
		existing = openAIStreamFunctionCall{}
	}
	if existing.CallID == "" {
		existing.CallID = call.CallID
	} else if call.CallID != "" && existing.CallID != call.CallID {
		return fmt.Errorf("%w: OpenAI function call %q call id conflicts with its stream item", ErrInvalidModelOutput, itemID)
	}
	if existing.Name == "" {
		existing.Name = call.Name
	} else if call.Name != "" && existing.Name != call.Name {
		return fmt.Errorf("%w: OpenAI function call %q name conflicts with its stream item", ErrInvalidModelOutput, itemID)
	}
	_, isDone := state.doneItems[itemID]
	if !isDone && !existing.HasObservedArguments {
		existing.ObservedArguments = call.Arguments
		existing.HasObservedArguments = call.Arguments != ""
	}
	if isDone {
		if existing.Finalized {
			if call.Arguments != "" && !jsonSemanticallyEqual(json.RawMessage(existing.Arguments), json.RawMessage(call.Arguments)) {
				return fmt.Errorf("%w: OpenAI function call %q item done arguments contradict finalization", ErrInvalidModelOutput, itemID)
			}
		} else {
			if err := state.validateFunctionArgumentEvidence(itemID, call.Arguments); err != nil {
				return err
			}
			existing.Arguments = call.Arguments
			existing.Finalized = true
		}
	}
	state.functionCalls[itemID] = existing
	return nil
}

func openAIStreamFinalTextKeys(item responses.ResponseOutputItemUnion) map[openAIStreamTextKey]struct{} {
	keys := make(map[openAIStreamTextKey]struct{})
	switch item.Type {
	case string(ModelOutputMessage):
		for index, content := range item.Content {
			if content.Type != "output_text" && content.Type != "refusal" {
				continue
			}
			keys[openAIStreamTextKey{ItemID: item.ID, Kind: content.Type, Index: int64(index)}] = struct{}{}
		}
	case string(ModelOutputReasoning):
		for index := range item.Summary {
			keys[openAIStreamTextKey{ItemID: item.ID, Kind: "reasoning_summary", Index: int64(index)}] = struct{}{}
		}
		for index := range item.Content {
			keys[openAIStreamTextKey{ItemID: item.ID, Kind: "reasoning_text", Index: int64(index)}] = struct{}{}
		}
	}
	return keys
}

func (state *openAIStreamState) validateItemTextEvidenceDomain(item responses.ResponseOutputItemUnion) error {
	if item.Type != string(ModelOutputMessage) && item.Type != string(ModelOutputReasoning) {
		return nil
	}
	allowed := openAIStreamFinalTextKeys(item)
	check := func(key openAIStreamTextKey, evidence string) error {
		if key.ItemID != item.ID {
			return nil
		}
		if _, exists := allowed[key]; !exists {
			return fmt.Errorf(
				"%w: OpenAI output item %q has %s for unbound %s index %d",
				ErrInvalidModelOutput, item.ID, evidence, key.Kind, key.Index,
			)
		}
		return nil
	}
	for key := range state.initialText {
		if err := check(key, "initial text evidence"); err != nil {
			return err
		}
	}
	for key := range state.partAddedText {
		if err := check(key, "content-part evidence"); err != nil {
			return err
		}
	}
	for key := range state.textEvidence {
		if err := check(key, "text-delta evidence"); err != nil {
			return err
		}
	}
	for key := range state.finalText {
		if err := check(key, "text-finalization evidence"); err != nil {
			return err
		}
	}
	for key := range state.textDone {
		if err := check(key, "text-done evidence"); err != nil {
			return err
		}
	}
	for key := range state.partsAdded {
		if err := check(key, "part-added evidence"); err != nil {
			return err
		}
	}
	for key := range state.partsDone {
		if err := check(key, "part-done evidence"); err != nil {
			return err
		}
	}
	for key := range state.partDoneJSON {
		if err := check(key, "part-done payload"); err != nil {
			return err
		}
	}
	for key := range state.partAnnotationCounts {
		if err := check(key, "annotation evidence"); err != nil {
			return err
		}
	}
	for key := range state.partAnnotations {
		if key.ItemID != item.ID {
			continue
		}
		textKey := openAIStreamTextKey{ItemID: key.ItemID, Kind: "output_text", Index: key.ContentIndex}
		if err := check(textKey, "annotation payload"); err != nil {
			return err
		}
	}
	return nil
}

func (state *openAIStreamState) reconcileTextEvidence(key openAIStreamTextKey, finalized string) error {
	if accumulated, saw := state.accumulatedTextEvidence(key); saw && accumulated != finalized {
		return fmt.Errorf("%w: OpenAI stream text evidence contradicts final text", ErrInvalidModelOutput)
	}
	return state.recordFinalText(key, finalized)
}

func (state *openAIStreamState) accumulatedTextEvidence(key openAIStreamTextKey) (string, bool) {
	text, exists := state.textEvidence[key]
	if !exists {
		return "", false
	}
	return text, true
}

func (state *openAIStreamState) recordFinalText(key openAIStreamTextKey, finalized string) error {
	if existing, ok := state.finalText[key]; ok && existing != finalized {
		return fmt.Errorf("%w: OpenAI stream repeats final text for item %q with different values", ErrInvalidModelOutput, key.ItemID)
	}
	if added, ok := state.partAddedText[key]; ok && added != "" && added != finalized {
		return fmt.Errorf("%w: OpenAI content part added payload contradicts finalized text", ErrInvalidModelOutput)
	}
	state.finalText[key] = finalized
	return nil
}

func (state *openAIStreamState) validateTextEvidenceAgainstItem(item responses.ResponseOutputItemUnion) error {
	itemID := item.ID
	switch item.Type {
	case string(ModelOutputMessage):
		for index, content := range item.Content {
			kind := content.Type
			if kind != "output_text" && kind != "refusal" {
				continue
			}
			key := openAIStreamTextKey{ItemID: itemID, Kind: kind, Index: int64(index)}
			text := openAIStreamContentText(content)
			if accumulated, saw := state.accumulatedTextEvidence(key); saw && accumulated != text {
				return fmt.Errorf("%w: OpenAI stream text evidence contradicts item text", ErrInvalidModelOutput)
			}
			if final, ok := state.finalText[key]; ok && final != text {
				return fmt.Errorf("%w: OpenAI stream final text contradicts item text", ErrInvalidModelOutput)
			}
			if added, ok := state.partAddedText[key]; ok && added != "" && added != text {
				return fmt.Errorf("%w: OpenAI content part payload contradicts item text", ErrInvalidModelOutput)
			}
		}
	case string(ModelOutputReasoning):
		for index, summary := range item.Summary {
			key := openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_summary", Index: int64(index)}
			if accumulated, saw := state.accumulatedTextEvidence(key); saw && accumulated != summary.Text {
				return fmt.Errorf("%w: OpenAI reasoning summary evidence contradicts item text", ErrInvalidModelOutput)
			}
			if final, ok := state.finalText[key]; ok && final != summary.Text {
				return fmt.Errorf("%w: OpenAI reasoning summary final text contradicts item text", ErrInvalidModelOutput)
			}
			if added, ok := state.partAddedText[key]; ok && added != "" && added != summary.Text {
				return fmt.Errorf("%w: OpenAI reasoning summary added text contradicts item text", ErrInvalidModelOutput)
			}
		}
		for index, content := range item.Content {
			key := openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_text", Index: int64(index)}
			if accumulated, saw := state.accumulatedTextEvidence(key); saw && accumulated != content.Text {
				return fmt.Errorf("%w: OpenAI reasoning evidence contradicts item text", ErrInvalidModelOutput)
			}
			if final, ok := state.finalText[key]; ok && final != content.Text {
				return fmt.Errorf("%w: OpenAI reasoning final text contradicts item text", ErrInvalidModelOutput)
			}
			if added, ok := state.partAddedText[key]; ok && added != "" && added != content.Text {
				return fmt.Errorf("%w: OpenAI reasoning added text contradicts item text", ErrInvalidModelOutput)
			}
		}
	}
	return nil
}

func (state *openAIStreamState) validateAnnotationsAgainstItem(item responses.ResponseOutputItemUnion) error {
	if item.Type != string(ModelOutputMessage) {
		return nil
	}
	for index, content := range item.Content {
		if content.Type != "output_text" {
			continue
		}
		expected := state.collectedPartAnnotations(item.ID, int64(index))
		if len(expected) == 0 && len(content.Annotations) == 0 {
			continue
		}
		if len(expected) != len(content.Annotations) {
			return fmt.Errorf("%w: OpenAI output item annotation count changed", ErrInvalidModelOutput)
		}
		for i, annotation := range content.Annotations {
			if !jsonSemanticallyEqual(expected[i], json.RawMessage(annotation.RawJSON())) {
				return fmt.Errorf("%w: OpenAI output item annotation changed", ErrInvalidModelOutput)
			}
		}
	}
	return nil
}

func (state *openAIStreamState) collectedPartAnnotations(itemID string, contentIndex int64) []json.RawMessage {
	out := make([]json.RawMessage, 0)
	for i := int64(0); ; i++ {
		annotationKey := openAIStreamAnnotationKey{ItemID: itemID, ContentIndex: contentIndex, AnnotationIndex: i}
		raw, exists := state.partAnnotations[annotationKey]
		if !exists {
			break
		}
		out = append(out, raw)
	}
	return out
}

func (state *openAIStreamState) validatePartAnnotations(key openAIStreamTextKey, annotations []responses.ResponseOutputTextAnnotationUnion) error {
	expected := state.collectedPartAnnotations(key.ItemID, key.Index)
	if len(expected) != len(annotations) {
		return fmt.Errorf("%w: OpenAI content part annotation disappears", ErrInvalidModelOutput)
	}
	for i, annotation := range annotations {
		if !jsonSemanticallyEqual(expected[i], json.RawMessage(annotation.RawJSON())) {
			return fmt.Errorf("%w: OpenAI content part annotation changed", ErrInvalidModelOutput)
		}
	}
	return nil
}

func (state *openAIStreamState) reconcileOpenAINonFunctionItem(doneRaw, completedRaw json.RawMessage, itemType, itemID string) error {
	var fields []string
	switch itemType {
	case string(ModelOutputMessage):
		fields = []string{"phase", "role", "content"}
	case string(ModelOutputReasoning):
		fields = []string{"summary", "content", "encrypted_content"}
	default:
		return nil
	}
	doneValue, err := decodeExactJSON(doneRaw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s item %q done JSON is ambiguous: %v", ErrInvalidModelOutput, itemType, itemID, err)
	}
	completedValue, err := decodeExactJSON(completedRaw)
	if err != nil {
		return fmt.Errorf("%w: OpenAI %s item %q completed JSON is ambiguous: %v", ErrInvalidModelOutput, itemType, itemID, err)
	}
	doneObject, doneOK := doneValue.(map[string]any)
	completedObject, completedOK := completedValue.(map[string]any)
	if !doneOK || !completedOK {
		return fmt.Errorf("%w: OpenAI %s item %q must be a JSON object", ErrInvalidModelOutput, itemType, itemID)
	}
	for _, field := range fields {
		doneField, doneHas := doneObject[field]
		completedField, completedHas := completedObject[field]
		if doneHas != completedHas {
			return fmt.Errorf("%w: OpenAI %s item %q %s changed after item done", ErrInvalidModelOutput, itemType, itemID, field)
		}
		if doneHas && !exactJSONValuesEqual(doneField, completedField) {
			return fmt.Errorf("%w: OpenAI %s item %q %s changed after item done", ErrInvalidModelOutput, itemType, itemID, field)
		}
	}
	return nil
}

func (state *openAIStreamState) reconcileCompletedResponse(response *responses.Response) error {
	completedIDs := make(map[string]struct{}, len(response.Output))
	for index, item := range response.Output {
		itemID := item.ID
		if _, exists := state.doneItems[itemID]; !exists {
			return fmt.Errorf("%w: OpenAI completed response contains unstreamed output item %q", ErrInvalidModelOutput, itemID)
		}
		completedIDs[itemID] = struct{}{}
		if expectedType, ok := state.itemTypes[itemID]; !ok || expectedType != item.Type {
			return fmt.Errorf("%w: OpenAI completed output item %q identity contradicts stream", ErrInvalidModelOutput, itemID)
		}
		if expectedIndex, ok := state.itemIndexes[itemID]; !ok || expectedIndex != int64(index) {
			return fmt.Errorf("%w: OpenAI completed output item %q identity contradicts stream", ErrInvalidModelOutput, itemID)
		}
		if item.Type == string(ModelOutputFunctionCall) {
			call := item.AsFunctionCall()
			if finalized, ok := state.functionCalls[itemID]; ok && finalized.Finalized {
				if finalized.CallID != "" && finalized.CallID != call.CallID {
					return fmt.Errorf("%w: OpenAI completed function call %q call id contradicts stream", ErrInvalidModelOutput, itemID)
				}
				if finalized.Name != "" && finalized.Name != call.Name {
					return fmt.Errorf("%w: OpenAI completed function call %q name contradicts stream", ErrInvalidModelOutput, itemID)
				}
				if call.Arguments != "" && finalized.Arguments != "" &&
					!jsonSemanticallyEqual(json.RawMessage(call.Arguments), json.RawMessage(finalized.Arguments)) {
					return fmt.Errorf("%w: OpenAI completed function call %q arguments contradict finalized stream", ErrInvalidModelOutput, itemID)
				}
			}
			continue
		}
		doneRaw, ok := state.doneItemJSON[itemID]
		if !ok {
			return fmt.Errorf("%w: OpenAI completed output item %q is missing its done payload", ErrInvalidModelOutput, itemID)
		}
		if err := state.reconcileOpenAINonFunctionItem(doneRaw, json.RawMessage(item.RawJSON()), item.Type, itemID); err != nil {
			return err
		}
	}
	for itemID := range state.doneItems {
		if _, exists := completedIDs[itemID]; !exists {
			return fmt.Errorf("%w: OpenAI streamed output item %q is missing from completed response", ErrInvalidModelOutput, itemID)
		}
	}
	parsed, err := parseOpenAIResponseWithFinalizedCalls(response, state.functionCalls)
	if err != nil {
		return err
	}
	state.parsed = parsed
	return nil
}

func (state *openAIStreamState) acceptResponseCreated(event responses.ResponseStreamEventUnion) error {
	created := event.Response
	if err := state.bindResponseID(event.Type, created.ID, true); err != nil {
		return err
	}
	if created.Status != responses.ResponseStatusInProgress || len(created.Output) != 0 {
		return fmt.Errorf("%w: OpenAI response.created must contain an in-progress response with no output", ErrInvalidModelOutput)
	}
	if strings.TrimSpace(string(created.Model)) == "" || created.CreatedAt < 0 || !created.JSON.CreatedAt.Valid() {
		return fmt.Errorf("%w: OpenAI response.created has invalid model or created_at", ErrInvalidModelOutput)
	}
	if err := state.bindResponseEnvelope(event.Type, created); err != nil {
		return err
	}
	state.emit(newOpenAIStreamChunk(event, ModelStreamResponseStarted))
	return nil
}

func (state *openAIStreamState) acceptResponseInProgress(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	response := event.Response
	if err := state.bindResponseID(event.Type, response.ID, false); err != nil {
		return err
	}
	return state.bindResponseEnvelope(event.Type, response)
}

func (state *openAIStreamState) acceptResponseCompleted(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	response := event.Response
	if err := state.bindResponseID(event.Type, response.ID, false); err != nil {
		return err
	}
	if err := state.bindResponseEnvelope(event.Type, response); err != nil {
		return err
	}
	if err := state.reconcileCompletedResponse(&response); err != nil {
		return err
	}
	state.emit(newOpenAIStreamChunk(event, ModelStreamResponseDone))
	state.terminal = true
	return nil
}

func (state *openAIStreamState) acceptResponseFailure(event responses.ResponseStreamEventUnion) error {
	status := "failed"
	if event.Type == "response.incomplete" {
		status = "incomplete"
	}
	response := event.Response
	message := openAIStreamResponseError(status, response).Error()
	code := strings.TrimSpace(string(response.Error.Code))
	if status == "incomplete" {
		code = "incomplete"
	}
	if code == "" {
		code = "unknown_error"
	}
	chunk := newOpenAIStreamChunk(event, ModelStreamError)
	chunk.ErrorCode = code
	chunk.ErrorMessage = message
	state.emit(chunk)
	state.terminal = true
	state.terminalError = errors.New(message)
	return nil
}

func (state *openAIStreamState) acceptOutputItemAdded(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	item := event.Item
	if err := state.observeItem(item.ID, event.OutputIndex, item.Type, true, false); err != nil {
		return err
	}
	if err := state.acceptItemEvent(event); err != nil {
		return err
	}
	state.emit(newOpenAIStreamChunk(event, ModelStreamItemAdded))
	return nil
}

func (state *openAIStreamState) acceptOutputItemDone(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	item := event.Item
	if err := state.observeItem(item.ID, event.OutputIndex, item.Type, false, true); err != nil {
		return err
	}
	if err := validateOpenAIStreamItem(item, responses.ResponseStatusCompleted, event.Type); err != nil {
		return err
	}
	if err := state.acceptItemEvent(event); err != nil {
		return err
	}
	state.doneItemJSON[item.ID] = json.RawMessage(item.RawJSON())
	chunk := newOpenAIStreamChunk(event, ModelStreamItemDone)
	if chunk.ItemID == "" {
		// output_item.done carries the item identity inside the item envelope,
		// not in a top-level item_id field.
		chunk.ItemID = item.ID
	}
	state.emit(chunk)
	return nil
}

func (state *openAIStreamState) acceptFunctionCallArgumentsDelta(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	itemID := event.ItemID
	if err := state.requireActiveItem(event.Type, itemID, event.OutputIndex); err != nil {
		return err
	}
	if state.itemTypes[itemID] != string(ModelOutputFunctionCall) {
		return fmt.Errorf("%w: OpenAI %s targets non-function-call item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	if _, exists := state.argumentsDone[itemID]; exists {
		return fmt.Errorf("%w: OpenAI %s arrived after arguments done for item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	state.appendFunctionArgumentEvidence(itemID, event.Delta)
	chunk := newOpenAIStreamChunk(event, ModelStreamToolArgumentsDelta)
	chunk.CallID = state.callIDs[itemID]
	state.emit(chunk)
	return nil
}

func (state *openAIStreamState) acceptFunctionCallArgumentsDone(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	itemID := event.ItemID
	if err := state.requireActiveItem(event.Type, itemID, event.OutputIndex); err != nil {
		return err
	}
	if state.itemTypes[itemID] != string(ModelOutputFunctionCall) {
		return fmt.Errorf("%w: OpenAI %s targets non-function-call item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	if _, exists := state.argumentsDone[itemID]; exists {
		return fmt.Errorf("%w: OpenAI %s repeats for item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	if err := state.validateFunctionArgumentEvidence(itemID, event.Arguments); err != nil {
		return err
	}
	state.argumentsDone[itemID] = struct{}{}
	fc := state.functionCalls[itemID]
	if fc.Name != "" && event.Name != "" && fc.Name != event.Name {
		return fmt.Errorf("%w: OpenAI function call %q name conflicts with its finalized arguments", ErrInvalidModelOutput, itemID)
	}
	if fc.Name == "" {
		fc.Name = event.Name
	}
	fc.Arguments = event.Arguments
	fc.Finalized = true
	state.functionCalls[itemID] = fc
	chunk := newOpenAIStreamChunk(event, ModelStreamToolArgumentsDone)
	chunk.Name = event.Name
	chunk.Arguments = event.Arguments
	chunk.CallID = state.callIDs[itemID]
	state.emit(chunk)
	return nil
}

func (state *openAIStreamState) acceptDeltaEvent(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	itemID := event.ItemID
	if err := state.requireActiveItem(event.Type, itemID, event.OutputIndex); err != nil {
		return err
	}
	var key openAIStreamTextKey
	var chunkType ModelStreamEventType
	switch event.Type {
	case "response.output_text.delta":
		if state.itemTypes[itemID] != string(ModelOutputMessage) {
			return fmt.Errorf("%w: OpenAI %s targets non-message item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "output_text", Index: event.ContentIndex}
		if openAIStreamMessagePhase(state.messagePhases[itemID]) == responses.ResponseOutputMessagePhaseCommentary {
			chunkType = ModelStreamCommentaryDelta
		} else {
			chunkType = ModelStreamTextDelta
		}
	case "response.refusal.delta":
		if state.itemTypes[itemID] != string(ModelOutputMessage) {
			return fmt.Errorf("%w: OpenAI %s targets non-message item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "refusal", Index: event.ContentIndex}
		chunkType = ModelStreamRefusalDelta
	case "response.reasoning_summary_text.delta":
		if state.itemTypes[itemID] != string(ModelOutputReasoning) {
			return fmt.Errorf("%w: OpenAI %s targets non-reasoning item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_summary", Index: event.SummaryIndex}
		chunkType = ModelStreamReasoningSummaryDelta
	case "response.reasoning_text.delta":
		if state.itemTypes[itemID] != string(ModelOutputReasoning) {
			return fmt.Errorf("%w: OpenAI %s targets non-reasoning item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_text", Index: event.ContentIndex}
	default:
		return openAIUnsupportedStreamEventError(event.Type)
	}
	state.textEvidence[key] += event.Delta
	if chunkType != "" {
		state.emit(newOpenAIStreamChunk(event, chunkType))
	}
	return nil
}

func (state *openAIStreamState) acceptTextDone(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	itemID := event.ItemID
	if err := state.requireActiveItem(event.Type, itemID, event.OutputIndex); err != nil {
		return err
	}
	var key openAIStreamTextKey
	switch event.Type {
	case "response.output_text.done":
		if state.itemTypes[itemID] != string(ModelOutputMessage) {
			return fmt.Errorf("%w: OpenAI %s targets non-message item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "output_text", Index: event.ContentIndex}
	case "response.reasoning_summary_text.done":
		if state.itemTypes[itemID] != string(ModelOutputReasoning) {
			return fmt.Errorf("%w: OpenAI %s targets non-reasoning item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_summary", Index: event.SummaryIndex}
	case "response.reasoning_text.done":
		if state.itemTypes[itemID] != string(ModelOutputReasoning) {
			return fmt.Errorf("%w: OpenAI %s targets non-reasoning item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_text", Index: event.ContentIndex}
	case "response.refusal.done":
		if state.itemTypes[itemID] != string(ModelOutputMessage) {
			return fmt.Errorf("%w: OpenAI %s targets non-message item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		key = openAIStreamTextKey{ItemID: itemID, Kind: "refusal", Index: event.ContentIndex}
	default:
		return openAIUnsupportedStreamEventError(event.Type)
	}
	if _, exists := state.textDone[key]; exists {
		return fmt.Errorf("%w: OpenAI %s repeats for item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	finalText := event.Text
	if event.Type == "response.refusal.done" {
		finalText = event.Refusal
	}
	if err := state.reconcileTextEvidence(key, finalText); err != nil {
		return err
	}
	state.textDone[key] = struct{}{}
	return nil
}

func (state *openAIStreamState) acceptContentPartAdded(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	itemID := event.ItemID
	if err := state.requireActiveItem(event.Type, itemID, event.OutputIndex); err != nil {
		return err
	}
	itemType := state.itemTypes[itemID]
	if itemType == string(ModelOutputReasoning) && event.Type == "response.reasoning_summary_part.added" {
		key := openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_summary", Index: event.SummaryIndex}
		if _, exists := state.partsAdded[key]; exists {
			return fmt.Errorf("%w: OpenAI %s repeats for item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		if _, exists := state.textDone[key]; exists {
			return fmt.Errorf("%w: OpenAI reasoning summary part added arrives after text done for item %q", ErrInvalidModelOutput, itemID)
		}
		text := event.Part.Text
		if accumulated, saw := state.accumulatedTextEvidence(key); saw && accumulated != text {
			return fmt.Errorf("%w: OpenAI reasoning summary part added contradicts delta evidence", ErrInvalidModelOutput)
		}
		if existing, ok := state.partAddedText[key]; ok && existing != text {
			return fmt.Errorf("%w: OpenAI reasoning summary part added contradicts existing part text", ErrInvalidModelOutput)
		}
		state.partAddedText[key] = text
		state.partsAdded[key] = struct{}{}
		return nil
	}
	if itemType != string(ModelOutputMessage) {
		return fmt.Errorf("%w: OpenAI %s targets non-message item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	part := event.Part
	if part.Type != "output_text" && part.Type != "refusal" {
		return fmt.Errorf("%w: OpenAI %s has unsupported part type %q", ErrInvalidModelOutput, event.Type, part.Type)
	}
	key := openAIStreamTextKey{ItemID: itemID, Kind: part.Type, Index: event.ContentIndex}
	if _, exists := state.partsAdded[key]; exists {
		return fmt.Errorf("%w: OpenAI %s repeats for item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	if _, exists := state.textDone[key]; exists {
		return fmt.Errorf("%w: OpenAI content part added arrives after text done for item %q", ErrInvalidModelOutput, itemID)
	}
	text := openAIStreamPartText(part)
	if accumulated, saw := state.accumulatedTextEvidence(key); saw && accumulated != text {
		return fmt.Errorf("%w: OpenAI content part added contradicts delta evidence", ErrInvalidModelOutput)
	}
	if existing, ok := state.partAddedText[key]; ok && existing != text {
		return fmt.Errorf("%w: OpenAI content part added contradicts existing part text", ErrInvalidModelOutput)
	}
	state.partAddedText[key] = text
	if err := state.recordAnnotationBase(key, part.Annotations); err != nil {
		return err
	}
	state.partsAdded[key] = struct{}{}
	return nil
}

func (state *openAIStreamState) acceptContentPartDone(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	itemID := event.ItemID
	if err := state.requireActiveItem(event.Type, itemID, event.OutputIndex); err != nil {
		return err
	}
	itemType := state.itemTypes[itemID]
	if itemType == string(ModelOutputReasoning) && event.Type == "response.reasoning_summary_part.done" {
		key := openAIStreamTextKey{ItemID: itemID, Kind: "reasoning_summary", Index: event.SummaryIndex}
		if _, exists := state.partsAdded[key]; !exists {
			return fmt.Errorf("%w: OpenAI reasoning summary part done arrives before part added for item %q", ErrInvalidModelOutput, itemID)
		}
		if _, exists := state.partsDone[key]; exists {
			return fmt.Errorf("%w: OpenAI %s repeats for item %q", ErrInvalidModelOutput, event.Type, itemID)
		}
		text := event.Part.Text
		if added, ok := state.partAddedText[key]; ok && added != "" && added != text {
			return fmt.Errorf("%w: OpenAI reasoning summary part payload contradicts finalized text", ErrInvalidModelOutput)
		}
		if final, ok := state.finalText[key]; ok && final != text {
			return fmt.Errorf("%w: OpenAI reasoning summary part done contradicts final text", ErrInvalidModelOutput)
		}
		state.partsDone[key] = struct{}{}
		if raw, err := openAIStreamEventField(event, "part"); err == nil {
			state.partDoneJSON[key] = raw
		}
		return nil
	}
	if itemType != string(ModelOutputMessage) {
		return fmt.Errorf("%w: OpenAI %s targets non-message item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	part := event.Part
	if part.Type != "output_text" && part.Type != "refusal" {
		return fmt.Errorf("%w: OpenAI %s has unsupported part type %q", ErrInvalidModelOutput, event.Type, part.Type)
	}
	key := openAIStreamTextKey{ItemID: itemID, Kind: part.Type, Index: event.ContentIndex}
	if _, exists := state.partsAdded[key]; !exists {
		return fmt.Errorf("%w: OpenAI content part done arrives before content part added for item %q", ErrInvalidModelOutput, itemID)
	}
	if _, exists := state.partsDone[key]; exists {
		return fmt.Errorf("%w: OpenAI %s repeats for item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	text := openAIStreamPartText(part)
	if added, ok := state.partAddedText[key]; ok && added != "" && added != text {
		return fmt.Errorf("%w: OpenAI content part added payload contradicts finalized text", ErrInvalidModelOutput)
	}
	if final, ok := state.finalText[key]; ok && final != text {
		return fmt.Errorf("%w: OpenAI content part done contradicts final text", ErrInvalidModelOutput)
	}
	if err := state.validatePartAnnotations(key, part.Annotations); err != nil {
		return err
	}
	state.partsDone[key] = struct{}{}
	if raw, err := openAIStreamEventField(event, "part"); err == nil {
		state.partDoneJSON[key] = raw
	}
	return nil
}

func (state *openAIStreamState) acceptAnnotationAdded(event responses.ResponseStreamEventUnion) error {
	if err := state.requireActiveResponse(event.Type); err != nil {
		return err
	}
	itemID := event.ItemID
	if err := state.requireActiveItem(event.Type, itemID, event.OutputIndex); err != nil {
		return err
	}
	if state.itemTypes[itemID] != string(ModelOutputMessage) {
		return fmt.Errorf("%w: OpenAI %s targets non-message item %q", ErrInvalidModelOutput, event.Type, itemID)
	}
	key := openAIStreamTextKey{ItemID: itemID, Kind: "output_text", Index: event.ContentIndex}
	count := state.partAnnotationCounts[key]
	if event.AnnotationIndex != count {
		return fmt.Errorf("%w: OpenAI output text annotation append skips an index", ErrInvalidModelOutput)
	}
	raw, err := openAIStreamEventField(event, "annotation")
	if err != nil {
		return err
	}
	annotationKey := openAIStreamAnnotationKey{ItemID: itemID, ContentIndex: event.ContentIndex, AnnotationIndex: event.AnnotationIndex}
	if existing, exists := state.partAnnotations[annotationKey]; exists && !jsonSemanticallyEqual(existing, raw) {
		return fmt.Errorf("%w: OpenAI output text annotation is replaced", ErrInvalidModelOutput)
	}
	state.partAnnotations[annotationKey] = raw
	state.annotations[annotationKey] = raw
	state.partAnnotationCounts[key] = count + 1
	return nil
}

func (state *openAIStreamState) acceptErrorEvent(event responses.ResponseStreamEventUnion) error {
	chunk := newOpenAIStreamChunk(event, ModelStreamError)
	chunk.ErrorCode = event.Code
	chunk.ErrorMessage = event.Message
	state.emit(chunk)
	state.terminal = true
	state.terminalError = fmt.Errorf("agent: OpenAI response error: %s: %s", event.Code, event.Message)
	return nil
}
