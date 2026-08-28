package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/ly95/agentruntime"
)

const (
	conformanceToolName = "modeltest_echo"
	cancelReturnTimeout = 5 * time.Second
)

var conformanceToolSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)

// RunModelConformance runs the fixed provider-neutral v1 corpus against
// factory. Every corpus member is mandatory; a Factory skip is converted to a
// failure, and the runner has no capability flags, provider branches, or
// fallback paths.
func RunModelConformance(t *testing.T, factory Factory) {
	t.Helper()
	if factory == nil {
		t.Fatal("modeltest: model factory is required")
	}

	for _, corpusMember := range v1Corpus {
		scenario := corpusMember
		t.Run(scenario.Name(), func(t *testing.T) {
			t.Cleanup(func() {
				failIfSkipped(t)
			})

			model := factory(t, scenario)
			requireUsableModel(t, model)
			bindingBefore := requireValidBinding(t, model, scenario)

			runScenario(t, model, scenario, bindingBefore)

			bindingAfter := requireValidBinding(t, model, scenario)
			if !reflect.DeepEqual(bindingBefore, bindingAfter) {
				t.Fatalf("modeltest: ModelBinding changed during scenario %q", scenario.Name())
			}
		})
	}
}

func runScenario(t *testing.T, model agent.BoundModel, scenario corpusScenario, binding agent.ModelBinding) {
	t.Helper()
	switch scenario.kind {
	case scenarioBinding:
		return
	case scenarioSuccessText:
		runTextSuccess(t, model, scenario)
	case scenarioSuccessRefusal:
		runRefusalSuccess(t, model, scenario)
	case scenarioSuccessReasoning:
		runReasoningSuccess(t, model, scenario)
	case scenarioSuccessTool:
		runToolSuccess(t, model, scenario)
	case scenarioSuccessStream:
		runStreamSuccess(t, model, scenario)
	case scenarioSuccessUsage:
		runUsageSuccess(t, model, scenario)
	case scenarioSuccessReplay:
		runReplaySuccess(t, model, scenario)
	case scenarioConcurrencyBoundModel:
		runConcurrentBoundModel(t, model, scenario, binding)
	case scenarioCancelPreCanceled:
		runPreCanceled(t, model, scenario)
	case scenarioCancelAfterResponseStarted:
		runCancelAfterResponseStarted(t, model, scenario)
	case scenarioErrorAuthentication:
		runProviderFailure(t, model, scenario, providerFailureExpectation{
			category: agent.ProviderErrorAuthentication,
			sentinel: agent.ErrProviderAuthentication,
		})
	case scenarioErrorQuota:
		runProviderFailure(t, model, scenario, providerFailureExpectation{
			category: agent.ProviderErrorQuota,
			sentinel: agent.ErrProviderQuotaExceeded,
		})
	case scenarioErrorRateLimit:
		runProviderFailure(t, model, scenario, providerFailureExpectation{
			category:  agent.ProviderErrorRateLimit,
			sentinel:  agent.ErrProviderRateLimited,
			retryable: true,
		})
	case scenarioErrorRejected:
		runProviderFailure(t, model, scenario, providerFailureExpectation{
			category: agent.ProviderErrorRejected,
			sentinel: agent.ErrProviderRequestRejected,
		})
	case scenarioErrorTransient:
		runProviderFailure(t, model, scenario, providerFailureExpectation{
			category:  agent.ProviderErrorTransient,
			sentinel:  agent.ErrProviderUnavailable,
			retryable: true,
		})
	case scenarioInvalidUnknownOutput, scenarioInvalidDuplicateOutput,
		scenarioInvalidReorderedOutput, scenarioInvalidContradictoryID,
		scenarioInvalidPartialCompletion:
		runInvalidOutput(t, model, scenario)
	default:
		t.Fatalf("modeltest: internal unsupported scenario kind %d", scenario.kind)
	}
}

func failIfSkipped(t *testing.T) {
	t.Helper()
	if t.Skipped() {
		t.Error("modeltest: factory called t.Skip or t.SkipNow; every fixed corpus scenario is mandatory")
	}
}

func requireUsableModel(t *testing.T, model agent.BoundModel) {
	t.Helper()
	if err := validateFactoryModel(model); err != nil {
		t.Fatalf("modeltest: %v", err)
	}
}

func validateFactoryModel(model agent.BoundModel) error {
	if model == nil {
		return errors.New("factory returned a nil agentruntime.BoundModel")
	}
	value := reflect.ValueOf(model)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("factory returned a typed-nil agentruntime.BoundModel")
		}
	}
	return nil
}

func requireValidBinding(t *testing.T, model agent.BoundModel, scenario Scenario) agent.ModelBinding {
	t.Helper()
	binding, err := validatedModelBinding(model, scenario.PayloadMarker())
	if err != nil {
		t.Fatalf("modeltest: %v", err)
	}
	return binding
}

func validatedModelBinding(model agent.BoundModel, marker string) (agent.ModelBinding, error) {
	binding := model.Binding()
	if err := binding.Validate(); err != nil {
		return agent.ModelBinding{}, fmt.Errorf("invalid ModelBinding: %w", err)
	}
	if field, leaked := modelBindingMarkerField(binding, marker); leaked {
		return agent.ModelBinding{}, fmt.Errorf("ModelBinding field %s contains scenario PayloadMarker", field)
	}
	if repeated := model.Binding(); !reflect.DeepEqual(binding, repeated) {
		return agent.ModelBinding{}, errors.New("BoundModel.Binding returned unstable values")
	}
	return binding, nil
}

func modelBindingMarkerField(binding agent.ModelBinding, marker string) (string, bool) {
	fields := []struct {
		name  string
		value string
	}{
		{name: "Provider", value: binding.Provider},
		{name: "Model", value: binding.Model},
		{name: "EndpointClass", value: binding.EndpointClass},
		{name: "CredentialPrincipal", value: binding.CredentialPrincipal},
		{name: "AdapterVersion", value: binding.AdapterVersion},
	}
	for _, field := range fields {
		if strings.Contains(field.value, marker) {
			return field.name, true
		}
	}
	return "", false
}

