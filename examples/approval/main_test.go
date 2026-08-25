package main

import (
	"testing"

	"github.com/ly95/agentruntime"
)

func TestApprovalScenario(t *testing.T) {
	status, err := runApprovalScenario(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status != agentruntime.RunStatusCompleted {
		t.Fatalf("status=%q", status)
	}
}

func TestNotAppliedScenario(t *testing.T) {
	status, err := runNotAppliedScenario(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status != agentruntime.OperationExecutionRetryable {
		t.Fatalf("status=%q", status)
	}
}

func TestReconciliationScenario(t *testing.T) {
	status, err := runReconciliationScenario(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status != agentruntime.OperationExecutionCompleted {
		t.Fatalf("status=%q", status)
	}
}
