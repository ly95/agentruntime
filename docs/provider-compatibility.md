# Provider mapping and compatibility

The public `Model` interface is provider-neutral. OpenAI transport code is split
by responsibility:

- `openai_transport.go` owns request/stream orchestration;
- `openai_mapping.go` maps validated SDK objects to runtime types;
- `openai_stream.go` owns ordered stream state;
- `openai_validate.go` owns closed-schema reflection;
- `openai_validate_deep.go` owns identity validation, while
  `openai_validate_{types,response,items,events}.go` own the other semantic domains;
- `provider_error.go` maps provider status/code metadata to stable categories.

Provider raw JSON is never public event JSON. Unsupported fields and
contradictory lifecycle evidence fail explicitly instead of being ignored.

Before bumping `openai-go`:

1. run `go test ./...` against `testdata/openai/*.sse`;
2. add a golden stream for each newly accepted lifecycle/union shape;
3. keep a negative golden for new authority fields until validation is updated;
4. run fuzz, race, Go 1.26, Windows, and macOS CI;
5. run the public API diff and classify every reported change in release notes.

Golden files are provider protocol evidence, not snapshots to regenerate
blindly. Review additions for authorization, identity, ordering, and public-data
impact.

## Additional provider readiness gate

OpenAI Responses remains the only bundled model adapter. A second adapter must
not be implemented merely because it can satisfy the small `Model` method
surface. Before selecting a provider, freeze a provider-neutral conformance
corpus covering:

- complete success, refusal, reasoning, tool-call, streaming, cancellation, and
  usage lifecycles;
- closed rejection of unknown union members, authority fields, contradictory
  identities, and partial completion evidence;
- stable provider error categories without leaking raw payloads into public
  events;
- immutable provider, model, endpoint class, credential principal, and adapter
  version binding for every durable run and approval resume;
- explicit behavior for transport failures before and after response evidence,
  with no runtime-wide retry, fallback, or provider downgrade.

The corpus and durable binding contract are the next roadmap deliverable. A
provider implementation follows only after those contracts can be applied
without changing the host-owned runtime boundary.
