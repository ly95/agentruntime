package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxModelTextAttachmentBytes = 200_000

type ModelInputItemType string

const (
	ModelInputUserMessage     ModelInputItemType = "user_message"
	ModelInputAssistantOutput ModelInputItemType = "assistant_output"
	ModelInputToolResult      ModelInputItemType = "tool_result"
)

type ModelInputItem struct {
	Type        ModelInputItemType     `json:"type"`
	Text        string                 `json:"text,omitempty"`
	Attachments []ModelInputAttachment `json:"attachments,omitempty"`
	// ResponseID binds retained assistant output to the model response that
	// produced it. Runtime writes it on every item in a response so compaction
	// can retain bounded cross-turn response identity without provider state.
	ResponseID string              `json:"response_id,omitempty"`
	OutputType ModelOutputItemType `json:"output_type,omitempty"`
	Raw        json.RawMessage     `json:"raw,omitempty"`
	CallID     string              `json:"call_id,omitempty"`
	Output     json.RawMessage     `json:"output,omitempty"`
}

type ModelInputAttachment struct {
	Kind       ModelInputAttachmentKind `json:"kind"`
	ID         string                   `json:"id"`
	Filename   string                   `json:"filename"`
	MIMEType   string                   `json:"mime_type"`
	StorageKey string                   `json:"storage_key,omitempty"`
	ExpiresAt  time.Time                `json:"expires_at,omitzero"`
	// URL is materialized for one model request and must never be persisted in
	// a Session transcript. Durable hosts provide StorageKey and ExpiresAt so a
	// later Run can resolve a fresh URL or explicitly retire unavailable history.
	URL string `json:"-"`
	// CurrentRun is set by Runtime on the current user turn. Persisted history
	// always clears it so unavailable history can be handled without weakening
	// fail-fast semantics for the user's current attachment choice.
	CurrentRun bool   `json:"-"`
	Text       string `json:"text,omitempty"`
}

type ModelInputAttachmentKind string

const (
	ModelInputAttachmentImage ModelInputAttachmentKind = "image"
	ModelInputAttachmentText  ModelInputAttachmentKind = "text"
)

type ImageAttachmentResolver interface {
	// Retryable infrastructure failures must wrap ErrRunInterrupted. Confirmed
	// expiry or deletion must wrap ErrImageAttachmentUnavailable.
	ResolveImageAttachment(ctx context.Context, attachment ModelInputAttachment) (ModelInputAttachment, error)
}

type ImageAttachmentResolverFunc func(context.Context, ModelInputAttachment) (ModelInputAttachment, error)

func (f ImageAttachmentResolverFunc) ResolveImageAttachment(ctx context.Context, attachment ModelInputAttachment) (ModelInputAttachment, error) {
	return f(ctx, attachment)
}

func NormalizeModelInputAttachment(attachment ModelInputAttachment) ModelInputAttachment {
	attachment.Kind = ModelInputAttachmentKind(strings.TrimSpace(string(attachment.Kind)))
	attachment.ID = strings.TrimSpace(attachment.ID)
	attachment.Filename = strings.TrimSpace(attachment.Filename)
	attachment.MIMEType = strings.ToLower(strings.TrimSpace(attachment.MIMEType))
	attachment.StorageKey = strings.TrimSpace(attachment.StorageKey)
	attachment.URL = strings.TrimSpace(attachment.URL)
	return attachment
}

func ValidateModelInputAttachment(attachment ModelInputAttachment) error {
	attachment = NormalizeModelInputAttachment(attachment)
	if attachment.ID == "" || attachment.Filename == "" || attachment.MIMEType == "" {
		return errors.New("agent: attachment id, filename, and MIME type are required")
	}
	if strings.IndexFunc(attachment.Filename, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("agent: attachment filename cannot contain control characters")
	}
	switch attachment.Kind {
	case ModelInputAttachmentImage:
		return ValidateImageAttachment(attachment)
	case ModelInputAttachmentText:
		if attachment.URL != "" {
			return errors.New("agent: text attachment cannot contain a URL")
		}
		if attachment.Text == "" || strings.TrimSpace(attachment.Text) == "" {
			return errors.New("agent: text attachment content is required")
		}
		if len(attachment.Text) > MaxModelTextAttachmentBytes {
			return fmt.Errorf("agent: text attachment exceeds %d bytes", MaxModelTextAttachmentBytes)
		}
		if !utf8.ValidString(attachment.Text) {
			return errors.New("agent: text attachment must be valid UTF-8")
		}
		if strings.ContainsRune(attachment.Text, '\x00') {
			return errors.New("agent: text attachment cannot contain NUL bytes")
		}
		filename := strings.ToLower(attachment.Filename)
		switch attachment.MIMEType {
		case "text/plain":
			if !strings.HasSuffix(filename, ".txt") &&
				!strings.HasSuffix(filename, ".md") &&
				!strings.HasSuffix(filename, ".fountain") &&
				!strings.HasSuffix(filename, ".fdx") {
				return errors.New(
					"agent: text/plain attachments must use a .txt, .md, .fountain, or .fdx filename",
				)
			}
		case "text/markdown", "text/x-markdown":
			if !strings.HasSuffix(filename, ".md") {
				return errors.New("agent: Markdown attachments must use a .md filename")
			}
		default:
			return fmt.Errorf("agent: unsupported text attachment MIME type %q", attachment.MIMEType)
		}
		return nil
	default:
		return fmt.Errorf("agent: unsupported attachment kind %q", attachment.Kind)
	}
}

