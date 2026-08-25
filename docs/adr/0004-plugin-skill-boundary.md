# ADR 0004: Codex Plugin and Skill execution boundary

Status: accepted.

## Decision

| Plugin material | Runtime treatment |
| --- | --- |
| `SKILL.md` | immutable trusted host-mounted workflow instructions |
| supporting files/assets | immutable snapshot bytes; read-only via explicit host/API access |
| scripts/hooks | inert files; never executed by `skills` or Runtime |
| MCP server declarations | not started or connected |
| apps/connectors | not installed, authenticated, or called |
| manifest metadata | used only to locate explicitly configured Skill directories |

Installing or mounting a Plugin is host configuration, not user/model
authority. `OperationRegistry` remains the only executable capability surface;
policy, approval, execution, verification, and stores retain their normal
roles.

## Threat model

A malicious Plugin may include traversal paths, symlinks, special files,
oversized trees, conflicting Skill names, prompt injection, executable bits, or
network declarations. Built-in sources reject unsafe filesystem structures and
copy a bounded immutable snapshot. Supporting content is still untrusted data
unless an explicitly selected Skill instruction tells the host/model how to use
it. Read-only resource access cannot escape the snapshot or cause execution.

## Persistence and compatibility

Every file contributes to the Skill digest and session SkillSet binding.
Enabling hooks, MCP, or apps later would introduce new authorities and requires
a separate ADR plus persisted binding; it cannot be a compatible reinterpretation
of an existing Skill snapshot.
