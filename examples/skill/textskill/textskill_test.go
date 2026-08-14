package textskill

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ly95/agentruntime"
)

func TestSkillRegistersAndExecutes(t *testing.T) {
	skill := New()
	registry := agentruntime.NewOperationRegistry()
	if err := skill.Register(registry); err != nil {
		t.Fatal(err)
	}
	arguments, err := registry.DecodeInput(
		OperationName,
		json.RawMessage(`{"text":"Agent skills\nstay small."}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := skill.Execute(context.Background(), agentruntime.OperationRequest{
		Operation: registry.Summaries()[0],
		Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Characters int `json:"characters"`
		Words      int `json:"words"`
		Lines      int `json:"lines"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatal(err)
	}
	if output.Characters != 24 || output.Words != 4 || output.Lines != 2 {
		t.Fatalf("unexpected analysis: %+v", output)
	}
}
