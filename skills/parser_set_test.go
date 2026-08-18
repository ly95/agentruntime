package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadSetParsesAndSnapshotsValidSkill(t *testing.T) {
	filesystem := fstest.MapFS{
		"SKILL.md":            &fstest.MapFile{Data: []byte(skillMarkdown("code-review", "Review code safely.", "Inspect the diff."))},
		"references/rules.md": &fstest.MapFile{Data: []byte("rule-v1")},
		"scripts/check.sh":    &fstest.MapFile{Data: []byte("exit 99")},
		"agents/openai.yaml":  &fstest.MapFile{Data: []byte("dependencies: {}")},
		"assets/template.txt": &fstest.MapFile{Data: []byte("template")},
		"metadata/meta.yaml":  &fstest.MapFile{Data: []byte("kind: ordinary-file")},
	}
	set := loadSingleArtifact(t, artifactWithFS("source-a", "skill/code-review", "rev-1", filesystem))
	if set.Len() != 1 || set.ID() == "" {
		t.Fatalf("set len=%d id=%q", set.Len(), set.ID())
	}
	skill := set.Skills()[0]
	if skill.SourceID() != "source-a" || skill.Locator() != "skill/code-review" || skill.Revision() != "rev-1" ||
		skill.Name() != "code-review" || skill.Description() != "Review code safely." || skill.Instructions() != "Inspect the diff." || skill.Digest() == "" {
		t.Fatalf("unexpected skill snapshot: source=%q locator=%q revision=%q name=%q description=%q instructions=%q digest=%q",
			skill.SourceID(), skill.Locator(), skill.Revision(), skill.Name(), skill.Description(), skill.Instructions(), skill.Digest())
	}
	if got := string(fileData(t, skill, "references/rules.md")); got != "rule-v1" {
		t.Fatalf("reference=%q", got)
	}

	filesystem["references/rules.md"].Data[0] = 'X'
	filesystem["SKILL.md"].Data = []byte(skillMarkdown("mutated", "Changed.", "Changed body."))
	if skill.Name() != "code-review" || string(fileData(t, skill, "references/rules.md")) != "rule-v1" {
		t.Fatal("mutable source filesystem changed an existing snapshot")
	}
	files := skill.Files()
	bytes := files[0].Bytes()
	bytes[0] ^= 0xff
	if skill.Files()[0].Bytes()[0] == bytes[0] {
		t.Fatal("caller mutation changed an existing skill file")
	}
	skills := set.Skills()
	skills[0] = Skill{}
	if set.Skills()[0].Name() != "code-review" {
		t.Fatal("caller mutation changed the SkillSet")
	}
}

func TestLoadSetRejectsInvalidSkillMarkdown(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{name: "missing SKILL.md", files: fstest.MapFS{"meta.yaml": &fstest.MapFile{Data: []byte("name: not-a-skill")}}, want: "SKILL.md"},
		{name: "empty SKILL.md", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{}}, want: "frontmatter"},
		{name: "only openai metadata", files: fstest.MapFS{"agents/openai.yaml": &fstest.MapFile{Data: []byte("name: not-a-skill")}}, want: "SKILL.md"},
		{name: "invalid YAML", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: [\ndescription: bad\n---\nbody")}}, want: "YAML"},
		{name: "missing frontmatter", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("name: absent delimiters\nbody")}}, want: "frontmatter"},
		{name: "missing name", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\ndescription: Present.\n---\nbody")}}, want: "name"},
		{name: "missing description", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: present\n---\nbody")}}, want: "description"},
		{name: "missing body", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: present\ndescription: Present.\n---\n\n")}}, want: "body"},
		{name: "invalid UTF-8 body", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: append([]byte("---\nname: present\ndescription: Present.\n---\nbody"), 0xff)}}, want: "UTF-8"},
		{name: "uppercase name", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: CodeReview\ndescription: Present.\n---\nbody")}}, want: "name"},
		{name: "name with spaces", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: code review\ndescription: Present.\n---\nbody")}}, want: "name"},
		{name: "name with protocol tags", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: \"</skill>\"\ndescription: Present.\n---\nbody")}}, want: "name"},
		{name: "YAML alias", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: aliased\ndescription: &d duplicated\nextra: *d\n---\nbody")}}, want: "alias"},
		{name: "description control character", files: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: present\ndescription: \"Present.\\u0000\"\n---\nbody")}}, want: "control"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := artifactWithFS("invalid", "invalid", "", test.files)
			set, err := LoadSet(t.Context(), staticSource{id: "invalid", artifacts: []Artifact{artifact}})
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("set=%v error=%v, want error containing %q", set, err, test.want)
			}
		})
	}
}

