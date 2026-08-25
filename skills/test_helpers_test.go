package skills

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

type staticSource struct {
	id        string
	artifacts []Artifact
	err       error
	order     *[]string
}

type afterResolveSource struct {
	source Source
	after  func()
}

type swapOnOpenFS struct {
	filesystem fs.FS
	target     string
	swap       func()
	once       sync.Once
}

type countingFS struct {
	filesystem fs.FS
	target     string
	bytesRead  int64
}

type countingFile struct {
	fs.File
	bytesRead *int64
}

type enumerationCountingFS struct {
	filesystem  fs.FS
	entriesRead int
}

type enumerationCountingFile struct {
	fs.File
	reader      fs.ReadDirFile
	entriesRead *int
}

func requireDescriptorRoot(t *testing.T) {
	t.Helper()
	if err := ensureDescriptorRootSupported(); err != nil {
		t.Skipf("descriptor-root source unsupported: %v", err)
	}
}

func testTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", directory, err)
	}
	return resolved
}

func openTestRootArtifactFS(directory string) (*rootArtifactFS, error) {
	root, err := openDescriptorRoot(directory)
	if err != nil {
		return nil, err
	}
	return &rootArtifactFS{root: root}, nil
}

func openTestSharedRoot(directory string) (*sharedRoot, error) {
	root, err := openDescriptorRoot(directory)
	if err != nil {
		return nil, err
	}
	return &sharedRoot{root: root}, nil
}

func (filesystem *swapOnOpenFS) Open(name string) (fs.File, error) {
	if name == filesystem.target {
		filesystem.once.Do(filesystem.swap)
	}
	return filesystem.filesystem.Open(name)
}

func (filesystem *countingFS) Open(name string) (fs.File, error) {
	file, err := filesystem.filesystem.Open(name)
	if err != nil || name != filesystem.target {
		return file, err
	}
	return &countingFile{File: file, bytesRead: &filesystem.bytesRead}, nil
}

func (file *countingFile) Read(buffer []byte) (int, error) {
	count, err := file.File.Read(buffer)
	*file.bytesRead += int64(count)
	return count, err
}

func (filesystem *enumerationCountingFS) Open(name string) (fs.File, error) {
	file, err := filesystem.filesystem.Open(name)
	if err != nil {
		return nil, err
	}
	reader, ok := file.(fs.ReadDirFile)
	if !ok {
		return file, nil
	}
	return &enumerationCountingFile{File: file, reader: reader, entriesRead: &filesystem.entriesRead}, nil
}

func (file *enumerationCountingFile) ReadDir(count int) ([]fs.DirEntry, error) {
	entries, err := file.reader.ReadDir(count)
	*file.entriesRead += len(entries)
	return entries, err
}

func (filesystem *swapOnOpenFS) closeArtifactFS() error {
	if closer, ok := filesystem.filesystem.(artifactFSCloser); ok {
		return closer.closeArtifactFS()
	}
	return nil
}

func (source afterResolveSource) ID() string { return source.source.ID() }

func (source afterResolveSource) Resolve(ctx context.Context) ([]Artifact, error) {
	artifacts, err := source.source.Resolve(ctx)
	if err == nil && source.after != nil {
		source.after()
	}
	return artifacts, err
}

func (source staticSource) ID() string { return source.id }

func (source staticSource) Resolve(context.Context) ([]Artifact, error) {
	if source.order != nil {
		*source.order = append(*source.order, source.id)
	}
	return source.artifacts, source.err
}

func mapArtifact(sourceID, locator, revision, name, description, body string, extras map[string]string) Artifact {
	files := fstest.MapFS{
		"SKILL.md": &fstest.MapFile{Data: []byte(skillMarkdown(name, description, body))},
	}
	for path, contents := range extras {
		files[path] = &fstest.MapFile{Data: []byte(contents)}
	}
	return Artifact{SourceID: sourceID, Locator: locator, Revision: revision, FS: files}
}

func skillMarkdown(name, description, body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", name, description, body)
}

func writeSkillDirectory(t *testing.T, parent, directory, name, description, body string, extras map[string]string) string {
	t.Helper()
	root := filepath.Join(parent, directory)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", root, err)
	}
	writeTestFile(t, filepath.Join(root, "SKILL.md"), skillMarkdown(name, description, body))
	for relative, contents := range extras {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(relative)), contents)
	}
	return root
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func loadSingleArtifact(t *testing.T, artifact Artifact) *SkillSet {
	t.Helper()
	set, err := LoadSet(t.Context(), staticSource{id: artifact.SourceID, artifacts: []Artifact{artifact}})
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return set
}

