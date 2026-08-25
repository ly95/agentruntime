package skills

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPGitHubFetcherReturnsCommitBoundedSnapshot(t *testing.T) {
	const (
		commitSHA = "1111111111111111111111111111111111111111"
		rootSHA   = "2222222222222222222222222222222222222222"
		skillSHA  = "3333333333333333333333333333333333333333"
		blobSHA   = "4444444444444444444444444444444444444444"
	)
	content := []byte("---\nname: remote-skill\ndescription: Remote.\n---\nRead only.\n")
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization=%q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.RequestURI() {
		case "/repos/owner/repo/commits/main":
			fmt.Fprintf(writer, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, commitSHA, rootSHA)
		case "/repos/owner/repo/git/trees/" + rootSHA:
			fmt.Fprintf(writer, `{"sha":%q,"tree":[{"path":"skills","mode":"040000","type":"tree","sha":%q}],"truncated":false}`, rootSHA, skillSHA)
		case "/repos/owner/repo/git/trees/" + skillSHA + "?recursive=1":
			fmt.Fprintf(writer, `{"sha":%q,"tree":[{"path":"SKILL.md","mode":"100644","type":"blob","sha":%q,"size":%d}],"truncated":false}`, skillSHA, blobSHA, len(content))
		case "/repos/owner/repo/git/blobs/" + blobSHA:
			fmt.Fprintf(writer, `{"sha":%q,"size":%d,"encoding":"base64","content":%q}`, blobSHA, len(content), base64.StdEncoding.EncodeToString(content))
		default:
			http.NotFound(writer, request)
		}
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	fetcher, err := NewHTTPGitHubFetcher(HTTPGitHubFetcherConfig{
		Client: server.Client(), BaseURL: server.URL, Token: "test-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fetcher.Fetch(t.Context(), GitHubFetchRequest{
		Repository: "owner/repo", Ref: "main", Path: "skills",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CommitSHA != commitSHA || len(result.Files) != 1 || string(result.Files[0].Data) != string(content) {
		t.Fatalf("result=%+v", result)
	}
	set, err := LoadSet(t.Context(), NewGitHubSource(GitHubSourceConfig{
		ID: "remote", Repository: "owner/repo", Ref: "main", Path: "skills", Fetcher: fetcher,
	}))
	if err != nil || set.Len() != 1 {
		t.Fatalf("set=%+v err=%v", set, err)
	}
}

func TestHTTPGitHubFetcherSurfacesRateLimitWithoutRetry(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writer.Header().Set("X-RateLimit-Remaining", "0")
		writer.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
		writer.Header().Set("Retry-After", "30")
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	fetcher, err := NewHTTPGitHubFetcher(HTTPGitHubFetcherConfig{Client: server.Client(), BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Fetch(t.Context(), GitHubFetchRequest{Repository: "owner/repo", Ref: "main", Path: "skills"})
	if !errors.Is(err, ErrGitHubRateLimited) || requests != 1 {
		t.Fatalf("error=%v requests=%d", err, requests)
	}
	var rate *GitHubRateLimitError
	if !errors.As(err, &rate) || rate.RetryAfter != 30*time.Second || rate.Reset.IsZero() {
		t.Fatalf("rate error=%+v", rate)
	}
}

func TestGitHubSourcePreservesSafeFetcherClassifications(t *testing.T) {
	reset := time.Unix(100, 0)
	for _, test := range []struct {
		name  string
		cause error
		want  error
	}{
		{name: "rate limit", cause: &GitHubRateLimitError{Reset: reset, RetryAfter: 30 * time.Second}, want: ErrGitHubRateLimited},
		{name: "authentication", cause: ErrGitHubAuthentication, want: ErrGitHubAuthentication},
		{name: "resource limit", cause: fmt.Errorf("private transport detail: %w", ErrLimitExceeded), want: ErrLimitExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := NewGitHubSource(GitHubSourceConfig{
				ID: "remote", Repository: "owner/repo", Ref: "main", Path: "skills",
				Fetcher: GitHubFetcherFunc(func(context.Context, GitHubFetchRequest) (GitHubFetchResult, error) {
					return GitHubFetchResult{}, test.cause
				}),
			})
			_, err := source.Resolve(t.Context())
			if !errors.Is(err, ErrInvalidSource) || !errors.Is(err, test.want) {
				t.Fatalf("Resolve error=%v, want ErrInvalidSource and %v", err, test.want)
			}
			if strings.Contains(err.Error(), "private transport detail") {
				t.Fatalf("Resolve exposed private fetcher detail: %v", err)
			}
			if test.want == ErrGitHubRateLimited {
				var rate *GitHubRateLimitError
				if !errors.As(err, &rate) || !rate.Reset.Equal(reset) || rate.RetryAfter != 30*time.Second {
					t.Fatalf("Resolve rate error=%+v", rate)
				}
			}
		})
	}
}

func TestHTTPGitHubFetcherRejectsRedirectWithoutForwardingCredentials(t *testing.T) {
	requests := 0
	redirectTargetReached := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path == "/redirect-target" {
			redirectTargetReached = true
			if request.Header.Get("Authorization") != "" {
				t.Error("authorization header reached redirect target")
			}
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(writer, request, "/redirect-target", http.StatusFound)
	}))
	defer server.Close()
	fetcher, err := NewHTTPGitHubFetcher(HTTPGitHubFetcherConfig{
		Client: server.Client(), BaseURL: server.URL, Token: "secret-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Fetch(t.Context(), GitHubFetchRequest{Repository: "owner/repo", Ref: "main", Path: "skills"})
	if err == nil || requests != 1 || redirectTargetReached {
		t.Fatalf("error=%v requests=%d redirect_target=%t", err, requests, redirectTargetReached)
	}
}

func TestHTTPGitHubFetcherRejectsOversizedResponseWithTrailingWhitespace(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}` + "     "))
	}))
	defer server.Close()
	fetcher, err := NewHTTPGitHubFetcher(HTTPGitHubFetcherConfig{
		Client: server.Client(), BaseURL: server.URL, MaxResponseBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Fetch(t.Context(), GitHubFetchRequest{Repository: "owner/repo", Ref: "main", Path: "skills"})
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("error=%v, want ErrLimitExceeded", err)
	}
}
