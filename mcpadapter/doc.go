// Package mcpadapter maps an explicitly allowlisted snapshot of remote MCP tools
// into agentruntime operations.
//
// The package intentionally does not implement HTTP, SSE, OAuth, credential
// storage, endpoint selection, retries, or fallback. A host supplies those
// concerns through Transport and pins a non-secret BindingID. Discovery runs
// once, is bounded and all-or-nothing, and produces only synchronous read
// operations. Server descriptions, instructions, annotations, and approval
// hints never grant runtime authority.
//
// Snapshot implements agentruntime.OperationExecutor. Hosts still install it
// behind their agentruntime.OperationPolicy; registering the snapshot does not
// authorize any call.
package mcpadapter
