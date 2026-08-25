package skills

import (
	"context"
	"errors"
	"io/fs"
)

const (
	defaultMaxSkillMarkdownBytes      int64 = 256 * 1024
	defaultMaxFileBytes               int64 = 1024 * 1024
	defaultMaxSkillBytes              int64 = 10 * 1024 * 1024
	defaultMaxFilesPerSkill                 = 256
	defaultMaxEntriesPerSkill               = 16 * 1024
	defaultMaxPathBytes                     = 4096
	defaultMaxPathDepth                     = 64
	defaultMaxSkills                        = 128
	defaultMaxTotalSkillMarkdownBytes int64 = 1024 * 1024
)

var (
	ErrInvalidSource   = errors.New("skills: invalid source")
	ErrInvalidArtifact = errors.New("skills: invalid artifact")
	ErrInvalidSkill    = errors.New("skills: invalid SKILL.md")
	ErrLimitExceeded   = errors.New("skills: resource limit exceeded")
	ErrNameConflict    = errors.New("skills: name conflict")
)

// Limits bounds one LoadSet operation. A zero value is invalid; callers that
// do not need custom bounds should use LoadSet, which applies DefaultLimits.
type Limits struct {
	MaxSkillMarkdownBytes      int64
	MaxFileBytes               int64
	MaxSkillBytes              int64
	MaxFilesPerSkill           int
	MaxEntriesPerSkill         int
	MaxPathBytes               int
	MaxPathDepth               int
	MaxSkills                  int
	MaxTotalSkillMarkdownBytes int64
}

func DefaultLimits() Limits {
	return Limits{
		MaxSkillMarkdownBytes:      defaultMaxSkillMarkdownBytes,
		MaxFileBytes:               defaultMaxFileBytes,
		MaxSkillBytes:              defaultMaxSkillBytes,
		MaxFilesPerSkill:           defaultMaxFilesPerSkill,
		MaxEntriesPerSkill:         defaultMaxEntriesPerSkill,
		MaxPathBytes:               defaultMaxPathBytes,
		MaxPathDepth:               defaultMaxPathDepth,
		MaxSkills:                  defaultMaxSkills,
		MaxTotalSkillMarkdownBytes: defaultMaxTotalSkillMarkdownBytes,
	}
}

// Source resolves explicitly configured locations into Skill-root artifacts.
// A custom Source is a trusted host adapter: it must return an immutable
// snapshot or an FS that confines and identity-checks every open. Artifacts
// returned by the built-in local source must either be consumed through LoadSet,
// which releases source-owned filesystem handles after copying, or closed by
// calling Close on every returned Artifact. LoadSet owns parsing, validation,
// copying, limits, conflicts, and hashing.
type Source interface {
	ID() string
	Resolve(ctx context.Context) ([]Artifact, error)
}

// Artifact is a transient source result. FS must be rooted at a directory
// containing SKILL.md and satisfy Source's snapshot/confinement contract.
// LoadSet copies every file, closes built-in local-source resources, and never
// retains FS. Callers that invoke LocalSource.Resolve directly must call Close
// on every returned Artifact. Close is idempotent and is a no-op for
// caller-owned custom-source filesystems.
type Artifact struct {
	SourceID string
	Locator  string
	Revision string
	FS       fs.FS
}

// Close releases filesystem resources owned by the built-in local source.
func (artifact Artifact) Close() error {
	if closer, ok := artifact.FS.(artifactFSCloser); ok {
		return closer.closeArtifactFS()
	}
	return nil
}

// File is one immutable file in a Skill snapshot.
type File struct {
	path string
	data []byte
}

func (file File) Path() string { return file.path }
func (file File) Size() int64  { return int64(len(file.data)) }
func (file File) Bytes() []byte {
	return append([]byte(nil), file.data...)
}

// Skill is an immutable, parsed, content-addressed Skill snapshot.
type Skill struct {
	sourceID     string
	locator      string
	revision     string
	name         string
	description  string
	instructions string
	digest       string
	files        []File
}

func (skill Skill) SourceID() string     { return skill.sourceID }
func (skill Skill) Locator() string      { return skill.locator }
func (skill Skill) Revision() string     { return skill.revision }
func (skill Skill) Name() string         { return skill.name }
func (skill Skill) Description() string  { return skill.description }
func (skill Skill) Instructions() string { return skill.instructions }
func (skill Skill) Digest() string       { return skill.digest }
func (skill Skill) Files() []File        { return cloneFiles(skill.files) }

// SkillSet is an immutable, deterministically ordered group of Skills.
type SkillSet struct {
	id     string
	skills []Skill
}

func (set *SkillSet) ID() string {
	if set == nil {
		return ""
	}
	return set.id
}

func (set *SkillSet) Len() int {
	if set == nil {
		return 0
	}
	return len(set.skills)
}

func (set *SkillSet) Skills() []Skill {
	if set == nil {
		return nil
	}
	return append([]Skill(nil), set.skills...)
}

func cloneFiles(files []File) []File {
	out := make([]File, len(files))
	for index := range files {
		out[index] = File{path: files[index].path, data: append([]byte(nil), files[index].data...)}
	}
	return out
}