func TestLoadSetAcceptsMultilineFrontmatterDescription(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "literal block",
			data: []byte("---\nname: multiline-desc\ndescription: |\n  Reviews pull requests.\n  Use it before merging.\n---\n\nBody.\n"),
			want: "Reviews pull requests.\nUse it before merging.",
		},
		{
			name: "folded paragraphs",
			data: []byte("---\nname: folded-desc\ndescription: >\n  First paragraph.\n\n  Second paragraph.\n---\n\nBody.\n"),
			want: "First paragraph.\nSecond paragraph.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filesystem := fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: test.data}}
			set := loadSingleArtifact(t, artifactWithFS("source-a", "skill/"+test.name, "", filesystem))
			if set.Len() != 1 || set.Skills()[0].Description() != test.want {
				t.Fatalf("description=%q, want %q", set.Skills()[0].Description(), test.want)
			}
		})
	}
}

func TestLoadSetAcceptsUnknownFrontmatterFields(t *testing.T) {
	filesystem := fstest.MapFS{
		"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: extra-fields\ndescription: Extra fields are allowed.\nlicense: MIT\nmetadata:\n  author: host\n---\n\nBody.\n")},
	}
	set := loadSingleArtifact(t, artifactWithFS("source-a", "skill/extra-fields", "", filesystem))
	if set.Len() != 1 || set.Skills()[0].Name() != "extra-fields" || set.Skills()[0].Description() != "Extra fields are allowed." {
		t.Fatalf("unexpected skill=%+v", set.Skills())
	}
}

func TestLoadSetMergesInSourceOrderAndSortsOutput(t *testing.T) {
	var order []string
	set, err := LoadSet(t.Context(),
		staticSource{id: "z-source", order: &order, artifacts: []Artifact{
			mapArtifact("z-source", "z-two", "", "zeta", "Zeta skill.", "Zeta body.", nil),
			mapArtifact("z-source", "a-one", "", "alpha-z", "Alpha z skill.", "Alpha z body.", nil),
		}},
		staticSource{id: "a-source", order: &order, artifacts: []Artifact{
			mapArtifact("a-source", "middle", "", "middle", "Middle skill.", "Middle body.", nil),
		}},
	)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if got := strings.Join(order, ","); got != "z-source,a-source" {
		t.Fatalf("resolve order=%q", got)
	}
	got := set.Skills()
	if len(got) != 3 || got[0].SourceID() != "a-source" || got[1].Name() != "alpha-z" || got[2].Name() != "zeta" {
		t.Fatalf("unstable output order: %+v", []string{got[0].SourceID() + "/" + got[0].Name(), got[1].SourceID() + "/" + got[1].Name(), got[2].SourceID() + "/" + got[2].Name()})
	}
}

func TestLoadSetRejectsConflictsAndPartialResults(t *testing.T) {
	tests := []struct {
		name    string
		sources []Source
		want    string
	}{
		{name: "duplicate skill name", sources: []Source{
			staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "same", "First.", "First body.", nil)}},
			staticSource{id: "b", artifacts: []Artifact{mapArtifact("b", "two", "", "same", "Second.", "Second body.", nil)}},
		}, want: "conflict"},
		{name: "duplicate source id", sources: []Source{staticSource{id: "a"}, staticSource{id: "a"}}, want: "source ID"},
		{name: "control source id", sources: []Source{staticSource{id: "bad\x00id"}}, want: "control"},
		{name: "source failure", sources: []Source{
			staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "valid", "Valid.", "Valid body.", nil)}},
			staticSource{id: "b", err: errors.New("source unavailable")},
		}, want: "source unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), test.sources...)
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("set=%v error=%v, want %q", set, err, test.want)
			}
		})
	}
}

