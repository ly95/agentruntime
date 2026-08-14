package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

var githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
var commitSHAPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// GitHubFetchRequest identifies one repository directory at an explicit ref.
type GitHubFetchRequest struct {
	Repository string
	Ref        string
	Path       string
}

// GitHubFile is one regular file copied from the requested repository
// directory. Path is relative to that directory. Data ownership transfers to
// Fetch's caller; GitHubSource validates and deep-copies it before returning.
// Mode is optional and uses Go's io/fs.FileMode representation, not a raw Git
// tree mode. Only Go permission bits are accepted; symbolic links, submodules,
// directories, special files, and raw Git modes are rejected.
type GitHubFile struct {
	Path string
	Data []byte
	Mode fs.FileMode
}

// GitHubFetchResult contains the resolved commit and an immutable-file input
// snapshot for the requested Skill directory. The injected host fetcher is
// responsible for ensuring both values describe the exact repository, ref, and
// path in the request.
type GitHubFetchResult struct {
	CommitSHA string
	Files     []GitHubFile
}

// GitHubFetcher is supplied by the host, which owns transport and credentials.
// Fetch must resolve Ref to a full commit SHA, must not return a parent tree,
// and must not retry unless the host explicitly chose that behavior.
type GitHubFetcher interface {
	Fetch(ctx context.Context, request GitHubFetchRequest) (GitHubFetchResult, error)
}

// GitHubFetcherFunc adapts a function to GitHubFetcher.
type GitHubFetcherFunc func(context.Context, GitHubFetchRequest) (GitHubFetchResult, error)

func (fetcher GitHubFetcherFunc) Fetch(ctx context.Context, request GitHubFetchRequest) (GitHubFetchResult, error) {
	return fetcher(ctx, request)
}

// GitHubSourceConfig selects one Skill directory in one GitHub repository.
type GitHubSourceConfig struct {
	ID         string
	Repository string
	Ref        string
	Path       string
	Fetcher    GitHubFetcher
}

// GitHubSource resolves a configured GitHub Skill through a host fetcher.
type GitHubSource struct {
	config GitHubSourceConfig
}

// NewGitHubSource returns a source validated during LoadSet.
func NewGitHubSource(config GitHubSourceConfig) *GitHubSource {
	return &GitHubSource{config: config}
}

func (source *GitHubSource) ID() string {
	if source == nil {
		return ""
	}
	return source.config.ID
}

func (source *GitHubSource) Resolve(ctx context.Context) ([]Artifact, error) {
	return source.resolveWithLimits(ctx, DefaultLimits())
}

func (source *GitHubSource) resolveWithLimits(ctx context.Context, limits Limits) ([]Artifact, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: GitHub source is nil", ErrInvalidSource)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidSource)
	}
	config := source.config
	if err := validateGitHubRepository(config.Repository); err != nil {
		return nil, err
	}
	if err := validateGitHubRef(config.Ref); err != nil {
		return nil, err
	}
	requestedPath, err := normalizedRepositoryPath(config.Path)
	if err != nil {
		return nil, err
	}
	if isNilValue(config.Fetcher) {
		return nil, fmt.Errorf("%w: GitHub fetcher is required", ErrInvalidSource)
	}
	request := GitHubFetchRequest{Repository: config.Repository, Ref: config.Ref, Path: requestedPath}
	result, err := config.Fetcher.Fetch(ctx, request)
	if err != nil {
		return nil, newSafeSourceError(fmt.Sprintf("skills: GitHub source %q fetch failed", config.ID), err)
	}
	commitSHA := strings.ToLower(strings.TrimSpace(result.CommitSHA))
	if result.CommitSHA != strings.TrimSpace(result.CommitSHA) || !commitSHAPattern.MatchString(commitSHA) || strings.Trim(commitSHA, "0") == "" {
		return nil, fmt.Errorf("%w: GitHub fetcher must return a resolved commit SHA", ErrInvalidArtifact)
	}
	filesystem, err := newGitHubSnapshot(result.Files, limits)
	if err != nil {
		return nil, err
	}
	return []Artifact{{
		SourceID: config.ID,
		Locator:  config.Repository + "@" + commitSHA + ":" + requestedPath,
		Revision: commitSHA,
		FS:       filesystem,
	}}, nil
}

func validateGitHubRepository(repository string) error {
	parts := strings.Split(repository, "/")
	if repository != strings.TrimSpace(repository) || len(parts) != 2 ||
		!githubOwnerPattern.MatchString(parts[0]) || !githubRepositoryPattern.MatchString(parts[1]) ||
		parts[1] == "." || parts[1] == ".." {
		return fmt.Errorf("%w: GitHub repository must use owner/repository without a URL or credentials", ErrInvalidSource)
	}
	return nil
}

