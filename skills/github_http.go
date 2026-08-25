package skills

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrGitHubRateLimited    = errors.New("skills: GitHub API rate limited")
	ErrGitHubAuthentication = errors.New("skills: GitHub API authentication failed")
)

const (
	defaultGitHubAPIBase          = "https://api.github.com"
	defaultGitHubAPIVersion       = "2022-11-28"
	defaultGitHubUserAgent        = "agentruntime-skills/1"
	defaultGitHubResponseMaxBytes = int64(8 * 1024 * 1024)
)

// GitHubRateLimitError exposes scheduling data without returning response
// bodies, URLs, or credentials. HTTPGitHubFetcher never sleeps or retries.
type GitHubRateLimitError struct {
	Reset      time.Time
	RetryAfter time.Duration
}

func (err *GitHubRateLimitError) Error() string { return ErrGitHubRateLimited.Error() }
func (err *GitHubRateLimitError) Unwrap() error { return ErrGitHubRateLimited }

type HTTPGitHubFetcherConfig struct {
	Client           *http.Client
	Token            string
	BaseURL          string
	APIVersion       string
	UserAgent        string
	MaxFiles         int
	MaxFileBytes     int64
	MaxTotalBytes    int64
	MaxResponseBytes int64
}

// HTTPGitHubFetcher is a bounded, read-only GitHub REST implementation of
// GitHubFetcher. It resolves Ref once, walks to the configured directory by Git
// tree identity, rejects truncated/symbolic/submodule entries, fetches exact
// blobs, and performs no implicit retry.
type HTTPGitHubFetcher struct {
	client           *http.Client
	token            string
	baseURL          string
	apiVersion       string
	userAgent        string
	maxFiles         int
	maxFileBytes     int64
	maxTotalBytes    int64
	maxResponseBytes int64
}

func NewHTTPGitHubFetcher(config HTTPGitHubFetcherConfig) (*HTTPGitHubFetcher, error) {
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = defaultGitHubAPIBase
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: GitHub API base URL must be absolute HTTPS without credentials, query, or fragment", ErrInvalidSource)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	parsed.RawPath = ""
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// GitHub object identity must remain bound to the configured API origin.
	// Clone the client so redirects are rejected without mutating host state or
	// forwarding an authorization header to another endpoint.
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &clientCopy
	if config.Token != strings.TrimSpace(config.Token) || strings.ContainsAny(config.Token, "\r\n") {
		return nil, fmt.Errorf("%w: GitHub token is malformed", ErrInvalidSource)
	}
	apiVersion := strings.TrimSpace(config.APIVersion)
	if apiVersion == "" {
		apiVersion = defaultGitHubAPIVersion
	}
	userAgent := strings.TrimSpace(config.UserAgent)
	if userAgent == "" {
		userAgent = defaultGitHubUserAgent
	}
	if strings.ContainsAny(apiVersion+userAgent, "\r\n") {
		return nil, fmt.Errorf("%w: GitHub API headers are malformed", ErrInvalidSource)
	}
	limits := DefaultLimits()
	maxFiles := config.MaxFiles
	if maxFiles == 0 {
		maxFiles = limits.MaxFilesPerSkill
	}
	maxFileBytes := config.MaxFileBytes
	if maxFileBytes == 0 {
		maxFileBytes = limits.MaxFileBytes
	}
	maxTotalBytes := config.MaxTotalBytes
	if maxTotalBytes == 0 {
		maxTotalBytes = limits.MaxSkillBytes
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultGitHubResponseMaxBytes
	}
	if maxFiles <= 0 || maxFileBytes <= 0 || maxTotalBytes <= 0 || maxResponseBytes <= 0 {
		return nil, fmt.Errorf("%w: GitHub fetch limits must be positive", ErrInvalidSource)
	}
	return &HTTPGitHubFetcher{
		client: client, token: config.Token, baseURL: strings.TrimSuffix(parsed.String(), "/"),
		apiVersion: apiVersion, userAgent: userAgent,
		maxFiles: maxFiles, maxFileBytes: maxFileBytes,
		maxTotalBytes: maxTotalBytes, maxResponseBytes: maxResponseBytes,
	}, nil
}

type githubCommitPayload struct {
	SHA    string `json:"sha"`
	Commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
}

