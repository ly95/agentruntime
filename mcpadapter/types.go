package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ly95/agentruntime"
)

// ProtocolVersion is the only MCP revision accepted by this adapter.
const ProtocolVersion = "2026-07-28"

const adapterContractVersion = "3"

// Transport is the host-owned MCP transport boundary. Implementations own the
// endpoint, authentication, SSRF controls, HTTP/SSE or stdio framing, response
// streaming limits, and cancellation. They must enforce Request limits while
// reading, atomically verify Request.ExpectedBindingID at dispatch, and must not
// retry one RoundTrip call. BindingID must be immutable, non-secret, and stable
// for the endpoint, credential principal, authorization scope, and transport
// semantics that can change execution outcomes.
// Methods may be called concurrently.
type Transport interface {
	BindingID() string
	RoundTrip(ctx context.Context, request Request) (json.RawMessage, error)
}

// Correlation carries runtime identities for host transport telemetry. It is
// not serialized into the MCP request body.
type Correlation struct {
	RunID       string
	CallID      string
	ExecutionID string
	AttemptID   string
}

// Request is one logical MCP JSON-RPC request. MetadataHeaders contains only
// protocol-mandated, header-ready values; credentials remain private to the
// Transport. A Streamable HTTP transport must copy these fields to the POST.
// MaxResponseBytes is also enforced after RoundTrip; the transport must enforce
// it while reading so an oversized response is never buffered in full.
type Request struct {
	ID                string
	Method            string
	Params            json.RawMessage
	MetadataHeaders   map[string]string
	ExpectedBindingID string
	MaxResponseBytes  int
	Correlation       Correlation
}

// MarshalJSON emits the JSON-RPC body and excludes transport-only metadata.
func (request Request) MarshalJSON() ([]byte, error) {
	type wireRequest struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	return json.Marshal(wireRequest{
		JSONRPC: "2.0",
		ID:      request.ID,
		Method:  request.Method,
		Params:  request.Params,
	})
}

// Mapping is a host-authored allowlist entry. ReadOnly is an explicit host
// attestation about the remote implementation; MCP annotations are untrusted
// and cannot satisfy it. Description is the only remote-tool description
// exposed to the model; schema annotation text is removed. HostVersion must
// change whenever the host's understanding of the remote read semantics
// changes. It and BindingID must not contain secrets.
type Mapping struct {
	RemoteName    string
	OperationName string
	Description   string
	Capabilities  []string
	HostVersion   string
	ReadOnly      bool
}

// Limits bound MCP RPCs and one startup discovery snapshot. MaxResponseBytes
// applies to discovery and every tools/call; the remaining fields apply only to
// discovery. Zero fields select conservative defaults. Negative values are
// invalid, and exceeding a bound fails instead of truncating.
type Limits struct {
	MaxPages            int
	MaxTools            int
	MaxResponseBytes    int
	MaxSchemaBytes      int
	MaxTotalSchemaBytes int
	MaxSchemaDepth      int
	MaxSchemaNodes      int
}

// Config fixes the transport binding, host allowlist, and discovery bounds.
// ExpectedSnapshotID optionally pins a previously reviewed or persisted
// snapshot. A mismatch fails before any operations can be registered.
type Config struct {
	Transport          Transport
	BindingID          string
	ExpectedSnapshotID string
	Mappings           []Mapping
	Limits             Limits
}

// Snapshot is an immutable discovered operation set and its executor.
type Snapshot struct {
	transport  Transport
	bindingID  string
	limits     Limits
	id         string
	operations []agentruntime.Operation
	byName     map[string]snapshotOperation
	validator  *agentruntime.OperationRegistry
}

type snapshotOperation struct {
	remoteName string
	operation  agentruntime.Operation
	summary    agentruntime.OperationSummary
	headers    []headerBinding
	normalizer *strictNullOmissionNormalizer
}

// ID returns the content digest of the protocol version, transport binding,
// mappings, and selected tool contracts. It never contains credentials.
func (snapshot *Snapshot) ID() string {
	if snapshot == nil {
		return ""
	}
	return snapshot.id
}

// Register atomically adds every operation in the snapshot to registry.
func (snapshot *Snapshot) Register(registry *agentruntime.OperationRegistry) error {
	if snapshot == nil {
		return errors.New("mcpadapter: nil snapshot")
	}
	if registry == nil {
		return errors.New("mcpadapter: nil operation registry")
	}
	return registry.RegisterAll(snapshot.operations)
}

var _ agentruntime.OperationExecutor = (*Snapshot)(nil)