func fileData(t *testing.T, skill Skill, wanted string) []byte {
	t.Helper()
	for _, file := range skill.Files() {
		if file.Path() == wanted {
			return file.Bytes()
		}
	}
	t.Fatalf("file %q not found", wanted)
	return nil
}

type fakeGitHubFetcher struct {
	requests []GitHubFetchRequest
	result   GitHubFetchResult
	err      error
}

func (fetcher *fakeGitHubFetcher) Fetch(_ context.Context, request GitHubFetchRequest) (GitHubFetchResult, error) {
	fetcher.requests = append(fetcher.requests, request)
	return fetcher.result, fetcher.err
}

func artifactWithFS(sourceID, locator, revision string, filesystem fs.FS) Artifact {
	return Artifact{SourceID: sourceID, Locator: locator, Revision: revision, FS: filesystem}
}

type malformedReadDirFS struct {
	name string
	only bool
}

type nilReadDirFS struct {
	typed bool
}

type nilStatFS struct {
	typed bool
}

type nilOpenFS struct {
	typed bool
}

type nilInfoFS struct {
	typed bool
}

type nilContentOpenFS struct {
	typed bool
}

type contradictoryMetadataFS struct {
	entryType     fs.FileMode
	entryIsDir    bool
	entryInfoMode fs.FileMode
	entryInfoName string
	openMode      fs.FileMode
	assetReader   io.Reader
	assetClosed   *int
	assetReads    *int
}

type contradictoryEntry struct {
	name     string
	typeMode fs.FileMode
	isDir    bool
	info     malformedInfo
}

type readerStep struct {
	data []byte
	err  error
}

type steppedReader struct {
	steps []readerStep
}

func (filesystem malformedReadDirFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return &malformedFile{reader: bytes.NewReader(nil), info: malformedInfo{name: ".", mode: fs.ModeDir}}, nil
	case "SKILL.md", "nested/SKILL.md", "C:/SKILL.md":
		data := []byte(skillMarkdown("valid", "Valid skill.", "Valid body."))
		return &malformedFile{reader: bytes.NewReader(data), info: malformedInfo{name: name, size: int64(len(data))}}, nil
	case "asset.txt":
		data := []byte("asset")
		return &malformedFile{reader: bytes.NewReader(data), info: malformedInfo{name: name, size: int64(len(data))}}, nil
	default:
		return nil, fs.ErrNotExist
	}
}

func (filesystem malformedReadDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	entries := []fs.DirEntry{malformedEntry{info: malformedInfo{name: filesystem.name}}}
	if !filesystem.only {
		entries = append([]fs.DirEntry{malformedEntry{info: malformedInfo{name: "SKILL.md"}}}, entries...)
	}
	return entries, nil
}

func (filesystem nilReadDirFS) Open(name string) (fs.File, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return &malformedFile{reader: bytes.NewReader(nil), info: malformedInfo{name: ".", mode: fs.ModeDir}}, nil
}

func (filesystem nilReadDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	if filesystem.typed {
		var entry *malformedEntry
		return []fs.DirEntry{entry}, nil
	}
	return []fs.DirEntry{nil}, nil
}

func (filesystem nilStatFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func (filesystem nilStatFS) Stat(string) (fs.FileInfo, error) {
	if filesystem.typed {
		var info *malformedInfo
		return info, nil
	}
	return nil, nil
}

func (filesystem nilOpenFS) Open(string) (fs.File, error) {
	if filesystem.typed {
		var file *malformedFile
		return file, nil
	}
	return nil, nil
}

func (filesystem nilInfoFS) Open(name string) (fs.File, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return &malformedFile{reader: bytes.NewReader(nil), info: malformedInfo{name: ".", mode: fs.ModeDir}}, nil
}

func (filesystem nilInfoFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return []fs.DirEntry{nilInfoEntry(filesystem)}, nil
}

func (filesystem nilContentOpenFS) Open(name string) (fs.File, error) {
	if name == "." {
		return &malformedFile{reader: bytes.NewReader(nil), info: malformedInfo{name: ".", mode: fs.ModeDir}}, nil
	}
	if name == "SKILL.md" {
		if filesystem.typed {
			var file *malformedFile
			return file, nil
		}
		return nil, nil
	}
	return nil, fs.ErrNotExist
}

func (filesystem nilContentOpenFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return malformedInfo{name: ".", mode: fs.ModeDir}, nil
	}
	return nil, fs.ErrNotExist
}