type callResult struct {
	response *agent.ModelResponse
	err      error
	events   []agent.ModelStreamEvent
}

type streamRecorder struct {
	mu     sync.Mutex
	events []agent.ModelStreamEvent
	hook   func(agent.ModelStreamEvent)
}

func (recorder *streamRecorder) record(event agent.ModelStreamEvent) {
	event = cloneStreamEvent(event)
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
	if recorder.hook != nil {
		recorder.hook(event)
	}
}

func (recorder *streamRecorder) snapshot() []agent.ModelStreamEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	out := make([]agent.ModelStreamEvent, len(recorder.events))
	for index, event := range recorder.events {
		out[index] = cloneStreamEvent(event)
	}
	return out
}

func cloneStreamEvent(event agent.ModelStreamEvent) agent.ModelStreamEvent {
	if event.SequenceNumber != nil {
		value := *event.SequenceNumber
		event.SequenceNumber = &value
	}
	if event.OutputIndex != nil {
		value := *event.OutputIndex
		event.OutputIndex = &value
	}
	return event
}

func complete(ctx context.Context, model agent.Model, request agent.ModelRequest) callResult {
	recorder := &streamRecorder{}
	request.StreamSink = recorder.record
	response, err := model.Complete(ctx, request)
	return callResult{response: response, err: err, events: recorder.snapshot()}
}

func baseRequest(scenario Scenario, call int) agent.ModelRequest {
	callName := strings.ReplaceAll(scenario.Name(), "/", "-")
	return agent.ModelRequest{
		Instructions: "Execute the selected modeltest v1 conformance scenario.",
		Input: []agent.ModelInputItem{{
			Type: agent.ModelInputUserMessage,
			Text: "modeltest request for " + scenario.Name(),
		}},
		ModelCallID: "modeltest-" + callName + "-call-" + strconv.Itoa(call),
	}
}

func requestWithTool(scenario Scenario, call int) agent.ModelRequest {
	request := baseRequest(scenario, call)
	request.Tools = []agent.ToolDefinition{{
		Name:        conformanceToolName,
		Description: "Returns the supplied value for conformance testing.",
		InputSchema: append(json.RawMessage(nil), conformanceToolSchema...),
	}}
	return request
}

func runTextSuccess(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	result := complete(t.Context(), model, baseRequest(scenario, 1))
	response := requireSuccessfulCall(t, scenario, result, true)
	if strings.TrimSpace(response.OutputText) == "" || response.Refusal != "" || response.HadReasoning {
		t.Fatalf("modeltest: text response has invalid semantic projection: %+v", response)
	}
	requireOutputSequence(t, response, agent.ModelOutputMessage)
}

func runRefusalSuccess(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	result := complete(t.Context(), model, baseRequest(scenario, 1))
	response := requireSuccessfulCall(t, scenario, result, true)
	if strings.TrimSpace(response.Refusal) == "" || response.OutputText != "" || response.HadReasoning {
		t.Fatalf("modeltest: refusal response has invalid semantic projection: %+v", response)
	}
	requireOutputSequence(t, response, agent.ModelOutputMessage)
}

func runReasoningSuccess(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	result := complete(t.Context(), model, baseRequest(scenario, 1))
	response := requireSuccessfulCall(t, scenario, result, true)
	if !response.HadReasoning || strings.TrimSpace(response.OutputText) == "" || response.Refusal != "" {
		t.Fatalf("modeltest: reasoning response has invalid semantic projection: %+v", response)
	}
	requireOutputSequence(t, response, agent.ModelOutputReasoning, agent.ModelOutputMessage)
}

func runToolSuccess(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	result := complete(t.Context(), model, requestWithTool(scenario, 1))
	response := requireSuccessfulCall(t, scenario, result, true)
	if response.OutputText != "" || response.Refusal != "" || response.HadReasoning {
		t.Fatalf("modeltest: tool response has invalid semantic projection: %+v", response)
	}
	requireOutputSequence(t, response, agent.ModelOutputFunctionCall)
	requireConformanceToolCall(t, response.Items[0].Call)
}

func runStreamSuccess(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	result := complete(t.Context(), model, baseRequest(scenario, 1))
	response := requireSuccessfulCall(t, scenario, result, true)
	if strings.TrimSpace(response.OutputText) == "" || response.Refusal != "" {
		t.Fatalf("modeltest: stream response has invalid semantic projection: %+v", response)
	}
	requireOutputSequence(t, response, agent.ModelOutputMessage)
	assertStreamProjection(t, response, result.events)
}

func runUsageSuccess(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	result := complete(t.Context(), model, baseRequest(scenario, 1))
	response := requireSuccessfulCall(t, scenario, result, true)
	usage := response.Usage
	if usage.InputTokens <= 0 || usage.OutputTokens <= 0 || usage.TotalTokens <= 0 {
		t.Fatalf("modeltest: usage scenario requires positive token counts, got %+v", usage)
	}
	if usage.TotalTokens != usage.InputTokens+usage.OutputTokens {
		t.Fatalf("modeltest: usage total=%d, want input+output=%d", usage.TotalTokens, usage.InputTokens+usage.OutputTokens)
	}
	requireOutputSequence(t, response, agent.ModelOutputMessage)
}

