package skills

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testCommitSHA = "0123456789abcdef0123456789abcdef01234567"

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

func TestBuiltInSourcesAcceptEmptyFileAtExactSkillByteLimit(t *testing.T) {
	markdown := skillMarkdown("exact-built-in", "Exact built-in boundary.", "Body.")
	limits := DefaultLimits()
	limits.MaxSkillBytes = int64(len(markdown))

	t.Run("local", func(t *testing.T) {
		requireDescriptorRoot(t)
		root := testTempDir(t)
		directory := writeSkillDirectory(t, root, "skill", "exact-built-in", "Exact built-in boundary.", "Body.", map[string]string{"empty.txt": ""})
		set, err := LoadSetWithLimits(t.Context(), limits, NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{directory}}))
		if err != nil || set == nil || set.Len() != 1 {
			t.Fatalf("set=%v error=%v", set, err)
		}
	})

	t.Run("github", func(t *testing.T) {
		fetcher := &fakeGitHubFetcher{result: GitHubFetchResult{CommitSHA: testCommitSHA, Files: []GitHubFile{
			{Path: "SKILL.md", Data: []byte(markdown)},
			{Path: "empty.txt", Data: []byte{}},
		}}}
		set, err := LoadSetWithLimits(t.Context(), limits, NewGitHubSource(GitHubSourceConfig{
			ID: "github", Repository: "owner/repository", Ref: "main", Path: "skills/exact", Fetcher: fetcher,
		}))
		if err != nil || set == nil || set.Len() != 1 {
			t.Fatalf("set=%v error=%v", set, err)
		}
	})
}