func (filesystem nilContentOpenFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return []fs.DirEntry{malformedEntry{info: malformedInfo{name: "SKILL.md"}}}, nil
}

func (filesystem contradictoryMetadataFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return &malformedFile{reader: bytes.NewReader(nil), info: malformedInfo{name: ".", mode: fs.ModeDir}}, nil
	case "SKILL.md":
		data := []byte(skillMarkdown("metadata", "Validate filesystem metadata.", "Body."))
		return &malformedFile{reader: bytes.NewReader(data), info: malformedInfo{name: name, size: int64(len(data))}}, nil
	case "asset.txt":
		data := []byte("must affect the digest")
		reader := filesystem.assetReader
		if reader == nil {
			reader = bytes.NewReader(data)
		}
		return &malformedFile{
			reader: reader, info: malformedInfo{name: name, size: int64(len(data)), mode: filesystem.openMode},
			closed: filesystem.assetClosed, reads: filesystem.assetReads,
		}, nil
	default:
		return nil, fs.ErrNotExist
	}
}

func (filesystem contradictoryMetadataFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "asset.txt" {
		return nil, nil
	}
	if name != "." {
		return nil, fs.ErrNotExist
	}
	infoName := filesystem.entryInfoName
	if infoName == "" {
		infoName = "asset.txt"
	}
	return []fs.DirEntry{
		malformedEntry{info: malformedInfo{name: "SKILL.md"}},
		contradictoryEntry{
			name: "asset.txt", typeMode: filesystem.entryType, isDir: filesystem.entryIsDir,
			info: malformedInfo{name: infoName, mode: filesystem.entryInfoMode},
		},
	}, nil
}

type malformedFile struct {
	reader io.Reader
	info   malformedInfo
	closed *int
	reads  *int
}

func (file *malformedFile) Read(buffer []byte) (int, error) {
	if file.reads != nil {
		*file.reads++
	}
	return file.reader.Read(buffer)
}
func (file *malformedFile) Close() error {
	if file.closed != nil {
		*file.closed++
	}
	return nil
}
func (file *malformedFile) Stat() (fs.FileInfo, error) { return file.info, nil }

type malformedInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (info malformedInfo) Name() string       { return info.name }
func (info malformedInfo) Size() int64        { return info.size }
func (info malformedInfo) Mode() fs.FileMode  { return info.mode }
func (info malformedInfo) ModTime() time.Time { return time.Time{} }
func (info malformedInfo) IsDir() bool        { return info.mode.IsDir() }
func (info malformedInfo) Sys() any           { return nil }

type malformedEntry struct {
	info malformedInfo
}

type nilInfoEntry struct {
	typed bool
}

func (entry malformedEntry) Name() string               { return entry.info.name }
func (entry malformedEntry) IsDir() bool                { return entry.info.IsDir() }
func (entry malformedEntry) Type() fs.FileMode          { return entry.info.Mode().Type() }
func (entry malformedEntry) Info() (fs.FileInfo, error) { return entry.info, nil }

func (entry nilInfoEntry) Name() string      { return "SKILL.md" }
func (entry nilInfoEntry) IsDir() bool       { return false }
func (entry nilInfoEntry) Type() fs.FileMode { return 0 }
func (entry nilInfoEntry) Info() (fs.FileInfo, error) {
	if entry.typed {
		var info *malformedInfo
		return info, nil
	}
	return nil, nil
}

func (entry contradictoryEntry) Name() string               { return entry.name }
func (entry contradictoryEntry) IsDir() bool                { return entry.isDir }
func (entry contradictoryEntry) Type() fs.FileMode          { return entry.typeMode }
func (entry contradictoryEntry) Info() (fs.FileInfo, error) { return entry.info, nil }

func (reader *steppedReader) Read(buffer []byte) (int, error) {
	if len(reader.steps) == 0 {
		return 0, io.EOF
	}
	step := reader.steps[0]
	reader.steps = reader.steps[1:]
	count := copy(buffer, step.data)
	return count, step.err
}

type credentialBearingError struct {
	message string
}

func (err *credentialBearingError) Error() string { return err.message }

type eofCredentialError struct {
	credential *credentialBearingError
}

func (err *eofCredentialError) Error() string { return err.credential.Error() }
func (err *eofCredentialError) Unwrap() error { return io.EOF }

type terminalReader struct {
	reader   *bytes.Reader
	terminal error
}

func (reader *terminalReader) Read(buffer []byte) (int, error) {
	if reader.reader.Len() > 0 {
		return reader.reader.Read(buffer)
	}
	return 0, reader.terminal
}
