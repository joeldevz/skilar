// Package trust contains independent authenticity checks for downloaded
// package sources. It intentionally does not implement signatures itself:
// GitHub's verified-commit result is the trust gate, and all failures are
// fail-closed.
package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	apiBaseURL = "https://api.github.com"
	httpClient = &http.Client{Timeout: 15 * time.Second}
)

type commitResponse struct {
	SHA    string `json:"sha"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Committer *struct {
		Login string `json:"login"`
	} `json:"committer"`
	Commit struct {
		Verification struct {
			Verified bool   `json:"verified"`
			Reason   string `json:"reason"`
		} `json:"verification"`
	} `json:"commit"`
}

// VerifyGitHubRef verifies that ref in repoURL resolves to expectedSHA and
// that GitHub reports the resulting commit as cryptographically verified.
// The repository owner/name is taken from the repository URL, so a response
// from another repository cannot satisfy this check. Network, HTTP, JSON,
// identity, and signature failures are all errors.
func VerifyGitHubRef(repoURL, ref, expectedSHA string) error {
	return verifyGitHubRef(repoURL, ref, expectedSHA, "")
}

// VerifyGitHubRefForIdentity additionally pins the repository owner and both
// GitHub identities reported for the commit. Callers use this for catalogued
// sources; arbitrary repository URLs are never accepted as equivalent.
func VerifyGitHubRefForIdentity(repoURL, ref, expectedSHA, expectedOwner, expectedLogin string) error {
	return verifyGitHubRef(repoURL, ref, expectedSHA, expectedOwner+"\x00"+expectedLogin)
}

func verifyGitHubRef(repoURL, ref, expectedSHA, identity string) error {
	owner, repo, err := githubRepository(repoURL)
	if err != nil {
		return err
	}
	if !validSHA(expectedSHA) || ref == "" {
		return errors.New("invalid release commit binding")
	}
	endpoint := strings.TrimRight(apiBaseURL, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/commits/" + url.PathEscape(ref)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create GitHub verification request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "skynex-installer")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub verification unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub verification failed: HTTP %s", resp.Status)
	}
	var result commitResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode GitHub verification: %w", err)
	}
	if !strings.EqualFold(result.SHA, expectedSHA) {
		return fmt.Errorf("GitHub ref %q resolved to %q, expected %q", ref, result.SHA, expectedSHA)
	}
	if !result.Commit.Verification.Verified {
		return fmt.Errorf("GitHub commit %s is not cryptographically verified (%s)", result.SHA, result.Commit.Verification.Reason)
	}
	if identity != "" {
		parts := strings.SplitN(identity, "\x00", 2)
		if !strings.EqualFold(owner, parts[0]) || len(parts) != 2 || result.Author == nil || result.Committer == nil || result.Author.Login != parts[1] || result.Committer.Login != parts[1] {
			return fmt.Errorf("GitHub commit identity is not pinned to %s", parts[1])
		}
	}
	return nil
}

// VerifyGitHubReleaseArchive is the release-archive entry point. Callers must
// pass the SHA resolved from the archive's tag before extracting bytes; it
// deliberately performs the same independent repository/ref/signature gate.
func VerifyGitHubReleaseArchive(repoURL, tag, resolvedSHA string) error {
	if tag == "" || tag == "HEAD" {
		return errors.New("release archive requires an explicit tag")
	}
	return VerifyGitHubRef(repoURL, tag, resolvedSHA)
}

func githubRepository(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" {
		return "", "", fmt.Errorf("trust gate requires a GitHub repository URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid GitHub repository URL")
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func validSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