func TestBuiltInSourceResolveRejectsNilContext(t *testing.T) {
	sources := []Source{
		NewLocalSource(LocalSourceConfig{ID: "local"}),
		NewGitHubSource(GitHubSourceConfig{ID: "github"}),
		NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex"}),
	}
	for _, source := range sources {
		if artifacts, err := source.Resolve(nil); err == nil || artifacts != nil || !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("source=%T artifacts=%v error=%v", source, artifacts, err)
		}
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

func TestBuiltInSourcesRejectNonCanonicalConfiguredDirectories(t *testing.T) {
	requireDescriptorRoot(t)
	root := testTempDir(t)
	local := writeSkillDirectory(t, root, "canonical-local", "canonical-local", "Canonical local.", "Body.", nil)
	plugin := filepath.Join(root, "canonical-plugin")
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin/plugin.json"), `{"version":"1.0.0","skills":"./skills/"}`)
	writeSkillDirectory(t, plugin, "skills/review", "canonical-plugin", "Canonical plugin.", "Body.", nil)

	aliases := func(directory string) []string {
		separator := string(filepath.Separator)
		return []string{
			directory + separator + ".",
			filepath.Dir(directory) + separator + separator + filepath.Base(directory),
			directory + separator,
		}
	}
	for _, test := range []struct {
		name   string
		paths  []string
		source func(string) Source
	}{
		{
			name: "local", paths: aliases(local),
			source: func(directory string) Source {
				return NewLocalSource(LocalSourceConfig{ID: "local-alias", Directories: []string{directory}})
			},
		},
		{
			name: "Codex Plugin", paths: aliases(plugin),
			source: func(directory string) Source {
				return NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex-alias", PluginDirectories: []string{directory}})
			},
		},
	} {
		for index, alias := range test.paths {
			t.Run(fmt.Sprintf("%s/%d", test.name, index), func(t *testing.T) {
				set, err := LoadSet(t.Context(), test.source(alias))
				if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) || !strings.Contains(err.Error(), "canonical") {
					t.Fatalf("alias=%q set=%v error=%v", alias, set, err)
				}
			})
		}
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

	pluginTarget := filepath.Join(parent, "plugin-target")
	writeTestFile(t, filepath.Join(pluginTarget, ".codex-plugin/plugin.json"), `{"version":"1.0.0","skills":"./skills/"}`)
	writeSkillDirectory(t, pluginTarget, "skills/review", "plugin-target", "Plugin target.", "Body.", nil)
	pluginAlias := filepath.Join(parent, "plugin-alias")
	if err := os.Symlink(pluginTarget, pluginAlias); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		source Source
	}{
		{name: "local root", source: NewLocalSource(LocalSourceConfig{ID: "local-root", Directories: []string{localAlias}})},
		{name: "local intermediate", source: NewLocalSource(LocalSourceConfig{ID: "local-intermediate", Directories: []string{intermediateAlias}})},
		{name: "Codex Plugin root", source: NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex-root", PluginDirectories: []string{pluginAlias}})},
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

func TestBuiltInResolveArtifactsAreCallerClosable(t *testing.T) {
	requireDescriptorRoot(t)
	root := testTempDir(t)
	localSkill := writeSkillDirectory(t, root, "local", "local-close", "Local close.", "Body.", nil)
	plugin := filepath.Join(root, "plugin")
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin/plugin.json"), `{"version":"1.0.0","skills":"./skills/"}`)
	writeSkillDirectory(t, plugin, "skills/review", "plugin-close", "Plugin close.", "Body.", nil)

	for _, test := range []struct {
		name   string
		source Source
	}{
		{name: "local", source: NewLocalSource(LocalSourceConfig{ID: "local-close", Directories: []string{localSkill}})},
		{name: "Codex Plugin", source: NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex-close", PluginDirectories: []string{plugin}})},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifacts, err := test.source.Resolve(t.Context())
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
		})
	}
}

func TestGitHubSourceUsesInjectedFetcherAndResolvedCommit(t *testing.T) {
	fetcher := &fakeGitHubFetcher{result: GitHubFetchResult{
		CommitSHA: testCommitSHA,
		Files: []GitHubFile{
			{Path: "SKILL.md", Data: []byte(skillMarkdown("security-review", "Review security changes.", "Trace inputs to sinks."))},
			{Path: "scripts/check.sh", Data: []byte("must-not-run")},
		},
	}}
	set, err := LoadSet(t.Context(), NewGitHubSource(GitHubSourceConfig{
		ID: "github", Repository: "owner/repository", Ref: "v1.2.0", Path: "skills/security-review", Fetcher: fetcher,
	}))
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if len(fetcher.requests) != 1 || fetcher.requests[0] != (GitHubFetchRequest{Repository: "owner/repository", Ref: "v1.2.0", Path: "skills/security-review"}) {
		t.Fatalf("requests=%+v", fetcher.requests)
	}
	skill := set.Skills()[0]
	if skill.Revision() != testCommitSHA || skill.Locator() != "owner/repository@"+testCommitSHA+":skills/security-review" {
		t.Fatalf("revision=%q locator=%q", skill.Revision(), skill.Locator())
	}
}

func TestGitHubSourceCopiesFetcherFilesBeforeParsing(t *testing.T) {
	markdown := []byte(skillMarkdown("immutable", "Immutable snapshot.", "Original body."))
	files := []GitHubFile{{Path: "SKILL.md", Data: markdown}}
	fetcher := &fakeGitHubFetcher{result: GitHubFetchResult{CommitSHA: testCommitSHA, Files: files}}
	source := afterResolveSource{
		source: NewGitHubSource(GitHubSourceConfig{
			ID: "github", Repository: "owner/repository", Ref: "main", Path: "skills/immutable", Fetcher: fetcher,
		}),
		after: func() {
			for index := range markdown {
				markdown[index] = 'x'
			}
			files[0].Path = "redirected.md"
			files[0].Data = []byte("replacement")
		},
	}
	set, err := LoadSet(t.Context(), source)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if set.Len() != 1 || set.Skills()[0].Name() != "immutable" || set.Skills()[0].Instructions() != "Original body." {
		t.Fatalf("set=%+v", set)
	}
}

func TestGitHubSourceRejectsMalformedFileSnapshots(t *testing.T) {
	skill := GitHubFile{Path: "SKILL.md", Data: []byte(skillMarkdown("malformed", "Malformed provider output.", "Body."))}
	tests := []struct {
		name  string
		files []GitHubFile
	}{
		{name: "parent traversal", files: []GitHubFile{skill, {Path: "../secret", Data: []byte("secret")}}},
		{name: "absolute path", files: []GitHubFile{skill, {Path: "/secret", Data: []byte("secret")}}},
		{name: "duplicate", files: []GitHubFile{skill, skill}},
		{name: "file before child", files: []GitHubFile{skill, {Path: "collision", Data: []byte("file")}, {Path: "collision/child", Data: []byte("child")}}},
		{name: "child before file", files: []GitHubFile{skill, {Path: "collision/child", Data: []byte("child")}, {Path: "collision", Data: []byte("file")}}},
		{name: "symbolic link", files: []GitHubFile{skill, {Path: "link", Data: []byte("target"), Mode: fs.ModeSymlink}}},
		{name: "directory", files: []GitHubFile{skill, {Path: "directory", Mode: fs.ModeDir}}},
		{name: "submodule", files: []GitHubFile{skill, {Path: "module", Mode: fs.ModeIrregular}}},
		{name: "raw Git regular mode", files: []GitHubFile{skill, {Path: "regular", Mode: 0o100644}}},
		{name: "raw Git symbolic-link mode", files: []GitHubFile{skill, {Path: "link", Mode: 0o120000}}},
		{name: "raw Git submodule mode", files: []GitHubFile{skill, {Path: "module", Mode: 0o160000}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetcher := &fakeGitHubFetcher{result: GitHubFetchResult{CommitSHA: testCommitSHA, Files: test.files}}
			set, err := LoadSet(t.Context(), NewGitHubSource(GitHubSourceConfig{
				ID: "github", Repository: "owner/repository", Ref: "main", Path: "skills/malformed", Fetcher: fetcher,
			}))
			if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("set=%v error=%v", set, err)
			}
		})
	}
}

func TestGitHubSnapshotReadDirHandlesMaxIntCount(t *testing.T) {
	filesystem, err := newGitHubSnapshot([]GitHubFile{
		{Path: "SKILL.md", Data: []byte(skillMarkdown("directory-count", "Directory count.", "Body."))},
		{Path: "one.txt", Data: []byte("one")},
		{Path: "two.txt", Data: []byte("two")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := filesystem.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	directory := opened.(fs.ReadDirFile)
	first, err := directory.ReadDir(1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%v error=%v", first, err)
	}
	rest, err := directory.ReadDir(math.MaxInt)
	if err != nil || len(rest) != 2 {
		t.Fatalf("rest=%v error=%v", rest, err)
	}
}

func TestGitHubSourceRejectsStructuralLimitAmplification(t *testing.T) {
	limits := DefaultLimits()
	deepPath := strings.Repeat("level/", limits.MaxPathDepth) + "tiny.txt"
	fetcher := &fakeGitHubFetcher{result: GitHubFetchResult{
		CommitSHA: testCommitSHA,
		Files: []GitHubFile{
			{Path: "SKILL.md", Data: []byte(skillMarkdown("structural", "Structural limits.", "Body."))},
			{Path: deepPath, Data: []byte("x")},
		},
	}}
	set, err := LoadSetWithLimits(t.Context(), limits, NewGitHubSource(GitHubSourceConfig{
		ID: "github", Repository: "owner/repository", Ref: "main", Path: "skills/structural", Fetcher: fetcher,
	}))
	if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) || len(fetcher.requests) != 1 {
		t.Fatalf("set=%v error=%v requests=%d", set, err, len(fetcher.requests))
	}
}

func TestGitHubSourceChecksEntryLimitBeforeRetainingInferredDirectories(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxEntriesPerSkill = 2
	limits.MaxPathDepth = 128
	fetcher := &fakeGitHubFetcher{result: GitHubFetchResult{
		CommitSHA: testCommitSHA,
		Files: []GitHubFile{
			{Path: "SKILL.md", Data: []byte(skillMarkdown("entries", "Entry limits.", "Body."))},
			{Path: "a/b/c/d/tiny.txt", Data: []byte("x")},
		},
	}}
	set, err := LoadSetWithLimits(t.Context(), limits, NewGitHubSource(GitHubSourceConfig{
		ID: "github", Repository: "owner/repository", Ref: "main", Path: "skills/entries", Fetcher: fetcher,
	}))
	if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) || len(fetcher.requests) != 1 {
		t.Fatalf("set=%v error=%v requests=%d", set, err, len(fetcher.requests))
	}
}

func TestGitHubSourceRejectsUnsafeOrIncompleteConfigurationBeforeFetching(t *testing.T) {
	tests := []struct {
		name   string
		config GitHubSourceConfig
		want   string
	}{
		{name: "empty repository", config: GitHubSourceConfig{ID: "github", Ref: "main", Path: "skills/a"}, want: "repository"},
		{name: "URL", config: GitHubSourceConfig{ID: "github", Repository: "https://github.com/owner/repo", Ref: "main", Path: "skills/a"}, want: "owner/repository"},
		{name: "credential URL", config: GitHubSourceConfig{ID: "github", Repository: "https://token-secret@github.com/owner/repo", Ref: "main", Path: "skills/a"}, want: "owner/repository"},
		{name: "dot owner", config: GitHubSourceConfig{ID: "github", Repository: "../repo", Ref: "main", Path: "skills/a"}, want: "owner/repository"},
		{name: "dot repository", config: GitHubSourceConfig{ID: "github", Repository: "owner/..", Ref: "main", Path: "skills/a"}, want: "owner/repository"},
		{name: "current directory owner", config: GitHubSourceConfig{ID: "github", Repository: "./repo", Ref: "main", Path: "skills/a"}, want: "owner/repository"},
		{name: "empty ref", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Path: "skills/a"}, want: "ref"},
		{name: "invalid UTF-8 ref", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: string([]byte{'m', 0xff}), Path: "skills/a"}, want: "ref"},
		{name: "dot dot ref", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "heads/../secret", Path: "skills/a"}, want: "ref"},
		{name: "reflog ref", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main@{1}", Path: "skills/a"}, want: "ref"},
		{name: "lock ref", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "refs/heads/main.lock", Path: "skills/a"}, want: "ref"},
		{name: "empty path", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main"}, want: "path"},
		{name: "absolute path", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: "/skills/a"}, want: "relative"},
		{name: "Windows absolute path", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: "C:/skills/a"}, want: "relative"},
		{name: "traversal", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: "skills/../secret"}, want: ".."},
		{name: "encoded traversal", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: "skills/%2e%2e/secret"}, want: "URL encoding"},
		{name: "backslash traversal", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: `skills\..\secret`}, want: "backslash"},
		{name: "newline path", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: "skills/a\nsecret"}, want: "control"},
		{name: "tab path", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: "skills/a\tsecret"}, want: "control"},
		{name: "invalid UTF-8 path", config: GitHubSourceConfig{ID: "github", Repository: "owner/repo", Ref: "main", Path: string([]byte{'s', 0xff})}, want: "UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetcher := &fakeGitHubFetcher{}
			test.config.Fetcher = fetcher
			set, err := LoadSet(t.Context(), NewGitHubSource(test.config))
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) || len(fetcher.requests) != 0 {
				t.Fatalf("set=%v error=%v requests=%d, want %q", set, err, len(fetcher.requests), test.want)
			}
			if strings.Contains(err.Error(), "token-secret") {
				t.Fatalf("credential leaked in error: %v", err)
			}
		})
	}
}