func ValidateImageAttachment(attachment ModelInputAttachment) error {
	attachment = NormalizeModelInputAttachment(attachment)
	if attachment.ID == "" || attachment.Filename == "" || attachment.MIMEType == "" {
		return errors.New("agent: image attachment id, filename, and MIME type are required")
	}
	if strings.IndexFunc(attachment.Filename, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("agent: image attachment filename cannot contain control characters")
	}
	if attachment.Kind != ModelInputAttachmentImage || attachment.URL == "" || attachment.Text != "" {
		return errors.New("agent: image attachment kind and URL are required and text must be empty")
	}
	switch attachment.MIMEType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
	default:
		return fmt.Errorf("agent: unsupported image attachment MIME type %q", attachment.MIMEType)
	}
	parsed, err := url.Parse(attachment.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("agent: image attachment URL must be an absolute HTTPS URL")
	}
	return nil
}

func RenderTextAttachment(attachment ModelInputAttachment) (string, error) {
	attachment = NormalizeModelInputAttachment(attachment)
	if err := ValidateModelInputAttachment(attachment); err != nil {
		return "", err
	}
	if attachment.Kind != ModelInputAttachmentText {
		return "", errors.New("agent: only text attachments can be rendered as text")
	}
	return fmt.Sprintf(
		"[BEGIN ATTACHMENT filename=%q media_type=%q]\n%s\n[END ATTACHMENT filename=%q]",
		attachment.Filename, attachment.MIMEType, attachment.Text, attachment.Filename,
	), nil
}

type ModelOutputItemType string

const (
	ModelOutputMessage      ModelOutputItemType = "message"
	ModelOutputReasoning    ModelOutputItemType = "reasoning"
	ModelOutputFunctionCall ModelOutputItemType = "function_call"
)

type ModelOutputItem struct {
	ID   string              `json:"id,omitempty"`
	Type ModelOutputItemType `json:"type"`
	Text string              `json:"text,omitempty"`
	Call *ToolCall           `json:"call,omitempty"`
	// Raw is the replayable provider envelope. It must be an exact JSON object
	// whose "type" equals Type; function-call envelopes must also identify the
	// same call ID, operation name, and semantic arguments as Call. Message and
	// reasoning envelopes must retain their required replay field shapes.
	Raw json.RawMessage `json:"raw,omitempty"`
}

