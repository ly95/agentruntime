package agentruntime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

func appendModelOutputItems(transcript []ModelInputItem, items []ModelOutputItem) ([]ModelInputItem, error) {
	for i, item := range items {
		if len(item.Raw) == 0 || !json.Valid(item.Raw) {
			return nil, fmt.Errorf("%w: output item %d raw payload is required for transcript replay", ErrInvalidModelOutput, i)
		}
		inputItem := ModelInputItem{
			Type:       ModelInputAssistantOutput,
			OutputType: item.Type,
			Raw:        append(json.RawMessage(nil), item.Raw...),
		}
		if item.Type == ModelOutputFunctionCall && item.Call != nil {
			inputItem.CallID = strings.TrimSpace(item.Call.ID)
		}
		transcript = append(transcript, inputItem)
	}
	return transcript, nil
}

func cloneModelInputItems(items []ModelInputItem) []ModelInputItem {
	out := make([]ModelInputItem, len(items))
	copy(out, items)
	for i := range out {
		out[i].Raw = append(json.RawMessage(nil), items[i].Raw...)
		out[i].Output = append(json.RawMessage(nil), items[i].Output...)
		out[i].Attachments = cloneModelInputAttachments(items[i].Attachments)
	}
	return out
}

func clonePersistentModelInputItems(items []ModelInputItem) []ModelInputItem {
	out := cloneModelInputItems(items)
	for itemIndex := range out {
		for attachmentIndex := range out[itemIndex].Attachments {
			if out[itemIndex].Attachments[attachmentIndex].Kind == ModelInputAttachmentImage {
				out[itemIndex].Attachments[attachmentIndex].URL = ""
			}
			out[itemIndex].Attachments[attachmentIndex].CurrentRun = false
		}
	}
	return out
}

func cloneModelInputAttachments(attachments []ModelInputAttachment) []ModelInputAttachment {
	return append([]ModelInputAttachment(nil), attachments...)
}

func cloneModelOutputItems(items []ModelOutputItem) []ModelOutputItem {
	out := make([]ModelOutputItem, len(items))
	for i := range items {
		out[i] = items[i]
		out[i].Raw = append(json.RawMessage(nil), items[i].Raw...)
		if items[i].Call != nil {
			call := *items[i].Call
			call.Input = append(json.RawMessage(nil), items[i].Call.Input...)
			out[i].Call = &call
		}
	}
	return out
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Errorf("agent runtime: generate random ID: %w", err))
	}
	return hex.EncodeToString(value[:])
}
