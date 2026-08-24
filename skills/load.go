package skills

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type limitAwareSource interface {
	resolveWithLimits(context.Context, Limits) ([]Artifact, error)
}

// LoadSet resolves the configured sources in order and returns one immutable,
// deterministically ordered SkillSet using DefaultLimits.
func LoadSet(ctx context.Context, sources ...Source) (*SkillSet, error) {
	return LoadSetWithLimits(ctx, DefaultLimits(), sources...)
}

// LoadSetWithLimits is LoadSet with caller-selected hard limits. Exceeding a
// limit fails the entire load; content is never truncated.
func LoadSetWithLimits(ctx context.Context, limits Limits, sources ...Source) (*SkillSet, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidSource)
	}
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: at least one source is required", ErrInvalidSource)
	}

	loaded := make([]Skill, 0)
	seenSources := make(map[string]struct{}, len(sources))
	sourceIDs := make([]string, len(sources))
	for index, source := range sources {
		if isNilValue(source) {
			return nil, fmt.Errorf("%w: source %d is nil", ErrInvalidSource, index)
		}
		sourceID := source.ID()
		if err := validateNormalizedIdentity("source ID", sourceID); err != nil {
			return nil, fmt.Errorf("%w: source %d: %v", ErrInvalidSource, index, err)
		}
		if _, exists := seenSources[sourceID]; exists {
			return nil, fmt.Errorf("%w: duplicate source ID %q", ErrInvalidSource, sourceID)
		}
		seenSources[sourceID] = struct{}{}
		sourceIDs[index] = sourceID
	}

	seenNames := make(map[string]Skill)
	var totalSkillMarkdownBytes int64
	for index, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sourceID := sourceIDs[index]
		remainingSkills := limits.MaxSkills - len(loaded)
		if remainingSkills <= 0 {
			return nil, fmt.Errorf("%w: SkillSet contains more than %d skills", ErrLimitExceeded, limits.MaxSkills)
		}
		remainingMarkdownBytes := limits.MaxTotalSkillMarkdownBytes - totalSkillMarkdownBytes
		if remainingMarkdownBytes <= 0 {
			return nil, fmt.Errorf("%w: total SKILL.md bytes exceed %d", ErrLimitExceeded, limits.MaxTotalSkillMarkdownBytes)
		}
		sourceLimits := limits
		sourceLimits.MaxSkills = remainingSkills
		if remainingMarkdownBytes < sourceLimits.MaxSkillMarkdownBytes {
			sourceLimits.MaxSkillMarkdownBytes = remainingMarkdownBytes
		}

		artifacts, err := resolveSource(ctx, source, sourceLimits)
		if err != nil {
			var markdownLimit *skillMarkdownLimitError
			if remainingMarkdownBytes < limits.MaxSkillMarkdownBytes && errors.As(err, &markdownLimit) {
				err = fmt.Errorf("%w: total SKILL.md bytes exceed %d", ErrLimitExceeded, limits.MaxTotalSkillMarkdownBytes)
			} else {
				err = fmt.Errorf("skills: source %q: %w", sourceID, err)
			}
			return nil, errors.Join(err, closeOwnedArtifacts(artifacts))
		}
		if len(artifacts) == 0 {
			return nil, fmt.Errorf("%w: source %q returned no artifacts", ErrInvalidSource, sourceID)
		}
		if err := func() (result error) {
			defer func() {
				result = errors.Join(result, closeOwnedArtifacts(artifacts))
			}()
			for artifactIndex, artifact := range artifacts {
				if len(loaded) >= limits.MaxSkills {
					return fmt.Errorf("%w: SkillSet contains more than %d skills", ErrLimitExceeded, limits.MaxSkills)
				}
				if artifact.SourceID != sourceID {
					return fmt.Errorf("%w: source %q artifact %d SourceID does not match", ErrInvalidArtifact, sourceID, artifactIndex)
				}
				if err := validateNormalizedIdentity("locator", artifact.Locator); err != nil {
					return fmt.Errorf("%w: source %q artifact %d: %v", ErrInvalidArtifact, sourceID, artifactIndex, err)
				}
				if err := validateOptionalNormalizedIdentity("revision", artifact.Revision); err != nil {
					return fmt.Errorf("%w: source %q artifact %d: %v", ErrInvalidArtifact, sourceID, artifactIndex, err)
				}
				if isNilValue(artifact.FS) {
					return fmt.Errorf("%w: source %q artifact %d filesystem is required", ErrInvalidArtifact, sourceID, artifactIndex)
				}

				remainingMarkdownBytes = limits.MaxTotalSkillMarkdownBytes - totalSkillMarkdownBytes
				if remainingMarkdownBytes <= 0 {
					return fmt.Errorf("%w: total SKILL.md bytes exceed %d", ErrLimitExceeded, limits.MaxTotalSkillMarkdownBytes)
				}
				artifactLimits := limits
				aggregateConstrained := remainingMarkdownBytes < artifactLimits.MaxSkillMarkdownBytes
				if aggregateConstrained {
					artifactLimits.MaxSkillMarkdownBytes = remainingMarkdownBytes
				}
				skill, markdownBytes, err := parseArtifact(artifact, artifactLimits)
				if err != nil {
					var markdownLimit *skillMarkdownLimitError
					if aggregateConstrained && errors.As(err, &markdownLimit) {
						return fmt.Errorf("%w: total SKILL.md bytes exceed %d", ErrLimitExceeded, limits.MaxTotalSkillMarkdownBytes)
					}
					return fmt.Errorf("skills: source %q artifact %d: %w", sourceID, artifactIndex, err)
				}
				if markdownBytes > limits.MaxTotalSkillMarkdownBytes-totalSkillMarkdownBytes {
					return fmt.Errorf("%w: total SKILL.md bytes exceed %d", ErrLimitExceeded, limits.MaxTotalSkillMarkdownBytes)
				}
				totalSkillMarkdownBytes += markdownBytes
				if existing, exists := seenNames[skill.name]; exists {
					return fmt.Errorf("%w: skill %q from source %q conflicts with source %q", ErrNameConflict, skill.name, sourceID, existing.sourceID)
				}
				seenNames[skill.name] = skill
				loaded = append(loaded, skill)
			}
			return nil
		}(); err != nil {
			return nil, err
		}
	}

	sort.Slice(loaded, func(left, right int) bool {
		if loaded[left].sourceID != loaded[right].sourceID {
			return loaded[left].sourceID < loaded[right].sourceID
		}
		if loaded[left].name != loaded[right].name {
			return loaded[left].name < loaded[right].name
		}
		return loaded[left].locator < loaded[right].locator
	})
	return &SkillSet{id: digestSkillSet(loaded), skills: loaded}, nil
}