func TestSkillAndSetDigestsAreDeterministicAndContentAddressed(t *testing.T) {
	first := loadSingleArtifact(t, mapArtifact("source", "locator", "revision", "digest", "Digest skill.", "Body.", map[string]string{
		"z-last.txt": "last", "a-first.txt": "first",
	}))
	second := loadSingleArtifact(t, mapArtifact("source", "locator", "revision", "digest", "Digest skill.", "Body.", map[string]string{
		"a-first.txt": "first", "z-last.txt": "last",
	}))
	if first.ID() != second.ID() || first.Skills()[0].Digest() != second.Skills()[0].Digest() {
		t.Fatal("file enumeration order changed a digest")
	}

	variants := []Artifact{
		mapArtifact("other-source", "locator", "revision", "digest", "Digest skill.", "Body.", map[string]string{"a-first.txt": "first", "z-last.txt": "last"}),
		mapArtifact("source", "other-locator", "revision", "digest", "Digest skill.", "Body.", map[string]string{"a-first.txt": "first", "z-last.txt": "last"}),
		mapArtifact("source", "locator", "other-revision", "digest", "Digest skill.", "Body.", map[string]string{"a-first.txt": "first", "z-last.txt": "last"}),
		mapArtifact("source", "locator", "revision", "digest", "Digest skill.", "Body.", map[string]string{"renamed.txt": "first", "z-last.txt": "last"}),
		mapArtifact("source", "locator", "revision", "digest", "Digest skill.", "Body.", map[string]string{"a-first.txt": "changed", "z-last.txt": "last"}),
	}
	for index, variant := range variants {
		set := loadSingleArtifact(t, variant)
		if set.ID() == first.ID() || set.Skills()[0].Digest() == first.Skills()[0].Digest() {
			t.Fatalf("variant %d did not change digest", index)
		}
	}
}

