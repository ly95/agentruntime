package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalSourceLoadsOnlyExplicitAbsoluteDirectories(t *testing.T) {
	requireDescriptorRoot(t)
	root := testTempDir(t)
	first := writeSkillDirectory(t, root, "first", "first", "First skill.", "First body.", map[string]string{"references/one.md": "one"})
	second := writeSkillDirectory(t, root, "second", "second", "Second skill.", "Second body.", nil)

	trapRoot := testTempDir(t)
	writeSkillDirectory(t, trapRoot, ".agents/skills", "unconfigured-home", "Must not load.", "trap", nil)
	writeSkillDirectory(t, trapRoot, ".codex/skills", "unconfigured-codex", "Must not load.", "trap", nil)
	writeSkillDirectory(t, trapRoot, "working-skill", "unconfigured-cwd", "Must not load.", "trap", nil)
	t.Setenv("HOME", trapRoot)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(trapRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	set, err := LoadSet(t.Context(), NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{second, first}}))
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	got := set.Skills()
	if len(got) != 2 || got[0].Name() != "first" || got[1].Name() != "second" {
		t.Fatalf("skills=%v", []string{got[0].Name(), got[1].Name()})
	}
}

func TestLocalSourceAcceptsEmptyFileAtExactSkillByteLimit(t *testing.T) {
	requireDescriptorRoot(t)
	markdown := skillMarkdown("exact-built-in", "Exact built-in boundary.", "Body.")
	limits := DefaultLimits()
	limits.MaxSkillBytes = int64(len(markdown))
	root := testTempDir(t)
	directory := writeSkillDirectory(t, root, "skill", "exact-built-in", "Exact built-in boundary.", "Body.", map[string]string{"empty.txt": ""})
	set, err := LoadSetWithLimits(t.Context(), limits, NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{directory}}))
	if err != nil || set == nil || set.Len() != 1 {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestLocalSourceResolveRejectsNilContext(t *testing.T) {
	source := NewLocalSource(LocalSourceConfig{ID: "local"})
	//lint:ignore SA1012 This test verifies that LocalSource rejects a nil context explicitly.
	if artifacts, err := source.Resolve(nil); err == nil || artifacts != nil || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("artifacts=%v error=%v", artifacts, err)
	}
}

func TestLocalSourceRejectsInvalidDirectoriesAtomically(t *testing.T) {
	requireDescriptorRoot(t)
	root := testTempDir(t)
	valid := writeSkillDirectory(t, root, "valid", "valid", "Valid skill.", "Valid body.", nil)
	missingSkill := filepath.Join(root, "missing-skill")
	if err := os.MkdirAll(missingSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(missingSkill, "meta.yaml"), "name: metadata-only")
	nestedOnly := filepath.Join(root, "nested-only")
	writeSkillDirectory(t, nestedOnly, "child", "nested", "Nested skill.", "Nested body.", nil)

	tests := []struct {
		name        string
		directories []string
		want        string
	}{
		{name: "relative", directories: []string{"relative/skill"}, want: "absolute"},
		{name: "missing", directories: []string{filepath.Join(root, "does-not-exist")}, want: "does not exist"},
		{name: "metadata only", directories: []string{missingSkill}, want: "SKILL.md"},
		{name: "nested only", directories: []string{nestedOnly}, want: "SKILL.md"},
		{name: "dot dot", directories: []string{root + string(filepath.Separator) + "valid" + string(filepath.Separator) + ".." + string(filepath.Separator) + "valid"}, want: ".."},
		{name: "one invalid loses all", directories: []string{valid, missingSkill}, want: "SKILL.md"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), NewLocalSource(LocalSourceConfig{ID: "local", Directories: test.directories}))
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("set=%v error=%v, want %q", set, err, test.want)
			}
		})
	}
}

func TestLocalSourceRejectsNonCanonicalConfiguredDirectories(t *testing.T) {
	requireDescriptorRoot(t)
	root := testTempDir(t)
	local := writeSkillDirectory(t, root, "canonical-local", "canonical-local", "Canonical local.", "Body.", nil)

	aliases := func(directory string) []string {
		separator := string(filepath.Separator)
		return []string{
			directory + separator + ".",
			filepath.Dir(directory) + separator + separator + filepath.Base(directory),
			directory + separator,
		}
	}
	for index, alias := range aliases(local) {
		t.Run(fmt.Sprintf("%d", index), func(t *testing.T) {
			set, err := LoadSet(t.Context(), NewLocalSource(LocalSourceConfig{ID: "local-alias", Directories: []string{alias}}))
			if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) || !strings.Contains(err.Error(), "canonical") {
				t.Fatalf("alias=%q set=%v error=%v", alias, set, err)
			}
		})
	}
}

