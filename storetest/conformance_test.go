package storetest

import (
	"testing"

	agent "github.com/ly95/agentruntime"
)

func TestInMemoryStoreConformance(t *testing.T) {
	RunRunStoreConformance(t, func() agent.RunStore { return agent.NewInMemoryRunStore() })
	RunExecutionStoreConformance(t, func() agent.ExecutionStore { return agent.NewInMemoryExecutionStore() })
}