func TestLoadSetEnforcesDefaultAndConfiguredLimitsWithoutTruncation(t *testing.T) {
	defaults := DefaultLimits()
	if defaults.MaxSkillMarkdownBytes != 256*1024 || defaults.MaxFileBytes != 1024*1024 ||
		defaults.MaxSkillBytes != 10*1024*1024 || defaults.MaxFilesPerSkill != 256 ||
		defaults.MaxEntriesPerSkill != 16*1024 || defaults.MaxPathBytes != 4096 || defaults.MaxPathDepth != 64 ||
		defaults.MaxSkills != 128 || defaults.MaxTotalSkillMarkdownBytes != 1024*1024 {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	base := DefaultLimits()
	base.MaxSkillMarkdownBytes = 128
	base.MaxFileBytes = 192
	base.MaxSkillBytes = 220
	base.MaxFilesPerSkill = 3
	base.MaxSkills = 2
	base.MaxTotalSkillMarkdownBytes = 160
	pathBytes := base
	pathBytes.MaxPathBytes = len("SKILL.md")
	pathDepth := base
	pathDepth.MaxPathDepth = 1
	entries := base
	entries.MaxEntriesPerSkill = 3
	singleFile := base
	singleFile.MaxSkillBytes = 1024
	wideFS := fstest.MapFS{
		"SKILL.md": &fstest.MapFile{Data: []byte(skillMarkdown("wide", "Wide.", "Body."))},
		"a":        &fstest.MapFile{Mode: fs.ModeDir},
		"b":        &fstest.MapFile{Mode: fs.ModeDir},
		"c":        &fstest.MapFile{Mode: fs.ModeDir},
	}

	largeBody := strings.Repeat("b", 180)
	tests := []struct {
		name    string
		limits  Limits
		sources []Source
		want    string
	}{
		{name: "SKILL.md", limits: base, sources: []Source{staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "one", "One.", largeBody, nil)}}}, want: "SKILL.md"},
		{name: "single file", limits: singleFile, sources: []Source{staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "one", "One.", "Body.", map[string]string{"asset.bin": strings.Repeat("x", 193)})}}}, want: "file"},
		{name: "skill bytes", limits: base, sources: []Source{staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "one", "One.", "Body.", map[string]string{"a": strings.Repeat("x", 100), "b": strings.Repeat("y", 100)})}}}, want: "skill"},
		{name: "file count", limits: base, sources: []Source{staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "one", "One.", "Body.", map[string]string{"a": "a", "b": "b", "c": "c"})}}}, want: "files"},
		{name: "path bytes", limits: pathBytes, sources: []Source{staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "one", "One.", "Body.", map[string]string{"123456789": "x"})}}}, want: "path exceeds"},
		{name: "path depth", limits: pathDepth, sources: []Source{staticSource{id: "a", artifacts: []Artifact{mapArtifact("a", "one", "", "one", "One.", "Body.", map[string]string{"nested/a": "x"})}}}, want: "depth"},
		{name: "entry count", limits: entries, sources: []Source{staticSource{id: "a", artifacts: []Artifact{artifactWithFS("a", "one", "", wideFS)}}}, want: "entry limit"},
		{name: "skill count", limits: base, sources: []Source{staticSource{id: "a", artifacts: []Artifact{
			mapArtifact("a", "one", "", "one", "One.", "Body.", nil),
			mapArtifact("a", "two", "", "two", "Two.", "Body.", nil),
			mapArtifact("a", "three", "", "three", "Three.", "Body.", nil),
		}}}, want: "skills"},
		{name: "aggregate SKILL.md", limits: base, sources: []Source{staticSource{id: "a", artifacts: []Artifact{
			mapArtifact("a", "one", "", "one", "One.", strings.Repeat("x", 45), nil),
			mapArtifact("a", "two", "", "two", "Two.", strings.Repeat("y", 45), nil),
		}}}, want: "total SKILL.md"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSetWithLimits(t.Context(), test.limits, test.sources...)
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("set=%v error=%v, want %q", set, err, test.want)
			}
		})
	}
}

func TestLoadSetAppliesAggregateByteLimitsBeforeReadingWholeFiles(t *testing.T) {
	t.Run("skill bytes", func(t *testing.T) {
		markdown := []byte(skillMarkdown("bounded", "Bounded reads.", "Body."))
		filesystem := &countingFS{
			filesystem: fstest.MapFS{
				"SKILL.md":  &fstest.MapFile{Data: markdown},
				"asset.bin": &fstest.MapFile{Data: bytes.Repeat([]byte("x"), 1024)},
			},
			target: "asset.bin",
		}
		limits := DefaultLimits()
		limits.MaxFileBytes = 1024
		limits.MaxSkillBytes = int64(len(markdown) + 5)
		set, err := LoadSetWithLimits(t.Context(), limits, staticSource{id: "bounded", artifacts: []Artifact{
			artifactWithFS("bounded", "bounded", "", filesystem),
		}})
		if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("set=%v error=%v", set, err)
		}
		if filesystem.bytesRead != 6 {
			t.Fatalf("asset bytes read=%d, want remaining five bytes plus one probe", filesystem.bytesRead)
		}
	})

	t.Run("total markdown bytes", func(t *testing.T) {
		firstMarkdown := []byte(skillMarkdown("first", "First bounded skill.", "Body."))
		secondFS := &countingFS{
			filesystem: fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: bytes.Repeat([]byte("x"), 1024)}},
			target:     "SKILL.md",
		}
		limits := DefaultLimits()
		limits.MaxSkillMarkdownBytes = 1024
		limits.MaxTotalSkillMarkdownBytes = int64(len(firstMarkdown) + 5)
		set, err := LoadSetWithLimits(t.Context(), limits, staticSource{id: "bounded", artifacts: []Artifact{
			artifactWithFS("bounded", "first", "", fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: firstMarkdown}}),
			artifactWithFS("bounded", "second", "", secondFS),
		}})
		if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) || !strings.Contains(err.Error(), "total SKILL.md") {
			t.Fatalf("set=%v error=%v", set, err)
		}
		if secondFS.bytesRead != 6 {
			t.Fatalf("second SKILL.md bytes read=%d, want remaining five bytes plus one probe", secondFS.bytesRead)
		}
	})
}

