package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ReconciliationEvidenceKind string

const (
	ReconciliationEvidenceNotStarted ReconciliationEvidenceKind = "executor_not_started"
	ReconciliationEvidenceCommitted  ReconciliationEvidenceKind = "executor_committed"
)

// BuildAbandonmentEvidence creates the minimum common evidence envelope for an
// OperationReconciliationAbandon decision. Proof remains host-defined exact
// JSON and should identify a durable, independently queryable source.
func BuildAbandonmentEvidence(source string, observedAt time.Time, proof json.RawMessage) (json.RawMessage, error) {
	return buildReconciliationEvidence(ReconciliationEvidenceNotStarted, source, observedAt, proof)
}

// BuildCompletionEvidence creates the minimum common evidence envelope for an
// evidence-bearing OperationReconciliationComplete decision.
func BuildCompletionEvidence(source string, observedAt time.Time, proof json.RawMessage) (json.RawMessage, error) {
	return buildReconciliationEvidence(ReconciliationEvidenceCommitted, source, observedAt, proof)
}

func buildReconciliationEvidence(kind ReconciliationEvidenceKind, source string, observedAt time.Time, proof json.RawMessage) (json.RawMessage, error) {
	source = strings.TrimSpace(source)
	if source == "" || strings.ContainsRune(source, '\x00') {
		return nil, fmt.Errorf("%w: reconciliation evidence source is required", ErrInvalidReconciliation)
	}
	if observedAt.IsZero() {
		return nil, fmt.Errorf("%w: reconciliation evidence timestamp is required", ErrInvalidReconciliation)
	}
	value, err := decodeExactJSON(proof)
	if err != nil || value == nil {
		if err == nil {
			err = errors.New("null is not evidence")
		}
		return nil, fmt.Errorf("%w: reconciliation proof must be non-null exact JSON: %v", ErrInvalidReconciliation, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("%w: reconciliation proof must be a JSON object", ErrInvalidReconciliation)
	}
	raw, err := json.Marshal(struct {
		Kind       ReconciliationEvidenceKind `json:"kind"`
		Source     string                     `json:"source"`
		ObservedAt time.Time                  `json:"observed_at"`
		Proof      json.RawMessage            `json:"proof"`
	}{Kind: kind, Source: source, ObservedAt: observedAt.UTC(), Proof: append(json.RawMessage(nil), proof...)})
	if err != nil {
		return nil, fmt.Errorf("agent: marshal reconciliation evidence: %w", err)
	}
	return raw, nil
}
