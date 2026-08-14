# Examples

These examples are small, runnable programs that exercise the public
`github.com/ly95/agentruntime` API.

| Example | What it demonstrates |
| --- | --- |
| [`basic`](basic/main.go) | A stateless OpenAI-backed agent run without tools |
| [`mcp`](mcp/main.go) | A read-only operation exposed to the model through the runtime's in-process MCP server |
| [`skill`](skill/main.go) | An explicit local `SKILL.md` snapshot mounted beside a host-owned operation |

## Configure

All examples use the OpenAI Responses API and fail explicitly when required
configuration is missing.

```bash
export OPENAI_API_KEY="..."
export OPENAI_MODEL="..."
# Optional; defaults to https://api.openai.com/v1
export OPENAI_BASE_URL="https://api.openai.com/v1"
```

## Run

Run commands from the repository root:

```bash
go run ./examples/basic "Explain agent loops in one paragraph."
go run ./examples/mcp "Use the tool to add 19 and 23."
go run ./examples/skill "Analyze this text: small tools make agents easier to test."
```

The MCP example uses the runtime's built-in, in-process MCP boundary:
`OperationRegistry` defines trusted tool contracts, the runtime discovers those
contracts through MCP, and the host still owns policy and execution. It does not
connect to an arbitrary remote MCP server.

The Skill example resolves the absolute `examples/skill/textskill` directory,
loads only that explicitly listed local directory, and mounts its immutable
snapshot through `RuntimeConfig.Skills`. The host separately registers and
executes `text_analyze`; mounting a Skill does not grant execution authority.
Supporting scripts would remain inert snapshot files rather than operations.

These examples are stateless and read-only. Production write operations also
require a durable `ExecutionStore`; confirmation-required writes additionally
require approval and verification implementations.
