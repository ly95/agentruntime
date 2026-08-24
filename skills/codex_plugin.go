package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"unicode/utf8"
)

// CodexPluginSourceConfig lists exact installed Plugin directories. It never
// searches the Codex installation or user directories.
type CodexPluginSourceConfig struct {
	ID                string
	PluginDirectories []string
}

// CodexPluginSource extracts Skills named by Codex Plugin manifests.
type CodexPluginSource struct {
	id                string
	pluginDirectories []string
}

// NewCodexPluginSource copies config and returns a source validated by LoadSet.
func NewCodexPluginSource(config CodexPluginSourceConfig) *CodexPluginSource {
	return &CodexPluginSource{id: config.ID, pluginDirectories: append([]string(nil), config.PluginDirectories...)}
}

func (source *CodexPluginSource) ID() string {
	if source == nil {
		return ""
	}
	return source.id
}

func (source *CodexPluginSource) Resolve(ctx context.Context) ([]Artifact, error) {
	return source.resolveWithLimits(ctx, DefaultLimits())
}

func (source *CodexPluginSource) resolveWithLimits(ctx context.Context, limits Limits) ([]Artifact, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: Codex Plugin source is nil", ErrInvalidSource)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidSource)
	}
	if len(source.pluginDirectories) == 0 {
		return nil, fmt.Errorf("%w: Codex Plugin source %q requires at least one directory", ErrInvalidSource, source.id)
	}
	artifacts := make([]Artifact, 0)
	for _, directory := range source.pluginDirectories {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, closeOwnedArtifacts(artifacts))
		}
		if len(artifacts) >= limits.MaxSkills {
			return nil, errors.Join(
				fmt.Errorf("%w: Codex Plugin source contains more than %d Skills", ErrLimitExceeded, limits.MaxSkills),
				closeOwnedArtifacts(artifacts),
			)
		}
		configured, descriptor, err := configuredDirectory(directory, "Codex Plugin")
		if err != nil {
			return nil, errors.Join(err, closeOwnedArtifacts(artifacts))
		}
		pluginArtifacts, err := source.resolvePlugin(
			configured, &sharedRoot{root: descriptor}, limits, limits.MaxSkills-len(artifacts),
		)
		if err != nil {
			return nil, errors.Join(err, closeOwnedArtifacts(artifacts))
		}
		artifacts = append(artifacts, pluginArtifacts...)
	}
	return artifacts, nil
}

type codexPluginManifest struct {
	Version string `json:"version"`
	Skills  string `json:"skills"`
}

func (source *CodexPluginSource) resolvePlugin(
	configured string,
	rooted *sharedRoot,
	limits Limits,
	maximumSkills int,
) (artifacts []Artifact, err error) {
	ownedArtifacts := make([]Artifact, 0)
	succeeded := false
	defer func() {
		if abortErr := rooted.abort(); abortErr != nil {
			err = errors.Join(err, fmt.Errorf("skills: close Codex Plugin root: %w", abortErr))
			succeeded = false
		}
		if !succeeded {
			err = errors.Join(err, closeOwnedArtifacts(ownedArtifacts))
			artifacts = nil
		}
	}()
	pluginFS := rooted.FS()
	manifestBytes, err := readFSFile(pluginFS, ".codex-plugin/plugin.json", limits.MaxFileBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: read .codex-plugin/plugin.json: %w", ErrInvalidSource, err)
	}
	manifest, err := parseCodexPluginManifest(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid .codex-plugin/plugin.json: %v", ErrInvalidSource, err)
	}
	skillsRelative, err := normalizedPluginPath(manifest.Skills)
	if err != nil {
		return nil, err
	}
	info, err := rooted.inspect(skillsRelative)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: plugin skills path is not a directory or is outside the plugin root", ErrInvalidSource)
	}

	direct, err := probeSkillMarkdown(rooted, skillsRelative)
	if err != nil {
		return nil, err
	}
	if direct {
		if maximumSkills < 1 {
			return nil, fmt.Errorf("%w: Codex Plugin source contains more than %d Skills", ErrLimitExceeded, limits.MaxSkills)
		}
		artifactFS, err := rooted.sub(skillsRelative)
		if err != nil {
			return nil, err
		}
		ownedArtifacts = []Artifact{{
			SourceID: source.id, Locator: configured + "#" + skillsRelative,
			Revision: manifest.Version, FS: artifactFS,
		}}
		artifacts = ownedArtifacts
		succeeded = true
		return artifacts, nil
	}
	entries, err := readArtifactDirectory(pluginFS, skillsRelative, limits.MaxEntriesPerSkill)
	if err != nil {
		if errors.Is(err, ErrLimitExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: read plugin skills directory: %v", ErrInvalidSource, err)
	}
	for _, entry := range entries {
		if err := validatePluginEntryName(entry.Name()); err != nil {
			return nil, err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: symbolic link in plugin skills directory is not allowed", ErrInvalidSource)
		}
		if !entry.IsDir() {
			continue
		}
		skillRelative := path.Join(skillsRelative, entry.Name())
		present, err := probeSkillMarkdown(rooted, skillRelative)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if len(ownedArtifacts) >= maximumSkills {
			return nil, fmt.Errorf("%w: Codex Plugin source contains more than %d Skills", ErrLimitExceeded, limits.MaxSkills)
		}
		artifactFS, err := rooted.sub(skillRelative)
		if err != nil {
			return nil, err
		}
		ownedArtifacts = append(ownedArtifacts, Artifact{
			SourceID: source.id, Locator: configured + "#" + skillRelative,
			Revision: manifest.Version, FS: artifactFS,
		})
	}
	if len(ownedArtifacts) == 0 {
		return nil, fmt.Errorf("%w: Codex Plugin contains no skills", ErrInvalidSource)
	}
	artifacts = ownedArtifacts
	succeeded = true
	return artifacts, nil
}