func validateGitHubRef(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: GitHub ref is required", ErrInvalidSource)
	}
	if value != strings.TrimSpace(value) || strings.ContainsAny(value, "~^:?*[\\") ||
		strings.Contains(value, "://") || strings.Contains(value, "..") || strings.Contains(value, "@{") ||
		strings.Contains(value, "//") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.HasSuffix(value, ".") || value == "@" {
		return fmt.Errorf("%w: GitHub ref is invalid", ErrInvalidSource)
	}
	if err := validateControlFreeText("GitHub ref", value); err != nil {
		return fmt.Errorf("%w: GitHub ref is invalid: %v", ErrInvalidSource, err)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("%w: GitHub ref is invalid", ErrInvalidSource)
		}
	}
	return nil
}

func normalizedRepositoryPath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: GitHub path is required", ErrInvalidSource)
	}
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("%w: GitHub path must be normalized", ErrInvalidSource)
	}
	if strings.Contains(value, `\`) {
		return "", fmt.Errorf("%w: GitHub path must not contain a backslash", ErrInvalidSource)
	}
	if strings.Contains(value, "%") {
		return "", fmt.Errorf("%w: GitHub path must not contain URL encoding", ErrInvalidSource)
	}
	if err := validateControlFreeText("GitHub path", value); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	if strings.HasPrefix(value, "/") || hasWindowsVolumePrefix(value) {
		return "", fmt.Errorf("%w: GitHub path must be relative", ErrInvalidSource)
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return "", fmt.Errorf("%w: GitHub path must not contain ..", ErrInvalidSource)
		}
		if component == "" || component == "." {
			return "", fmt.Errorf("%w: GitHub path must be normalized", ErrInvalidSource)
		}
	}
	return value, nil
}

type safeSourceError struct {
	message         string
	classifications []error
}

func (err *safeSourceError) Error() string { return err.message }

func (err *safeSourceError) Is(target error) bool {
	for _, classification := range err.classifications {
		if classification == target {
			return true
		}
	}
	return false
}

func newSafeSourceError(message string, cause error) error {
	classifications := []error{ErrInvalidSource}
	for _, classification := range []error{context.Canceled, context.DeadlineExceeded, fs.ErrNotExist, fs.ErrPermission} {
		if errors.Is(cause, classification) {
			classifications = append(classifications, classification)
		}
	}
	return &safeSourceError{message: message, classifications: classifications}
}

func newGitHubSnapshot(files []GitHubFile, limits Limits) (*githubSnapshotFS, error) {
	if len(files) > limits.MaxFilesPerSkill {
		return nil, fmt.Errorf("%w: GitHub Skill contains more than %d files", ErrLimitExceeded, limits.MaxFilesPerSkill)
	}
	contents := make(map[string][]byte, len(files))
	directories := map[string]struct{}{".": {}}
	var totalBytes int64
	for _, file := range files {
		if err := validateArtifactPath(file.Path); err != nil {
			return nil, fmt.Errorf("%w: GitHub fetcher returned an invalid file path", ErrInvalidArtifact)
		}
		if err := validateArtifactStructure(file.Path, limits); err != nil {
			return nil, err
		}
		if file.Mode&^fs.ModePerm != 0 {
			return nil, fmt.Errorf("%w: GitHub fetcher returned a non-regular or raw-mode file", ErrInvalidArtifact)
		}
		if _, exists := contents[file.Path]; exists {
			return nil, fmt.Errorf("%w: GitHub fetcher returned duplicate file paths", ErrInvalidArtifact)
		}
		if _, exists := directories[file.Path]; exists {
			return nil, fmt.Errorf("%w: GitHub fetcher returned a file/directory collision", ErrInvalidArtifact)
		}
		maximum := limits.MaxFileBytes
		if file.Path == "SKILL.md" && limits.MaxSkillMarkdownBytes < maximum {
			maximum = limits.MaxSkillMarkdownBytes
		}
		if int64(len(file.Data)) > maximum {
			if file.Path == "SKILL.md" && maximum == limits.MaxSkillMarkdownBytes {
				return nil, &skillMarkdownLimitError{maximum: maximum}
			}
			return nil, fmt.Errorf("%w: GitHub file exceeds %d bytes", ErrLimitExceeded, maximum)
		}
		if int64(len(file.Data)) > limits.MaxSkillBytes-totalBytes {
			return nil, fmt.Errorf("%w: GitHub Skill exceeds %d bytes", ErrLimitExceeded, limits.MaxSkillBytes)
		}
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			if _, exists := contents[parent]; exists {
				return nil, fmt.Errorf("%w: GitHub fetcher returned a file/directory collision", ErrInvalidArtifact)
			}
			if _, exists := directories[parent]; !exists {
				if len(contents)+1+len(directories) > limits.MaxEntriesPerSkill {
					return nil, fmt.Errorf("%w: GitHub Skill contains more than %d entries", ErrLimitExceeded, limits.MaxEntriesPerSkill)
				}
				directories[parent] = struct{}{}
			}
		}
		if len(contents)+1+len(directories)-1 > limits.MaxEntriesPerSkill {
			return nil, fmt.Errorf("%w: GitHub Skill contains more than %d entries", ErrLimitExceeded, limits.MaxEntriesPerSkill)
		}
		totalBytes += int64(len(file.Data))
		contents[file.Path] = nil
	}
	for _, file := range files {
		contents[file.Path] = append([]byte(nil), file.Data...)
	}
	return buildGitHubSnapshotFS(contents, directories), nil
}

type githubSnapshotFS struct {
	files       map[string][]byte
	directories map[string]struct{}
	entries     map[string][]githubSnapshotEntry
}

func buildGitHubSnapshotFS(files map[string][]byte, directories map[string]struct{}) *githubSnapshotFS {
	filesystem := &githubSnapshotFS{
		files:       files,
		directories: directories,
		entries:     make(map[string][]githubSnapshotEntry, len(directories)),
	}
	for directory := range directories {
		filesystem.entries[directory] = nil
		if directory == "." {
			continue
		}
		parent := path.Dir(directory)
		filesystem.entries[parent] = append(filesystem.entries[parent], githubSnapshotEntry{
			info: githubSnapshotInfo{name: path.Base(directory), mode: fs.ModeDir | 0o555},
		})
	}
	for filePath, data := range files {
		parent := path.Dir(filePath)
		filesystem.entries[parent] = append(filesystem.entries[parent], githubSnapshotEntry{
			info: githubSnapshotInfo{name: path.Base(filePath), size: int64(len(data)), mode: 0o444},
		})
	}
	for directory := range filesystem.entries {
		sort.Slice(filesystem.entries[directory], func(left, right int) bool {
			return filesystem.entries[directory][left].Name() < filesystem.entries[directory][right].Name()
		})
	}
	return filesystem
}

func (filesystem *githubSnapshotFS) Open(name string) (fs.File, error) {
	if name != "." && !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if data, exists := filesystem.files[name]; exists {
		info := githubSnapshotInfo{name: path.Base(name), size: int64(len(data)), mode: 0o444}
		return &githubSnapshotFile{reader: bytes.NewReader(data), info: info}, nil
	}
	if _, exists := filesystem.directories[name]; exists {
		entries := append([]githubSnapshotEntry(nil), filesystem.entries[name]...)
		return &githubSnapshotDirectory{
			info:    githubSnapshotInfo{name: snapshotBase(name), mode: fs.ModeDir | 0o555},
			entries: entries,
		}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (filesystem *githubSnapshotFS) Stat(name string) (fs.FileInfo, error) {
	if data, exists := filesystem.files[name]; exists {
		return githubSnapshotInfo{name: path.Base(name), size: int64(len(data)), mode: 0o444}, nil
	}
	if _, exists := filesystem.directories[name]; exists {
		return githubSnapshotInfo{name: snapshotBase(name), mode: fs.ModeDir | 0o555}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (filesystem *githubSnapshotFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, exists := filesystem.entries[name]
	if !exists {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, len(entries))
	for index := range entries {
		out[index] = entries[index]
	}
	return out, nil
}

func snapshotBase(name string) string {
	if name == "." {
		return "."
	}
	return path.Base(name)
}

type githubSnapshotInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (info githubSnapshotInfo) Name() string       { return info.name }
func (info githubSnapshotInfo) Size() int64        { return info.size }
func (info githubSnapshotInfo) Mode() fs.FileMode  { return info.mode }
func (info githubSnapshotInfo) ModTime() time.Time { return time.Time{} }
func (info githubSnapshotInfo) IsDir() bool        { return info.mode.IsDir() }
func (info githubSnapshotInfo) Sys() any           { return nil }

type githubSnapshotEntry struct {
	info githubSnapshotInfo
}

func (entry githubSnapshotEntry) Name() string               { return entry.info.Name() }
func (entry githubSnapshotEntry) IsDir() bool                { return entry.info.IsDir() }
func (entry githubSnapshotEntry) Type() fs.FileMode          { return entry.info.Mode().Type() }
func (entry githubSnapshotEntry) Info() (fs.FileInfo, error) { return entry.info, nil }

type githubSnapshotFile struct {
	reader *bytes.Reader
	info   githubSnapshotInfo
}

func (file *githubSnapshotFile) Read(buffer []byte) (int, error) { return file.reader.Read(buffer) }
func (file *githubSnapshotFile) Close() error                    { return nil }
func (file *githubSnapshotFile) Stat() (fs.FileInfo, error)      { return file.info, nil }

type githubSnapshotDirectory struct {
	info    githubSnapshotInfo
	entries []githubSnapshotEntry
	offset  int
}

func (directory *githubSnapshotDirectory) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: directory.info.name, Err: fs.ErrInvalid}
}
func (directory *githubSnapshotDirectory) Close() error               { return nil }
func (directory *githubSnapshotDirectory) Stat() (fs.FileInfo, error) { return directory.info, nil }
func (directory *githubSnapshotDirectory) ReadDir(count int) ([]fs.DirEntry, error) {
	if directory.offset >= len(directory.entries) {
		if count > 0 {
			return nil, io.EOF
		}
		return []fs.DirEntry{}, nil
	}
	remaining := len(directory.entries) - directory.offset
	take := remaining
	if count > 0 && count < take {
		take = count
	}
	end := directory.offset + take
	out := make([]fs.DirEntry, end-directory.offset)
	for index, entry := range directory.entries[directory.offset:end] {
		out[index] = entry
	}
	directory.offset = end
	return out, nil
}