func TestGitHubSourceDoesNotRetryAndValidatesFetcherResult(t *testing.T) {
	network := errors.New("network unavailable")
	tests := []struct {
		name   string
		result GitHubFetchResult
		err    error
		want   string
	}{
		{name: "network error", err: network, want: "fetch failed"},
		{name: "symbolic revision", result: GitHubFetchResult{CommitSHA: "main"}, want: "commit SHA"},
		{name: "null SHA-1 revision", result: GitHubFetchResult{CommitSHA: strings.Repeat("0", 40)}, want: "commit SHA"},
		{name: "null SHA-256 revision", result: GitHubFetchResult{CommitSHA: strings.Repeat("0", 64)}, want: "commit SHA"},
		{name: "missing directory contents", result: GitHubFetchResult{CommitSHA: testCommitSHA}, want: "SKILL.md"},
		{name: "oversized file", result: GitHubFetchResult{CommitSHA: testCommitSHA, Files: []GitHubFile{
			{Path: "SKILL.md", Data: []byte(skillMarkdown("large", "Large skill.", "Body."))},
			{Path: "asset.bin", Data: []byte(strings.Repeat("x", 1024*1024+1))},
		}}, want: "file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetcher := &fakeGitHubFetcher{result: test.result, err: test.err}
			set, err := LoadSet(t.Context(), NewGitHubSource(GitHubSourceConfig{
				ID: "github", Repository: "owner/repo", Ref: "main", Path: "skills/a", Fetcher: fetcher,
			}))
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) || len(fetcher.requests) != 1 {
				t.Fatalf("set=%v error=%v requests=%d, want %q", set, err, len(fetcher.requests), test.want)
			}
			if test.err != nil && errors.Is(err, test.err) {
				t.Fatalf("error=%v exposes unsafe provider cause %v", err, test.err)
			}
		})
	}
}

