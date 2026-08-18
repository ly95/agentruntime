package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxSkillNameBytes            = 64
	maxSkillDescriptionRunes     = 1024
	maxSkillFrontmatterYAMLNodes = 1024
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type skillMarkdownLimitError struct {
	maximum int64
}

type artifactFileInfo struct {
	name  string
	mode  fs.FileMode
	isDir bool
}

type artifactEntry struct {
	name string
	info artifactFileInfo
}

func (entry artifactEntry) Name() string      { return entry.name }
func (entry artifactEntry) IsDir() bool       { return entry.info.isDir }
func (entry artifactEntry) Type() fs.FileMode { return entry.info.mode.Type() }

func (err *skillMarkdownLimitError) Error() string {
	return fmt.Sprintf("%s: SKILL.md exceeds %d bytes", ErrLimitExceeded, err.maximum)
}

func (err *skillMarkdownLimitError) Unwrap() error { return ErrLimitExceeded }

func parseArtifact(artifact Artifact, limits Limits) (Skill, int64, error) {
	files, err := snapshotArtifactFiles(artifact.FS, limits)
	if err != nil {
		return Skill{}, 0, err
	}
	var markdown []byte
	markdownFound := false
	for _, file := range files {
		if file.path == "SKILL.md" {
			markdown = file.data
			markdownFound = true
			break
		}
	}
	if !markdownFound {
		return Skill{}, 0, fmt.Errorf("%w: root SKILL.md is required", ErrInvalidSkill)
	}
	metadata, instructions, err := parseSkillMarkdown(markdown)
	if err != nil {
		return Skill{}, 0, err
	}
	skill := Skill{
		sourceID: artifact.SourceID, locator: artifact.Locator, revision: artifact.Revision,
		name: metadata.Name, description: metadata.Description, instructions: instructions,
		files: files,
	}
	skill.digest = digestSkill(skill)
	return skill, int64(len(markdown)), nil
}