func TestLocalSourceEnforcesSkillLimitBeforeOpeningDirectories(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSkills = 1
	missingRoot := filepath.Join(testTempDir(t), "missing")
	set, err := LoadSetWithLimits(t.Context(), limits, NewLocalSource(LocalSourceConfig{
		ID: "local", Directories: []string{filepath.Join(missingRoot, "one"), filepath.Join(missingRoot, "two")},
	}))
	if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestLocalSourceRejectsSymlinkEscape(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	root := testTempDir(t)
	skill := writeSkillDirectory(t, root, "skill", "linked", "Linked skill.", "Linked body.", nil)
	outside := filepath.Join(testTempDir(t), "secret.txt")
	writeTestFile(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(skill, "references-secret")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	set, err := LoadSet(t.Context(), NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{skill}}))
	if err == nil || set != nil || !strings.Contains(strings.ToLower(err.Error()), "symbolic link") {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestConfiguredRootsRejectSymbolicLinkComponents(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}

	parent := testTempDir(t)
	localTarget := writeSkillDirectory(t, parent, "local-target", "local-target", "Local target.", "Body.", nil)
	localAlias := filepath.Join(parent, "local-alias")
	if err := os.Symlink(localTarget, localAlias); err != nil {
		t.Fatal(err)
	}

	realParent := filepath.Join(parent, "real-parent")
	intermediateTarget := writeSkillDirectory(t, realParent, "skill", "intermediate-target", "Intermediate target.", "Body.", nil)
	aliasParent := filepath.Join(parent, "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	intermediateAlias := filepath.Join(aliasParent, filepath.Base(intermediateTarget))

	tests := []struct {
		name   string
		source Source
	}{
		{name: "local root", source: NewLocalSource(LocalSourceConfig{ID: "local-root", Directories: []string{localAlias}})},
		{name: "local intermediate", source: NewLocalSource(LocalSourceConfig{ID: "local-intermediate", Directories: []string{intermediateAlias}})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), test.source)
			if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("set=%v error=%v, want ErrInvalidSource", set, err)
			}
		})
	}
}

func TestLocalSourceResolveArtifactsAreCallerClosable(t *testing.T) {
	requireDescriptorRoot(t)
	root := testTempDir(t)
	localSkill := writeSkillDirectory(t, root, "local", "local-close", "Local close.", "Body.", nil)

	artifacts, err := NewLocalSource(LocalSourceConfig{ID: "local-close", Directories: []string{localSkill}}).Resolve(t.Context())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts=%d, want 1", len(artifacts))
	}
	closer, ok := any(artifacts[0]).(interface{ Close() error })
	if !ok {
		t.Fatal("Artifact does not expose Close")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if file, err := artifacts[0].FS.Open("SKILL.md"); err == nil {
		_ = file.Close()
		t.Fatal("artifact filesystem remained usable after Close")
	}
}

func TestLocalSourceSnapshotCannotBeRedirectedAfterResolve(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	parent := testTempDir(t)
	configured := writeSkillDirectory(t, parent, "configured", "original", "Original skill.", "ORIGINAL_BODY", nil)
	outside := writeSkillDirectory(t, testTempDir(t), "outside", "escaped", "Escaped skill.", "OUTSIDE_SECRET", nil)
	backup := filepath.Join(parent, "original-directory")
	source := afterResolveSource{
		source: NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{configured}}),
		after: func() {
			if err := os.Rename(configured, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, configured); err != nil {
				t.Fatal(err)
			}
		},
	}
	set, err := LoadSet(t.Context(), source)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if got := set.Skills()[0]; got.Name() != "original" || strings.Contains(got.Instructions(), "OUTSIDE_SECRET") {
		t.Fatalf("snapshot followed replacement outside its root: name=%q instructions=%q", got.Name(), got.Instructions())
	}
}