func TestGitHubSourceRedactsFetcherCredentialErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		make func(*credentialBearingError) error
	}{
		{name: "direct", make: func(credential *credentialBearingError) error { return credential }},
		{name: "wrapped EOF", make: func(credential *credentialBearingError) error { return &eofCredentialError{credential: credential} }},
		{name: "joined EOF", make: func(credential *credentialBearingError) error { return errors.Join(io.EOF, credential) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			credential := &credentialBearingError{message: "Authorization: Bearer token-secret"}
			fetcher := &fakeGitHubFetcher{err: test.make(credential)}
			set, err := LoadSet(t.Context(), NewGitHubSource(GitHubSourceConfig{
				ID: "github", Repository: "owner/repo", Ref: "main", Path: "skills/a", Fetcher: fetcher,
			}))
			if err == nil || set != nil || len(fetcher.requests) != 1 || errors.Is(err, credential) || !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("set=%v error=%v requests=%d", set, err, len(fetcher.requests))
			}
			var exposed *credentialBearingError
			if errors.As(err, &exposed) || strings.Contains(err.Error(), "token-secret") || strings.Contains(err.Error(), "Authorization") {
				t.Fatalf("credential-bearing fetch error leaked: %v", err)
			}
		})
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

func TestGitHubSourceAppliesCallerLimitsBeforeBuildingSnapshot(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFileBytes = 32
	fetcher := &fakeGitHubFetcher{result: GitHubFetchResult{
		CommitSHA: testCommitSHA,
		Files: []GitHubFile{
			{Path: "SKILL.md", Data: []byte(skillMarkdown("limited", "Limited skill.", "Body."))},
		},
	}}
	set, err := LoadSetWithLimits(t.Context(), limits, NewGitHubSource(GitHubSourceConfig{
		ID: "github", Repository: "owner/repository", Ref: "main", Path: "skills/limited", Fetcher: fetcher,
	}))
	if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) || len(fetcher.requests) != 1 {
		t.Fatalf("set=%v error=%v requests=%d", set, err, len(fetcher.requests))
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

func TestCodexPluginSourceLoadsManifestSkillsAndIgnoresExecutionSurfaces(t *testing.T) {
	requireDescriptorRoot(t)
	plugin := testTempDir(t)
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{
		"name":"example","version":"1.2.3","skills":"./skills/",
		"apps":"./.app.json","mcpServers":"./.mcp.json","hooks":"./hooks/hooks.json",
		"interface":{"displayName":"Example"}
	}`)
	writeTestFile(t, filepath.Join(plugin, ".mcp.json"), `{"mcpServers":{"danger":{}}}`)
	writeTestFile(t, filepath.Join(plugin, ".app.json"), `{"apps":["danger"]}`)
	writeTestFile(t, filepath.Join(plugin, "hooks", "hooks.json"), `{"hooks":["danger"]}`)
	writeSkillDirectory(t, filepath.Join(plugin, "skills"), "beta", "beta", "Beta skill.", "Beta body.", map[string]string{"scripts/run.sh": "touch should-not-exist"})
	writeSkillDirectory(t, filepath.Join(plugin, "skills"), "alpha", "alpha", "Alpha skill.", "Alpha body.", nil)
	if err := os.MkdirAll(filepath.Join(plugin, "skills", "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	set, err := LoadSet(t.Context(), NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}))
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	got := set.Skills()
	if len(got) != 2 || got[0].Name() != "alpha" || got[1].Name() != "beta" || got[0].Revision() != "1.2.3" {
		t.Fatalf("skills=%+v", got)
	}
	for _, skill := range got {
		if strings.Contains(skill.Instructions(), "danger") || strings.Contains(skill.Instructions(), "touch should-not-exist") {
			t.Fatalf("non-SKILL execution content entered instructions: %q", skill.Instructions())
		}
	}
}

func TestCodexPluginSourceEnforcesDiscoveryLimits(t *testing.T) {
	requireDescriptorRoot(t)
	t.Run("collection entries", func(t *testing.T) {
		plugin := testTempDir(t)
		writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"skills":"./skills/"}`)
		for _, name := range []string{"one", "two", "three"} {
			if err := os.MkdirAll(filepath.Join(plugin, "skills", name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		limits := DefaultLimits()
		limits.MaxEntriesPerSkill = 2
		set, err := LoadSetWithLimits(t.Context(), limits, NewCodexPluginSource(CodexPluginSourceConfig{
			ID: "codex", PluginDirectories: []string{plugin},
		}))
		if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("set=%v error=%v", set, err)
		}
	})

	t.Run("cumulative skills", func(t *testing.T) {
		plugin := testTempDir(t)
		writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"skills":"./skill/"}`)
		writeSkillDirectory(t, plugin, "skill", "limited", "Limited skill.", "Body.", nil)
		plugins := []string{plugin, filepath.Join(testTempDir(t), "missing")}
		limits := DefaultLimits()
		limits.MaxSkills = 1
		set, err := LoadSetWithLimits(t.Context(), limits, NewCodexPluginSource(CodexPluginSourceConfig{
			ID: "codex", PluginDirectories: plugins,
		}))
		if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("set=%v error=%v", set, err)
		}
	})
}

func TestCodexPluginSourceHonorsCallerManifestFileLimit(t *testing.T) {
	requireDescriptorRoot(t)
	const manifestPrefix = `{"skills":"./skill/"}`

	t.Run("rejects above caller limit below default", func(t *testing.T) {
		plugin := testTempDir(t)
		markdown := skillMarkdown("limited", "Limited.", "Body.")
		manifest := manifestPrefix + strings.Repeat(" ", len(markdown)+1)
		writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), manifest)
		writeTestFile(t, filepath.Join(plugin, "skill", "SKILL.md"), markdown)

		limits := DefaultLimits()
		limits.MaxFileBytes = int64(len(markdown))
		set, err := LoadSetWithLimits(t.Context(), limits, NewCodexPluginSource(CodexPluginSourceConfig{
			ID: "codex", PluginDirectories: []string{plugin},
		}))
		if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) || !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("manifest=%d limit=%d set=%v error=%v", len(manifest), limits.MaxFileBytes, set, err)
		}
	})

	t.Run("accepts below caller limit above default", func(t *testing.T) {
		plugin := testTempDir(t)
		manifest := manifestPrefix + strings.Repeat(" ", int(defaultMaxFileBytes)-len(manifestPrefix)+1)
		writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), manifest)
		writeSkillDirectory(t, plugin, "skill", "permitted", "Permitted.", "Body.", nil)

		limits := DefaultLimits()
		limits.MaxFileBytes = int64(len(manifest))
		set, err := LoadSetWithLimits(t.Context(), limits, NewCodexPluginSource(CodexPluginSourceConfig{
			ID: "codex", PluginDirectories: []string{plugin},
		}))
		if err != nil || set == nil || set.Len() != 1 {
			t.Fatalf("manifest=%d limit=%d set=%v error=%v", len(manifest), limits.MaxFileBytes, set, err)
		}
	})
}

func TestCodexPluginSourceRejectsInvalidManifestAndPathEscape(t *testing.T) {
	requireDescriptorRoot(t)
	root := testTempDir(t)
	missingManifest := filepath.Join(root, "missing")
	if err := os.MkdirAll(missingManifest, 0o755); err != nil {
		t.Fatal(err)
	}
	emptyPlugin := filepath.Join(root, "empty")
	writeTestFile(t, filepath.Join(emptyPlugin, ".codex-plugin", "plugin.json"), `{"name":"empty","skills":"./skills/"}`)
	if err := os.MkdirAll(filepath.Join(emptyPlugin, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	escapePlugin := filepath.Join(root, "escape")
	writeTestFile(t, filepath.Join(escapePlugin, ".codex-plugin", "plugin.json"), `{"name":"escape","skills":"../outside"}`)
	invalidPlugin := filepath.Join(root, "invalid")
	writeTestFile(t, filepath.Join(invalidPlugin, ".codex-plugin", "plugin.json"), `{"skills":`)
	noSkillsField := filepath.Join(root, "no-skills")
	writeTestFile(t, filepath.Join(noSkillsField, ".codex-plugin", "plugin.json"), `{"name":"no-skills"}`)

	tests := []struct {
		name   string
		plugin string
		want   string
	}{
		{name: "missing manifest", plugin: missingManifest, want: "plugin.json"},
		{name: "empty plugin", plugin: emptyPlugin, want: "no skills"},
		{name: "path escape", plugin: escapePlugin, want: ".."},
		{name: "invalid JSON", plugin: invalidPlugin, want: "plugin.json"},
		{name: "missing skills", plugin: noSkillsField, want: "skills"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{test.plugin}}))
			if err == nil || set != nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("set=%v error=%v, want %q", set, err, test.want)
			}
		})
	}
}

func TestCodexPluginSourceRejectsAmbiguousManifestJSON(t *testing.T) {
	requireDescriptorRoot(t)
	tests := []struct {
		name     string
		manifest []byte
	}{
		{name: "duplicate skills first order", manifest: []byte(`{"skills":"./skills-a/","skills":"./skills-b/"}`)},
		{name: "duplicate skills reverse order", manifest: []byte(`{"skills":"./skills-b/","skills":"./skills-a/"}`)},
		{name: "case variant skills", manifest: []byte(`{"skills":"./skills-a/","Skills":"./skills-b/"}`)},
		{name: "duplicate version", manifest: []byte(`{"version":"1","version":"2","skills":"./skills-a/"}`)},
		{name: "control in version", manifest: []byte(`{"version":"1\n2","skills":"./skills-a/"}`)},
		{name: "non-normalized skills path", manifest: []byte(`{"skills":"././skills-a/"}`)},
		{name: "control in skills path", manifest: []byte(`{"skills":"./skills-a\nsecret/"}`)},
		{name: "null in skills path", manifest: []byte(`{"skills":"./skills-a\u0000secret/"}`)},
		{name: "invalid UTF-8", manifest: append([]byte(`{"name":"`), append([]byte{0xff}, []byte(`","skills":"./skills-a/"}`)...)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := testTempDir(t)
			writeSkillDirectory(t, plugin, "skills-a", "alpha", "Alpha skill.", "Alpha body.", nil)
			writeSkillDirectory(t, plugin, "skills-b", "beta", "Beta skill.", "Beta body.", nil)
			manifestPath := filepath.Join(plugin, ".codex-plugin", "plugin.json")
			if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, test.manifest, 0o644); err != nil {
				t.Fatal(err)
			}
			set, err := LoadSet(t.Context(), NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}))
			if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("set=%v error=%v", set, err)
			}
		})
	}
}

func TestCodexPluginSnapshotCannotBeRedirectedAfterResolve(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	plugin := testTempDir(t)
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"stable","skills":"./skills/"}`)
	configured := writeSkillDirectory(t, filepath.Join(plugin, "skills"), "stable", "original-plugin", "Original plugin skill.", "ORIGINAL_PLUGIN_BODY", nil)
	outside := writeSkillDirectory(t, testTempDir(t), "outside", "escaped-plugin", "Escaped plugin skill.", "PLUGIN_OUTSIDE_SECRET", nil)
	backup := filepath.Join(plugin, "skills", "original-directory")
	source := afterResolveSource{
		source: NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}),
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
	if err == nil {
		for _, skill := range set.Skills() {
			if skill.Name() == "escaped-plugin" || strings.Contains(skill.Instructions(), "PLUGIN_OUTSIDE_SECRET") {
				t.Fatalf("snapshot followed replacement outside its root: %+v", skill)
			}
		}
		return
	}
	if set != nil {
		t.Fatalf("failed load returned a partial set: %+v", set)
	}
}

