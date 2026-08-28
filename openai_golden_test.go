package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestOpenAIStreamGoldenCorpus(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantID     string
		wantOutput string
		wantCall   string
		wantError  error
	}{
		{name: "message", file: "testdata/openai/message.sse", wantID: "resp_golden_message", wantOutput: "golden"},
		{name: "function call", file: "testdata/openai/function_call.sse", wantID: "resp_golden_call", wantCall: "call_golden"},
		{name: "unknown authority field", file: "testdata/openai/unknown_authority_field.sse", wantError: ErrInvalidModelOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := os.ReadFile(test.file)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = writer.Write(payload)
			}))
			defer server.Close()
			model, err := NewOpenAIModel(newOpenAITestClient(server.URL+"/v1"), openAITestModelConfig("test-model"))
			if err != nil {
				t.Fatal(err)
			}
			response, err := model.Complete(context.Background(), ModelRequest{
				Instructions: "Golden corpus.", Input: []ModelInputItem{{Type: ModelInputUserMessage, Text: "test"}},
			})
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.ID != test.wantID || response.OutputText != test.wantOutput {
				t.Fatalf("response=%+v", response)
			}
			if test.wantCall != "" {
				if len(response.Items) != 1 || response.Items[0].Call == nil || response.Items[0].Call.ID != test.wantCall {
					t.Fatalf("response=%+v, want call %s", response, test.wantCall)
				}
			}
		})
	}
}

func FuzzPublicJSONBoundaries(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"value":1}`), []byte(`{"value":1,"value":2}`), []byte(`null`), []byte{0xff},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("public JSON boundary panicked for %q: %v", data, recovered)
			}
		}()
		type arguments struct {
			Value int64 `json:"value"`
		}
		_, _ = DecodeArguments[arguments](jsonRawCopy(data))
		_, _ = SanitizeEvent(Event{Type: EventOperationFailed, Data: jsonRawCopy(data), Error: string(data)}).MarshalJSON()
		_ = fmt.Sprint(ClassifyRunOutcome(nil, errors.New(string(data))))
	})
}

func jsonRawCopy(data []byte) json.RawMessage { return append(json.RawMessage(nil), data...) }
