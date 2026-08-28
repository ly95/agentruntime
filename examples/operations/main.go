package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ly95/agentruntime"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const addOperation = "math_add"

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	model, err := newOpenAIModel()
	if err != nil {
		return err
	}
	operations, err := mathOperations()
	if err != nil {
		return err
	}
	agent, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
		Model:      model,
		Operations: operations,
		Instructions: "For every addition request, call math_add. " +
			"Do not calculate the sum yourself.",
		Policy:    agentruntime.OperationPolicyFunc(allowReadOperations),
		Executor:  agentruntime.OperationExecutorFunc(executeMath),
		EventSink: logToolEvents,
	})
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := agent.Run(ctx, agentruntime.Input{
		User: commandPrompt("Use the tool to add 19 and 23. Return only the result."),
	})
	if err != nil {
		return fmt.Errorf("run agent: %w", err)
	}
	fmt.Println(result.Output)
	return nil
}

func mathOperations() (*agentruntime.OperationRegistry, error) {
	registry := agentruntime.NewOperationRegistry()
	err := registry.Register(agentruntime.Operation{
		Name:        addOperation,
		Description: "Add two integers and return their sum.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"left":{"type":"integer","minimum":-1000000000,"maximum":1000000000},
				"right":{"type":"integer","minimum":-1000000000,"maximum":1000000000}
			},
			"required":["left","right"],
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"sum":{"type":"integer"}},
			"required":["sum"],
			"additionalProperties":false
		}`),
		Effect:       agentruntime.OperationEffectRead,
		Capabilities: []string{"math"},
		Confirmation: agentruntime.ConfirmationSpec{Mode: agentruntime.ConfirmationNone},
	})
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", addOperation, err)
	}
	return registry, nil
}

func allowReadOperations(_ context.Context, request agentruntime.OperationRequest) (agentruntime.PolicyDecision, error) {
	if request.Operation.Effect != agentruntime.OperationEffectRead {
		return agentruntime.PolicyDecision{
			Action: agentruntime.PolicyDeny,
			Reason: "this example permits read-only operations",
		}, nil
	}
	return agentruntime.PolicyDecision{Action: agentruntime.PolicyAllow}, nil
}

func executeMath(_ context.Context, request agentruntime.OperationRequest) (agentruntime.OperationResult, error) {
	if request.Operation.Name != addOperation {
		return agentruntime.OperationResult{}, fmt.Errorf("unsupported operation %q", request.Operation.Name)
	}
	arguments, ok := request.Arguments.(map[string]any)
	if !ok {
		return agentruntime.OperationResult{}, errors.New("math_add arguments must be an object")
	}
	left, err := integerArgument(arguments, "left")
	if err != nil {
		return agentruntime.OperationResult{}, err
	}
	right, err := integerArgument(arguments, "right")
	if err != nil {
		return agentruntime.OperationResult{}, err
	}
	output, err := json.Marshal(struct {
		Sum int64 `json:"sum"`
	}{Sum: left + right})
	if err != nil {
		return agentruntime.OperationResult{}, fmt.Errorf("encode math_add output: %w", err)
	}
	return agentruntime.OperationResult{Output: output}, nil
}

func integerArgument(arguments map[string]any, name string) (int64, error) {
	number, ok := arguments[name].(json.Number)
	if !ok {
		return 0, fmt.Errorf("math_add argument %q must be an integer", name)
	}
	value, err := number.Int64()
	if err != nil {
		return 0, fmt.Errorf("decode math_add argument %q: %w", name, err)
	}
	return value, nil
}

func logToolEvents(event agentruntime.Event) {
	switch event.Type {
	case agentruntime.EventOperationStarted:
		fmt.Fprintf(os.Stderr, "tool started: %s\n", event.Operation)
	case agentruntime.EventOperationCompleted:
		fmt.Fprintf(os.Stderr, "tool completed: %s\n", event.Operation)
	}
}

func newOpenAIModel() (*agentruntime.OpenAIModel, error) {
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	client := openai.NewClient(
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(2),
	)
	model, err := agentruntime.NewOpenAIModel(client, agentruntime.OpenAIModelConfig{
		Model:               os.Getenv("OPENAI_MODEL"),
		EndpointClass:       os.Getenv("OPENAI_ENDPOINT_CLASS"),
		CredentialPrincipal: os.Getenv("OPENAI_CREDENTIAL_PRINCIPAL"),
	})
	if err != nil {
		return nil, fmt.Errorf("create OpenAI model: %w", err)
	}
	return model, nil
}

func commandPrompt(fallback string) string {
	if value := strings.TrimSpace(strings.Join(os.Args[1:], " ")); value != "" {
		return value
	}
	return fallback
}