func TestCodexPluginSnapshotCannotSwitchToSiblingAfterResolve(t *testing.T) {
	requireDescriptorRoot(t)
	plugin := testTempDir(t)
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"stable","skills":"./selected/"}`)
	selected := writeSkillDirectory(t, plugin, "selected", "selected", "Selected skill.", "SELECTED_BODY", nil)
	sibling := writeSkillDirectory(t, plugin, "sibling", "sibling", "Sibling skill.", "SIBLING_BODY", nil)
	backup := filepath.Join(plugin, "selected-original")
	source := afterResolveSource{
		source: NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}),
		after: func() {
			if err := os.Rename(selected, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(sibling, selected); err != nil {
				t.Fatal(err)
			}
		},
	}
	set, err := LoadSet(t.Context(), source)
	if err != nil {
		if set != nil {
			t.Fatalf("failed load returned a partial set: %+v", set)
		}
		return
	}
	if got := set.Skills()[0]; got.Name() != "selected" || strings.Contains(got.Instructions(), "SIBLING_BODY") {
		t.Fatalf("snapshot switched to sibling: name=%q instructions=%q", got.Name(), got.Instructions())
	}
}

func TestCodexPluginSourceRejectsStableInternalSkillsSymlink(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	plugin := testTempDir(t)
	writeSkillDirectory(t, plugin, "real-skills", "linked", "Linked skill.", "Linked body.", nil)
	if err := os.Symlink("real-skills", filepath.Join(plugin, "linked-skills")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"linked","skills":"./linked-skills/"}`)

	set, err := LoadSet(t.Context(), NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}))
	if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestCodexPluginSourceRejectsStableIntermediateSkillsSymlink(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	plugin := testTempDir(t)
	writeSkillDirectory(t, filepath.Join(plugin, "real-parent"), "skills", "linked", "Linked skill.", "Linked body.", nil)
	if err := os.Symlink("real-parent", filepath.Join(plugin, "linked-parent")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"linked","skills":"./linked-parent/skills/"}`)

	set, err := LoadSet(t.Context(), NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}))
	if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestCodexPluginSourceFailsAtomicallyOnInvalidSkillProbe(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	for _, direct := range []bool{false, true} {
		name := "mixed child"
		if direct {
			name = "direct skill fallback"
		}
		t.Run(name, func(t *testing.T) {
			plugin := testTempDir(t)
			skillsRoot := filepath.Join(plugin, "skills")
			writeSkillDirectory(t, skillsRoot, "valid", "valid", "Valid skill.", "Valid body.", nil)
			invalidRoot := filepath.Join(skillsRoot, "invalid")
			if err := os.MkdirAll(invalidRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(testTempDir(t), "outside.md")
			writeTestFile(t, outside, skillMarkdown("outside", "Outside skill.", "Outside body."))
			target := filepath.Join(invalidRoot, "SKILL.md")
			if direct {
				target = filepath.Join(skillsRoot, "SKILL.md")
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"atomic","skills":"./skills/"}`)

			set, err := LoadSet(t.Context(), NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}))
			if err == nil || set != nil || !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("set=%v error=%v", set, err)
			}
		})
	}
}