func runReplaySuccess(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	firstRequest := requestWithTool(scenario, 1)
	first := complete(t.Context(), model, firstRequest)
	firstResponse := requireSuccessfulCall(t, scenario, first, true)
	if !firstResponse.HadReasoning || strings.TrimSpace(firstResponse.OutputText) == "" || firstResponse.Refusal != "" {
		t.Fatalf("modeltest: replay source response has invalid semantic projection: %+v", firstResponse)
	}
	requireOutputSequence(
		t,
		firstResponse,
		agent.ModelOutputReasoning,
		agent.ModelOutputMessage,
		agent.ModelOutputFunctionCall,
	)

	transcript := append([]agent.ModelInputItem(nil), firstRequest.Input...)
	var call *agent.ToolCall
	for _, item := range firstResponse.Items {
		replayed := agent.ModelInputItem{
			Type:       agent.ModelInputAssistantOutput,
			ResponseID: firstResponse.ID,
			OutputType: item.Type,
			Raw:        append(json.RawMessage(nil), item.Raw...),
		}
		if item.Type == agent.ModelOutputFunctionCall {
			requireConformanceToolCall(t, item.Call)
			call = item.Call
			replayed.CallID = item.Call.ID
		}
		transcript = append(transcript, replayed)
	}
	if call == nil {
		t.Fatal("modeltest: replay source response omitted its function call")
	}
	transcript = append(transcript, agent.ModelInputItem{
		Type:   agent.ModelInputToolResult,
		CallID: call.ID,
		Output: json.RawMessage(`{"value":"modeltest replay result"}`),
	})

	secondRequest := requestWithTool(scenario, 2)
	secondRequest.Input = transcript
	second := complete(t.Context(), model, secondRequest)
	secondResponse := requireSuccessfulCall(t, scenario, second, true)
	if strings.TrimSpace(secondResponse.OutputText) == "" || secondResponse.Refusal != "" {
		t.Fatalf("modeltest: replay continuation has invalid semantic projection: %+v", secondResponse)
	}
	if secondResponse.ID == firstResponse.ID {
		t.Fatalf("modeltest: replay continuation reused response ID %q", secondResponse.ID)
	}
	requireOutputSequence(t, secondResponse, agent.ModelOutputMessage)
}

type gatedConcurrentCall struct {
	recorder    *streamRecorder
	entered     chan struct{}
	release     chan struct{}
	returned    chan callResult
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newGatedConcurrentCall() *gatedConcurrentCall {
	call := &gatedConcurrentCall{
		recorder: &streamRecorder{},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan callResult, 1),
	}
	call.recorder.hook = func(event agent.ModelStreamEvent) {
		if event.Type != agent.ModelStreamResponseDone {
			return
		}
		call.enteredOnce.Do(func() { close(call.entered) })
		<-call.release
	}
	return call
}

func (call *gatedConcurrentCall) releaseSink() {
	call.releaseOnce.Do(func() { close(call.release) })
}

func runConcurrentBoundModel(
	t *testing.T,
	model agent.BoundModel,
	scenario corpusScenario,
	binding agent.ModelBinding,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := [2]*gatedConcurrentCall{newGatedConcurrentCall(), newGatedConcurrentCall()}
	defer calls[0].releaseSink()
	defer calls[1].releaseSink()
	start := make(chan struct{})
	for index, call := range calls {
		request := requestWithTool(scenario, index+1)
		request.StreamSink = call.recorder.record
		go func(call *gatedConcurrentCall, request agent.ModelRequest) {
			<-start
			response, err := model.Complete(ctx, request)
			call.returned <- callResult{response: response, err: err, events: call.recorder.snapshot()}
		}(call, request)
	}
	close(start)

	first := waitForAnyConcurrentSink(t, calls)
	assertConcurrentCallBlocked(t, calls[first])
	requireConcurrentBinding(t, model, scenario, binding)
	calls[first].releaseSink()
	results := [2]callResult{}
	results[first] = waitForConcurrentReturn(t, calls[first])

	second := 1 - first
	waitForConcurrentSink(t, calls[second])
	assertConcurrentCallBlocked(t, calls[second])
	requireConcurrentBinding(t, model, scenario, binding)
	calls[second].releaseSink()
	results[second] = waitForConcurrentReturn(t, calls[second])

	responses := [2]*agent.ModelResponse{}
	for index, result := range results {
		response := requireSuccessfulCall(t, scenario, result, true)
		if strings.TrimSpace(response.OutputText) == "" || response.Refusal != "" || response.HadReasoning {
			t.Fatalf("modeltest: concurrent response %d has invalid semantic projection: %+v", index, response)
		}
		requireOutputSequence(t, response, agent.ModelOutputMessage)
		responses[index] = response
	}
	if responses[0].ID == responses[1].ID || responses[0].Items[0].ID == responses[1].Items[0].ID {
		t.Fatalf(
			"modeltest: concurrent calls reused response/item identity %q/%q",
			responses[0].ID,
			responses[0].Items[0].ID,
		)
	}
}

func waitForAnyConcurrentSink(t *testing.T, calls [2]*gatedConcurrentCall) int {
	t.Helper()
	timer := time.NewTimer(cancelReturnTimeout)
	defer timer.Stop()
	select {
	case <-calls[0].entered:
		return 0
	case <-calls[1].entered:
		return 1
	case result := <-calls[0].returned:
		t.Fatalf("modeltest: concurrent Complete 0 returned before its terminal StreamSink completed: response=%+v error=%v", result.response, result.err)
	case result := <-calls[1].returned:
		t.Fatalf("modeltest: concurrent Complete 1 returned before its terminal StreamSink completed: response=%+v error=%v", result.response, result.err)
	case <-timer.C:
		t.Fatalf("modeltest: concurrent Complete calls produced no terminal StreamSink event within %s", cancelReturnTimeout)
	}
	return -1
}

func waitForConcurrentSink(t *testing.T, call *gatedConcurrentCall) {
	t.Helper()
	timer := time.NewTimer(cancelReturnTimeout)
	defer timer.Stop()
	select {
	case <-call.entered:
	case result := <-call.returned:
		t.Fatalf("modeltest: concurrent Complete returned before its terminal StreamSink completed: response=%+v error=%v", result.response, result.err)
	case <-timer.C:
		t.Fatalf("modeltest: concurrent Complete produced no terminal StreamSink event within %s", cancelReturnTimeout)
	}
}

func assertConcurrentCallBlocked(t *testing.T, call *gatedConcurrentCall) {
	t.Helper()
	select {
	case result := <-call.returned:
		t.Fatalf("modeltest: Complete returned while its terminal StreamSink was blocked: response=%+v error=%v", result.response, result.err)
	default:
	}
}

