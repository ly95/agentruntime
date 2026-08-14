// Package agentruntime provides a business-neutral agent runtime.
//
// The runtime treats the LLM as the control loop and discovers executable tools
// through MCP. MCP discovery never grants permission: host applications inject
// policy, approval, operation execution, and independent result-verification
// boundaries. Hosts decide whether a write requires confirmation; confirmed
// writes cannot complete without a successful verifier result. RunStore serializes
// stateful session runs with expiring, renewable, generation-fenced leases.
// ExecutionStore seals idempotent write plans and keeps append-only execution
// transition history, including executed results awaiting verification. The
// package deliberately contains no application business behavior; hosts supply
// capabilities through narrow interfaces.
package agentruntime