func TestCodexPluginSourceRejectsSymlinkEscapingPluginRoot(t *testing.T) {
	requireDescriptorRoot(t)
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	plugin := testTempDir(t)
	outside := testTempDir(t)
	writeSkillDirectory(t, outside, "skill", "escaped", "Escaped skill.", "Escaped body.", nil)
	if err := os.Symlink(outside, filepath.Join(plugin, "linked-skills")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"escape","skills":"./linked-skills/"}`)

	set, err := LoadSet(t.Context(), NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}}))
	if err == nil || set != nil || !strings.Contains(strings.ToLower(err.Error()), "outside") {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestCodexPluginExecutionMetadataDoesNotAffectSkillSetID(t *testing.T) {
	requireDescriptorRoot(t)
	plugin := testTempDir(t)
	writeTestFile(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"stable","version":"1.0.0","skills":"./skills/","apps":"./.app.json"}`)
	writeTestFile(t, filepath.Join(plugin, ".app.json"), `{"value":"one"}`)
	writeSkillDirectory(t, filepath.Join(plugin, "skills"), "stable", "stable", "Stable skill.", "Stable body.", nil)
	source := NewCodexPluginSource(CodexPluginSourceConfig{ID: "codex", PluginDirectories: []string{plugin}})
	first, err := LoadSet(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(plugin, ".app.json"), `{"value":"two"}`)
	second, err := LoadSet(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() {
		t.Fatal("plugin UI metadata changed the SkillSet ID")
	}
	writeTestFile(t, filepath.Join(plugin, "skills", "stable", "assets", "new.txt"), "new")
	third, err := LoadSet(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID() == third.ID() {
		t.Fatal("skill-local asset did not change the SkillSet ID")
	}
}