func requireConcurrentBinding(
	t *testing.T,
	model agent.BoundModel,
	scenario Scenario,
	want agent.ModelBinding,
) {
	t.Helper()
	type bindingResult struct {
		binding agent.ModelBinding
		err     error
	}
	returned := make(chan bindingResult, 1)
	go func() {
		binding, err := validatedModelBinding(model, scenario.PayloadMarker())
		returned <- bindingResult{binding: binding, err: err}
	}()
	timer := time.NewTimer(cancelReturnTimeout)
	defer timer.Stop()
	select {
	case result := <-returned:
		if result.err != nil {
			t.Fatalf("modeltest: Binding failed during Complete: %v", result.err)
		}
		if !reflect.DeepEqual(result.binding, want) {
			t.Fatal("modeltest: ModelBinding changed while Complete was active")
		}
	case <-timer.C:
		t.Fatalf("modeltest: Binding did not return during Complete within %s", cancelReturnTimeout)
	}
}

func waitForConcurrentReturn(t *testing.T, call *gatedConcurrentCall) callResult {
	t.Helper()
	timer := time.NewTimer(cancelReturnTimeout)
	defer timer.Stop()
	select {
	case result := <-call.returned:
		return result
	case <-timer.C:
		t.Fatalf("modeltest: Complete did not return within %s after its StreamSink was released", cancelReturnTimeout)
	}
	return callResult{}
}

func runPreCanceled(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	returned := make(chan callResult, 1)
	go func() {
		returned <- complete(ctx, model, baseRequest(scenario, 1))
	}()

	timer := time.NewTimer(cancelReturnTimeout)
	defer timer.Stop()
	var result callResult
	select {
	case result = <-returned:
	case <-timer.C:
		t.Fatalf("modeltest: pre-canceled Complete did not return within %s", cancelReturnTimeout)
	}
	err := requireFailedCall(t, scenario, result, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("modeltest: pre-canceled Complete error=%v, want context.Canceled", err)
	}
	assertNotProviderFailure(t, err)
	for _, event := range result.events {
		if event.Type == agent.ModelStreamResponseStarted {
			t.Fatal("modeltest: pre-canceled Complete emitted response_started")
		}
	}
}

func runCancelAfterResponseStarted(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var cancelOnce sync.Once
	recorder := &streamRecorder{}
	recorder.hook = func(event agent.ModelStreamEvent) {
		if event.Type == agent.ModelStreamResponseStarted {
			cancelOnce.Do(cancel)
		}
	}
	request := baseRequest(scenario, 1)
	request.StreamSink = recorder.record

	returned := make(chan callResult, 1)
	go func() {
		response, err := model.Complete(ctx, request)
		returned <- callResult{response: response, err: err}
	}()

	timer := time.NewTimer(cancelReturnTimeout)
	defer timer.Stop()
	var result callResult
	select {
	case result = <-returned:
		result.events = recorder.snapshot()
	case <-timer.C:
		cancel()
		t.Fatalf("modeltest: Complete did not return within %s after response_started cancellation", cancelReturnTimeout)
	}

	err := requireFailedCall(t, scenario, result, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("modeltest: post-start cancellation error=%v, want context.Canceled", err)
	}
	assertNotProviderFailure(t, err)
	started := 0
	for _, event := range result.events {
		if event.Type == agent.ModelStreamResponseStarted {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("modeltest: post-start cancellation emitted %d response_started events, want 1", started)
	}
}

type providerFailureExpectation struct {
	category  agent.ProviderErrorCategory
	sentinel  error
	retryable bool
}

func runProviderFailure(t *testing.T, model agent.Model, scenario corpusScenario, want providerFailureExpectation) {
	t.Helper()
	result := complete(t.Context(), model, baseRequest(scenario, 1))
	err := requireFailedCall(t, scenario, result, false)
	if !errors.Is(err, want.sentinel) {
		t.Fatalf("modeltest: provider error=%v, want errors.Is(%v)", err, want.sentinel)
	}
	category, ok := agent.ProviderErrorCategoryOf(err)
	if !ok || category != want.category {
		t.Fatalf("modeltest: provider category=%q ok=%v, want %q", category, ok, want.category)
	}
	if retryable := agent.IsRetryableProviderError(err); retryable != want.retryable {
		t.Fatalf("modeltest: provider retryability=%v, want %v", retryable, want.retryable)
	}
	var providerErr *agent.ProviderError
	if !errors.As(err, &providerErr) || providerErr == nil {
		t.Fatalf("modeltest: provider error %T does not expose *agentruntime.ProviderError", err)
	}
	if providerErr.Category != want.category || strings.TrimSpace(providerErr.Provider) == "" {
		t.Fatalf("modeltest: ProviderError metadata=%+v, want category %q and non-empty provider", providerErr, want.category)
	}
	if err := validateProviderErrorPrivacyAndChain(err, providerErr, scenario.PayloadMarker(), want); err != nil {
		t.Fatalf("modeltest: %v", err)
	}
}

func validateProviderErrorPrivacyAndChain(err error, providerErr *agent.ProviderError, marker string, want providerFailureExpectation) error {
	privateCause := providerErr.Unwrap()
	if privateCause == nil || !strings.Contains(privateCause.Error(), marker) {
		return errors.New("provider fixture did not retain PayloadMarker in its trusted error cause")
	}

	providerSentinels := []struct {
		category agent.ProviderErrorCategory
		sentinel error
	}{
		{category: agent.ProviderErrorAuthentication, sentinel: agent.ErrProviderAuthentication},
		{category: agent.ProviderErrorQuota, sentinel: agent.ErrProviderQuotaExceeded},
		{category: agent.ProviderErrorRateLimit, sentinel: agent.ErrProviderRateLimited},
		{category: agent.ProviderErrorRejected, sentinel: agent.ErrProviderRequestRejected},
		{category: agent.ProviderErrorTransient, sentinel: agent.ErrProviderUnavailable},
	}
	for _, candidate := range providerSentinels {
		if candidate.category != want.category && errors.Is(err, candidate.sentinel) {
			return fmt.Errorf("provider error also matched contradictory category %q", candidate.category)
		}
	}

	return visitErrorChain(err, func(chainErr error) error {
		current, ok := chainErr.(*agent.ProviderError)
		if !ok || current == nil {
			return nil
		}
		if current.Category != want.category {
			return fmt.Errorf("ProviderError chain category=%q, want only %q", current.Category, want.category)
		}
		return validateProviderErrorPublicData(current, marker)
	})
}

func validateProviderErrorPublicData(providerErr *agent.ProviderError, marker string) error {
	fields := []struct {
		name  string
		value string
	}{
		{name: "Provider", value: providerErr.Provider},
		{name: "Category", value: string(providerErr.Category)},
		{name: "Code", value: providerErr.Code},
		{name: "RequestID", value: providerErr.RequestID},
		{name: "Error()", value: providerErr.Error()},
	}
	for _, field := range fields {
		if strings.Contains(field.value, marker) {
			return fmt.Errorf("ProviderError public field %s contains scenario PayloadMarker", field.name)
		}
	}
	encoded, err := json.Marshal(providerErr)
	if err != nil {
		return fmt.Errorf("marshal ProviderError public JSON: %w", err)
	}
	if strings.Contains(string(encoded), marker) {
		return errors.New("ProviderError public JSON contains scenario PayloadMarker")
	}
	return nil
}

func visitErrorChain(root error, visit func(error) error) error {
	const maxErrors = 100
	seen := make(map[error]struct{})
	visited := 0

	var walk func(error) error
	walk = func(current error) error {
		if current == nil {
			return nil
		}
		visited++
		if visited > maxErrors {
			return errors.New("provider error chain exceeds validation limit")
		}
		currentType := reflect.TypeOf(current)
		if currentType != nil && currentType.Comparable() {
			if _, exists := seen[current]; exists {
				return nil
			}
			seen[current] = struct{}{}
		}
		if err := visit(current); err != nil {
			return err
		}
		switch unwrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, next := range unwrapped.Unwrap() {
				if err := walk(next); err != nil {
					return err
				}
			}
		case interface{ Unwrap() error }:
			return walk(unwrapped.Unwrap())
		}
		return nil
	}

	return walk(root)
}