func TestLoadSetAcceptsEmptyFileAtExactSkillByteLimit(t *testing.T) {
	markdown := skillMarkdown("exact-bytes", "Exact byte boundary.", "Body.")
	limits := DefaultLimits()
	limits.MaxSkillBytes = int64(len(markdown))
	set, err := LoadSetWithLimits(t.Context(), limits, staticSource{id: "exact", artifacts: []Artifact{
		mapArtifact("exact", "exact", "", "exact-bytes", "Exact byte boundary.", "Body.", map[string]string{"empty.txt": ""}),
	}})
	if err != nil || set == nil || set.Len() != 1 {
		t.Fatalf("set=%v error=%v", set, err)
	}
	if data := fileData(t, set.Skills()[0], "empty.txt"); len(data) != 0 {
		t.Fatalf("empty file bytes=%q", data)
	}
}

func TestReadBoundedZeroLimitAcceptsOnlyEmptyInput(t *testing.T) {
	if data, err := readBounded(strings.NewReader(""), 0); err != nil || len(data) != 0 {
		t.Fatalf("empty data=%q error=%v", data, err)
	}
	if data, err := readBounded(strings.NewReader("x"), 0); !errors.Is(err, ErrLimitExceeded) || data != nil {
		t.Fatalf("non-empty data=%q error=%v", data, err)
	}
}

func TestReadBoundedRejectsNoProgressAcrossTheWholeRead(t *testing.T) {
	t.Run("before first byte", func(t *testing.T) {
		reader := &steppedReader{steps: append(make([]readerStep, 100), readerStep{err: io.EOF})}
		data, err := readBounded(reader, 8)
		if data != nil || !errors.Is(err, io.ErrNoProgress) {
			t.Fatalf("data=%q error=%v", data, err)
		}
	})

	t.Run("after partial content", func(t *testing.T) {
		steps := []readerStep{{data: []byte("a")}}
		steps = append(steps, make([]readerStep, 100)...)
		steps = append(steps, readerStep{err: io.EOF})
		data, err := readBounded(&steppedReader{steps: steps}, 8)
		if data != nil || !errors.Is(err, io.ErrNoProgress) {
			t.Fatalf("data=%q error=%v", data, err)
		}
	})

	t.Run("progress resets the consecutive counter", func(t *testing.T) {
		steps := make([]readerStep, 99)
		steps = append(steps, readerStep{data: []byte("x")})
		steps = append(steps, make([]readerStep, 99)...)
		steps = append(steps, readerStep{err: io.EOF})
		data, err := readBounded(&steppedReader{steps: steps}, 1)
		if err != nil || string(data) != "x" {
			t.Fatalf("data=%q error=%v", data, err)
		}
	})
}

func TestLoadSetReservesDiscoveredEntriesAgainstOneGlobalSkillBudget(t *testing.T) {
	files := fstest.MapFS{
		"SKILL.md": &fstest.MapFile{Data: []byte(skillMarkdown("entry-budget", "Bound directory discovery.", "Body."))},
	}
	parent := ""
	for depth := 0; depth < 4; depth++ {
		chain := "a"
		if parent != "" {
			chain = parent + "/a"
		}
		files[chain] = &fstest.MapFile{Mode: fs.ModeDir}
		for sibling := 0; sibling < 8; sibling++ {
			name := fmt.Sprintf("b%d", sibling)
			if parent != "" {
				name = parent + "/" + name
			}
			files[name] = &fstest.MapFile{Mode: fs.ModeDir}
		}
		parent = chain
	}
	filesystem := &enumerationCountingFS{filesystem: files}
	limits := DefaultLimits()
	limits.MaxEntriesPerSkill = 20
	set, err := LoadSetWithLimits(t.Context(), limits, staticSource{id: "entry-budget", artifacts: []Artifact{
		artifactWithFS("entry-budget", "entry-budget", "", filesystem),
	}})
	if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("set=%v error=%v", set, err)
	}
	if filesystem.entriesRead > limits.MaxEntriesPerSkill+1 {
		t.Fatalf("enumerated %d entries before rejection, want at most %d", filesystem.entriesRead, limits.MaxEntriesPerSkill+1)
	}
}

