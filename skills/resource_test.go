package skills

import (
	"errors"
	"io/fs"
	"testing"
)

func TestSkillSetReadFileUsesImmutableSnapshot(t *testing.T) {
	root := testTempDir(t)
	directory := writeSkillDirectory(t, root, "resource", "resource-reader", "Read resources.", "Use the reference.", map[string]string{
		"references/example.txt": "snapshot value",
	})
	set, err := LoadSet(t.Context(), NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{directory}}))
	if err != nil {
		t.Fatal(err)
	}
	first, err := set.ReadFile("resource-reader", "references/example.txt")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'X'
	second, err := set.ReadFile("resource-reader", "references/example.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != "snapshot value" {
		t.Fatalf("resource=%q", second)
	}
	if _, err := set.ReadFile("resource-reader", "../secret"); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("traversal error=%v", err)
	}
	if _, err := set.ReadFile("missing", "SKILL.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing Skill error=%v", err)
	}
}