func runInvalidOutput(t *testing.T, model agent.Model, scenario corpusScenario) {
	t.Helper()
	result := complete(t.Context(), model, baseRequest(scenario, 1))
	err := requireFailedCall(t, scenario, result, true)
	if !errors.Is(err, agent.ErrInvalidModelOutput) {
		t.Fatalf("modeltest: invalid provider output error=%v, want ErrInvalidModelOutput", err)
	}
	assertNotProviderFailure(t, err)
	if agent.IsRetryableProviderError(err) {
		t.Fatal("modeltest: invalid model output was classified retryable")
	}
}

func requireSuccessfulCall(t *testing.T, scenario Scenario, result callResult, requireMarker bool) *agent.ModelResponse {
	t.Helper()
	assertPublicStreamBoundary(t, scenario, result.events, requireMarker)
	if result.err != nil {
		if strings.Contains(result.err.Error(), scenario.PayloadMarker()) {
			t.Fatal("modeltest: successful scenario error string leaked PayloadMarker")
		}
		t.Fatalf("modeltest: Complete returned error: %v", result.err)
	}
	if result.response == nil {
		t.Fatal("modeltest: successful Complete returned a nil response")
	}
	validateSuccessfulResponse(t, scenario, result.response)
	assertSuccessfulLifecycle(t, result.response, result.events)
	return result.response
}

func requireFailedCall(t *testing.T, scenario Scenario, result callResult, requireMarker bool) error {
	t.Helper()
	assertPublicStreamBoundary(t, scenario, result.events, requireMarker)
	if result.response != nil {
		t.Fatalf("modeltest: failed Complete returned non-nil response %+v", result.response)
	}
	if result.err == nil {
		t.Fatal("modeltest: failed Complete returned a nil error")
	}
	if strings.Contains(result.err.Error(), scenario.PayloadMarker()) {
		t.Fatal("modeltest: returned error string leaked PayloadMarker")
	}
	for _, event := range result.events {
		if event.Type == agent.ModelStreamResponseDone {
			t.Fatal("modeltest: failed Complete emitted response_done")
		}
	}
	return result.err
}

func assertPublicStreamBoundary(t *testing.T, scenario Scenario, events []agent.ModelStreamEvent, requireMarker bool) {
	t.Helper()
	marker := scenario.PayloadMarker()
	trustedMarker := false
	for index, event := range events {
		if strings.Contains(event.RawJSON, marker) || strings.Contains(event.ErrorMessage, marker) {
			trustedMarker = true
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("modeltest: marshal public stream event %d: %v", index, err)
		}
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("modeltest: public stream event %d JSON leaked PayloadMarker", index)
		}
		var public map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &public); err != nil {
			t.Fatalf("modeltest: decode public stream event %d JSON: %v", index, err)
		}
		if _, exists := public["raw_json"]; exists {
			t.Fatalf("modeltest: public stream event %d exposed raw_json", index)
		}
		if _, exists := public["error_message"]; exists {
			t.Fatalf("modeltest: public stream event %d exposed error_message", index)
		}
	}
	if requireMarker && !trustedMarker {
		t.Fatal("modeltest: fixture did not expose PayloadMarker at a trusted stream boundary")
	}
}

