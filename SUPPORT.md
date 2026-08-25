# Support

## Supported versions

Until v1.0, the latest tagged minor release and current `main` receive bug and
security fixes. Older pre-1.0 release lines are not guaranteed backports. The
v0.1 release line requires Go 1.26 or newer; a later release may advance that
minimum with explicit release notes.

## Getting help

- Reproducible runtime defects: use the Bug report template.
- Runtime integration questions: use the Support request template.
- Store implementations: use the Store adapter template and include the
  failing `storetest` conformance subtest plus the transaction, isolation,
  lease, fencing, and idempotency mapping.
- Security vulnerabilities: follow [SECURITY.md](SECURITY.md) and do not put
  exploit details in a public issue.

This project supports only the business-neutral runtime contract. Host
application handlers, database implementations, queues, billing, domain tools,
product prompts, provider accounts, and deployment infrastructure remain the
host application's responsibility.

Include the agentruntime version or commit, Go version, provider adapter,
operation effect, store types, stable error code, and the smallest relevant
event sequence. Remove credentials, user data, raw approval arguments,
attachment contents, artifact internal data, and provider payloads.