func snapshotArtifactFiles(filesystem fs.FS, limits Limits) ([]File, error) {
	files := make([]File, 0)
	seen := make(map[string]struct{})
	rootSeen := false
	var totalBytes int64
	err := walkArtifactEntries(filesystem, limits.MaxEntriesPerSkill, func(filePath string, entry artifactEntry) error {
		if err := validateArtifactEntry(filePath, entry); err != nil {
			return err
		}
		if filePath == "." {
			if rootSeen {
				return fmt.Errorf("%w: duplicate root directory entry", ErrInvalidArtifact)
			}
			rootSeen = true
			return nil
		}
		if err := validateArtifactPath(filePath); err != nil {
			return err
		}
		if err := validateArtifactStructure(filePath, limits); err != nil {
			return err
		}
		if _, exists := seen[filePath]; exists {
			return fmt.Errorf("%w: duplicate entry path %q", ErrInvalidArtifact, filePath)
		}
		seen[filePath] = struct{}{}
		if entry.info.mode&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: symbolic link %q is not allowed", ErrInvalidArtifact, filePath)
		}
		if entry.info.isDir {
			return nil
		}
		if !entry.info.mode.IsRegular() {
			return fmt.Errorf("%w: non-regular file %q is not allowed", ErrInvalidArtifact, filePath)
		}
		if len(files) >= limits.MaxFilesPerSkill {
			return fmt.Errorf("%w: skill contains more than %d files", ErrLimitExceeded, limits.MaxFilesPerSkill)
		}
		fileMaximum := limits.MaxFileBytes
		limitName := "file"
		if filePath == "SKILL.md" && limits.MaxSkillMarkdownBytes < fileMaximum {
			fileMaximum = limits.MaxSkillMarkdownBytes
			limitName = "SKILL.md"
		}
		remainingSkillBytes := limits.MaxSkillBytes - totalBytes
		if remainingSkillBytes < 0 {
			return fmt.Errorf("%w: skill exceeds %d bytes", ErrLimitExceeded, limits.MaxSkillBytes)
		}
		maximum := fileMaximum
		if remainingSkillBytes < maximum {
			maximum = remainingSkillBytes
		}
		data, err := readVerifiedArtifactFile(filesystem, filePath, maximum, entry)
		if err != nil {
			if errors.Is(err, ErrLimitExceeded) {
				if remainingSkillBytes < fileMaximum {
					return fmt.Errorf("%w: skill exceeds %d bytes", ErrLimitExceeded, limits.MaxSkillBytes)
				}
				if filePath == "SKILL.md" && maximum == limits.MaxSkillMarkdownBytes {
					return &skillMarkdownLimitError{maximum: maximum}
				}
				return fmt.Errorf("%w: %s %q exceeds %d bytes", ErrLimitExceeded, limitName, filePath, maximum)
			}
			return err
		}
		totalBytes += int64(len(data))
		files = append(files, File{path: filePath, data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(left, right int) bool { return files[left].path < files[right].path })
	return files, nil
}

func walkArtifactEntries(filesystem fs.FS, maximumEntries int, visit func(string, artifactEntry) error) error {
	rootInfo, err := statFSPath(filesystem, ".")
	if err != nil {
		return err
	}
	rootSnapshot, err := snapshotArtifactFileInfo(rootInfo, "root")
	if err != nil {
		return err
	}
	rootEntry := artifactEntry{name: rootSnapshot.name, info: rootSnapshot}
	if !rootEntry.info.isDir {
		return fmt.Errorf("%w: artifact root is not a directory", ErrInvalidArtifact)
	}
	remainingEntries := maximumEntries
	var walk func(string, artifactEntry) error
	walk = func(filePath string, entry artifactEntry) error {
		if err := visit(filePath, entry); err != nil {
			return err
		}
		if !entry.info.isDir {
			return nil
		}
		entries, err := readVerifiedArtifactDirectory(filesystem, filePath, remainingEntries, entry)
		if err != nil {
			return err
		}
		remainingEntries -= len(entries)
		sort.SliceStable(entries, func(left, right int) bool {
			leftName, rightName := entries[left].name, entries[right].name
			if filePath == "." && (leftName == "SKILL.md" || rightName == "SKILL.md") {
				return leftName == "SKILL.md" && rightName != "SKILL.md"
			}
			return leftName < rightName
		})
		for _, child := range entries {
			childPath := child.name
			if filePath != "." {
				childPath = filePath + "/" + childPath
			}
			if err := walk(childPath, child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(".", rootEntry)
}

func readArtifactDirectory(filesystem fs.FS, directory string, remaining int) ([]artifactEntry, error) {
	info, err := statFSPath(filesystem, directory)
	if err != nil {
		return nil, err
	}
	snapshot, err := snapshotArtifactFileInfo(info, fmt.Sprintf("directory %q", directory))
	if err != nil {
		return nil, err
	}
	return readVerifiedArtifactDirectory(filesystem, directory, remaining, artifactEntry{name: snapshot.name, info: snapshot})
}

func readVerifiedArtifactDirectory(filesystem fs.FS, directory string, remaining int, expected artifactEntry) ([]artifactEntry, error) {
	opened, err := filesystem.Open(directory)
	if err != nil {
		if !isNilValue(opened) {
			_ = opened.Close()
		}
		return nil, err
	}
	if isNilValue(opened) {
		return nil, fmt.Errorf("%w: opening directory %q returned a nil file", ErrInvalidArtifact, directory)
	}
	openedInfo, statErr := opened.Stat()
	if statErr != nil {
		_ = opened.Close()
		return nil, statErr
	}
	openedSnapshot, snapshotErr := snapshotArtifactFileInfo(openedInfo, fmt.Sprintf("opened directory %q", directory))
	if snapshotErr != nil {
		_ = opened.Close()
		return nil, snapshotErr
	}
	if openedSnapshot.name != expected.info.name || openedSnapshot.mode.Type() != expected.info.mode.Type() || !openedSnapshot.isDir {
		_ = opened.Close()
		return nil, fmt.Errorf("%w: directory %q changed type or identity while being opened", ErrInvalidArtifact, directory)
	}
	if reader, ok := opened.(fs.ReadDirFile); ok {
		entries := make([]artifactEntry, 0)
		var resultErr error
		for {
			batchSize := 128
			if remaining-len(entries) < batchSize {
				batchSize = remaining - len(entries) + 1
			}
			batch, readErr := reader.ReadDir(batchSize)
			if len(batch) > remaining-len(entries) {
				resultErr = fmt.Errorf("%w: skill contains more than the configured entry limit", ErrLimitExceeded)
				break
			}
			for _, rawEntry := range batch {
				entry, entryErr := snapshotArtifactDirEntry(rawEntry)
				if entryErr != nil {
					resultErr = entryErr
					break
				}
				entries = append(entries, entry)
			}
			if resultErr != nil {
				break
			}
			if readErr != nil {
				if readErr != io.EOF {
					resultErr = readErr
				}
				break
			}
			if len(batch) == 0 {
				resultErr = io.ErrNoProgress
				break
			}
		}
		closeErr := opened.Close()
		if resultErr != nil {
			return nil, resultErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return entries, nil
	}
	closeErr := opened.Close()
	if closeErr != nil {
		return nil, closeErr
	}
	reader, ok := filesystem.(fs.ReadDirFS)
	if !ok {
		return nil, fmt.Errorf("%w: directory %q cannot be enumerated", ErrInvalidArtifact, directory)
	}
	rawEntries, err := reader.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	if len(rawEntries) > remaining {
		return nil, fmt.Errorf("%w: skill contains more than the configured entry limit", ErrLimitExceeded)
	}
	entries := make([]artifactEntry, 0, len(rawEntries))
	for _, rawEntry := range rawEntries {
		entry, entryErr := snapshotArtifactDirEntry(rawEntry)
		if entryErr != nil {
			return nil, entryErr
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func snapshotArtifactDirEntry(entry fs.DirEntry) (artifactEntry, error) {
	if isNilValue(entry) {
		return artifactEntry{}, fmt.Errorf("%w: directory returned a nil entry", ErrInvalidArtifact)
	}
	name := entry.Name()
	entryType := entry.Type()
	entryIsDir := entry.IsDir()
	if entryType&^fs.ModeType != 0 {
		return artifactEntry{}, fmt.Errorf("%w: directory entry %q returned invalid type bits", ErrInvalidArtifact, name)
	}
	info, err := entry.Info()
	if err != nil {
		return artifactEntry{}, err
	}
	snapshot, err := snapshotArtifactFileInfo(info, fmt.Sprintf("directory entry %q", name))
	if err != nil {
		return artifactEntry{}, err
	}
	if name != snapshot.name || entryType != snapshot.mode.Type() || entryIsDir != entryType.IsDir() || entryIsDir != snapshot.isDir {
		return artifactEntry{}, fmt.Errorf("%w: directory entry %q returned contradictory metadata", ErrInvalidArtifact, name)
	}
	return artifactEntry{name: name, info: snapshot}, nil
}

func snapshotArtifactFileInfo(info fs.FileInfo, label string) (artifactFileInfo, error) {
	if isNilValue(info) {
		return artifactFileInfo{}, fmt.Errorf("%w: %s returned nil information", ErrInvalidArtifact, label)
	}
	name := info.Name()
	mode := info.Mode()
	isDir := info.IsDir()
	if isDir != mode.IsDir() {
		return artifactFileInfo{}, fmt.Errorf("%w: %s returned contradictory file information", ErrInvalidArtifact, label)
	}
	return artifactFileInfo{name: name, mode: mode, isDir: isDir}, nil
}

func validateArtifactStructure(filePath string, limits Limits) error {
	if len(filePath) > limits.MaxPathBytes {
		return fmt.Errorf("%w: file path exceeds %d bytes", ErrLimitExceeded, limits.MaxPathBytes)
	}
	depth := strings.Count(filePath, "/") + 1
	if depth > limits.MaxPathDepth {
		return fmt.Errorf("%w: file path exceeds depth %d", ErrLimitExceeded, limits.MaxPathDepth)
	}
	return nil
}

func validateArtifactEntry(filePath string, entry artifactEntry) error {
	name := entry.name
	if filePath == "." {
		if name != "." {
			return fmt.Errorf("%w: root entry name %q is invalid", ErrInvalidArtifact, name)
		}
		return nil
	}
	if strings.Contains(name, `\`) {
		return fmt.Errorf("%w: directory entry name %q contains a backslash", ErrInvalidArtifact, name)
	}
	if name == "" || name == "." || name == ".." || !fs.ValidPath(name) || path.Clean(name) != name ||
		strings.Contains(name, "/") || hasWindowsVolumePrefix(name) {
		return fmt.Errorf("%w: directory entry name %q is not a normalized relative path component", ErrInvalidArtifact, name)
	}
	expected := name
	if parent := path.Dir(filePath); parent != "." {
		expected = parent + "/" + name
	}
	if expected != filePath {
		return fmt.Errorf("%w: directory entry %q does not match visited path %q", ErrInvalidArtifact, name, filePath)
	}
	return nil
}

func validateArtifactPath(filePath string) error {
	if filePath == "." || !fs.ValidPath(filePath) || path.Clean(filePath) != filePath || hasWindowsVolumePrefix(filePath) {
		return fmt.Errorf("%w: file path %q is not a normalized relative path", ErrInvalidArtifact, filePath)
	}
	if strings.Contains(filePath, `\`) {
		return fmt.Errorf("%w: file path %q contains a backslash", ErrInvalidArtifact, filePath)
	}
	for _, component := range strings.Split(filePath, "/") {
		if component == ".." {
			return fmt.Errorf("%w: file path %q contains ..", ErrInvalidArtifact, filePath)
		}
	}
	return nil
}

func readFSFile(filesystem fs.FS, filePath string, maximum int64) ([]byte, error) {
	info, err := statFSPath(filesystem, filePath)
	if err != nil {
		return nil, err
	}
	snapshot, err := snapshotArtifactFileInfo(info, fmt.Sprintf("file %q", filePath))
	if err != nil {
		return nil, err
	}
	return readVerifiedArtifactFile(filesystem, filePath, maximum, artifactEntry{name: snapshot.name, info: snapshot})
}

func readVerifiedArtifactFile(filesystem fs.FS, filePath string, maximum int64, expected artifactEntry) ([]byte, error) {
	file, err := filesystem.Open(filePath)
	if err != nil {
		if !isNilValue(file) {
			_ = file.Close()
		}
		return nil, err
	}
	if isNilValue(file) {
		return nil, fmt.Errorf("%w: opening file %q returned a nil file", ErrInvalidArtifact, filePath)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, statErr
	}
	openedSnapshot, snapshotErr := snapshotArtifactFileInfo(openedInfo, fmt.Sprintf("opened file %q", filePath))
	if snapshotErr != nil {
		_ = file.Close()
		return nil, snapshotErr
	}
	if openedSnapshot.name != expected.info.name || openedSnapshot.mode.Type() != expected.info.mode.Type() || !openedSnapshot.mode.IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: file %q changed type or identity while being opened", ErrInvalidArtifact, filePath)
	}
	data, readErr := readBounded(file, maximum)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func statFSPath(filesystem fs.FS, filePath string) (fs.FileInfo, error) {
	if statter, ok := filesystem.(fs.StatFS); ok {
		info, err := statter.Stat(filePath)
		if err != nil {
			return nil, err
		}
		if isNilValue(info) {
			return nil, fmt.Errorf("%w: stat %q returned nil information", ErrInvalidArtifact, filePath)
		}
		return info, nil
	}
	file, err := filesystem.Open(filePath)
	if err != nil {
		if !isNilValue(file) {
			_ = file.Close()
		}
		return nil, err
	}
	if isNilValue(file) {
		return nil, fmt.Errorf("%w: opening %q for stat returned a nil file", ErrInvalidArtifact, filePath)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return nil, statErr
	}
	if isNilValue(info) {
		return nil, fmt.Errorf("%w: stat %q returned nil information", ErrInvalidArtifact, filePath)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return info, nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	if maximum < 0 {
		return nil, ErrLimitExceeded
	}
	const maximumEmptyReads = 100
	const bufferSize = 32 * 1024
	var buffer []byte
	if maximum > 0 {
		size := int64(bufferSize)
		if maximum < size {
			size = maximum
		}
		buffer = make([]byte, int(size))
	}
	data := make([]byte, 0)
	remaining := maximum
	emptyReads := 0
	for remaining > 0 {
		readSize := len(buffer)
		if remaining < int64(readSize) {
			readSize = int(remaining)
		}
		count, readErr := reader.Read(buffer[:readSize])
		if count < 0 || count > readSize {
			return nil, fmt.Errorf("%w: reader returned invalid byte count %d", ErrInvalidArtifact, count)
		}
		if count > 0 {
			data = append(data, buffer[:count]...)
			remaining -= int64(count)
			emptyReads = 0
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= maximumEmptyReads {
				return nil, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return append([]byte(nil), data...), nil
			}
			return nil, readErr
		}
	}
	var probe [1]byte
	for {
		count, readErr := reader.Read(probe[:])
		if count < 0 || count > len(probe) {
			return nil, fmt.Errorf("%w: reader returned invalid byte count %d", ErrInvalidArtifact, count)
		}
		if count > 0 {
			return nil, ErrLimitExceeded
		}
		if readErr != nil {
			if readErr == io.EOF {
				return append([]byte(nil), data...), nil
			}
			return nil, readErr
		}
		emptyReads++
		if emptyReads >= maximumEmptyReads {
			return nil, io.ErrNoProgress
		}
	}
}

func parseSkillMarkdown(markdown []byte) (skillFrontmatter, string, error) {
	if !utf8.Valid(markdown) {
		return skillFrontmatter{}, "", fmt.Errorf("%w: SKILL.md is not valid UTF-8", ErrInvalidSkill)
	}
	content := string(markdown)
	firstEnd := strings.IndexByte(content, '\n')
	if firstEnd < 0 || strings.TrimSuffix(content[:firstEnd], "\r") != "---" {
		return skillFrontmatter{}, "", fmt.Errorf("%w: YAML frontmatter opening delimiter is required", ErrInvalidSkill)
	}
	frontmatterStart := firstEnd + 1
	frontmatterEnd := -1
	bodyStart := -1
	for offset := frontmatterStart; offset <= len(content); {
		lineEnd := strings.IndexByte(content[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content) - offset
		}
		line := strings.TrimSuffix(content[offset:offset+lineEnd], "\r")
		if line == "---" {
			frontmatterEnd = offset
			bodyStart = offset + lineEnd
			if bodyStart < len(content) && content[bodyStart] == '\n' {
				bodyStart++
			}
			break
		}
		if offset+lineEnd >= len(content) {
			break
		}
		offset += lineEnd + 1
	}
	if frontmatterEnd < 0 {
		return skillFrontmatter{}, "", fmt.Errorf("%w: YAML frontmatter closing delimiter is required", ErrInvalidSkill)
	}
	metadata, err := parseSkillFrontmatter([]byte(content[frontmatterStart:frontmatterEnd]))
	if err != nil {
		return skillFrontmatter{}, "", err
	}
	instructions := strings.TrimSpace(content[bodyStart:])
	if instructions == "" {
		return skillFrontmatter{}, "", fmt.Errorf("%w: Markdown body is required", ErrInvalidSkill)
	}
	return metadata, instructions, nil
}

func parseSkillFrontmatter(raw []byte) (skillFrontmatter, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return skillFrontmatter{}, fmt.Errorf("%w: invalid YAML frontmatter: %v", ErrInvalidSkill, err)
	}
	remaining := maxSkillFrontmatterYAMLNodes
	if err := inspectSkillYAMLNode(&document, &remaining); err != nil {
		return skillFrontmatter{}, err
	}
	mapping, err := skillFrontmatterMapping(&document)
	if err != nil {
		return skillFrontmatter{}, err
	}
	var metadata skillFrontmatter
	seen := make(map[string]struct{}, 2)
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		keyNode := mapping.Content[index]
		valueNode := mapping.Content[index+1]
		if keyNode == nil || keyNode.Kind != yaml.ScalarNode {
			return skillFrontmatter{}, fmt.Errorf("%w: YAML frontmatter keys must be strings", ErrInvalidSkill)
		}
		key := strings.TrimSpace(keyNode.Value)
		if key == "<<" || keyNode.Tag == "!!merge" {
			return skillFrontmatter{}, fmt.Errorf("%w: YAML merge keys are not allowed", ErrInvalidSkill)
		}
		if key != "name" && key != "description" {
			continue
		}
		if _, exists := seen[key]; exists {
			return skillFrontmatter{}, fmt.Errorf("%w: duplicate %s", ErrInvalidSkill, key)
		}
		seen[key] = struct{}{}
		value, err := skillFrontmatterScalar(valueNode, key)
		if err != nil {
			return skillFrontmatter{}, err
		}
		switch key {
		case "name":
			metadata.Name = value
		case "description":
			metadata.Description = value
		}
	}
	if metadata.Name == "" {
		return skillFrontmatter{}, fmt.Errorf("%w: name is required", ErrInvalidSkill)
	}
	if metadata.Description == "" {
		return skillFrontmatter{}, fmt.Errorf("%w: description is required", ErrInvalidSkill)
	}
	if err := validateSkillName(metadata.Name); err != nil {
		return skillFrontmatter{}, err
	}
	if err := validateSkillDescription(metadata.Description); err != nil {
		return skillFrontmatter{}, err
	}
	return metadata, nil
}

func inspectSkillYAMLNode(node *yaml.Node, remaining *int) error {
	if node == nil {
		return fmt.Errorf("%w: YAML frontmatter contains a nil node", ErrInvalidSkill)
	}
	if *remaining <= 0 {
		return fmt.Errorf("%w: YAML frontmatter exceeds %d nodes", ErrLimitExceeded, maxSkillFrontmatterYAMLNodes)
	}
	*remaining--
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("%w: YAML aliases are not allowed", ErrInvalidSkill)
	}
	if node.Tag == "!!merge" || node.Tag == "!!binary" {
		return fmt.Errorf("%w: YAML tag %s is not allowed", ErrInvalidSkill, node.Tag)
	}
	for _, child := range node.Content {
		if err := inspectSkillYAMLNode(child, remaining); err != nil {
			return err
		}
	}
	return nil
}

func skillFrontmatterMapping(document *yaml.Node) (*yaml.Node, error) {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0] == nil {
		return nil, fmt.Errorf("%w: YAML frontmatter must be a mapping", ErrInvalidSkill)
	}
	mapping := document.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: YAML frontmatter must be a mapping", ErrInvalidSkill)
	}
	if len(mapping.Content)%2 != 0 {
		return nil, fmt.Errorf("%w: YAML frontmatter mapping is malformed", ErrInvalidSkill)
	}
	return mapping, nil
}

func skillFrontmatterScalar(node *yaml.Node, field string) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("%w: %s must be a string", ErrInvalidSkill, field)
	}
	switch node.Tag {
	case "", "!", "!!str":
	default:
		return "", fmt.Errorf("%w: %s must be a string", ErrInvalidSkill, field)
	}
	value := strings.TrimSpace(node.Value)
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidSkill, field)
	}
	return value, nil
}

func validateSkillName(name string) error {
	if len(name) > maxSkillNameBytes || !skillNamePattern.MatchString(name) {
		return fmt.Errorf("%w: name must be 1-%d lowercase alphanumeric kebab-case characters", ErrInvalidSkill, maxSkillNameBytes)
	}
	return nil
}

func validateSkillDescription(description string) error {
	if !utf8.ValidString(description) {
		return fmt.Errorf("%w: description must be valid UTF-8", ErrInvalidSkill)
	}
	for _, character := range description {
		if unicode.IsControl(character) && character != '\t' && character != '\n' && character != '\r' {
			return fmt.Errorf("%w: description must not contain control characters", ErrInvalidSkill)
		}
	}
	if utf8.RuneCountInString(description) > maxSkillDescriptionRunes {
		return fmt.Errorf("%w: description exceeds %d characters", ErrInvalidSkill, maxSkillDescriptionRunes)
	}
	return nil
}

func digestSkill(skill Skill) string {
	hasher := sha256.New()
	writeHashField(hasher, "source-id", []byte(skill.sourceID))
	writeHashField(hasher, "locator", []byte(skill.locator))
	writeHashField(hasher, "revision", []byte(skill.revision))
	for _, file := range skill.files {
		writeHashField(hasher, "file-path", []byte(file.path))
		writeHashField(hasher, "file-bytes", file.data)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
