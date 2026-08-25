package skills

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// ReadFile returns one immutable supporting file from this Skill snapshot.
// The path is slash-separated, relative to the Skill root, and must already be
// normalized. The returned bytes are a defensive copy and cannot mutate the
// content-addressed SkillSet.
func (skill Skill) ReadFile(filePath string) ([]byte, error) {
	if filePath != strings.TrimSpace(filePath) || !fs.ValidPath(filePath) || filePath == "." {
		return nil, fmt.Errorf("%w: Skill resource path must be a normalized relative file path", ErrInvalidArtifact)
	}
	if err := validateArtifactPath(filePath); err != nil {
		return nil, fmt.Errorf("%w: invalid Skill resource path", ErrInvalidArtifact)
	}
	index, found := sort.Find(len(skill.files), func(index int) int {
		return strings.Compare(filePath, skill.files[index].path)
	})
	if !found {
		return nil, &fs.PathError{Op: "read", Path: filePath, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), skill.files[index].data...), nil
}

// ReadFile resolves a Skill by its immutable name and reads one supporting
// resource. It never reads the live source filesystem or performs execution.
func (set *SkillSet) ReadFile(skillName, filePath string) ([]byte, error) {
	if set == nil {
		return nil, fmt.Errorf("%w: SkillSet is nil", ErrInvalidSkill)
	}
	if skillName != strings.TrimSpace(skillName) || !skillNamePattern.MatchString(skillName) {
		return nil, fmt.Errorf("%w: invalid Skill name", ErrInvalidSkill)
	}
	index, found := sort.Find(len(set.skills), func(index int) int {
		return strings.Compare(skillName, set.skills[index].name)
	})
	if !found {
		return nil, fmt.Errorf("%w: Skill %q does not exist", fs.ErrNotExist, skillName)
	}
	return set.skills[index].ReadFile(filePath)
}