type githubTreePayload struct {
	SHA       string            `json:"sha"`
	Tree      []githubTreeEntry `json:"tree"`
	Truncated bool              `json:"truncated"`
}

type githubTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

type githubBlobPayload struct {
	SHA      string `json:"sha"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

func (fetcher *HTTPGitHubFetcher) Fetch(ctx context.Context, request GitHubFetchRequest) (GitHubFetchResult, error) {
	if fetcher == nil {
		return GitHubFetchResult{}, fmt.Errorf("%w: HTTP GitHub fetcher is nil", ErrInvalidSource)
	}
	if ctx == nil {
		return GitHubFetchResult{}, fmt.Errorf("%w: context is required", ErrInvalidSource)
	}
	if err := validateGitHubRepository(request.Repository); err != nil {
		return GitHubFetchResult{}, err
	}
	if err := validateGitHubRef(request.Ref); err != nil {
		return GitHubFetchResult{}, err
	}
	requestedPath, err := normalizedRepositoryPath(request.Path)
	if err != nil {
		return GitHubFetchResult{}, err
	}
	root := "/repos/" + request.Repository
	var commit githubCommitPayload
	if err := fetcher.getJSON(ctx, root+"/commits/"+url.PathEscape(request.Ref), &commit); err != nil {
		return GitHubFetchResult{}, err
	}
	commitSHA := normalizedGitHubSHA(commit.SHA)
	treeSHA := normalizedGitHubSHA(commit.Commit.Tree.SHA)
	if commitSHA == "" || treeSHA == "" {
		return GitHubFetchResult{}, fmt.Errorf("%w: GitHub commit response has invalid identity", ErrInvalidArtifact)
	}
	for _, component := range strings.Split(requestedPath, "/") {
		tree, err := fetcher.getTree(ctx, root, treeSHA, false)
		if err != nil {
			return GitHubFetchResult{}, err
		}
		found := false
		for _, entry := range tree.Tree {
			if entry.Path != component {
				continue
			}
			if entry.Type != "tree" || entry.Mode != "040000" {
				return GitHubFetchResult{}, fmt.Errorf("%w: requested GitHub Skill path is not a directory", ErrInvalidArtifact)
			}
			treeSHA = normalizedGitHubSHA(entry.SHA)
			if treeSHA == "" {
				return GitHubFetchResult{}, fmt.Errorf("%w: GitHub directory has invalid identity", ErrInvalidArtifact)
			}
			found = true
			break
		}
		if !found {
			return GitHubFetchResult{}, fmt.Errorf("%w: requested GitHub Skill directory does not exist", fs.ErrNotExist)
		}
	}
	tree, err := fetcher.getTree(ctx, root, treeSHA, true)
	if err != nil {
		return GitHubFetchResult{}, err
	}
	if tree.Truncated {
		return GitHubFetchResult{}, fmt.Errorf("%w: GitHub Skill tree was truncated", ErrLimitExceeded)
	}
	files := make([]GitHubFile, 0, len(tree.Tree))
	var totalBytes int64
	for _, entry := range tree.Tree {
		if entry.Type == "tree" && entry.Mode == "040000" {
			continue
		}
		mode, ok := githubRegularFileMode(entry.Type, entry.Mode)
		if !ok {
			return GitHubFetchResult{}, fmt.Errorf("%w: GitHub Skill contains a symbolic link, submodule, or special entry", ErrInvalidArtifact)
		}
		if err := validateArtifactPath(entry.Path); err != nil {
			return GitHubFetchResult{}, fmt.Errorf("%w: GitHub tree returned an invalid path", ErrInvalidArtifact)
		}
		if len(files) >= fetcher.maxFiles {
			return GitHubFetchResult{}, fmt.Errorf("%w: GitHub Skill contains more than %d files", ErrLimitExceeded, fetcher.maxFiles)
		}
		if entry.Size < 0 || entry.Size > fetcher.maxFileBytes || entry.Size > fetcher.maxTotalBytes-totalBytes {
			return GitHubFetchResult{}, fmt.Errorf("%w: GitHub Skill blob exceeds configured bounds", ErrLimitExceeded)
		}
		blobSHA := normalizedGitHubSHA(entry.SHA)
		if blobSHA == "" {
			return GitHubFetchResult{}, fmt.Errorf("%w: GitHub blob has invalid identity", ErrInvalidArtifact)
		}
		data, err := fetcher.getBlob(ctx, root, blobSHA, entry.Size)
		if err != nil {
			return GitHubFetchResult{}, err
		}
		totalBytes += int64(len(data))
		files = append(files, GitHubFile{Path: entry.Path, Data: data, Mode: mode})
	}
	return GitHubFetchResult{CommitSHA: commitSHA, Files: files}, nil
}

func (fetcher *HTTPGitHubFetcher) getTree(ctx context.Context, root, sha string, recursive bool) (githubTreePayload, error) {
	endpoint := root + "/git/trees/" + sha
	if recursive {
		endpoint += "?recursive=1"
	}
	var tree githubTreePayload
	if err := fetcher.getJSON(ctx, endpoint, &tree); err != nil {
		return githubTreePayload{}, err
	}
	if returnedSHA := normalizedGitHubSHA(tree.SHA); returnedSHA == "" || returnedSHA != sha {
		return githubTreePayload{}, fmt.Errorf("%w: GitHub tree response has mismatched identity", ErrInvalidArtifact)
	}
	return tree, nil
}

func (fetcher *HTTPGitHubFetcher) getBlob(ctx context.Context, root, sha string, expectedSize int64) ([]byte, error) {
	var blob githubBlobPayload
	if err := fetcher.getJSON(ctx, root+"/git/blobs/"+sha, &blob); err != nil {
		return nil, err
	}
	if normalizedGitHubSHA(blob.SHA) != sha || blob.Size != expectedSize || blob.Encoding != "base64" {
		return nil, fmt.Errorf("%w: GitHub blob response differs from the tree", ErrInvalidArtifact)
	}
	content := strings.NewReplacer("\n", "", "\r", "").Replace(blob.Content)
	data, err := base64.StdEncoding.DecodeString(content)
	if err != nil || int64(len(data)) != expectedSize {
		return nil, fmt.Errorf("%w: GitHub blob content is invalid", ErrInvalidArtifact)
	}
	return data, nil
}

func (fetcher *HTTPGitHubFetcher) getJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fetcher.baseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: construct GitHub API request", ErrInvalidSource)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", fetcher.apiVersion)
	request.Header.Set("User-Agent", fetcher.userAgent)
	if fetcher.token != "" {
		request.Header.Set("Authorization", "Bearer "+fetcher.token)
	}
	response, err := fetcher.client.Do(request)
	if err != nil {
		return newSafeSourceError("skills: GitHub API request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return githubHTTPStatusError(response)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, fetcher.maxResponseBytes+1))
	if err != nil {
		return newSafeSourceError("skills: GitHub API response read failed", err)
	}
	if int64(len(payload)) > fetcher.maxResponseBytes {
		return fmt.Errorf("%w: GitHub API response exceeds configured bounds", ErrLimitExceeded)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: GitHub API returned invalid JSON", ErrInvalidArtifact)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: GitHub API response exceeded bounds or contained trailing JSON", ErrInvalidArtifact)
	}
	return nil
}

func githubHTTPStatusError(response *http.Response) error {
	if response.StatusCode == http.StatusUnauthorized {
		return ErrGitHubAuthentication
	}
	if response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden &&
			(response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "")) {
		return &GitHubRateLimitError{
			Reset:      parseGitHubReset(response.Header.Get("X-RateLimit-Reset")),
			RetryAfter: parseGitHubRetryAfter(response.Header.Get("Retry-After")),
		}
	}
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: GitHub resource does not exist", fs.ErrNotExist)
	}
	if response.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: GitHub API denied access", fs.ErrPermission)
	}
	return fmt.Errorf("%w: GitHub API returned HTTP %d", ErrInvalidSource, response.StatusCode)
}

func parseGitHubReset(value string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func parseGitHubRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		duration := time.Until(deadline)
		if duration > 0 {
			return duration
		}
	}
	return 0
}

func githubRegularFileMode(entryType, mode string) (fs.FileMode, bool) {
	if entryType != "blob" {
		return 0, false
	}
	switch mode {
	case "100644":
		return 0o644, true
	case "100755":
		return 0o755, true
	default:
		return 0, false
	}
}

func normalizedGitHubSHA(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !commitSHAPattern.MatchString(value) || strings.Trim(value, "0") == "" {
		return ""
	}
	return value
}
