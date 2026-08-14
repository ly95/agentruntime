# Contributing to agentruntime

Thanks for helping improve `agentruntime`. Contributions should preserve the
runtime's business-neutral scope and its explicit safety and durability
contracts.

## Project boundary

This repository owns the Go agent runtime: the model loop, operation contracts,
in-process MCP integration, approvals, persistence ports, reconciliation,
context management, and provider transports.

Application handlers, database implementations, queue workers, billing rules,
product prompts, and domain-specific tools belong in host applications. Please
open a design issue before proposing a new abstraction that changes this
boundary or expands the public API.

## Development setup

Requirements:

- Go 1.26 or newer
- Git

From the repository root, download dependencies and run the verification suite:

```bash
go mod download
go test ./...
go vet ./...
```

The examples that call the OpenAI Responses API additionally require
`OPENAI_API_KEY` and `OPENAI_MODEL`. The unit test suite does not require live
provider credentials.

## Making a change

1. Keep the change focused on one runtime problem.
2. Add or update tests for success, failure, and ambiguous-outcome paths.
3. Fail explicitly for invalid configuration or unsupported state.
4. Preserve idempotency, session fencing, approval-resume, verification, and
   execution-transition invariants.
5. Avoid speculative extension points, silent fallbacks, and application-owned
   behavior in the runtime.
6. Update public documentation when behavior or a public contract changes.

Do not leave TODOs, FIXMEs, placeholder packages, or empty commands in a
submitted change.

## Pull requests

Before opening a pull request:

- Run `go test ./...`.
- Run `go vet ./...`.
- Review the diff for unrelated generated or local files.
- Explain any public API or persisted-state compatibility impact.
- Describe how the change behaves after cancellation, retry, lease expiry, or
  an uncertain external side effect when those cases apply.

Small, reviewable pull requests are preferred. Breaking API changes are
possible before v1.0, but they still require a clear motivation and migration
notes.

## Security reports

Do not disclose suspected vulnerabilities in a public issue. Follow
[SECURITY.md](SECURITY.md) for private reporting instructions.

## License

By contributing, you agree that your contribution will be licensed under the
repository's [MIT License](LICENSE).
