package skills

import (
	"context"
	"errors"
	"fmt"
)

// LocalSourceConfig lists exact host-selected Skill directories. Directories
// must be absolute; no discovery or recursive Skill search is performed.
type LocalSourceConfig struct {
	ID          string
	Directories []string
}

// LocalSource resolves explicitly configured local Skill directories.
type LocalSource struct {
	id          string
	directories []string
}

// NewLocalSource copies config and returns a source validated during LoadSet.
func NewLocalSource(config LocalSourceConfig) *LocalSource {
	return &LocalSource{id: config.ID, directories: append([]string(nil), config.Directories...)}
}

func (source *LocalSource) ID() string {
	if source == nil {
		return ""
	}
	return source.id
}

func (source *LocalSource) Resolve(ctx context.Context) ([]Artifact, error) {
	return source.resolveWithLimits(ctx, DefaultLimits())
}

func (source *LocalSource) resolveWithLimits(ctx context.Context, limits Limits) ([]Artifact, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: local source is nil", ErrInvalidSource)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidSource)
	}
	if len(source.directories) == 0 {
		return nil, fmt.Errorf("%w: local source %q requires at least one directory", ErrInvalidSource, source.id)
	}
	if len(source.directories) > limits.MaxSkills {
		return nil, fmt.Errorf("%w: local source contains more than %d Skill directories", ErrLimitExceeded, limits.MaxSkills)
	}
	artifacts := make([]Artifact, 0, len(source.directories))
	for _, directory := range source.directories {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, closeOwnedArtifacts(artifacts))
		}
		configured, root, err := configuredDirectory(directory, "local Skill")
		if err != nil {
			return nil, errors.Join(err, closeOwnedArtifacts(artifacts))
		}
		filesystem := &rootArtifactFS{root: root}
		artifacts = append(artifacts, Artifact{
			SourceID: source.id, Locator: configured, FS: filesystem,
		})
	}
	return artifacts, nil
}