func parseCodexPluginManifest(data []byte) (codexPluginManifest, error) {
	if !utf8.Valid(data) {
		return codexPluginManifest{}, fmt.Errorf("manifest is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := validateUniqueJSONValue(decoder, true); err != nil {
		return codexPluginManifest{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return codexPluginManifest{}, fmt.Errorf("manifest contains more than one JSON value")
		}
		return codexPluginManifest{}, err
	}
	var manifest codexPluginManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return codexPluginManifest{}, err
	}
	if err := validateOptionalNormalizedIdentity("manifest version", manifest.Version); err != nil {
		return codexPluginManifest{}, err
	}
	return manifest, nil
}

func validateUniqueJSONValue(decoder *json.Decoder, topLevel bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("manifest object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("manifest contains duplicate key %q", key)
			}
			seen[key] = struct{}{}
			if topLevel {
				for _, material := range []string{"skills", "version"} {
					if strings.EqualFold(key, material) && key != material {
						return fmt.Errorf("manifest field %q must use exact casing %q", key, material)
					}
				}
			}
			if err := validateUniqueJSONValue(decoder, false); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("manifest object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := validateUniqueJSONValue(decoder, false); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("manifest array is not closed")
		}
	default:
		return fmt.Errorf("manifest has an invalid JSON delimiter")
	}
	return nil
}

func validatePluginEntryName(name string) error {
	if err := validateControlFreeText("plugin skill directory name", name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	if name == "" || name == "." || name == ".." || !fs.ValidPath(name) || path.Base(name) != name ||
		strings.ContainsAny(name, `/\`) || hasWindowsVolumePrefix(name) {
		return fmt.Errorf("%w: plugin skill directory name is invalid", ErrInvalidSource)
	}
	return nil
}

func probeSkillMarkdown(rooted *sharedRoot, root string) (bool, error) {
	info, err := rooted.inspect(path.Join(root, "SKILL.md"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: inspect plugin SKILL.md: %v", ErrInvalidSource, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: plugin SKILL.md is not a regular file", ErrInvalidSource)
	}
	return true, nil
}

func normalizedPluginPath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: plugin manifest skills path is required", ErrInvalidSource)
	}
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("%w: plugin manifest skills path must be normalized", ErrInvalidSource)
	}
	if err := validateControlFreeText("plugin manifest skills path", value); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	if strings.Contains(value, `\`) {
		return "", fmt.Errorf("%w: plugin manifest skills path contains a backslash", ErrInvalidSource)
	}
	if strings.HasPrefix(value, "/") || hasWindowsVolumePrefix(value) {
		return "", fmt.Errorf("%w: plugin manifest skills path must be relative", ErrInvalidSource)
	}
	if strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	value = strings.TrimSuffix(value, "/")
	if value == "" {
		return "", fmt.Errorf("%w: plugin manifest skills path is required", ErrInvalidSource)
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return "", fmt.Errorf("%w: plugin manifest skills path must not contain ..", ErrInvalidSource)
		}
		if component == "" || component == "." {
			return "", fmt.Errorf("%w: plugin manifest skills path must be normalized", ErrInvalidSource)
		}
	}
	return value, nil
}