func validateSuccessfulResponse(t *testing.T, scenario Scenario, response *agent.ModelResponse) {
	t.Helper()
	if strings.TrimSpace(response.ID) == "" || response.ID != strings.TrimSpace(response.ID) {
		t.Fatalf("modeltest: response ID %q is empty or non-canonical", response.ID)
	}
	if strings.TrimSpace(response.FinishReason) == "" {
		t.Fatal("modeltest: successful response omitted finish reason")
	}
	if response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 || response.Usage.TotalTokens < 0 {
		t.Fatalf("modeltest: response contains negative usage %+v", response.Usage)
	}
	if response.Usage.TotalTokens != response.Usage.InputTokens+response.Usage.OutputTokens {
		t.Fatalf("modeltest: response usage total=%d, want input+output=%d", response.Usage.TotalTokens, response.Usage.InputTokens+response.Usage.OutputTokens)
	}
	assertSemanticMarkerAbsent(t, scenario, response)
	if err := agent.ValidateModelResponse(response); err != nil {
		t.Fatalf("modeltest: response is not replayable: %v", err)
	}

	seenItemIDs := make(map[string]struct{}, len(response.Items))
	seenCallIDs := make(map[string]struct{})
	hadReasoning := false
	for index, item := range response.Items {
		if strings.TrimSpace(item.ID) == "" || item.ID != strings.TrimSpace(item.ID) {
			t.Fatalf("modeltest: output item %d ID %q is empty or non-canonical", index, item.ID)
		}
		if _, exists := seenItemIDs[item.ID]; exists {
			t.Fatalf("modeltest: successful response repeats output item ID %q", item.ID)
		}
		seenItemIDs[item.ID] = struct{}{}
		if !json.Valid(item.Raw) {
			t.Fatalf("modeltest: output item %q Raw is not valid JSON", item.ID)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(item.Raw, &raw); err != nil || raw == nil {
			t.Fatalf("modeltest: output item %q Raw must be a JSON object: %v", item.ID, err)
		}
		var rawType string
		if err := json.Unmarshal(raw["type"], &rawType); err != nil || rawType != string(item.Type) {
			t.Fatalf("modeltest: output item %q raw type=%q error=%v, want %q", item.ID, rawType, err, item.Type)
		}
		if rawID, exists := raw["id"]; exists {
			var id string
			if err := json.Unmarshal(rawID, &id); err != nil || id != item.ID {
				t.Fatalf("modeltest: output item %q raw ID=%q error=%v", item.ID, id, err)
			}
		}

		switch item.Type {
		case agent.ModelOutputMessage:
			if item.Call != nil {
				t.Fatalf("modeltest: message item %q contains a ToolCall", item.ID)
			}
		case agent.ModelOutputReasoning:
			if item.Call != nil {
				t.Fatalf("modeltest: reasoning item %q contains a ToolCall", item.ID)
			}
			hadReasoning = true
		case agent.ModelOutputFunctionCall:
			if item.Call == nil {
				t.Fatalf("modeltest: function-call item %q omitted its ToolCall", item.ID)
			}
			if strings.TrimSpace(item.Call.ID) == "" || strings.TrimSpace(item.Call.Name) == "" || !json.Valid(item.Call.Input) {
				t.Fatalf("modeltest: function-call item %q has invalid structured call %+v", item.ID, item.Call)
			}
			if _, exists := seenCallIDs[item.Call.ID]; exists {
				t.Fatalf("modeltest: successful response repeats call ID %q", item.Call.ID)
			}
			seenCallIDs[item.Call.ID] = struct{}{}
		default:
			t.Fatalf("modeltest: successful response contains unknown output type %q", item.Type)
		}
	}
	if response.HadReasoning != hadReasoning {
		t.Fatalf("modeltest: HadReasoning=%v, reasoning item present=%v", response.HadReasoning, hadReasoning)
	}
}

func assertSemanticMarkerAbsent(t *testing.T, scenario Scenario, response *agent.ModelResponse) {
	t.Helper()
	marker := scenario.PayloadMarker()
	values := []string{response.ID, response.OutputText, response.Refusal, response.FinishReason}
	for _, item := range response.Items {
		values = append(values, item.ID, item.Text)
		if item.Call != nil {
			values = append(values, item.Call.ID, item.Call.Name, string(item.Call.Input))
		}
	}
	for _, value := range values {
		if strings.Contains(value, marker) {
			t.Fatal("modeltest: semantic ModelResponse data contains PayloadMarker")
		}
	}
}

func assertSuccessfulLifecycle(t *testing.T, response *agent.ModelResponse, events []agent.ModelStreamEvent) {
	t.Helper()
	if err := validateSuccessfulLifecycle(response, events); err != nil {
		t.Fatalf("modeltest: %v", err)
	}
}

func validateSuccessfulLifecycle(response *agent.ModelResponse, events []agent.ModelStreamEvent) error {
	if response == nil {
		return errors.New("successful lifecycle requires a non-nil ModelResponse")
	}
	if len(events) < 2 {
		return fmt.Errorf("successful Complete emitted %d stream events, want at least response_started and response_done", len(events))
	}
	if events[0].Type != agent.ModelStreamResponseStarted {
		return fmt.Errorf("first success event=%q, want response_started", events[0].Type)
	}
	if events[len(events)-1].Type != agent.ModelStreamResponseDone {
		return fmt.Errorf("last success event=%q, want response_done", events[len(events)-1].Type)
	}

	started, done := 0, 0
	var lastSequence int64
	haveSequence := false
	for index, event := range events {
		if event.ResponseID != "" && event.ResponseID != response.ID {
			return fmt.Errorf("stream event %d response ID=%q, want %q", index, event.ResponseID, response.ID)
		}
		switch event.Type {
		case agent.ModelStreamResponseStarted:
			started++
		case agent.ModelStreamResponseDone:
			done++
		case agent.ModelStreamError:
			return fmt.Errorf("successful Complete emitted stream error at event %d", index)
		}
		if event.SequenceNumber != nil {
			if *event.SequenceNumber < 0 || haveSequence && *event.SequenceNumber <= lastSequence {
				return fmt.Errorf("stream event %d has non-increasing sequence number %d", index, *event.SequenceNumber)
			}
			lastSequence = *event.SequenceNumber
			haveSequence = true
		}
	}
	if started != 1 || done != 1 {
		return fmt.Errorf("success lifecycle response_started=%d response_done=%d, want 1 each", started, done)
	}
	if events[0].ResponseID != response.ID || events[len(events)-1].ResponseID != response.ID {
		return errors.New("success lifecycle boundaries did not bind the final response ID")
	}
	return validateOutputItemEvidence(response, events)
}

type outputItemEvidenceState struct {
	added              bool
	done               bool
	argumentsDone      bool
	sawArgumentDelta   bool
	argumentDeltaBytes strings.Builder
}

func validateOutputItemEvidence(response *agent.ModelResponse, events []agent.ModelStreamEvent) error {
	states := make([]outputItemEvidenceState, len(response.Items))
	nextAdded, nextDone := 0, 0

	for eventIndex, event := range events {
		switch event.Type {
		case agent.ModelStreamItemAdded:
			itemIndex, err := outputEvidenceItemIndex(response, event, eventIndex)
			if err != nil {
				return err
			}
			if itemIndex != nextAdded {
				return fmt.Errorf("item_added event %d has output index %d, want next final item index %d", eventIndex, itemIndex, nextAdded)
			}
			state := &states[itemIndex]
			if state.added {
				return fmt.Errorf("final output item %q has duplicate item_added evidence", response.Items[itemIndex].ID)
			}
			if err := validateItemBoundaryCallEvidence(response.Items[itemIndex], event, eventIndex); err != nil {
				return err
			}
			state.added = true
			nextAdded++

		case agent.ModelStreamItemDone:
			itemIndex, err := outputEvidenceItemIndex(response, event, eventIndex)
			if err != nil {
				return err
			}
			if itemIndex != nextDone {
				return fmt.Errorf("item_done event %d has output index %d, want next final item index %d", eventIndex, itemIndex, nextDone)
			}
			state := &states[itemIndex]
			if !state.added {
				return fmt.Errorf("final output item %q emitted item_done before item_added", response.Items[itemIndex].ID)
			}
			if state.done {
				return fmt.Errorf("final output item %q has duplicate item_done evidence", response.Items[itemIndex].ID)
			}
			if err := validateItemBoundaryCallEvidence(response.Items[itemIndex], event, eventIndex); err != nil {
				return err
			}
			state.done = true
			nextDone++

		case agent.ModelStreamReasoningSummaryDelta,
			agent.ModelStreamCommentaryDelta,
			agent.ModelStreamTextDelta,
			agent.ModelStreamRefusalDelta,
			agent.ModelStreamToolArgumentsDelta,
			agent.ModelStreamToolArgumentsDone:
			itemIndex, err := outputEvidenceItemIndex(response, event, eventIndex)
			if err != nil {
				return err
			}
			state := &states[itemIndex]
			if !state.added || state.done {
				return fmt.Errorf("item-scoped event %d type=%q occurred outside item %q added/done boundaries", eventIndex, event.Type, response.Items[itemIndex].ID)
			}
			if err := validateItemDataEvidence(response.Items[itemIndex], state, event, eventIndex); err != nil {
				return err
			}
		}
	}

	if nextAdded != len(response.Items) || nextDone != len(response.Items) {
		return fmt.Errorf("final response has %d items but stream evidence has %d item_added and %d item_done events", len(response.Items), nextAdded, nextDone)
	}
	for itemIndex := range response.Items {
		item := response.Items[itemIndex]
		state := &states[itemIndex]
		if !state.added || !state.done {
			return fmt.Errorf("final output item %q does not have exactly one item_added and item_done", item.ID)
		}
		if item.Type != agent.ModelOutputFunctionCall {
			continue
		}
		if item.Call == nil {
			return fmt.Errorf("function-call item %q omitted its ToolCall", item.ID)
		}
		if !state.argumentsDone {
			return fmt.Errorf("function-call item %q omitted tool_arguments_done evidence", item.ID)
		}
		if state.sawArgumentDelta && !jsonArgumentsEqual(item.Call.Input, state.argumentDeltaBytes.String()) {
			return fmt.Errorf("function-call item %q argument deltas do not match final ToolCall input", item.ID)
		}
	}
	return nil
}

func outputEvidenceItemIndex(response *agent.ModelResponse, event agent.ModelStreamEvent, eventIndex int) (int, error) {
	if event.OutputIndex == nil {
		return 0, fmt.Errorf("item-scoped event %d type=%q omitted output index", eventIndex, event.Type)
	}
	if *event.OutputIndex < 0 || *event.OutputIndex >= int64(len(response.Items)) {
		return 0, fmt.Errorf("item-scoped event %d type=%q has out-of-range output index %d", eventIndex, event.Type, *event.OutputIndex)
	}
	itemIndex := int(*event.OutputIndex)
	if event.ItemID != response.Items[itemIndex].ID {
		return 0, fmt.Errorf("item-scoped event %d type=%q item ID=%q, want final item %q at output index %d", eventIndex, event.Type, event.ItemID, response.Items[itemIndex].ID, itemIndex)
	}
	return itemIndex, nil
}

func validateItemBoundaryCallEvidence(item agent.ModelOutputItem, event agent.ModelStreamEvent, eventIndex int) error {
	if item.Type != agent.ModelOutputFunctionCall {
		if event.CallID != "" || event.Name != "" || event.Arguments != "" {
			return fmt.Errorf("non-function output item %q has function-call evidence at event %d", item.ID, eventIndex)
		}
		return nil
	}
	if item.Call == nil {
		return fmt.Errorf("function-call item %q omitted its ToolCall", item.ID)
	}
	if event.CallID != item.Call.ID {
		return fmt.Errorf("function-call boundary event %d call ID=%q, want %q", eventIndex, event.CallID, item.Call.ID)
	}
	if event.Name != "" && event.Name != item.Call.Name {
		return fmt.Errorf("function-call boundary event %d name=%q, want %q", eventIndex, event.Name, item.Call.Name)
	}
	if event.Arguments != "" && !jsonArgumentsEqual(item.Call.Input, event.Arguments) {
		return fmt.Errorf("function-call boundary event %d arguments do not match final ToolCall input", eventIndex)
	}
	return nil
}

func validateItemDataEvidence(item agent.ModelOutputItem, state *outputItemEvidenceState, event agent.ModelStreamEvent, eventIndex int) error {
	switch event.Type {
	case agent.ModelStreamReasoningSummaryDelta:
		if item.Type != agent.ModelOutputReasoning {
			return fmt.Errorf("reasoning event %d targets final output type %q", eventIndex, item.Type)
		}
	case agent.ModelStreamCommentaryDelta, agent.ModelStreamTextDelta, agent.ModelStreamRefusalDelta:
		if item.Type != agent.ModelOutputMessage {
			return fmt.Errorf("message event %d type=%q targets final output type %q", eventIndex, event.Type, item.Type)
		}
	case agent.ModelStreamToolArgumentsDelta, agent.ModelStreamToolArgumentsDone:
		if item.Type != agent.ModelOutputFunctionCall || item.Call == nil {
			return fmt.Errorf("tool-arguments event %d targets non-function output item %q", eventIndex, item.ID)
		}
		if event.CallID != item.Call.ID {
			return fmt.Errorf("tool-arguments event %d call ID=%q, want %q", eventIndex, event.CallID, item.Call.ID)
		}
		if event.Name != "" && event.Name != item.Call.Name {
			return fmt.Errorf("tool-arguments event %d name=%q, want %q", eventIndex, event.Name, item.Call.Name)
		}
		if event.Type == agent.ModelStreamToolArgumentsDelta {
			if state.argumentsDone {
				return fmt.Errorf("tool-arguments delta event %d followed tool_arguments_done for item %q", eventIndex, item.ID)
			}
			if event.Arguments != "" && !jsonArgumentsEqual(item.Call.Input, event.Arguments) {
				return fmt.Errorf("tool-arguments delta event %d arguments do not match final ToolCall input", eventIndex)
			}
			state.sawArgumentDelta = true
			state.argumentDeltaBytes.WriteString(event.Delta)
			return nil
		}
		if state.argumentsDone {
			return fmt.Errorf("function-call item %q has duplicate tool_arguments_done evidence", item.ID)
		}
		if event.Name != item.Call.Name {
			return fmt.Errorf("tool_arguments_done event %d name=%q, want %q", eventIndex, event.Name, item.Call.Name)
		}
		if !jsonArgumentsEqual(item.Call.Input, event.Arguments) {
			return fmt.Errorf("tool_arguments_done event %d arguments do not match final ToolCall input", eventIndex)
		}
		state.argumentsDone = true
	}
	return nil
}

func jsonArgumentsEqual(final json.RawMessage, evidence string) bool {
	var finalValue any
	if err := json.Unmarshal(final, &finalValue); err != nil {
		return false
	}
	var evidenceValue any
	if err := json.Unmarshal([]byte(evidence), &evidenceValue); err != nil {
		return false
	}
	return reflect.DeepEqual(finalValue, evidenceValue)
}

func requireOutputSequence(t *testing.T, response *agent.ModelResponse, want ...agent.ModelOutputItemType) {
	t.Helper()
	got := make([]agent.ModelOutputItemType, len(response.Items))
	for index, item := range response.Items {
		got[index] = item.Type
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modeltest: output type sequence=%v, want %v", got, want)
	}
}

func requireConformanceToolCall(t *testing.T, call *agent.ToolCall) {
	t.Helper()
	if call == nil {
		t.Fatal("modeltest: function-call output omitted its structured ToolCall")
	}
	if strings.TrimSpace(call.ID) == "" || call.Name != conformanceToolName {
		t.Fatalf("modeltest: ToolCall identity=%+v, want non-empty ID and name %q", call, conformanceToolName)
	}
	var arguments map[string]json.RawMessage
	if err := json.Unmarshal(call.Input, &arguments); err != nil || len(arguments) != 1 {
		t.Fatalf("modeltest: ToolCall input=%s error=%v, want one-field JSON object", call.Input, err)
	}
	var value string
	if err := json.Unmarshal(arguments["value"], &value); err != nil || strings.TrimSpace(value) == "" {
		t.Fatalf("modeltest: ToolCall value=%q error=%v, want non-empty string", value, err)
	}
}

func assertStreamProjection(t *testing.T, response *agent.ModelResponse, events []agent.ModelStreamEvent) {
	t.Helper()
	addedIndex, doneIndex := -1, -1
	firstDelta, lastDelta := -1, -1
	deltaCount := 0
	itemID := ""
	var text strings.Builder
	for index, event := range events {
		switch event.Type {
		case agent.ModelStreamItemAdded:
			if addedIndex >= 0 {
				t.Fatal("modeltest: stream scenario emitted duplicate item_added")
			}
			addedIndex = index
			itemID = event.ItemID
		case agent.ModelStreamTextDelta:
			if firstDelta < 0 {
				firstDelta = index
			}
			lastDelta = index
			deltaCount++
			if itemID == "" || event.ItemID != itemID {
				t.Fatalf("modeltest: text delta item ID=%q, want active item %q", event.ItemID, itemID)
			}
			text.WriteString(event.Delta)
		case agent.ModelStreamItemDone:
			if event.ItemID == itemID {
				if doneIndex >= 0 {
					t.Fatal("modeltest: stream scenario emitted duplicate item_done")
				}
				doneIndex = index
			}
		}
	}
	if itemID == "" || addedIndex < 0 || firstDelta <= addedIndex || deltaCount < 2 || doneIndex <= lastDelta {
		t.Fatalf("modeltest: incomplete stream projection: item=%q added=%d first_delta=%d deltas=%d last_delta=%d done=%d", itemID, addedIndex, firstDelta, deltaCount, lastDelta, doneIndex)
	}
	if text.String() != response.OutputText {
		t.Fatalf("modeltest: concatenated text deltas=%q, final output=%q", text.String(), response.OutputText)
	}
}

func assertNotProviderFailure(t *testing.T, err error) {
	t.Helper()
	if category, ok := agent.ProviderErrorCategoryOf(err); ok {
		t.Fatalf("modeltest: non-provider failure was classified as provider category %q", category)
	}
	if agent.IsRetryableProviderError(err) {
		t.Fatal("modeltest: non-provider failure was classified as retryable provider error")
	}
}