func TestLoadSetPassesRemainingSkillLimitToLaterSources(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSkills = 1
	missingRoot := filepath.Join(testTempDir(t), "missing")
	set, err := LoadSetWithLimits(t.Context(), limits,
		staticSource{id: "first", artifacts: []Artifact{mapArtifact("first", "first", "", "first", "First skill.", "Body.", nil)}},
		NewLocalSource(LocalSourceConfig{ID: "local", Directories: []string{missingRoot}}),
	)
	if err == nil || set != nil || !errors.Is(err, ErrLimitExceeded) || strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func TestLoadSetPreservesBytesAtMaxInt64Limits(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSkillMarkdownBytes = math.MaxInt64
	limits.MaxFileBytes = math.MaxInt64
	firstArtifact := mapArtifact("source", "max-int", "revision", "max-int", "Boundary skill.", "Body.", map[string]string{
		"asset.bin": "non-empty-first",
	})
	first, err := LoadSetWithLimits(t.Context(), limits, staticSource{id: "source", artifacts: []Artifact{firstArtifact}})
	if err != nil {
		t.Fatalf("LoadSetWithLimits: %v", err)
	}
	if got := string(fileData(t, first.Skills()[0], "asset.bin")); got != "non-empty-first" {
		t.Fatalf("asset bytes=%q", got)
	}

	secondArtifact := mapArtifact("source", "max-int", "revision", "max-int", "Boundary skill.", "Body.", map[string]string{
		"asset.bin": "non-empty-second",
	})
	second, err := LoadSetWithLimits(t.Context(), limits, staticSource{id: "source", artifacts: []Artifact{secondArtifact}})
	if err != nil {
		t.Fatalf("LoadSetWithLimits changed asset: %v", err)
	}
	if first.ID() == second.ID() || first.Skills()[0].Digest() == second.Skills()[0].Digest() {
		t.Fatal("asset bytes changed without changing content digests")
	}

}

func TestReadBoundedRejectsWrappedOrJoinedEOFProbeErrors(t *testing.T) {
	credential := &credentialBearingError{message: "Authorization: Bearer probe-token"}
	tests := []struct {
		name     string
		terminal error
		wantOK   bool
	}{
		{name: "exact EOF", terminal: io.EOF, wantOK: true},
		{name: "wrapped EOF", terminal: &eofCredentialError{credential: credential}},
		{name: "joined EOF", terminal: errors.Join(io.EOF, credential)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &terminalReader{reader: bytes.NewReader([]byte("exact")), terminal: test.terminal}
			data, err := readBounded(reader, int64(len("exact")))
			if test.wantOK {
				if err != nil || string(data) != "exact" {
					t.Fatalf("data=%q error=%v", data, err)
				}
				return
			}
			if err == nil || data != nil {
				t.Fatalf("data=%q error=%v", data, err)
			}
		})
	}
}

func TestLoadSetRejectsInvalidArtifactIdentityAndFilesystem(t *testing.T) {
	validFS := fstest.MapFS{"SKILL.md": &fstest.MapFile{Data: []byte(skillMarkdown("valid", "Valid.", "Body."))}}
	tests := []struct {
		name     string
		sourceID string
		artifact Artifact
		want     string
	}{
		{name: "mismatched source", sourceID: "source", artifact: artifactWithFS("other", "locator", "", validFS), want: "SourceID"},
		{name: "empty locator", sourceID: "source", artifact: artifactWithFS("source", "", "", validFS), want: "locator"},
		{name: "control locator", sourceID: "source", artifact: artifactWithFS("source", "safe\x00unsafe", "", validFS), want: "control"},
		{name: "control revision", sourceID: "source", artifact: artifactWithFS("source", "locator", "rev\nunsafe", validFS), want: "revision"},
		{name: "invalid UTF-8 revision", sourceID: "source", artifact: artifactWithFS("source", "locator", string([]byte{'r', 0xff}), validFS), want: "UTF-8"},
		{name: "nil filesystem", sourceID: "source", artifact: artifactWithFS("source", "locator", "", nil), want: "filesystem"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), staticSource{id: test.sourceID, artifacts: []Artifact{test.artifact}})
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("set=%v error=%v, want %q", set, err, test.want)
			}
		})
	}
}