func resolveSource(ctx context.Context, source Source, limits Limits) ([]Artifact, error) {
	if aware, ok := source.(limitAwareSource); ok {
		return aware.resolveWithLimits(ctx, limits)
	}
	return source.Resolve(ctx)
}

func validateLimits(limits Limits) error {
	if limits.MaxSkillMarkdownBytes <= 0 || limits.MaxFileBytes <= 0 || limits.MaxSkillBytes <= 0 ||
		limits.MaxFilesPerSkill <= 0 || limits.MaxEntriesPerSkill <= 0 || limits.MaxPathBytes <= 0 ||
		limits.MaxPathDepth <= 0 || limits.MaxSkills <= 0 || limits.MaxTotalSkillMarkdownBytes <= 0 {
		return fmt.Errorf("%w: every limit must be positive", ErrInvalidSource)
	}
	return nil
}

func validateNormalizedIdentity(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be normalized", name)
	}
	if err := validateControlFreeText(name, value); err != nil {
		return err
	}
	return nil
}

func validateOptionalNormalizedIdentity(name, value string) error {
	if value == "" {
		return nil
	}
	return validateNormalizedIdentity(name, value)
}

func validateControlFreeText(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s must not contain control characters", name)
		}
	}
	return nil
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func digestSkillSet(skills []Skill) string {
	hasher := sha256.New()
	writeHashField(hasher, "skill-count", []byte(strconv.Itoa(len(skills))))
	for _, skill := range skills {
		writeHashField(hasher, "skill-digest", []byte(skill.digest))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func writeHashField(hasher hash.Hash, label string, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(label)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write([]byte(label))
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hasher.Write(size[:])
	_, _ = hasher.Write(value)
}
