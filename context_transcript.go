package agentruntime

import (
	"context"
	"errors"
	"fmt"
)

func materializeModelInputAttachments(ctx context.Context, resolver ImageAttachmentResolver, transcript []ModelInputItem) ([]ModelInputItem, error) {
	hasImages := false
	for _, item := range transcript {
		for _, attachment := range item.Attachments {
			if attachment.Kind == ModelInputAttachmentImage {
				hasImages = true
				break
			}
		}
		if hasImages {
			break
		}
	}
	if !hasImages {
		return transcript, nil
	}

	out := cloneModelInputItems(transcript)
	for itemIndex := range out {
		if len(out[itemIndex].Attachments) == 0 {
			continue
		}
		materializedAttachments := make([]ModelInputAttachment, 0, len(out[itemIndex].Attachments))
		for attachmentIndex, attachment := range out[itemIndex].Attachments {
			attachment = NormalizeModelInputAttachment(attachment)
			if attachment.Kind != ModelInputAttachmentImage {
				materializedAttachments = append(materializedAttachments, attachment)
				continue
			}

			if isNilDependency(resolver) {
				if !attachment.CurrentRun || attachment.URL == "" {
					return nil, fmt.Errorf("agent: input item %d historical image attachment %d requires a resolver", itemIndex, attachmentIndex)
				}
				if err := ValidateImageAttachment(attachment); err != nil {
					return nil, fmt.Errorf("agent: input item %d image attachment %d: %w", itemIndex, attachmentIndex, err)
				}
				materializedAttachments = append(materializedAttachments, attachment)
				continue
			}

			materialized, err := resolver.ResolveImageAttachment(ctx, attachment)
			if err != nil {
				if errors.Is(err, ErrImageAttachmentUnavailable) && !attachment.CurrentRun {
					// FALLBACK: justified because confirmed-expired or deleted historical
					// image bytes cannot be reconstructed. An explicit ordered text part
					// keeps the Session usable without claiming the model can see them.
					materializedAttachments = append(materializedAttachments, unavailableImageTombstone(attachment))
					continue
				}
				return nil, fmt.Errorf("agent: resolve input item %d image attachment %d: %w", itemIndex, attachmentIndex, err)
			}
			materialized = NormalizeModelInputAttachment(materialized)
			if materialized.ID != attachment.ID || materialized.Filename != attachment.Filename || materialized.MIMEType != attachment.MIMEType {
				return nil, fmt.Errorf("agent: resolver changed input item %d image attachment %d identity", itemIndex, attachmentIndex)
			}
			if attachment.StorageKey != "" && materialized.StorageKey != attachment.StorageKey {
				return nil, fmt.Errorf("agent: resolver changed input item %d image attachment %d storage key", itemIndex, attachmentIndex)
			}
			if !attachment.ExpiresAt.IsZero() && !materialized.ExpiresAt.Equal(attachment.ExpiresAt) {
				return nil, fmt.Errorf("agent: resolver changed input item %d image attachment %d expiry", itemIndex, attachmentIndex)
			}
			if materialized.StorageKey == "" || materialized.ExpiresAt.IsZero() {
				return nil, fmt.Errorf("agent: resolver omitted stable metadata for input item %d image attachment %d", itemIndex, attachmentIndex)
			}
			materialized.CurrentRun = attachment.CurrentRun
			if err := ValidateImageAttachment(materialized); err != nil {
				return nil, fmt.Errorf("agent: resolved input item %d image attachment %d: %w", itemIndex, attachmentIndex, err)
			}
			materializedAttachments = append(materializedAttachments, materialized)
		}
		out[itemIndex].Attachments = materializedAttachments
	}
	return out, nil
}

func unavailableImageTombstone(attachment ModelInputAttachment) ModelInputAttachment {
	return ModelInputAttachment{
		Kind: ModelInputAttachmentText, ID: attachment.ID,
		Filename: "historical-image-unavailable.txt", MIMEType: "text/plain",
		Text: fmt.Sprintf(
			"Historical image attachment unavailable: filename=%q attachment_id=%q. The image expired or was removed; do not claim to have inspected its contents.",
			attachment.Filename, attachment.ID,
		),
	}
}

func contextCompactionPrefixEnd(transcript []ModelInputItem, preserveRecentTurns int) (int, error) {
	if err := validateContextTranscriptToolSequences(transcript); err != nil {
		return 0, err
	}
	userStarts := make([]int, 0)
	for i, item := range transcript {
		if item.Type == ModelInputUserMessage {
			userStarts = append(userStarts, i)
		}
	}
	if len(userStarts) <= preserveRecentTurns {
		return 0, nil
	}
	return userStarts[len(userStarts)-preserveRecentTurns], nil
}

func validateContextTranscriptToolSequences(transcript []ModelInputItem) error {
	if len(transcript) == 0 {
		return errors.New("transcript is empty")
	}
	if transcript[0].Type != ModelInputUserMessage {
		return fmt.Errorf("transcript starts with %q instead of a user message", transcript[0].Type)
	}
	pendingCallIDs := make(map[string]struct{})
	seenCallIDs := make(map[string]struct{})
	for i, item := range transcript {
		switch item.Type {
		case ModelInputUserMessage:
			if i > 0 && len(pendingCallIDs) != 0 {
				return fmt.Errorf(
					"tool calls %v are missing results before user message at transcript index %d",
					sortedCallIDs(pendingCallIDs), i,
				)
			}
		case ModelInputAssistantOutput:
			if item.OutputType == ModelOutputFunctionCall {
				if item.CallID == "" {
					return fmt.Errorf("function call at transcript index %d has an empty call ID", i)
				}
				if _, exists := seenCallIDs[item.CallID]; exists {
					return fmt.Errorf("function call at transcript index %d repeats call ID %q", i, item.CallID)
				}
				seenCallIDs[item.CallID] = struct{}{}
				pendingCallIDs[item.CallID] = struct{}{}
			}
		case ModelInputToolResult:
			if item.CallID == "" {
				return fmt.Errorf("tool result at transcript index %d has an empty call ID", i)
			}
			if _, exists := pendingCallIDs[item.CallID]; !exists {
				return fmt.Errorf(
					"tool result at transcript index %d references call ID %q, pending call IDs are %v",
					i, item.CallID, sortedCallIDs(pendingCallIDs),
				)
			}
			delete(pendingCallIDs, item.CallID)
		default:
			return fmt.Errorf("unsupported transcript item type %q at index %d", item.Type, i)
		}
	}
	if len(pendingCallIDs) != 0 {
		return fmt.Errorf("tool calls %v are missing results at the end of the transcript", sortedCallIDs(pendingCallIDs))
	}
	return nil
}

func (r *Runtime) failContextCompaction(run *RunRecord, inputTokens, compactedItems int, cause error) error {
	r.emit(Event{
		Type: EventContextCompactionFailed, RunID: run.ID, SessionID: run.SessionID,
		InputTokens: inputTokens, CompactedItems: compactedItems,
		ErrorCode: errorCode(cause), Error: cause.Error(),
	})
	return cause
}
