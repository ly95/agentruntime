package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ly95/agentruntime"
	"github.com/ly95/agentruntime/examples/skill/textskill"
	"github.com/ly95/agentruntime/skills"
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	model, err := newOpenAIModel()
	if err != nil {
		return err
	}
	skill := textskill.New()
	operations := agentruntime.NewOperationRegistry()
	if err := skill.Register(operations); err != nil {
		return fmt.Errorf("install text skill: %w", err)
	}
	skillDirectory, err := filepath.Abs("examples/skill/textskill")
	if err != nil {
		return fmt.Errorf("resolve text skill directory: %w", err)
	}
	mountedSkills, err := skills.LoadSet(ctx, skills.NewLocalSource(skills.LocalSourceConfig{
		ID:          "example-local",
		Directories: []string{skillDirectory},
	}))
	if err != nil {
		return fmt.Errorf("load text skill: %w", err)
	}
	agent, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
		Model:      model,
		Skills:     mountedSkills,
		Operations: operations,
		Policy: agentruntime.OperationPolicyFunc(
			func(_ context.Context, request agentruntime.OperationRequest) (agentruntime.PolicyDecision, error) {
				if request.Operation.Name != textskill.OperationName {
					return agentruntime.PolicyDecision{
						Action: agentruntime.PolicyDeny,
						Reason: "operation is not part of the installed text skill",
					}, nil
				}
				return agentruntime.PolicyDecision{Action: agentruntime.PolicyAllow}, nil
			},
		),
		Executor: skill,
	})
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}

	result, err := agent.Run(ctx, agentruntime.Input{
		User: commandPrompt("Analyze this text with the installed skill: Agent runtimes keep business logic outside the model loop."),
	})
	if err != nil {
		return fmt.Errorf("run agent: %w", err)
	}
	fmt.Println(result.Output)
	return nil
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
		Model: os.Getenv("OPENAI_MODEL"),
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
