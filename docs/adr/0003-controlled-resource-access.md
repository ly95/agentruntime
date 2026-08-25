# ADR 0003: controlled resource access

Status: accepted for read-only Skill resources and bounded GitHub snapshots;
deferred for PDF/audio model attachments.

## Context

Skills may contain references and assets, while attachments may eventually need
PDF/audio forms. Resource access must not turn snapshot instructions into
arbitrary filesystem, network, or execution authority.

## Decision

`Skill.ReadFile` and `SkillSet.ReadFile` expose only immutable bytes already
copied, limited, hashed, and bound into the SkillSet. Paths must be normalized
relative paths and cannot traverse. Reading never reopens the live source and
never executes scripts.

`HTTPGitHubFetcher` is an optional bounded implementation. It resolves a ref to
a commit, walks Git tree identities to the requested directory, rejects
truncated trees, symlinks, submodules, special entries, invalid paths, and blob
identity/size drift. It uses optional bearer authentication, rejects HTTP
redirects so credentials and object identity stay bound to the configured API
origin, performs no retry, and returns `GitHubRateLimitError` with reset/retry
scheduling data. Response
bodies and credentials never enter public errors.

PDF/audio attachments remain unsupported until a provider-neutral content
contract defines durable identity, MIME verification, extraction limits,
malware handling, model support, expiry, replay, and public logging. The runtime
continues to reject them explicitly rather than treating them as text or image.

## Security boundary

- Host configuration chooses repositories, refs, paths, credentials, and
  attachment resolvers.
- A Skill cannot request a new path or URL through model output.
- Snapshot files have instruction authority only when the host/application
  explicitly interprets them; `SKILL.md` is the only file injected by Runtime.
- Rate-limit/auth failures are visible; no sleep, retry, ref downgrade, parent
  tree fallback, or partial snapshot occurs.

## Persistence and compatibility

Commit SHA and every file byte affect the SkillSet digest already bound to a
session. The read-only APIs add no new persisted format. Future attachment
types require a separate versioned model/transcript contract.

## Rejected alternatives

Live filesystem handles, arbitrary URL fetches, model-selected Git refs, and
executing snapshot scripts were rejected because they bypass immutable host
selection and session binding.
