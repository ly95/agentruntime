# agentruntime

[![CI](https://github.com/ly95/agentruntime/actions/workflows/ci.yml/badge.svg)](https://github.com/ly95/agentruntime/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ly95/agentruntime.svg)](https://pkg.go.dev/github.com/ly95/agentruntime)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **Status:** `agentruntime` is pre-1.0. The public API may change between minor
> versions while the runtime contracts are refined.

`agentruntime` is an embeddable Go **kernel** for agents built on the OpenAI
Responses API. It owns the model/tool loop. The host keeps authorization,
approvals, persistence, side effects, verification, and domain behavior.

This is not an agent platform, remote MCP client, Codex plugin runtime, or
product framework. It has no HTTP server, database, queue, UI, billing, or
built-in domain tools.

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
permission. Every operation is evaluated by host policy before execution. A
confirmation-required write cannot complete successfully without a positive
verifier result carrying non-empty, non-`null`, unambiguous JSON evidence. The
same evidence requirement applies to normal completion, durable replay, store
transition acknowledgements, and reconciliation.

| Runtime owns | Host application owns |
| --- | --- |
| Model iteration and OpenAI event mapping | API keys, model selection, and product instructions |
| Operation contracts and JSON Schema validation | Authorization and capability policy |
| Approval pause/resume orchestration | Approval UI and approval decisions |
| Transcript, lease, plan, and execution state transitions | Durable `RunStore` and `ExecutionStore` implementations |
| Verification and reconciliation orchestration | Side effects, receipts, verification logic, and reconciliation decisions |
| Context-window and attachment protocols | Domain data, attachment resolution, and trusted external context |

## Core concepts

| Type | Purpose |
| --- | --- |
| `Model` / `OpenAIModel` | Streams model output into the runtime's provider-neutral event contracts. |
| `OperationRegistry` / `Operation` | Defines immutable operation names, schemas, effects, capabilities, and confirmation requirements. |
| `OperationPolicy` | Allows, denies, or routes each proposed operation through approval. |
| `OperationExecutor` | Performs the host-owned side effect or read and returns schema-validated output. |
| `Approver` / `ApprovalResumer` | Requests approval and resumes a previously paused run. |
| `ResultVerifier` | Independently confirms the result of confirmation-required operations. |
| `RunStore` | Persists sessions and serializes stateful runs with renewable, generation-fenced leases. |
| `ExecutionStore` | Persists sealed write plans, idempotent execution records, and append-only transitions. |
| `OperationReconciler` | Settles uncertain persisted writes without starting a model run. |
| `ContextWindowConfig` / `EventSink` | Controls transcript compaction and observes structured runtime events. |
| `skills.SkillSet` | Holds immutable, content-addressed Skill snapshots loaded from explicit host-selected sources. |

## Install

```bash
go get github.com/ly95/agentruntime@latest
```

The module requires Go 1.26 or newer.

## Quick start

Set the credentials and model used by the OpenAI Responses API:

```bash
export OPENAI_API_KEY="..."
export OPENAI_MODEL="your-model-name"
```

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
		option.WithMaxRetries(2),
	)
	model, err := agentruntime.NewOpenAIModel(client, agentruntime.OpenAIModelConfig{
		Model: os.Getenv("OPENAI_MODEL"),
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
instructions. For a new Skill-enabled session, `RunStore.CreateRunV3` atomically
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
filesystem resources after snapshotting. A host that calls a built-in source's
public `Resolve` method directly must call `Close` on every returned
`Artifact`.

GitHub access is also host-owned: an injected `GitHubFetcher` resolves the
configured ref to a commit SHA and returns copied `GitHubFile` records rooted at
the requested Skill directory. The source rejects non-regular entry modes,
validates path collisions and limits, and deep-copies all bytes before parsing;
an arbitrary host filesystem is never retained as a confinement boundary.
`GitHubFile.Mode` uses Go's `io/fs.FileMode` permission bits, not raw Git tree
modes. Local filesystem sources require descriptor-relative, no-follow path
primitives and fail explicitly on unsupported targets.
Default limits also cap each Skill at 16,384 total filesystem entries, 4,096
bytes per relative path, and 64 path components so empty-directory or deep-path
structures cannot bypass the file and byte limits.

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
	// githubFetcher is implemented and credentialed by the host. It is called
	// once; the runtime does not add retries or persist its credentials.
	skills.NewGitHubSource(skills.GitHubSourceConfig{
		ID:         "github",
		Repository: "owner/repository",
		Ref:        "v1.2.0",
		Path:       "skills/security-review",
		Fetcher:    githubFetcher,
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

Supporting files such as `references/`, `assets/`, and `meta.yaml` are preserved
in the snapshot and covered by its digest, but the runtime injects only
`SKILL.md`. It never executes `scripts/` or treats Skill files as tools.

## Operation requirements

The runtime validates required dependencies when it is constructed:

| Configuration | Required host dependencies |
| --- | --- |
| No registered operations | `Model` only |
| Any registered operation | `OperationPolicy` and `OperationExecutor` |
| Any write operation | `ExecutionStore` in addition to policy and executor |
| `ConfirmationRequired` operation | `ResultVerifier`; approval flows also use `Approver` and `ApprovalResumer` |
| Stateful session | `RunStore` |
| Custom `NewID` or explicit `Input.RunID` | `RunStore` as durable identity authority |

Run identity, approval authority, and operation contracts are versioned and
bound to durable state; the authoritative rules live in the corresponding
interface and configuration documentation (`store.go`, `operation_registry.go`,
`runtime_config.go`, `openai_transport.go`). In summary:

- **Run identity**: generated RunIDs only request creation and must collide with
  every existing run; an explicit `Input.RunID` may resume the exact waiting run.
  `RunStore` adopts the split `CreateRunV3`/`ResumeRunV3` methods and a
  synchronous, exactly-once, pre-commit acceptance callback; only an exact
  `ErrRunNotFound` selects the explicit-ID create fallback. A runtime without a
  `RunStore` uses the built-in random factory and rejects custom `NewID`.
- **Reconciliation evidence**: a started attempt may only be abandoned with
  durable JSON evidence proving the executor never began, or completed with
  evidence proving it committed; the store fences that exact attempt atomically.
- **Operation contracts**: every write operation declares a stable
  `ContractVersion`; Runtime binds the resulting contract digest to sessions,
  plans, executions, reconciliation, and approval resume state, and fails
  explicitly on any mismatch. Terminal writes declare their session projection
  via `ProjectTerminalSession` before execution.
- **Approval resume**: `RunStore` must round-trip the complete authority-version-1
  `PendingApprovalCommit` (request, decision, audit, normalized arguments,
  operation summary, input, checkpoint, and replay) with its digest; pre-versioned
  subset digests are not resumable. Resume is rejected when the session revision
  advanced past the checkpoint.
- **Model output authority**: `OpenAIModel` enforces one ordered
  `response.created`→`response.completed` lifecycle with strictly increasing
  sequence numbers, exactly-once item add/finalize/complete, argument-delta
  reconciliation against `arguments.done` and the completed response, immutable
  response-field drift detection, and full replay validation before emitting
  observer-visible completion. Missing, repeated, out-of-order, unsupported, or
  contradictory evidence fails as invalid model output. Only function tools are
  offered to the provider, so tool-call lifecycles for other tool classes
  (web search, code interpreter, MCP, file search, image generation, custom
  tools) are rejected as invalid model output.
- **Inputs and bounds**: `Input.IdempotencyScope` is trusted host state, ignored
  by JSON decoding. `RuntimeConfig.MaxCallsPerTurn` bounds one response's
  operation fanout (default 32).

## Examples

Runnable examples live in [`examples`](examples/README.md):

| Example | Demonstrates |
| --- | --- |
| [`basic`](examples/basic/main.go) | A stateless agent without tools |
| [`operations`](examples/operations/main.go) | A read-only host operation offered to the model as a function tool |
| [`skill`](examples/skill/main.go) | An explicitly loaded local `SKILL.md` mounted beside a host-owned operation |

The examples intentionally keep their side effects read-only. Production write
operations require the durable execution, approval, and verification boundaries
described above.

## Compatibility and guarantees

- `OpenAIModel` targets the OpenAI Responses API. Client authentication,
  endpoints, middleware, timeouts, and bounded pre-stream retries are configured
  on the injected `openai.Client`; compatibility with non-OpenAI implementations
  is not guaranteed.
- Invalid configuration, unsupported provider output, missing resources, and
  ambiguous execution outcomes fail explicitly.
- The runtime does not retry semantic model turns or side-effecting operations,
  downgrade, truncate, or substitute a caller choice. An injected provider client
  may perform explicitly configured transport retries before a response stream is
  established.
- Operation contracts are frozen when the runtime is constructed; persisted
  write plans and execution transitions are treated as immutable history.

## Development

```bash
go test ./...
go vet ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md) before proposing changes. Report
vulnerabilities according to [SECURITY.md](SECURITY.md), not through a public
issue.

## Releases, support, and roadmap

The source contract version is exposed as `agentruntime.Version`; Git tags use
the same semantic version with a leading `v`. Release notes and the checked-in
public API baseline are gated in CI before a tag is published.

- [Changelog](CHANGELOG.md)
- [Support policy](SUPPORT.md)
- [Roadmap and known limits](ROADMAP.md)
- [v0.1.0 release notes](docs/releases/v0.1.0.md)

## License

`agentruntime` is available under the [MIT License](LICENSE).