func TestLoadSetRejectsUnsafeArtifactFileEntries(t *testing.T) {
	tests := []struct {
		name       string
		filesystem fs.FS
		want       string
	}{
		{name: "dot dot", filesystem: malformedReadDirFS{name: "../escape"}, want: "relative path"},
		{name: "backslash traversal", filesystem: malformedReadDirFS{name: `nested\..\escape`}, want: "backslash"},
		{name: "symbolic link", filesystem: fstest.MapFS{
			"SKILL.md": &fstest.MapFile{Data: []byte(skillMarkdown("valid", "Valid skill.", "Valid body."))},
			"linked":   &fstest.MapFile{Mode: fs.ModeSymlink | 0o777, Data: []byte("../outside")},
		}, want: "symbolic link"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), staticSource{id: "unsafe", artifacts: []Artifact{
				artifactWithFS("unsafe", "unsafe", "", test.filesystem),
			}})
			if err == nil || set != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("set=%v error=%v, want %q", set, err, test.want)
			}
		})
	}
}

func TestLoadSetRejectsContradictoryFilesystemMetadata(t *testing.T) {
	tests := []struct {
		name       string
		filesystem fs.FS
	}{
		{name: "directory entry opens as regular file", filesystem: contradictoryMetadataFS{
			entryType: fs.ModeDir, entryIsDir: true, entryInfoMode: fs.ModeDir,
		}},
		{name: "regular entry opens as directory", filesystem: contradictoryMetadataFS{
			entryInfoMode: 0, openMode: fs.ModeDir,
		}},
		{name: "Type and IsDir disagree", filesystem: contradictoryMetadataFS{
			entryType: fs.ModeDir, entryIsDir: false, entryInfoMode: 0,
		}},
		{name: "IsDir and Info disagree", filesystem: contradictoryMetadataFS{
			entryType: fs.ModeDir, entryIsDir: true, entryInfoMode: 0,
		}},
		{name: "Name and Info disagree", filesystem: contradictoryMetadataFS{
			entryInfoMode: 0, entryInfoName: "other.txt",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), staticSource{id: "metadata", artifacts: []Artifact{
				artifactWithFS("metadata", "metadata", "", test.filesystem),
			}})
			if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("set=%v error=%v", set, err)
			}
		})
	}
}

func TestLoadSetRejectsOpenedTypeChangesBeforeReadingAndCloses(t *testing.T) {
	for _, test := range []struct {
		name       string
		filesystem func(closed, reads *int) fs.FS
	}{
		{
			name: "directory becomes file",
			filesystem: func(closed, reads *int) fs.FS {
				return contradictoryMetadataFS{
					entryType: fs.ModeDir, entryIsDir: true, entryInfoMode: fs.ModeDir,
					assetClosed: closed, assetReads: reads,
				}
			},
		},
		{
			name: "file becomes directory",
			filesystem: func(closed, reads *int) fs.FS {
				return contradictoryMetadataFS{
					entryInfoMode: 0, openMode: fs.ModeDir,
					assetClosed: closed, assetReads: reads,
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			closed, reads := 0, 0
			set, err := LoadSet(t.Context(), staticSource{id: "opened-type", artifacts: []Artifact{
				artifactWithFS("opened-type", "opened-type", "", test.filesystem(&closed, &reads)),
			}})
			if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("set=%v error=%v", set, err)
			}
			if closed != 1 || reads != 0 {
				t.Fatalf("closed=%d reads=%d, want close before content read", closed, reads)
			}
		})
	}
}

