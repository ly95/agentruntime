# agentruntime

[![CI](https://github.com/ly95/agentruntime/actions/workflows/ci.yml/badge.svg)](https://github.com/ly95/agentruntime/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ly95/agentruntime.svg)](https://pkg.go.dev/github.com/ly95/agentruntime)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **Status:** `agentruntime` is pre-1.0. The public API may change between minor
> versions while the runtime contracts are refined.
>
> **Documentation target:** this README describes unreleased `main`. The latest
> tag is `v0.1.0`, whose API is not compatible with all examples below. Use the
> [v0.1.0 README](https://github.com/ly95/agentruntime/tree/v0.1.0#readme) with
> the tagged module, or install `@main` to evaluate the current documentation.

`agentruntime` is an embeddable Go **kernel** for agents built on the OpenAI
Responses API. It owns the model/tool loop. The host keeps authorization,
approvals, persistence, side effects, verification, and domain behavior.

This is not an agent platform, general-purpose MCP network client, Codex plugin
runtime, or product framework. It has no HTTP server, database, queue, UI,
billing, or built-in domain tools. The optional `mcpadapter` package maps a
host-transported, explicitly allowlisted snapshot of remote read tools into the
same operation policy boundary.

## Why agentruntime

- Embed a streaming model/tool loop in an existing Go application.
- Offer JSON Schema-validated operation contracts to the model as function tools.
- Keep policy, human approval, execution, and independent verification under
  host control.
- Resume approvals and stateful conversations without giving the model
  authority over persisted state.
- Protect writes with durable plans, idempotent execution records, session
  leases, fencing, and reconciliation.
- Observe the runtime through structured events without coupling it to an
  application transport or storage implementation.
- Freeze explicitly allowlisted remote MCP reads into ordinary operation
  contracts without giving a server endpoint, credential, policy, or approval
  authority.

## Architecture and trust boundaries

```mermaid
flowchart LR
    Host["Host application"] --> Runtime["agentruntime"]
    Runtime <--> Model["OpenAI Responses API"]
    Runtime --> Registry["OperationRegistry"]
    Registry --> Policy["Host policy"]
    Policy -->|allow| Execute["Host executor"]
    Policy -->|require approval| Approve["Host approval"]
    Approve --> Execute
    Execute -->|when required| Verify["Host verifier"]
    Runtime <--> Runs["RunStore"]
    Runtime <--> Executions["ExecutionStore"]
    Executions --> Reconcile["OperationReconciler"]
```

Registering an operation only exposes a contract to the model; it never grants
permission. Within Runtime, every proposed operation is evaluated by host policy
before executor dispatch. A confirmation-required write cannot complete
successfully without a positive verifier result carrying non-empty, non-`null`,
unambiguous JSON evidence. The
same evidence requirement applies to normal completion, durable replay, store
transition acknowledgements, and reconciliation.

| Runtime owns | Host application owns |
| --- | --- |
| Model iteration, provider-neutral model contracts, OpenAI event mapping, and durable binding checks | API keys, model selection, non-secret endpoint/principal labels, injected-client transport/retry policy, and product instructions |
| Operation contracts and JSON Schema validation | Authorization and capability policy |
| Approval pause/resume orchestration | Approval UI and approval decisions |
| Transcript, lease, plan, and execution state transitions | Durable `RunStore` and `ExecutionStore` implementations |
| Verification and reconciliation orchestration | Side effects, receipts, verification logic, and reconciliation decisions |
| Context-window and attachment protocols | Domain data, attachment resolution, and trusted external context |
| Bounded MCP discovery, immutable read contracts, and structured-result validation | MCP endpoint, transport framing, credentials, SSRF controls, authorization scope, and read-only attestation |

## Core concepts

| Type | Purpose |
| --- | --- |
| `Model` / `BoundModel` / `OpenAIModel` | Streams provider output into neutral event contracts; durable runtimes require an immutable five-part model binding. |
| `OperationRegistry` / `Operation` | Defines immutable operation names, schemas, effects, capabilities, and confirmation requirements. |
| `OperationPolicy` | Allows, denies, or routes each proposed operation through approval. |
| `OperationExecutor` | Performs the host-owned side effect or read and returns schema-validated output. |
| `Approver` / `ApprovalResumer` | Requests approval and resumes a previously paused run. |
| `ResultVerifier` | Independently confirms the result of confirmation-required operations. |
| `RunStore` | Persists sessions and serializes stateful runs with renewable, generation-fenced leases. |
| `ExecutionStore` | Persists sealed write plans, idempotent execution records, and append-only transitions. |
| `InMemoryStore` | Provides a concurrency-safe reference implementation for examples and protocol tests; production hosts still need durable transactional storage. |
| `modeltest` | Exposes the fixed provider-neutral v1 conformance corpus that every model adapter must pass without capability skips. |
| `OperationReconciler` | Settles uncertain persisted writes without starting a model run. |
| `ContextWindowConfig` / `EventDispatcher` | Controls transcript compaction and fans structured runtime events out through panic-safe adapters. |
| `RunOutcome` / `SanitizedEvent` | Maps runtime results and events into stable host-facing status, retry, reconciliation, and safe JSON contracts. |
| `skills.SkillSet` | Holds immutable, content-addressed Skill snapshots loaded from explicit host-selected sources. |
| `mcpadapter.Snapshot` | Freezes host-allowlisted MCP `2026-07-28` read tools and, when installed as Runtime's executor, dispatches them after the normal host policy. |

## Install

For the tagged `v0.1.0` API, use its matching documentation:

```bash
go get github.com/ly95/agentruntime@v0.1.0
```

To evaluate the unreleased API documented by this README:

```bash
go get github.com/ly95/agentruntime@main
```

`@latest` currently resolves to `v0.1.0`; do not combine it with this branch's
quick start. No `v0.2.0` release has been published.

The module requires Go 1.26 or newer.

## Quick start

Set the credentials, model, and stable non-secret binding labels used by the
OpenAI Responses API:

```bash
export OPENAI_API_KEY="..."
export OPENAI_MODEL="your-model-name"
export OPENAI_ENDPOINT_CLASS="openai-public-api"
export OPENAI_CREDENTIAL_PRINCIPAL="agent-runtime-development"
```

`OPENAI_ENDPOINT_CLASS` is a deployment class, not a URL.
`OPENAI_CREDENTIAL_PRINCIPAL` identifies the authenticated account or service,
not its API key or token.

Then create and run a stateless agent:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ly95/agentruntime"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func main() {
	client := openai.NewClient(
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		option.WithMaxRetries(2), // Explicit host-owned pre-stream retry policy.
	)
	model, err := agentruntime.NewOpenAIModel(client, agentruntime.OpenAIModelConfig{
		Model:               os.Getenv("OPENAI_MODEL"),
		EndpointClass:       os.Getenv("OPENAI_ENDPOINT_CLASS"),
		CredentialPrincipal: os.Getenv("OPENAI_CREDENTIAL_PRINCIPAL"),
	})
	if err != nil {
		panic(err)
	}

	runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{Model: model})
	if err != nil {
		panic(err)
	}

	result, err := runtime.Run(context.Background(), agentruntime.Input{
		User: "Explain why the sky is blue in one paragraph.",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Output)
}
```

## Mounted Skills

The `skills` package loads every source through the same `SKILL.md` parser,
copies all files into an immutable snapshot, and derives deterministic Skill and
SkillSet hashes. A Runtime freezes the mounted SkillSet at construction and
adds only each Skill's name, description, and `SKILL.md` body to model
instructions. For a new Skill-enabled session, `RunStore.CreateRunV4` atomically
creates a revision-zero binding state before the first transcript snapshot can
exist; later snapshots retain that SkillSet ID. Implementations must compare the
incoming `RunRecord.SkillSetID` with stored session, waiting-run, and active-run
bindings inside the same transaction, returning `ErrSkillSetMismatch` before
claiming or fencing a lease or changing run status. This binding survives a
first-run failure and abandoned-lease recovery.

Local loading is explicit-only: `NewLocalSource` reads exactly the absolute
directories listed by the host. It never scans `~/.agents/skills`,
`~/.codex/skills`, environment-derived locations, or the working directory.
Local directories must be canonical absolute paths with no symbolic-link
components; aliases are rejected instead of resolved. `LoadSet` closes
filesystem resources after snapshotting. A host that calls `LocalSource.Resolve`
directly must call `Close` on every returned `Artifact`.

The runtime does not fetch Skills from GitHub or other remotes. A host that
needs remote content must snapshot it into a local directory (or implement a
trusted custom `skills.Source`) before `LoadSet`. Local filesystem loading
requires descriptor-relative, no-follow path primitives and fails explicitly on
unsupported targets. Default limits also cap each Skill at 16,384 total
filesystem entries, 4,096 bytes per relative path, and 64 path components so
empty-directory or deep-path structures cannot bypass the file and byte limits.

```go
skillSet, err := skills.LoadSet(ctx,
	// Each entry is the Skill directory itself and must contain SKILL.md.
	skills.NewLocalSource(skills.LocalSourceConfig{
		ID: "team-local",
		Directories: []string{
			"/opt/agent-skills/code-review",
			"/opt/agent-skills/release-notes",
		},
	}),
)
if err != nil {
	return err
}

runtime, err := agentruntime.NewRuntime(agentruntime.RuntimeConfig{
	Model:  model,
	Skills: skillSet,
})
```

Supporting files such as `references/`, `assets/`, `meta.yaml`, and
`agents/openai.yaml` are preserved in the snapshot and covered by its digest,
and can be read as defensive copies through `Skill.ReadFile` or
`SkillSet.ReadFile`. Only `SKILL.md` is injected into model instructions. The
runtime never executes `scripts/`, loads hooks,
starts Plugin MCP servers, interprets `.app.json`, or implements the rest of the
Codex Plugin runtime.

## Operation requirements

The runtime validates required dependencies when it is constructed:

| Configuration | Required host dependencies |
| --- | --- |
| No registered operations | `Model` only |
| Any registered operation | `OperationPolicy` and `OperationExecutor` |
| Any write operation | `ExecutionStore` in addition to policy and executor |
| `ConfirmationRequired` operation | `ResultVerifier`; approval flows also use `Approver` and `ApprovalResumer` |
| Stateful session | `RunStore` |
| Any configured `RunStore` | The `Model` must implement `BoundModel`; the store must implement the V4 contract. |
| Custom `IDFactory` (or deprecated `NewID`) or explicit `Input.RunID` | `RunStore` as durable identity authority |

Run identity, approval authority, and operation contracts are versioned and
bound to durable state; the authoritative rules live in the corresponding
interface and configuration documentation (`store.go`, `operation_registry.go`,
`runtime_config.go`, `openai_transport.go`). In summary:

- **Run identity**: generated RunIDs only request creation and must collide with
  every existing run; an explicit `Input.RunID` may resume the exact waiting run.
  `RunStore` adopts the split `CreateRunV4`/`ResumeRunV4` methods and a
  synchronous, exactly-once, pre-commit acceptance callback; only an exact
  `ErrRunNotFound` selects the explicit-ID create fallback. A runtime without a
  `RunStore` uses the built-in random factory and rejects custom `IDFactory`
  and deprecated `NewID`. An `IDFactory` returns `(string, error)`, so entropy
  failures remain ordinary runtime errors rather than panics.
- **Model binding**: a Runtime with `RunStore` requires `BoundModel`. Its
  immutable `(provider, model, endpoint class, credential principal, adapter
  version)` binding is hashed with SHA-256; durable run, session, and approval
  records retain only the resulting `ModelBindingID`. V4 rejects drift and empty
  legacy bindings without mutation and never infers or upgrades them. An adapter
  version change creates a new binding, so old runtimes/adapters must remain
  available to drain their sessions. Secret rotation may retain a binding only
  when the stable principal is unchanged.
- **Reconciliation evidence**: a started attempt may only be abandoned with
  durable JSON evidence proving the executor never began, or completed with
  evidence proving it committed; the store fences that exact attempt atomically.
- **Operation contracts**: every write operation declares a stable
  `ContractVersion`; Runtime binds the resulting contract digest to sessions,
  plans, executions, reconciliation, and approval resume state, and fails
  explicitly on any mismatch. Terminal writes declare their session projection
  via `ProjectTerminalSession` before execution.
- **Approval resume**: `RunStore` must round-trip the complete authority-version-2
  `PendingApprovalCommit` (request, decision, audit, normalized arguments,
  operation summary, input identity, checkpoint, and replay) with its digest;
  older subset authority is not resumable. Resume is rejected when the model
  binding or input digest changes, or when the session revision advances past
  the checkpoint.
- **Model output authority**: `OpenAIModel` enforces one ordered
  `response.created`→`response.completed` lifecycle with strictly increasing
  sequence numbers, exactly-once item add/finalize/complete, argument-delta
  reconciliation against `arguments.done` and the completed response, immutable
  response-field drift detection, exact requested-model validation, and full
  replay validation before emitting observer-visible completion. Missing,
  repeated, out-of-order, unsupported, substituted, or contradictory evidence
  fails as invalid model output. Only function tools are offered to the provider,
  so tool-call lifecycles for other tool classes (web search, code interpreter,
  MCP, file search, image generation, custom tools) are rejected as invalid
  model output.
- **Adapter conformance**: the public `modeltest` package defines fixed binding,
  lifecycle, error, usage, cancellation, invalid-output, replay, concurrency, and
  synchronous sink-lifetime scenarios. Every adapter must pass every scenario;
  there are no capability flags, provider branches, fallback paths, or skips.
- **Inputs and bounds**: `Input.IdempotencyScope` is trusted host state, ignored
  by JSON decoding. `RuntimeConfig.MaxCallsPerTurn` bounds one response's
  operation fanout (default 32).

## Constrained remote MCP reads

`mcpadapter.Discover` uses a host-injected transport to call
`server/discover` and bounded `tools/list` pagination for MCP `2026-07-28`.
Every selected tool must be explicitly mapped and attested read-only by the
host, must declare an output schema, and becomes a non-terminal read operation.
Remote schema annotation text is removed before model exposure. The resulting
`Snapshot` registers atomically and implements `OperationExecutor`, so
`OperationPolicy` still runs before any `tools/call` made through Runtime.

The adapter does not implement HTTP/SSE/OAuth, choose endpoints, hold
credentials, downgrade, hot-refresh schemas, execute remote writes, or follow
`input_required`. It emits current Streamable HTTP metadata and validated
`x-mcp-header` values for a host transport that must enforce streaming limits,
atomic binding checks, cancellation, and no transparent retry. Production hosts
can pin `ExpectedSnapshotID`; durable sessions require the same pinned catalog
after restart. MCP reads can be allowed or denied, but Runtime's write approval
flow is not a per-call consent mechanism for them. Only schema-valid
`structuredContent` enters Runtime. See the
[constrained MCP adapter guide](docs/mcp-adapter-guide.md).

## Examples

Runnable examples live in [`examples`](examples/README.md):

| Example | Demonstrates |
| --- | --- |
| [`basic`](examples/basic/main.go) | A stateless agent without tools |
| [`operations`](examples/operations/main.go) | A read-only host operation offered to the model as a function tool |
| [`skill`](examples/skill/main.go) | An explicitly loaded local `SKILL.md` mounted beside a host-owned operation |
| [`approval`](examples/approval/main.go) | A fully offline approval, resume, safe retry, and reconciliation workflow using the reference store |

The approval example performs only simulated writes. Production write operations
require the durable execution, approval, and verification boundaries described
above.

## Integration guides

- [Host integration and approval resume](docs/host-integration-guide.md)
- [Store adapter contract and conformance suites](docs/store-adapter-guide.md)
- [Safety guarantees, non-guarantees, and failure windows](docs/safety-guarantees.md)
- [Operation JSON Schema and argument decoding](docs/operation-schema-guide.md)
- [Write and terminal-operation execution](docs/write-terminal-executor-guide.md)
- [Context-window defaults and tuning](docs/context-window.md)
- [Outcome and provider-error mapping](docs/error-outcome-guide.md)
- [Safe events, metrics, tracing, and UI projection](docs/observability.md)
- [Approval UI and log security](docs/approval-ui-security.md)
- [Provider compatibility and additional-adapter readiness](docs/provider-compatibility.md)
- [Constrained remote MCP adapter](docs/mcp-adapter-guide.md)

## Compatibility and guarantees

- OpenAI Responses is the only bundled model adapter. The public `modeltest`
  corpus freezes readiness requirements for additional adapters; no second
  provider adapter is implemented.
- `OpenAIModel` targets the OpenAI Responses API and rejects a response whose
  model differs from the configured request. Compatibility with non-OpenAI
  implementations is not guaranteed.
- Client authentication, endpoints, middleware, timeouts, transport, and retry
  policy belong to the injected `openai.Client`. A host may explicitly configure
  bounded retries before a response stream starts; Runtime does not add a silent
  retry after response evidence or retry a semantic model turn.
- `mcpadapter` targets MCP `2026-07-28` only. Its injected transport owns network,
  credential, dispatch-binding, streaming-limit, and no-retry compliance; older
  protocol fallback is intentionally not performed.
- Invalid configuration, unsupported provider output, missing resources, and
  ambiguous execution outcomes fail explicitly. Durable Runtime checks the
  frozen model binding before and after every provider call, and store adapters
  must validate complete approval authority with the exported canonical helper.
- The runtime does not fall back, downgrade, truncate, or substitute a caller's
  provider, model, operation, or other choice.
- Operation contracts are frozen when the runtime is constructed; persisted
  write plans and execution transitions are treated as immutable history.

## Development

```bash
go test ./...
go vet ./...
go test -race ./...
GOTOOLCHAIN=go1.26.0 go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md) before proposing changes. Report
vulnerabilities according to [SECURITY.md](SECURITY.md), not through a public
issue.

## Releases, support, and roadmap

The source contract version is exposed as `agentruntime.Version`; Git tags use
the same semantic version with a leading `v`. `api/v0.1.0.txt` records the tagged
release API. The separate `api/main.txt` records the explicitly reviewed,
unreleased API on `main`; CI fails on any addition, removal, or incompatible
change until that baseline is deliberately regenerated and reviewed. Updating
the `main` baseline does not publish or imply a new release.

- [Changelog](CHANGELOG.md)
- [Support policy](SUPPORT.md)
- [Roadmap and known limits](ROADMAP.md)
- [v0.1.0 release notes](docs/releases/v0.1.0.md)
## License

`agentruntime` is available under the [MIT License](LICENSE).