type ModelRequest struct {
	Instructions string           `json:"instructions"`
	Input        []ModelInputItem `json:"input"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
	// ModelCallID is assigned by Runtime immediately before Complete. Model
	// decorators may use it for idempotent metering or provider attribution;
	// transports must not serialize it into the provider request.
	ModelCallID string `json:"-"`
	// DisableReasoning asks adapters with an explicit thinking mode to turn it
	// off for this call. Runtime uses it only for the single corrective retry
	// after a reasoning-only response.
	DisableReasoning bool `json:"disable_reasoning,omitempty"`
	// ToolSetID is reserved for Runtime's content hash. Direct model callers
	// should leave it empty; OpenAIModel rejects IDs that do not match Tools.
	ToolSetID  string          `json:"tool_set_id,omitempty"`
	StreamSink ModelStreamSink `json:"-"`
}

type ModelStreamEventType string

const (
	ModelStreamResponseStarted       ModelStreamEventType = "response_started"
	ModelStreamItemAdded             ModelStreamEventType = "item_added"
	ModelStreamReasoningSummaryDelta ModelStreamEventType = "reasoning_summary_delta"
	ModelStreamCommentaryDelta       ModelStreamEventType = "commentary_delta"
	ModelStreamTextDelta             ModelStreamEventType = "text_delta"
	ModelStreamRefusalDelta          ModelStreamEventType = "refusal_delta"
	ModelStreamToolArgumentsDelta    ModelStreamEventType = "tool_arguments_delta"
	ModelStreamToolArgumentsDone     ModelStreamEventType = "tool_arguments_done"
	ModelStreamItemDone              ModelStreamEventType = "item_done"
	ModelStreamResponseDone          ModelStreamEventType = "response_done"
	ModelStreamError                 ModelStreamEventType = "error"
)

type ModelStreamEvent struct {
	Type           ModelStreamEventType `json:"type"`
	ProviderType   string               `json:"provider_type,omitempty"`
	ModelCallID    string               `json:"model_call_id,omitempty"`
	SequenceNumber *int64               `json:"sequence_number,omitempty"`
	ItemID         string               `json:"item_id,omitempty"`
	OutputIndex    *int64               `json:"output_index,omitempty"`
	ResponseID     string               `json:"response_id,omitempty"`
	CallID         string               `json:"call_id,omitempty"`
	Name           string               `json:"name,omitempty"`
	Phase          string               `json:"phase,omitempty"`
	Delta          string               `json:"delta,omitempty"`
	Arguments      string               `json:"arguments,omitempty"`
	ErrorCode      string               `json:"error_code,omitempty"`
	ErrorMessage   string               `json:"-"`
	RawJSON        string               `json:"-"`
}

// MarshalJSON defines the public event boundary. Provider raw payloads and
// incremental tool arguments remain available to trusted in-process sinks but
// are not exposed by adapters that directly encode Event as JSON.
func (e ModelStreamEvent) MarshalJSON() ([]byte, error) {
	if err := validateUTF8Boundary("model stream event", e); err != nil {
		return nil, err
	}
	type publicModelStreamEvent ModelStreamEvent
	public := publicModelStreamEvent(e)
	public.RawJSON = ""
	public.ErrorMessage = ""
	public.ErrorCode = safePublicErrorCode(public.ErrorCode)
	if e.Type == ModelStreamToolArgumentsDelta || e.Type == ModelStreamToolArgumentsDone {
		public.Delta = ""
		public.Arguments = ""
	}
	return json.Marshal(public)
}

func safePublicErrorCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	if len(code) > 64 {
		return "provider_error"
	}
	for _, char := range code {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return "provider_error"
	}
	return code
}

// ModelStreamSink observes ordered transport chunks and completion boundaries.
// Callbacks run synchronously and provide backpressure. Implementations must not
// mutate event data after invoking the callback. Invalid UTF-8 in any event field
// fails the model turn before observer delivery. A completed ModelResponse remains
// authoritative: tool argument chunks are never executable input.
type ModelStreamSink func(ModelStreamEvent)

type ModelResponse struct {
	ID           string            `json:"id"`
	OutputText   string            `json:"output_text,omitempty"`
	Refusal      string            `json:"refusal,omitempty"`
	Items        []ModelOutputItem `json:"items"`
	FinishReason string            `json:"finish_reason,omitempty"`
	HadReasoning bool              `json:"had_reasoning,omitempty"`
	Usage        Usage             `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type ToolDefinition struct {
	Name          string          `json:"name"`
	PreviousNames []string        `json:"-"`
	Description   string          `json:"description,omitempty"`
	InputSchema   json.RawMessage `json:"input_schema"`
}

// Model executes one native model turn. Input is the complete local transcript;
// implementations must not depend on provider-side response storage. Requests
// and their slices are immutable for the duration of Complete.
type Model interface {
	Complete(ctx context.Context, req ModelRequest) (*ModelResponse, error)
}

func toolDefinitionsID(tools []ToolDefinition) string {
	digest := sha256.New()
	writeHashField(digest, []byte("agentruntime.tool-definitions.v2"))
	writeHashUint64(digest, uint64(len(tools)))
	for index, tool := range tools {
		writeHashField(digest, []byte("tool"))
		writeHashUint64(digest, uint64(index))
		writeHashField(digest, []byte("name"))
		writeHashField(digest, []byte(tool.Name))
		writeHashField(digest, []byte("previous_names"))
		writeHashUint64(digest, uint64(len(tool.PreviousNames)))
		for _, previousName := range tool.PreviousNames {
			writeHashField(digest, []byte(previousName))
		}
		writeHashField(digest, []byte("description"))
		writeHashField(digest, []byte(tool.Description))
		writeHashField(digest, []byte("input_schema"))
		writeHashField(digest, operationSchemaIdentity(tool.InputSchema, nil))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeHashUint64(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = digest.Write(encoded[:])
}

func writeHashField(digest hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}
