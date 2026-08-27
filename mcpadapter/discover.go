package mcpadapter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ly95/agentruntime"
)

const (
	defaultMaxPages            = 16
	defaultMaxTools            = 256
	defaultMaxResponseBytes    = 1 << 20
	defaultMaxSchemaBytes      = 256 << 10
	defaultMaxTotalSchemaBytes = 2 << 20
	defaultMaxSchemaDepth      = 64
	defaultMaxSchemaNodes      = 10_000
)

type discoveredTool struct {
	name              string
	inputSchema       json.RawMessage
	inputDocument     map[string]any
	outputSchema      json.RawMessage
	outputDocument    map[string]any
	readOnlyHintFalse bool
}

type preparedMapping struct {
	mapping          Mapping
	tool             discoveredTool
	headers          []headerBinding
	normalizer       *strictNullOmissionNormalizer
	maxResponseBytes int
	toolDigest       string
}

// Discover calls server/discover and bounded tools/list pagination exactly once
// per logical RPC, then freezes the selected host mappings into a Snapshot.
func Discover(ctx context.Context, config Config) (*Snapshot, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	params, err := requestParams(nil)
	if err != nil {
		return nil, err
	}
	result, err := roundTrip(
		ctx,
		normalized.Transport,
		normalized.BindingID,
		normalized.Limits,
		"server/discover",
		"",
		params,
		nil,
		Correlation{},
	)
	if err != nil {
		return nil, err
	}
	if err := requireCompleteResult(result); err != nil {
		return nil, err
	}
	if err := validateDiscoveryResult(result); err != nil {
		return nil, err
	}
	tools, err := discoverTools(ctx, normalized)
	if err != nil {
		return nil, err
	}
	snapshot, err := buildSnapshot(normalized, tools)
	if err != nil {
		return nil, err
	}
	if normalized.ExpectedSnapshotID != "" && snapshot.ID() != normalized.ExpectedSnapshotID {
		return nil, errors.New("mcpadapter: discovered snapshot does not match the host pin")
	}
	return snapshot, nil
}

func normalizeConfig(config Config) (Config, error) {
	if isNilTransport(config.Transport) {
		return Config{}, errors.New("mcpadapter: transport is required")
	}
	if err := validateStableText("binding ID", config.BindingID, false); err != nil {
		return Config{}, err
	}
	if config.Transport.BindingID() != config.BindingID {
		return Config{}, errors.New("mcpadapter: configured binding does not match the transport")
	}
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return Config{}, err
	}
	if config.ExpectedSnapshotID != "" && !validSnapshotID(config.ExpectedSnapshotID) {
		return Config{}, errors.New("mcpadapter: expected snapshot ID is invalid")
	}
	if len(config.Mappings) == 0 {
		return Config{}, errors.New("mcpadapter: at least one host mapping is required")
	}
	mappings := make([]Mapping, len(config.Mappings))
	remoteNames := make(map[string]struct{}, len(config.Mappings))
	operationNames := make(map[string]struct{}, len(config.Mappings))
	for index, source := range config.Mappings {
		mapping, err := normalizeMapping(source)
		if err != nil {
			return Config{}, err
		}
		if _, duplicate := remoteNames[mapping.RemoteName]; duplicate {
			return Config{}, errors.New("mcpadapter: remote tool is mapped more than once")
		}
		if _, duplicate := operationNames[mapping.OperationName]; duplicate {
			return Config{}, errors.New("mcpadapter: operation name is mapped more than once")
		}
		remoteNames[mapping.RemoteName] = struct{}{}
		operationNames[mapping.OperationName] = struct{}{}
		mappings[index] = mapping
	}
	return Config{
		Transport:          config.Transport,
		BindingID:          config.BindingID,
		ExpectedSnapshotID: config.ExpectedSnapshotID,
		Mappings:           mappings,
		Limits:             limits,
	}, nil
}

