package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
)

const maxOpenAIOAuthBytes int64 = 64 << 10

// openAIOAuthCredential mirrors the pinned OpenCode 1.18.16 OAuth storage
// contract. It deliberately excludes API-key fields. Optional fields are kept
// because the built-in Codex plugin uses accountId and enterpriseUrl when they
// are present.
type openAIOAuthCredential struct {
	Type          string `json:"type"`
	Refresh       string `json:"refresh"`
	Access        string `json:"access"`
	Expires       int64  `json:"expires"`
	AccountID     string `json:"accountId,omitempty"`
	EnterpriseURL string `json:"enterpriseUrl,omitempty"`
}

// ErrOpenAIOAuthSessionCredentialChanged is returned when a runtime changes its
// private OAuth credential. A process in the runtime (including a Bash tool)
// has the same filesystem authority as OpenCode, so a legitimate refresh cannot
// be distinguished from an untrusted write without a credential broker. The
// session therefore fails closed and is permanently unusable after any change.
var ErrOpenAIOAuthSessionCredentialChanged = errors.New("OpenAI OAuth credential changed inside the runtime; refresh cannot be distinguished from an untrusted write without a provider proxy")

// OpenAIOAuthSession carries a trusted, validated OAuth credential across a
// serialized local experiment. It never refreshes or writes the source: a
// dedicated profile must be renewed before the experiment and remain valid for
// each requested run horizon. Each runtime gets a fresh data directory. On
// release the private auth.json must still contain the exact credential that
// was staged; otherwise the session fails closed.
type OpenAIOAuthSession struct {
	mu         sync.Mutex
	credential openAIOAuthCredential
	taintErr   error
	now        func() time.Time
}

type openAIOAuthLease struct {
	session     *OpenAIOAuthSession
	authPath    string
	releaseOnce sync.Once
	err         error
}

// NewOpenAIOAuthSession imports a protected auth.json and retains only its
// validated OpenAI OAuth entry. Callers should create one session per serialized
// experiment and must never reuse the user's ambient XDG data directory.
func NewOpenAIOAuthSession(sourcePath string) (*OpenAIOAuthSession, error) {
	if err := requireCurrentCleanOAuthPlatform(); err != nil {
		return nil, err
	}
	credential, err := loadExactOpenAIOAuth(sourcePath)
	if err != nil {
		return nil, err
	}
	return &OpenAIOAuthSession{
		credential: credential,
		now:        time.Now,
	}, nil
}

