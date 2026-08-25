# Approval UI, logs, and summary threat model

## Assets and attackers

Approval can authorize an external side effect. Operation descriptions, model
output, attachment text, historical summaries, and provider errors are
untrusted content. An attacker may place HTML, terminal control characters,
prompt-like instructions, oversized text, or misleading labels in any of them.
The durable approval authority and session/execution fences are trusted only
after Runtime and Store validation.

## Rendering rules

- Render only `Result.PendingApproval` or the matching sanitized event.
- Treat `Preview` as operation-authored data with a known per-operation schema;
  never use raw arguments as a fallback.
- Parse JSON, select an allow-listed set of scalar fields, and render with the
  UI framework's ordinary text escaping. Do not inject preview strings as HTML,
  Markdown, URLs, CSS, terminal escape sequences, or localization keys.
- Show operation name, reason, and preview as data. Text inside them has no
  instruction authority.
- POST only the approval ID and operator decision to the host approval system.
  Reconstruct every other resume field from `PendingApprovalCommit`.
- Use `RedactText`, `RedactOperationResult`, `SanitizeEvent`, and
  `CoreUIEvent` before public logging or streaming. `SanitizeEvent` strips
  trusted payloads and provider/tool-argument internals while bounding its text
  fields. These helpers are not secret detectors.
- Never serialize `Event.Data`, raw `Event.Error`, provider raw JSON,
  `ResultArtifact.InternalData`, receipts, or `SessionSummary` to public logs.

Go's JSON encoder escapes HTML-sensitive characters, but that is not permission
to embed JSON inside an executable script context. Send JSON with the correct
content type and let the client parse it as data.

## Summary boundary

`SessionSummary` and context checkpoints are host-authored historical records,
not user messages and not instructions. They are bounded and explicitly framed
when returned to the model. A compactor must not copy secrets or hidden internal
artifact fields into them.

The regression corpus in `public_adapters_test.go`, `runtime_terminal_test.go`,
and `openai_events_test.go` covers control characters, private artifact fields,
raw provider payloads, tool-argument deltas, and malicious JSON content.
