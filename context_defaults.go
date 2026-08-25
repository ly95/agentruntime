package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ConservativeTokenCounter counts one token per UTF-8 byte. For byte-fallback
// tokenizers this is a conservative upper bound, not a billing count. Hosts
// should replace it with a model-specific tokenizer when utilization matters.
type ConservativeTokenCounter struct{}

func (ConservativeTokenCounter) CountModelRequest(ctx context.Context, request ModelRequest) (int, error) {
	if cause := context.Cause(ctx); cause != nil {
		return 0, cause
	}
	request = cloneModelRequest(request)
	request.StreamSink = nil
	raw, err := json.Marshal(request)
	if err != nil {
		return 0, fmt.Errorf("agent: count model request: %w", err)
	}
	return len(raw), nil
}

func (ConservativeTokenCounter) CountText(ctx context.Context, text string) (int, error) {
	if cause := context.Cause(ctx); cause != nil {
		return 0, cause
	}
	if !utf8.ValidString(text) {
		return 0, errors.New("agent: count text: invalid UTF-8")
	}
	return len(text), nil
}

// ExtractiveContextCompactor builds a deterministic, untrusted checkpoint from
// prior user messages. It never calls a model or executes content. The output is
// intentionally modest; semantic production summaries should use a host-owned
// compactor and preserve the same checkpoint trust boundary.
type ExtractiveContextCompactor struct{}

func (ExtractiveContextCompactor) Compact(ctx context.Context, request ContextCompactionRequest) (ContextSummary, error) {
	if cause := context.Cause(ctx); cause != nil {
		return ContextSummary{}, cause
	}
	if request.MaxCheckpointTokens <= 0 || len(request.Items) == 0 {
		return ContextSummary{}, fmt.Errorf("%w: compaction items and checkpoint budget are required", ErrContextCompactionFailed)
	}
	summary := ContextSummary{Summary: fmt.Sprintf("Compacted %d historical transcript items.", len(request.Items))}
	if request.Checkpoint != nil && strings.TrimSpace(request.Checkpoint.Summary.Summary) != "" {
		summary.Facts = append(summary.Facts, "Prior checkpoint: "+compactContextText(request.Checkpoint.Summary.Summary, 256))
	}
	for _, item := range request.Items {
		if item.Type != ModelInputUserMessage || strings.TrimSpace(item.Text) == "" {
			continue
		}
		fact := "Earlier user input: " + compactContextText(item.Text, 256)
		candidate := cloneContextSummary(summary)
		candidate.Facts = append(candidate.Facts, fact)
		if contextSummaryBytes(candidate) > request.MaxCheckpointTokens {
			break
		}
		summary = candidate
	}
	if contextSummaryBytes(summary) > request.MaxCheckpointTokens {
		return ContextSummary{}, fmt.Errorf("%w: checkpoint budget %d is too small for the minimum summary", ErrContextCompactionFailed, request.MaxCheckpointTokens)
	}
	return summary, nil
}

func compactContextText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return strings.TrimSpace(text) + "…"
}

func contextSummaryBytes(summary ContextSummary) int {
	raw, err := json.Marshal(summary)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return len(raw)
}

// NewDefaultContextWindowConfig returns a safe, conservative reference
// configuration. MaxContextTokens is interpreted in ConservativeTokenCounter
// units; callers using a model tokenizer should replace TokenCounter and tune
// thresholds from observed request sizes.
func NewDefaultContextWindowConfig(maxContextTokens int) (ContextWindowConfig, error) {
	if maxContextTokens < 256 {
		return ContextWindowConfig{}, errors.New("agent: default context window requires at least 256 tokens")
	}
	reserved := maxContextTokens / 8
	inputBudget := maxContextTokens - reserved
	trigger := inputBudget * 3 / 4
	target := trigger / 2
	checkpoint := target / 3
	config := ContextWindowConfig{
		MaxContextTokens: maxContextTokens, ReservedOutputTokens: reserved,
		CompactionTriggerTokens: trigger, CompactionTargetTokens: target,
		MaxCheckpointTokens: checkpoint, PreserveRecentTurns: 4,
		TokenCounter: ConservativeTokenCounter{}, ContextCompactor: ExtractiveContextCompactor{},
	}
	if err := validateContextWindowConfig(&config); err != nil {
		return ContextWindowConfig{}, err
	}
	return config, nil
}