func validSnapshotID(value string) bool {
	const prefix = "mcp_snapshot_"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

func isNilTransport(transport Transport) bool {
	if transport == nil {
		return true
	}
	value := reflect.ValueOf(transport)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func normalizeLimits(limits Limits) (Limits, error) {
	fields := []struct {
		name         string
		value        *int
		defaultValue int
	}{
		{name: "MaxPages", value: &limits.MaxPages, defaultValue: defaultMaxPages},
		{name: "MaxTools", value: &limits.MaxTools, defaultValue: defaultMaxTools},
		{name: "MaxResponseBytes", value: &limits.MaxResponseBytes, defaultValue: defaultMaxResponseBytes},
		{name: "MaxSchemaBytes", value: &limits.MaxSchemaBytes, defaultValue: defaultMaxSchemaBytes},
		{name: "MaxTotalSchemaBytes", value: &limits.MaxTotalSchemaBytes, defaultValue: defaultMaxTotalSchemaBytes},
		{name: "MaxSchemaDepth", value: &limits.MaxSchemaDepth, defaultValue: defaultMaxSchemaDepth},
		{name: "MaxSchemaNodes", value: &limits.MaxSchemaNodes, defaultValue: defaultMaxSchemaNodes},
	}
	for _, field := range fields {
		if *field.value < 0 {
			return Limits{}, errors.New("mcpadapter: discovery limits cannot be negative")
		}
		if *field.value == 0 {
			*field.value = field.defaultValue
		}
	}
	return limits, nil
}

func normalizeMapping(mapping Mapping) (Mapping, error) {
	if err := validateStableText("remote tool name", mapping.RemoteName, false); err != nil {
		return Mapping{}, err
	}
	if err := validateStableText("operation name", mapping.OperationName, false); err != nil {
		return Mapping{}, err
	}
	mapping.Description = strings.TrimSpace(mapping.Description)
	if err := validateStableText("host description", mapping.Description, true); err != nil {
		return Mapping{}, err
	}
	mapping.HostVersion = strings.TrimSpace(mapping.HostVersion)
	if err := validateStableText("host version", mapping.HostVersion, false); err != nil {
		return Mapping{}, err
	}
	if !mapping.ReadOnly {
		return Mapping{}, errors.New("mcpadapter: every mapping requires an explicit read-only host attestation")
	}
	capabilities := make([]string, 0, len(mapping.Capabilities))
	seen := make(map[string]struct{}, len(mapping.Capabilities))
	for _, source := range mapping.Capabilities {
		capability := strings.TrimSpace(source)
		if err := validateStableText("capability", capability, false); err != nil {
			return Mapping{}, err
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	mapping.Capabilities = capabilities
	return mapping, nil
}

func validateStableText(name, value string, allowLineBreaks bool) error {
	if value == "" || !utf8.ValidString(value) {
		return errors.New("mcpadapter: " + name + " is required and must be valid UTF-8")
	}
	if strings.TrimSpace(value) != value {
		return errors.New("mcpadapter: " + name + " cannot have surrounding whitespace")
	}
	for _, character := range value {
		if character >= 0x20 && character != 0x7f {
			continue
		}
		if allowLineBreaks && (character == '\n' || character == '\r' || character == '\t') {
			continue
		}
		return errors.New("mcpadapter: " + name + " contains a control character")
	}
	return nil
}

func validateDiscoveryResult(result map[string]any) error {
	if _, err := validateCacheableResult(result); err != nil {
		return err
	}
	versions, ok := result["supportedVersions"].([]any)
	if !ok {
		return errors.New("mcpadapter: discovery result must declare supportedVersions")
	}
	supported := false
	for _, value := range versions {
		version, ok := value.(string)
		if !ok {
			return errors.New("mcpadapter: discovery supportedVersions must contain only strings")
		}
		if version == ProtocolVersion {
			supported = true
		}
	}
	if !supported {
		return errors.New("mcpadapter: server does not support the pinned protocol version")
	}
	capabilities, ok := result["capabilities"].(map[string]any)
	if !ok || capabilities == nil {
		return errors.New("mcpadapter: discovery result must declare capabilities")
	}
	tools, ok := capabilities["tools"].(map[string]any)
	if !ok || tools == nil {
		return errors.New("mcpadapter: server does not declare the tools capability")
	}
	if value, exists := tools["listChanged"]; exists {
		if _, ok := value.(bool); !ok {
			return errors.New("mcpadapter: tools listChanged capability must be a boolean")
		}
	}
	return nil
}

func validateCacheableResult(result map[string]any) (string, error) {
	ttl, ok := result["ttlMs"].(json.Number)
	if !ok {
		return "", errors.New("mcpadapter: cacheable MCP result must declare ttlMs")
	}
	integer, ok := parseIntegralJSONNumber(ttl)
	if !ok || integer.Sign() < 0 {
		return "", errors.New("mcpadapter: cacheable MCP result ttlMs must be a non-negative integer")
	}
	scope, ok := result["cacheScope"].(string)
	if !ok || (scope != "public" && scope != "private") {
		return "", errors.New("mcpadapter: cacheable MCP result cacheScope is invalid")
	}
	return scope, nil
}

func discoverTools(ctx context.Context, config Config) ([]discoveredTool, error) {
	var cursor *string
	seenCursors := make(map[string]struct{})
	seenTools := make(map[string]struct{})
	tools := make([]discoveredTool, 0)
	totalSchemaBytes := 0
	cacheScope := ""
	for page := 1; page <= config.Limits.MaxPages; page++ {
		fields := make(map[string]any)
		if cursor != nil {
			fields["cursor"] = *cursor
		}
		params, err := requestParams(fields)
		if err != nil {
			return nil, err
		}
		result, err := roundTrip(
			ctx,
			config.Transport,
			config.BindingID,
			config.Limits,
			"tools/list",
			"",
			params,
			nil,
			Correlation{},
		)
		if err != nil {
			return nil, err
		}
		if err := requireCompleteResult(result); err != nil {
			return nil, err
		}
		values, ok := result["tools"].([]any)
		if !ok {
			return nil, errors.New("mcpadapter: tools/list result must contain a tools array")
		}
		if len(tools)+len(values) > config.Limits.MaxTools {
			return nil, errors.New("mcpadapter: tool discovery exceeds the configured tool limit")
		}
		pageTools, nextCursor, pageScope, err := parseToolPage(result, config.Limits, &totalSchemaBytes)
		if err != nil {
			return nil, err
		}
		if cacheScope == "" {
			cacheScope = pageScope
		} else if cacheScope != pageScope {
			return nil, errors.New("mcpadapter: tools/list cacheScope changed across pages")
		}
		for _, tool := range pageTools {
			if _, duplicate := seenTools[tool.name]; duplicate {
				return nil, errors.New("mcpadapter: tool discovery contains a duplicate name")
			}
			seenTools[tool.name] = struct{}{}
			tools = append(tools, tool)
		}
		if nextCursor == nil {
			return tools, nil
		}
		if _, cycle := seenCursors[*nextCursor]; cycle {
			return nil, errors.New("mcpadapter: tool pagination cursor cycle detected")
		}
		seenCursors[*nextCursor] = struct{}{}
		if page == config.Limits.MaxPages {
			return nil, errors.New("mcpadapter: tool discovery exceeds the configured page limit")
		}
		value := *nextCursor
		cursor = &value
	}
	return nil, errors.New("mcpadapter: tool discovery exceeds the configured page limit")
}

func parseToolPage(result map[string]any, limits Limits, totalSchemaBytes *int) ([]discoveredTool, *string, string, error) {
	cacheScope, err := validateCacheableResult(result)
	if err != nil {
		return nil, nil, "", err
	}
	values, ok := result["tools"].([]any)
	if !ok {
		return nil, nil, "", errors.New("mcpadapter: tools/list result must contain a tools array")
	}
	tools := make([]discoveredTool, 0, len(values))
	for _, value := range values {
		document, ok := value.(map[string]any)
		if !ok || document == nil {
			return nil, nil, "", errors.New("mcpadapter: tools/list contains an invalid tool definition")
		}
		name, ok := document["name"].(string)
		if !ok || name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
			return nil, nil, "", errors.New("mcpadapter: remote tool name is invalid")
		}
		inputDocument, ok := document["inputSchema"].(map[string]any)
		if !ok || inputDocument == nil {
			return nil, nil, "", errors.New("mcpadapter: remote tool inputSchema must be a JSON object")
		}
		if inputDocument["type"] != "object" {
			return nil, nil, "", errors.New("mcpadapter: remote tool inputSchema type must be object")
		}
		if err := validateSchemaNativeValue(inputDocument); err != nil {
			return nil, nil, "", err
		}
		inputSchema, err := canonicalJSONValue(inputDocument)
		if err != nil {
			return nil, nil, "", errors.New("mcpadapter: encode remote tool input schema")
		}
		if err := validateSchemaDocument(inputDocument, inputSchema, limits); err != nil {
			return nil, nil, "", err
		}
		tool := discoveredTool{name: name, inputSchema: inputSchema, inputDocument: inputDocument}
		if outputValue, exists := document["outputSchema"]; exists {
			outputDocument, ok := outputValue.(map[string]any)
			if !ok || outputDocument == nil {
				return nil, nil, "", errors.New("mcpadapter: remote tool outputSchema must be a JSON object")
			}
			if err := validateSchemaNativeValue(outputDocument); err != nil {
				return nil, nil, "", err
			}
			outputSchema, err := canonicalJSONValue(outputDocument)
			if err != nil {
				return nil, nil, "", errors.New("mcpadapter: encode remote tool output schema")
			}
			if err := validateSchemaDocument(outputDocument, outputSchema, limits); err != nil {
				return nil, nil, "", err
			}
			tool.outputSchema = outputSchema
			tool.outputDocument = outputDocument
		}
		if annotationsValue, exists := document["annotations"]; exists {
			annotations, ok := annotationsValue.(map[string]any)
			if !ok || annotations == nil {
				return nil, nil, "", errors.New("mcpadapter: remote tool annotations must be a JSON object")
			}
			if hintValue, exists := annotations["readOnlyHint"]; exists {
				hint, ok := hintValue.(bool)
				if !ok {
					return nil, nil, "", errors.New("mcpadapter: remote tool readOnlyHint must be a boolean")
				}
				tool.readOnlyHintFalse = !hint
			}
		}
		*totalSchemaBytes += len(tool.inputSchema) + len(tool.outputSchema)
		if *totalSchemaBytes > limits.MaxTotalSchemaBytes {
			return nil, nil, "", errors.New("mcpadapter: tool discovery exceeds the total schema byte limit")
		}
		tools = append(tools, tool)
	}
	var nextCursor *string
	if cursorValue, exists := result["nextCursor"]; exists {
		cursor, ok := cursorValue.(string)
		if !ok {
			return nil, nil, "", errors.New("mcpadapter: tools/list nextCursor must be a string when present")
		}
		nextCursor = &cursor
	}
	return tools, nextCursor, cacheScope, nil
}

func buildSnapshot(config Config, tools []discoveredTool) (*Snapshot, error) {
	byRemoteName := make(map[string]discoveredTool, len(tools))
	for _, tool := range tools {
		byRemoteName[tool.name] = tool
	}
	prepared := make([]preparedMapping, 0, len(config.Mappings))
	selectedSchemaBytes := 0
	for _, mapping := range config.Mappings {
		tool, exists := byRemoteName[mapping.RemoteName]
		if !exists {
			return nil, errors.New("mcpadapter: an allowlisted remote tool was not discovered")
		}
		if len(tool.outputSchema) == 0 {
			return nil, errors.New("mcpadapter: every selected remote tool must declare outputSchema")
		}
		if tool.readOnlyHintFalse {
			return nil, errors.New("mcpadapter: remote tool contradicts the host read-only attestation")
		}
		headers, err := collectHeaderBindings(tool.inputDocument)
		if err != nil {
			return nil, err
		}
		inputDocument, _, err := sanitizeOperationSchema(tool.inputDocument, operationInputSchema)
		if err != nil {
			return nil, err
		}
		if err := constrainHeaderIntegers(inputDocument, headers); err != nil {
			return nil, err
		}
		inputSchema, err := canonicalJSONValue(inputDocument)
		if err != nil {
			return nil, errors.New("mcpadapter: encode constrained input schema")
		}
		if err := validateSchemaDocument(inputDocument, inputSchema, config.Limits); err != nil {
			return nil, err
		}
		outputDocument, outputSchema, err := sanitizeOperationSchema(tool.outputDocument, operationOutputSchema)
		if err != nil {
			return nil, err
		}
		if err := validateSchemaDocument(outputDocument, outputSchema, config.Limits); err != nil {
			return nil, err
		}
		normalizer, err := newStrictNullOmissionNormalizer(inputSchema)
		if err != nil {
			return nil, errors.New("mcpadapter: build selected remote tool input normalizer")
		}
		selectedSchemaBytes += len(inputSchema) + len(outputSchema)
		if selectedSchemaBytes > config.Limits.MaxTotalSchemaBytes {
			return nil, errors.New("mcpadapter: tool discovery exceeds the total schema byte limit")
		}
		tool.inputDocument = inputDocument
		tool.inputSchema = inputSchema
		tool.outputDocument = outputDocument
		tool.outputSchema = outputSchema
		entry := preparedMapping{
			mapping: mapping, tool: tool, headers: headers, normalizer: normalizer,
			maxResponseBytes: config.Limits.MaxResponseBytes,
		}
		entry.toolDigest = selectedToolDigest(ProtocolVersion, config.BindingID, entry)
		prepared = append(prepared, entry)
	}
	sort.Slice(prepared, func(i, j int) bool {
		if prepared[i].mapping.OperationName != prepared[j].mapping.OperationName {
			return prepared[i].mapping.OperationName < prepared[j].mapping.OperationName
		}
		return prepared[i].mapping.RemoteName < prepared[j].mapping.RemoteName
	})
	snapshotID := snapshotDigest(ProtocolVersion, config.BindingID, prepared)
	operations := make([]agentruntime.Operation, len(prepared))
	for index, entry := range prepared {
		operations[index] = agentruntime.Operation{
			Name:            entry.mapping.OperationName,
			ContractVersion: "mcpadapter/" + adapterContractVersion + "/" + ProtocolVersion + "/" + snapshotID + "/" + entry.toolDigest,
			Description:     entry.mapping.Description,
			InputSchema:     append(json.RawMessage(nil), entry.tool.inputSchema...),
			OutputSchema:    append(json.RawMessage(nil), entry.tool.outputSchema...),
			NormalizeInput:  entry.normalizer.normalize,
			Effect:          agentruntime.OperationEffectRead,
			Capabilities:    append([]string(nil), entry.mapping.Capabilities...),
			Confirmation:    agentruntime.ConfirmationSpec{Mode: agentruntime.ConfirmationNone},
		}
	}
	validator := agentruntime.NewOperationRegistry()
	if err := validator.RegisterAll(operations); err != nil {
		return nil, errors.New("mcpadapter: selected remote tool contracts are not supported by the runtime")
	}
	summaries := validator.Summaries()
	if err := validator.Freeze(); err != nil {
		return nil, errors.New("mcpadapter: freeze selected remote tool contracts")
	}
	byName := make(map[string]snapshotOperation, len(prepared))
	for index, entry := range prepared {
		registered, ok := validator.Get(entry.mapping.OperationName)
		if !ok {
			return nil, errors.New("mcpadapter: selected remote tool contract is missing")
		}
		operations[index] = registered
		byName[entry.mapping.OperationName] = snapshotOperation{
			remoteName: entry.mapping.RemoteName,
			operation:  registered,
			summary:    summaries[index],
			headers:    cloneHeaderBindings(entry.headers),
			normalizer: entry.normalizer,
		}
	}
	return &Snapshot{
		transport:  config.Transport,
		bindingID:  config.BindingID,
		limits:     config.Limits,
		id:         snapshotID,
		operations: operations,
		byName:     byName,
		validator:  validator,
	}, nil
}

func selectedToolDigest(protocolVersion, bindingID string, entry preparedMapping) string {
	digest := sha256.New()
	writeDigestField(digest, "mcpadapter-tool-v"+adapterContractVersion)
	writeDigestField(digest, protocolVersion)
	writeDigestField(digest, bindingID)
	writePreparedMappingDigest(digest, entry)
	return "mcp_tool_" + hex.EncodeToString(digest.Sum(nil))
}

func snapshotDigest(protocolVersion, bindingID string, prepared []preparedMapping) string {
	digest := sha256.New()
	writeDigestField(digest, "mcpadapter-snapshot-v"+adapterContractVersion)
	writeDigestField(digest, protocolVersion)
	writeDigestField(digest, bindingID)
	writeDigestField(digest, strconv.Itoa(len(prepared)))
	for _, entry := range prepared {
		writePreparedMappingDigest(digest, entry)
	}
	return "mcp_snapshot_" + hex.EncodeToString(digest.Sum(nil))
}

func writePreparedMappingDigest(digest hash.Hash, entry preparedMapping) {
	writeDigestField(digest, "remote_name")
	writeDigestField(digest, entry.mapping.RemoteName)
	writeDigestField(digest, "operation_name")
	writeDigestField(digest, entry.mapping.OperationName)
	writeDigestField(digest, "description")
	writeDigestField(digest, entry.mapping.Description)
	writeDigestField(digest, "host_version")
	writeDigestField(digest, entry.mapping.HostVersion)
	writeDigestField(digest, "read_only")
	writeDigestField(digest, strconv.FormatBool(entry.mapping.ReadOnly))
	writeDigestField(digest, "max_response_bytes")
	writeDigestField(digest, strconv.Itoa(entry.maxResponseBytes))
	writeDigestField(digest, "capability_count")
	writeDigestField(digest, strconv.Itoa(len(entry.mapping.Capabilities)))
	for _, capability := range entry.mapping.Capabilities {
		writeDigestField(digest, capability)
	}
	writeDigestField(digest, "input_schema")
	writeDigestField(digest, string(entry.tool.inputSchema))
	writeDigestField(digest, "output_schema")
	writeDigestField(digest, string(entry.tool.outputSchema))
	writeDigestField(digest, "header_count")
	writeDigestField(digest, strconv.Itoa(len(entry.headers)))
	for _, header := range entry.headers {
		writeDigestField(digest, strconv.Itoa(len(header.path)))
		for _, component := range header.path {
			writeDigestField(digest, component)
		}
		writeDigestField(digest, header.name)
		writeDigestField(digest, strconv.Itoa(int(header.kind)))
	}
}

func writeDigestField(digest hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write([]byte(value))
}

func cloneHeaderBindings(bindings []headerBinding) []headerBinding {
	cloned := make([]headerBinding, len(bindings))
	for index, binding := range bindings {
		cloned[index] = binding
		cloned[index].path = append([]string(nil), binding.path...)
	}
	return cloned
}
