# ADR 0003: controlled resource access

Status: accepted for read-only Skill resources; GitHub snapshot fetching was
withdrawn from the runtime.

## Context

Skills may contain references and assets, while attachments may eventually need
PDF/audio forms. Resource access must not turn snapshot instructions into
arbitrary filesystem, network, or execution authority.

## Decision

`Skill.ReadFile` and `SkillSet.ReadFile` expose only immutable bytes already
copied, limited, hashed, and bound into the SkillSet. Paths must be normalized
relative paths and cannot traverse. Reading never reopens the live source and
never executes scripts.

The runtime's built-in Skill source is local and explicit: `NewLocalSource`
reads host-listed absolute directories. Remote acquisition, including GitHub
API access, credentials, retries, and ref resolution, is host-owned. A host
that needs remote content snapshots it into a local directory or implements a
trusted custom `skills.Source` that already returns an immutable confined
filesystem.

PDF/audio attachments remain unsupported until a provider-neutral content
contract defines durable identity, MIME verification, extraction limits,
malware handling, model support, expiry, replay, and public logging. The runtime
continues to reject them explicitly rather than treating them as text or image.

## Security boundary

- Host configuration chooses Skill directories, custom sources, and attachment
  resolvers.
- A Skill cannot request a new path or URL through model output.
- Snapshot files have instruction authority only when the host/application
  explicitly interprets them; `SKILL.md` is the only file injected by Runtime.

## Persistence and compatibility

Every file byte affects the SkillSet digest already bound to a session. The
read-only APIs add no new persisted format. Future attachment types require a
separate versioned model/transcript contract.

## Rejected alternatives

Live filesystem handles, arbitrary URL fetches, model-selected Git refs, and
executing snapshot scripts were rejected because they bypass immutable host
selection and session binding.