func TestLocalSnapshotRejectsInternalSymlinkSwapBetweenInspectionAndOpen(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	skillRoot := writeSkillDirectory(t, testTempDir(t), "skill", "internal-swap", "Internal swap.", "Body.", map[string]string{
		"a.txt": "original",
		"z.txt": "replacement",
	})
	rooted, err := openTestRootArtifactFS(skillRoot)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &swapOnOpenFS{
		filesystem: rooted,
		target:     "a.txt",
		swap: func() {
			asset := filepath.Join(skillRoot, "a.txt")
			if err := os.Rename(asset, filepath.Join(skillRoot, "a-original.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("z.txt", asset); err != nil {
				t.Fatal(err)
			}
		},
	}
	set, err := LoadSet(t.Context(), staticSource{id: "local-swap", artifacts: []Artifact{
		artifactWithFS("local-swap", "local-swap", "", filesystem),
	}})
	if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestLocalSnapshotRejectsIntermediateSymlinkSwapBetweenInspectionAndOpen(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	skillRoot := writeSkillDirectory(t, testTempDir(t), "skill", "intermediate-swap", "Intermediate swap.", "Body.", map[string]string{
		"references/a.txt":  "original",
		"replacement/a.txt": "replacement",
	})
	rooted, err := openTestRootArtifactFS(skillRoot)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &swapOnOpenFS{
		filesystem: rooted,
		target:     "references/a.txt",
		swap: func() {
			references := filepath.Join(skillRoot, "references")
			if err := os.Rename(references, filepath.Join(skillRoot, "references-original")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("replacement", references); err != nil {
				t.Fatal(err)
			}
		},
	}
	set, err := LoadSet(t.Context(), staticSource{id: "local-intermediate-swap", artifacts: []Artifact{
		artifactWithFS("local-intermediate-swap", "local-intermediate-swap", "", filesystem),
	}})
	if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestDescriptorRootPinsOpenedIntermediateDirectory(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	parent := testTempDir(t)
	container := filepath.Join(parent, "container")
	original := writeSkillDirectory(t, container, "mounted", "original", "Original skill.", "ORIGINAL_BODY", nil)
	writeSkillDirectory(t, container, "replacement", "replacement", "Replacement skill.", "REPLACEMENT_BODY", nil)
	rooted, err := openTestSharedRoot(container)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rooted.abort() })
	pinned, err := rooted.sub("mounted")
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(container, "mounted-original")
	if err := os.Rename(original, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("replacement", original); err != nil {
		t.Fatal(err)
	}

	set, err := LoadSet(t.Context(), staticSource{id: "pinned", artifacts: []Artifact{
		artifactWithFS("pinned", "pinned", "", pinned),
	}})
	if err != nil {
		t.Fatalf("LoadSet pinned directory: %v", err)
	}
	if skill := set.Skills()[0]; skill.Name() != "original" || skill.Instructions() != "ORIGINAL_BODY" {
		t.Fatalf("pinned directory was redirected: name=%q instructions=%q", skill.Name(), skill.Instructions())
	}
	if file, err := rooted.FS().Open("mounted/SKILL.md"); err == nil {
		_ = file.Close()
		t.Fatal("base traversal unexpectedly followed replacement symlink")
	}
}

func TestConfiguredDirectoryPinsRootBeforePathReplacement(t *testing.T) {
	requireDescriptorRoot(t)
	parent := testTempDir(t)
	configured := writeSkillDirectory(t, parent, "configured", "original", "Original skill.", "ORIGINAL_BODY", nil)
	replacement := writeSkillDirectory(t, parent, "replacement", "replacement", "Replacement skill.", "REPLACEMENT_BODY", nil)
	_, descriptor, err := configuredDirectory(configured, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(configured, filepath.Join(parent, "configured-original")); err != nil {
		_ = descriptor.close()
		t.Fatal(err)
	}
	if err := os.Rename(replacement, configured); err != nil {
		_ = descriptor.close()
		t.Fatal(err)
	}
	set, err := LoadSet(t.Context(), staticSource{id: "pinned-root", artifacts: []Artifact{
		artifactWithFS("pinned-root", "pinned-root", "", &rootArtifactFS{root: descriptor}),
	}})
	if err != nil {
		t.Fatalf("LoadSet pinned root: %v", err)
	}
	if skill := set.Skills()[0]; skill.Name() != "original" || skill.Instructions() != "ORIGINAL_BODY" {
		t.Fatalf("configured root was redirected: name=%q instructions=%q", skill.Name(), skill.Instructions())
	}
}

func TestConfinedSnapshotRejectsFileSwapBetweenInspectionAndOpen(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	parent := testTempDir(t)
	skillRoot := writeSkillDirectory(t, parent, "skill", "confined", "Confined skill.", "Confined body.", map[string]string{
		"asset.txt": "inside",
	})
	outside := filepath.Join(testTempDir(t), "secret.txt")
	writeTestFile(t, outside, "TOCTOU_OUTSIDE_SECRET")
	rooted, err := openTestRootArtifactFS(skillRoot)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &swapOnOpenFS{
		filesystem: rooted,
		target:     "asset.txt",
		swap: func() {
			asset := filepath.Join(skillRoot, "asset.txt")
			if err := os.Rename(asset, filepath.Join(skillRoot, "asset-original.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, asset); err != nil {
				t.Fatal(err)
			}
		},
	}
	set, err := LoadSet(t.Context(), staticSource{id: "confined", artifacts: []Artifact{
		artifactWithFS("confined", "confined", "", filesystem),
	}})
	if err == nil || set != nil {
		t.Fatalf("set=%v error=%v", set, err)
	}
	if strings.Contains(err.Error(), "TOCTOU_OUTSIDE_SECRET") {
		t.Fatalf("outside content leaked through error: %v", err)
	}
}
