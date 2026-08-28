package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ly95/agentruntime"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := runWithModel(ctx, model, commandPrompt("Explain what an agent control loop does in one paragraph."))
	if err != nil {
		return err
	}
	fmt.Println(result.Output)
	return nil
}

func runWithModel(ctx context.Context, model agentruntime.Model, prompt string) (*agentruntime.Result, error) {
	agent, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{Model: model})
	if err != nil {
		return nil, fmt.Errorf("create runtime: %w", err)
	}
	result, err := agent.Run(ctx, agentruntime.Input{
		User: prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("run agent: %w", err)
	}
	return result, nil
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