func TestLoadSetClosesFileAfterNoProgressRead(t *testing.T) {
	closed, reads := 0, 0
	reader := &steppedReader{steps: append(make([]readerStep, 100), readerStep{err: io.EOF})}
	filesystem := contradictoryMetadataFS{
		entryInfoMode: 0, openMode: 0,
		assetReader: reader, assetClosed: &closed, assetReads: &reads,
	}
	set, err := LoadSet(t.Context(), staticSource{id: "no-progress", artifacts: []Artifact{
		artifactWithFS("no-progress", "no-progress", "", filesystem),
	}})
	if err == nil || set != nil || !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("set=%v error=%v", set, err)
	}
	if closed != 1 || reads != 100 {
		t.Fatalf("closed=%d reads=%d, want one close after 100 bounded attempts", closed, reads)
	}
}

func TestLoadSetRejectsMalformedRawDirectoryEntryNames(t *testing.T) {
	tests := []struct {
		name    string
		rawName string
		only    bool
	}{
		{name: "Unix absolute required file", rawName: "/SKILL.md", only: true},
		{name: "Unix absolute supporting file", rawName: "/asset.txt"},
		{name: "Windows volume", rawName: "C:/SKILL.md", only: true},
		{name: "embedded separator", rawName: "nested/SKILL.md", only: true},
		{name: "dot component", rawName: ".", only: true},
		{name: "normalization collision", rawName: "/SKILL.md"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), staticSource{id: "unsafe", artifacts: []Artifact{
				artifactWithFS("unsafe", "unsafe", "", malformedReadDirFS{name: test.rawName, only: test.only}),
			}})
			if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("set=%v error=%v", set, err)
			}
		})
	}
}

func TestLoadSetRejectsNilDirectoryEntriesWithoutPanicking(t *testing.T) {
	for _, typed := range []bool{false, true} {
		name := "nil"
		if typed {
			name = "typed nil"
		}
		t.Run(name, func(t *testing.T) {
			set, err := LoadSet(t.Context(), staticSource{id: "nil-entry", artifacts: []Artifact{
				artifactWithFS("nil-entry", "nil-entry", "", nilReadDirFS{typed: typed}),
			}})
			if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("set=%v error=%v", set, err)
			}
		})
	}
}

func TestLoadSetRejectsNilFilesystemResultsWithoutPanicking(t *testing.T) {
	for _, typed := range []bool{false, true} {
		kind := "nil"
		if typed {
			kind = "typed nil"
		}
		tests := []struct {
			name       string
			filesystem fs.FS
		}{
			{name: "root stat", filesystem: nilStatFS{typed: typed}},
			{name: "root open", filesystem: nilOpenFS{typed: typed}},
			{name: "entry info", filesystem: nilInfoFS{typed: typed}},
			{name: "content open", filesystem: nilContentOpenFS{typed: typed}},
		}
		for _, test := range tests {
			t.Run(test.name+"/"+kind, func(t *testing.T) {
				set, err := LoadSet(t.Context(), staticSource{id: "nil-result", artifacts: []Artifact{
					artifactWithFS("nil-result", "nil-result", "", test.filesystem),
				}})
				if err == nil || set != nil || !errors.Is(err, ErrInvalidArtifact) {
					t.Fatalf("set=%v error=%v", set, err)
				}
			})
		}
	}
}

func TestLoadSetRejectsNoSources(t *testing.T) {
	set, err := LoadSet(t.Context())
	if err == nil || set != nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("set=%v error=%v", set, err)
	}
}

func ExampleLoadSet() {
	set, err := LoadSet(context.Background(), staticSource{id: "example", artifacts: []Artifact{
		mapArtifact("example", "review", "", "review", "Review changes.", "Inspect the full diff.", nil),
	}})
	if err != nil {
		panic(err)
	}
	fmt.Println(set.Len(), set.Skills()[0].Name())
	// Output: 1 review
}
