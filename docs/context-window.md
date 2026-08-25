# Context-window defaults and tuning

`NewDefaultContextWindowConfig(max)` supplies a conservative reference
configuration. `ConservativeTokenCounter` counts UTF-8 bytes, so its values are
safe upper bounds rather than provider billing tokens. Replace it with the
selected model's tokenizer when utilization matters.

`ExtractiveContextCompactor` is deterministic and executes no content. It
retains bounded excerpts from prior user messages and labels them as historical
facts. Production semantic summaries may use a host compactor, but must preserve
the same untrusted checkpoint boundary and exact budget checks.

| Setting | Default formula | Tune when |
| --- | --- | --- |
| Reserved output | `max / 8` | responses routinely need more/less headroom |
| Input budget | `max - reserved` | model/provider limits differ |
| Compaction trigger | `75%` of input budget | request latency rises before limit |
| Compaction target | `50%` of trigger | compaction runs too often or drops too much |
| Checkpoint maximum | `1/3` of target | summaries dominate retained context |
| Recent turns | `4` | short corrections need more local continuity |

Measure request counts from `EventModelCompleted` token fields after replacing
the conservative counter. A compactor must finish well inside the session lease
renewal interval; long external summarization calls increase lease-loss and
cancellation-cleanup risk.

| Runtime setting | Reference | Operational rule |
| --- | --- | --- |
| Session lease TTL | 30 s | exceed normal store tail latency and short model callbacks |
| Renewal interval | 10 s | remain positive and shorter than TTL; target at most TTL/3 |
| Cleanup timeout | 5 s | cover one bounded final store transaction |
| Max iterations | 8 | lower for narrow workflows; never use as token accounting |

Compaction failures are explicit. Runtime does not silently truncate, substitute
a different compactor, or call the model with a request over the configured
limit.
