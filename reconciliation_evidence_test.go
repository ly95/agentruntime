package agentruntime

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestReconciliationEvidenceTemplates(t *testing.T) {
	now := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	for _, build := range []func(string, time.Time, json.RawMessage) (json.RawMessage, error){
		BuildAbandonmentEvidence, BuildCompletionEvidence,
	} {
		raw, err := build("authoritative-executor-log", now, json.RawMessage(`{"record_id":"record-1"}`))
		if err != nil || !json.Valid(raw) {
			t.Fatalf("evidence=%s err=%v", raw, err)
		}
		if _, err := build("source", now, json.RawMessage(`{"duplicate":1,"duplicate":2}`)); !errors.Is(err, ErrInvalidReconciliation) {
			t.Fatalf("duplicate-key error=%v", err)
		}
	}
}
