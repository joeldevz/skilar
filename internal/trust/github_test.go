package trust

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyGitHubRefRequiresBoundVerifiedCommit(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/example/skills/commits/v1.2.3" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"sha":"` + sha + `","commit":{"verification":{"verified":true,"reason":"valid"}}}`))
	}))
	defer server.Close()
	old := apiBaseURL
	apiBaseURL = server.URL
	t.Cleanup(func() { apiBaseURL = old })

	if err := VerifyGitHubRef("https://github.com/example/skills.git", "v1.2.3", sha); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyGitHubRefFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sha":"0123456789abcdef0123456789abcdef01234567","commit":{"verification":{"verified":false,"reason":"unsigned"}}}`))
	}))
	defer server.Close()
	old := apiBaseURL
	apiBaseURL = server.URL
	t.Cleanup(func() { apiBaseURL = old })

	if err := VerifyGitHubRef("https://github.com/example/skills", "v1", "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("expected unsigned commit rejection")
	}
	if err := VerifyGitHubRef("https://git.example/skills", "v1", "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("expected non-GitHub repository rejection")
	}
}