func (s *OpenAIOAuthSession) stage(ctx context.Context, dataHome string, minimumValidity time.Duration) (*openAIOAuthLease, error) {
	if s == nil {
		return nil, fmt.Errorf("OpenAI OAuth session is nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("OpenAI OAuth stage context is nil")
	}
	if minimumValidity < 0 {
		return nil, fmt.Errorf("OpenAI OAuth minimum validity must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.taintErr != nil {
		err := fmt.Errorf("OpenAI OAuth session is not reusable: %w", s.taintErr)
		s.mu.Unlock()
		return nil, err
	}
	if err := s.ensureCredentialValidityLocked(minimumValidity); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	authPath, err := saveOpenAIOAuthCredential(s.credential, dataHome)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return &openAIOAuthLease{session: s, authPath: authPath}, nil
}

func (l *openAIOAuthLease) release(inspect bool) error {
	if l == nil {
		return nil
	}
	l.releaseOnce.Do(func() {
		defer l.session.mu.Unlock()
		if !inspect {
			return
		}
		updated, err := loadExactOpenAIOAuth(l.authPath)
		if err != nil {
			l.err = l.session.taint(fmt.Errorf("%w: runtime profile is invalid: %v", ErrOpenAIOAuthSessionCredentialChanged, err))
			return
		}
		if updated != l.session.credential {
			l.err = l.session.taint(ErrOpenAIOAuthSessionCredentialChanged)
		}
	})
	return l.err
}

func (s *OpenAIOAuthSession) taint(reason error) error {
	if s.taintErr == nil {
		s.taintErr = reason
	}
	return s.taintErr
}

func saveOpenAIOAuthCredential(credential openAIOAuthCredential, dataHome string) (string, error) {
	if dataHome == "" || !filepath.IsAbs(dataHome) {
		return "", fmt.Errorf("private XDG data home must be absolute")
	}
	destinationDirectory := filepath.Join(dataHome, "opencode")
	if err := os.MkdirAll(destinationDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create private OpenCode data directory: %w", err)
	}
	if err := os.Chmod(destinationDirectory, 0o700); err != nil {
		return "", fmt.Errorf("secure private OpenCode data directory: %w", err)
	}
	destination := filepath.Join(destinationDirectory, "auth.json")
	if err := baseline.SaveJSON(destination, map[string]openAIOAuthCredential{
		"openai": credential,
	}, baseline.IOOptions{MaxBytes: maxOpenAIOAuthBytes}); err != nil {
		return "", fmt.Errorf("write private OpenAI OAuth credential: %w", err)
	}
	return destination, nil
}

func loadExactOpenAIOAuth(path string) (openAIOAuthCredential, error) {
	return loadOpenAIOAuthWithPolicy(path)
}

func loadOpenAIOAuthWithPolicy(path string) (openAIOAuthCredential, error) {
	var zero openAIOAuthCredential
	if path == "" || !filepath.IsAbs(path) {
		return zero, fmt.Errorf("OpenAI OAuth source must be an absolute path")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return zero, fmt.Errorf("inspect OpenAI OAuth source: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return zero, fmt.Errorf("OpenAI OAuth source must be a regular non-symlink file")
	}
	if before.Mode().Perm() != 0o600 {
		return zero, fmt.Errorf("OpenAI OAuth source permissions must be 0600, got %04o", before.Mode().Perm())
	}
	if before.Size() > maxOpenAIOAuthBytes {
		return zero, fmt.Errorf("OpenAI OAuth source exceeds %d bytes", maxOpenAIOAuthBytes)
	}
	if err := validateCredentialFileOwner(before); err != nil {
		return zero, err
	}

	var providers map[string]json.RawMessage
	if err := baseline.LoadJSON(path, &providers, baseline.IOOptions{MaxBytes: maxOpenAIOAuthBytes}); err != nil {
		return zero, fmt.Errorf("load OpenAI OAuth source: %w", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return zero, fmt.Errorf("reinspect OpenAI OAuth source: %w", err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		return zero, fmt.Errorf("OpenAI OAuth source changed while reading")
	}
	if len(providers) != 1 {
		return zero, fmt.Errorf("dedicated OAuth profile must contain exactly one provider")
	}
	raw, ok := providers["openai"]
	if !ok {
		return zero, fmt.Errorf("OpenAI OAuth source has no openai credential")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var credential openAIOAuthCredential
	if err := decoder.Decode(&credential); err != nil {
		return zero, fmt.Errorf("decode openai OAuth credential: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		// json.Decoder reports io.EOF after exactly one value. Avoid accepting a
		// second value even though the outer document was already validated.
		if err == nil {
			return zero, fmt.Errorf("decode openai OAuth credential: multiple values")
		}
		return zero, fmt.Errorf("decode trailing openai OAuth credential: %w", err)
	}
	if credential.Type != "oauth" {
		return zero, fmt.Errorf("openai credential type must be oauth")
	}
	if strings.TrimSpace(credential.Access) == "" || strings.TrimSpace(credential.Refresh) == "" {
		return zero, fmt.Errorf("openai OAuth access and refresh tokens must not be empty")
	}
	if credential.Access != strings.TrimSpace(credential.Access) || credential.Refresh != strings.TrimSpace(credential.Refresh) {
		return zero, fmt.Errorf("openai OAuth tokens must not contain surrounding whitespace")
	}
	if credential.Expires < 0 {
		return zero, fmt.Errorf("openai OAuth expires must be non-negative")
	}
	if credential.AccountID != strings.TrimSpace(credential.AccountID) || credential.EnterpriseURL != strings.TrimSpace(credential.EnterpriseURL) {
		return zero, fmt.Errorf("openai OAuth optional identifiers must not contain surrounding whitespace")
	}
	if credential.EnterpriseURL != "" {
		return zero, fmt.Errorf("enterprise OpenAI OAuth endpoints are not supported by the clean-profile contract")
	}
	return credential, nil
}

func validateControlledConfigHome(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("controlled OpenCode config home must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect controlled OpenCode config home: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("controlled OpenCode config home must be a non-symlink directory")
	}
	return nil
}

func rejectManagedOpenCodeConfig() error {
	for _, path := range []string{"/etc/opencode/opencode.json", "/etc/opencode/opencode.jsonc"} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("managed OpenCode configuration %q prevents a clean evaluation profile", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect managed OpenCode configuration %q: %w", path, err)
		}
	}
	return nil
}
