# Error and run-outcome guide

`ClassifyRunOutcome` turns a `Result`/error pair into stable host decisions.
`Message` is safe product copy; raw dependency errors remain for trusted audit
and must not be shown directly.

| Outcome | Retryable | Reconcile first | Host action |
| --- | ---: | ---: | --- |
| `completed` | no | no | return artifacts/output |
| `waiting_user` | no | no | render `PendingApproval`, then resume exact Run ID |
| provider rate limit/unavailable | yes | no | back off and retry the run safely |
| provider quota/authentication | no | no | repair provider configuration |
| session lease lost/interrupted | yes | no | retry only through normal run/store fencing |
| write outcome unknown | no | yes | inspect the authoritative side-effect system |
| cancelled | no | no | report cancellation |
| failed | no by default | no | operator/developer diagnosis |

Provider errors use `ProviderErrorCategory`: `rate_limit`, `quota`,
`authentication`, `transient`, or `rejected`. `ErrInsufficientCredits` remains
only as a deprecated alias of `ErrProviderQuotaExceeded`; the runtime does not
own product billing semantics.

HTTP adapters may map completed to 200, waiting to 202, invalid input to 400,
authentication/configuration failures to 502/503 according to local policy, and
transient interruptions to 503. Queue workers should use `Retryable`, never
string matching. `RequiresReconciliation` always overrides generic retry logic.
