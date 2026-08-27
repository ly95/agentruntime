package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/ly95/agentruntime"
)

const (
	protocolVersionMetaKey    = "io.modelcontextprotocol/protocolVersion"
	clientInfoMetaKey         = "io.modelcontextprotocol/clientInfo"
	clientCapabilitiesMetaKey = "io.modelcontextprotocol/clientCapabilities"
)

var requestSequence atomic.Uint64

type transportFailure struct {
	canceled bool
	deadline bool
}

func (failure *transportFailure) Error() string {
	return "mcpadapter: transport request failed"
}

func (failure *transportFailure) Is(target error) bool {
	return (failure.canceled && target == context.Canceled) ||
		(failure.deadline && target == context.DeadlineExceeded)
}

type serverRPCError struct {
	code int64
}

func (failure *serverRPCError) Error() string {
	return fmt.Sprintf("mcpadapter: MCP server returned JSON-RPC error code %d", failure.code)
}

func requestParams(fields map[string]any) (json.RawMessage, error) {
	params := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		params[key] = value
	}
	params["_meta"] = map[string]any{
		protocolVersionMetaKey: ProtocolVersion,
		clientInfoMetaKey: map[string]any{
			"name":    "github.com/ly95/agentruntime/mcpadapter",
			"version": agentruntime.Version,
		},
		clientCapabilitiesMetaKey: map[string]any{},
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, errors.New("mcpadapter: encode MCP request parameters")
	}
	return json.RawMessage(raw), nil
}

func nextRequestID() string {
	sequence := requestSequence.Add(1)
	if sequence == 0 {
		sequence = requestSequence.Add(1)
	}
	return "mcpadapter-" + strconv.FormatUint(sequence, 10)
}

func roundTrip(
	ctx context.Context,
	transport Transport,
	bindingID string,
	limits Limits,
	method string,
	name string,
	params json.RawMessage,
	parameterHeaders map[string]string,
	correlation Correlation,
) (map[string]any, error) {
	if ctx == nil {
		return nil, errors.New("mcpadapter: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateBinding(transport, bindingID); err != nil {
		return nil, err
	}

	headers := map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           method,
	}
	if name != "" {
		headers["Mcp-Name"] = encodeHeaderValue(name)
	}
	for key, value := range parameterHeaders {
		for existing := range headers {
			if strings.EqualFold(existing, key) {
				return nil, errors.New("mcpadapter: duplicate MCP metadata header")
			}
		}
		headers[key] = value
	}
	request := Request{
		ID:                nextRequestID(),
		Method:            method,
		Params:            append(json.RawMessage(nil), params...),
		MetadataHeaders:   headers,
		ExpectedBindingID: bindingID,
		MaxResponseBytes:  limits.MaxResponseBytes,
		Correlation:       correlation,
	}
	response, transportErr := transport.RoundTrip(ctx, request)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err := validateBinding(transport, bindingID); err != nil {
		return nil, err
	}
	if transportErr != nil {
		return nil, &transportFailure{
			canceled: errors.Is(transportErr, context.Canceled),
			deadline: errors.Is(transportErr, context.DeadlineExceeded),
		}
	}
	if len(response) == 0 {
		return nil, errors.New("mcpadapter: transport returned an empty response")
	}
	if len(response) > limits.MaxResponseBytes {
		return nil, errors.New("mcpadapter: MCP response exceeds the configured byte limit")
	}
	return parseRPCResponse(response, request.ID)
}

func validateBinding(transport Transport, expected string) error {
	if transport.BindingID() != expected {
		return errors.New("mcpadapter: transport binding changed")
	}
	return nil
}

func parseRPCResponse(raw json.RawMessage, requestID string) (map[string]any, error) {
	value, err := decodeExactJSON(raw)
	if err != nil {
		return nil, errors.New("mcpadapter: MCP response is not unambiguous valid JSON")
	}
	response, ok := value.(map[string]any)
	if !ok || response == nil {
		return nil, errors.New("mcpadapter: MCP response must be a JSON object")
	}
	for key := range response {
		switch key {
		case "jsonrpc", "id", "result", "error":
		default:
			return nil, errors.New("mcpadapter: MCP response contains an unsupported top-level member")
		}
	}
	if version, ok := response["jsonrpc"].(string); !ok || version != "2.0" {
		return nil, errors.New("mcpadapter: MCP response has an invalid JSON-RPC version")
	}
	if id, ok := response["id"].(string); !ok || id != requestID {
		return nil, errors.New("mcpadapter: MCP response ID does not match its request")
	}
	resultValue, hasResult := response["result"]
	errorValue, hasError := response["error"]
	if hasResult == hasError {
		return nil, errors.New("mcpadapter: MCP response must contain exactly one result or error")
	}
	if hasError {
		return nil, parseRPCError(errorValue)
	}
	result, ok := resultValue.(map[string]any)
	if !ok || result == nil {
		return nil, errors.New("mcpadapter: MCP result must be a JSON object")
	}
	return result, nil
}

func parseRPCError(value any) error {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return errors.New("mcpadapter: MCP JSON-RPC error has an invalid shape")
	}
	for key := range object {
		switch key {
		case "code", "message", "data":
		default:
			return errors.New("mcpadapter: MCP JSON-RPC error contains an unsupported member")
		}
	}
	codeValue, ok := object["code"].(json.Number)
	if !ok {
		return errors.New("mcpadapter: MCP JSON-RPC error code must be an integer")
	}
	code, ok := parseJSONInt64(codeValue)
	if !ok {
		return errors.New("mcpadapter: MCP JSON-RPC error code must be an integer")
	}
	if _, ok := object["message"].(string); !ok {
		return errors.New("mcpadapter: MCP JSON-RPC error message must be a string")
	}
	if code <= -32020 && code >= -32099 && code != -32020 && code != -32021 && code != -32022 {
		return errors.New("mcpadapter: MCP JSON-RPC error code is reserved but undefined")
	}
	if err := validateProtocolErrorData(code, object["data"]); err != nil {
		return err
	}
	return &serverRPCError{code: code}
}

func parseJSONInt64(number json.Number) (int64, bool) {
	return parseJSONNumberInt64(number)
}

func validateProtocolErrorData(code int64, value any) error {
	switch code {
	case -32021:
		data, ok := value.(map[string]any)
		if !ok {
			return errors.New("mcpadapter: missing-capability error data is invalid")
		}
		if _, ok := data["requiredCapabilities"].(map[string]any); !ok {
			return errors.New("mcpadapter: missing-capability error data is invalid")
		}
	case -32022:
		data, ok := value.(map[string]any)
		if !ok {
			return errors.New("mcpadapter: unsupported-version error data is invalid")
		}
		if _, ok := data["requested"].(string); !ok {
			return errors.New("mcpadapter: unsupported-version error data is invalid")
		}
		versions, ok := data["supported"].([]any)
		if !ok {
			return errors.New("mcpadapter: unsupported-version error data is invalid")
		}
		for _, version := range versions {
			if _, ok := version.(string); !ok {
				return errors.New("mcpadapter: unsupported-version error data is invalid")
			}
		}
	}
	return nil
}

func requireCompleteResult(result map[string]any) error {
	resultType, ok := result["resultType"].(string)
	if !ok || resultType == "" {
		return errors.New("mcpadapter: MCP resultType is required")
	}
	switch resultType {
	case "complete":
		return nil
	case "input_required":
		return errors.New("mcpadapter: MCP multi-round-trip input is not supported")
	default:
		return errors.New("mcpadapter: MCP resultType is not supported")
	}
}
