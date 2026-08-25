# Operation schema and argument guide

Operation input schemas are JSON object schemas. Runtime compiles the declared
schema and an OpenAI-strict representation, then freezes the registry. A useful
baseline is:

```json
{
  "type": "object",
  "properties": {
    "value": { "type": "string", "minLength": 1, "maxLength": 64 },
    "note": { "type": ["string", "null"] }
  },
  "required": ["value"],
  "additionalProperties": false
}
```

- Top-level `type` must be `object`.
- Use `additionalProperties: false`; do not rely on silently ignored fields.
- Represent an optional strict property with a nullable type. Runtime may
  normalize omission to explicit `null` for provider strictness.
- Bound strings, arrays, and numeric ranges at the contract boundary.
- Output schemas must describe the complete executor result visible to the
  model. A mismatched output never becomes an accepted durable result.
- Bump `ContractVersion` whenever write normalization, preview, projection, or
  executor semantics change even if the JSON schema is unchanged.

Decode validated arguments without `map[string]any` assertions:

```go
type args struct {
    Value string  `json:"value"`
    Note  *string `json:"note"`
}

typed, err := agentruntime.DecodeArguments[args](request.Arguments)
```

`DecodeArguments` preserves exact JSON integers, rejects duplicate-key or
ambiguous JSON, rejects unknown Go fields, and rejects trailing values. The Go
type must therefore change with the schema rather than silently dropping new
properties.

Negative tests should cover unknown fields, missing required fields, an
out-of-range value, duplicate object keys, an executor output outside the
schema, and a normalized value that changes approval or execution identity.
