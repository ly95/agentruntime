# Examples

These examples are small, runnable programs that exercise the public
`github.com/ly95/agentruntime` API.

| Example | What it demonstrates |
| --- | --- |
| [`basic`](basic/main.go) | A stateless OpenAI-backed agent run without tools |
| [`operations`](operations/main.go) | A read-only operation registered as a model tool, with host-owned policy and execution |
| [`skill`](skill/main.go) | An explicit local `SKILL.md` snapshot mounted beside a host-owned operation |
| [`approval`](approval/main.go) | Offline durable writes: approval resume, safe not-applied handling, and evidence reconciliation |

## Configure

The `basic`, `operations`, and `skill` examples use the OpenAI Responses API and
fail explicitly when required configuration is missing. The `approval` example
is fully offline.

```bash
export OPENAI_API_KEY="..."
export OPENAI_MODEL="..."
# Required stable, non-secret binding labels. Do not put URLs, keys, or tokens here.
export OPENAI_ENDPOINT_CLASS="openai-public"
export OPENAI_CREDENTIAL_PRINCIPAL="service-account-name"
# Optional; defaults to https://api.openai.com/v1
export OPENAI_BASE_URL="https://api.openai.com/v1"
```

## Run

Run commands from the repository root:

```bash
go run ./examples/basic "Explain agent loops in one paragraph."
go run ./examples/operations "Use the tool to add 19 and 23."
go run ./examples/skill "Analyze this text: small tools make agents easier to test."
go run ./examples/approval
```

`OPENAI_ENDPOINT_CLASS` identifies the deployment class rather than the literal
endpoint URL. `OPENAI_CREDENTIAL_PRINCIPAL` identifies the stable account or
service identity rather than its rotating secret. The examples do not infer
these values from `OPENAI_BASE_URL` or `OPENAI_API_KEY`.

The operations example registers `math_add` in `OperationRegistry`. The runtime
offers that contract to the model as a function tool. Policy and execution stay
with the host.

The Skill example resolves the absolute `examples/skill/textskill` directory,
loads only that explicitly listed local directory, and mounts its immutable
snapshot through `RuntimeConfig.Skills`. The host separately registers and
executes `text_analyze`; mounting a Skill does not grant execution authority.
Supporting scripts remain inert snapshot files rather than operations.

The first three examples are stateless and read-only. The approval example is
fully offline and uses `InMemoryStore` as executable protocol documentation;
production hosts must replace it with durable transactional `RunStore` and
`ExecutionStore` adapters.
