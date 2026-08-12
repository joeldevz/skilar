package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/joeldevz/skynex/internal/safefs"
)

const maxHermeticOpenCodeProfileFile = int64(8 << 20)

func hermeticOpenCodeMode() bool {
	return os.Getenv("OPENCODE_DISABLE_PROJECT_CONFIG") == "1"
}

func resolveHermeticOpenCodeExecutable(declared, worktree string) (string, error) {
	if declared == "" || strings.TrimSpace(declared) != declared || strings.IndexByte(declared, 0) >= 0 {
		return "", errors.New("execution: hermetic OpenCode requires an exact configured executable")
	}
	if filepath.IsAbs(declared) {
		if filepath.Clean(declared) != declared {
			return "", errors.New("execution: hermetic OpenCode absolute executable path is not canonical")
		}
		if err := validateHermeticOpenCodeExecutableFile(declared); err != nil {
			return "", err
		}
		return declared, nil
	}

	if !strings.HasPrefix(declared, "./") || strings.Contains(declared, `\`) {
		return "", errors.New("execution: hermetic relative OpenCode executable must be canonical ./path")
	}
	relative := strings.TrimPrefix(declared, "./")
	if relative == "" || path.Clean(relative) != relative {
		return "", errors.New("execution: hermetic relative OpenCode executable path is not canonical")
	}
	components := strings.Split(relative, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("execution: hermetic relative OpenCode executable path is unsafe")
		}
	}
	if !filepath.IsAbs(worktree) || filepath.Clean(worktree) != worktree {
		return "", errors.New("execution: hermetic OpenCode worktree is not an exact absolute path")
	}
	worktreeRoot, err := safefs.Open(worktree)
	if err != nil {
		return "", errors.New("execution: hermetic OpenCode worktree is unavailable or unsafe")
	}
	defer worktreeRoot.Close()

	candidate := filepath.Join(worktree, filepath.FromSlash(relative))
	contained, err := filepath.Rel(worktree, candidate)
	if err != nil || contained == "." || filepath.IsAbs(contained) || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return "", errors.New("execution: hermetic relative OpenCode executable escapes its worktree")
	}
	current := ""
	for index, component := range components {
		current = path.Join(current, component)
		info, statErr := worktreeRoot.Lstat(current)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || (index < len(components)-1 && !info.IsDir()) {
			return "", errors.New("execution: hermetic relative OpenCode executable traverses an unsafe directory")
		}
	}
	if err := validateHermeticOpenCodeExecutableFile(candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

func snapshotHermeticOpenCodeExecutable(executable string) (os.FileInfo, error) {
	info, err := os.Lstat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return nil, errors.New("execution: hermetic OpenCode executable is unavailable or unsafe")
	}
	return info, nil
}

func revalidateHermeticOpenCodeExecutable(declared, worktree, executable string, expected os.FileInfo) error {
	currentPath, err := resolveHermeticOpenCodeExecutable(declared, worktree)
	if err != nil {
		return err
	}
	current, err := snapshotHermeticOpenCodeExecutable(currentPath)
	if err != nil || currentPath != executable || expected == nil || !os.SameFile(expected, current) ||
		expected.Mode() != current.Mode() || expected.Size() != current.Size() || !expected.ModTime().Equal(current.ModTime()) {
		return errors.New("execution: hermetic OpenCode executable changed before launch")
	}
	return nil
}

func validateHermeticOpenCodeExecutableFile(executable string) error {
	_, err := snapshotHermeticOpenCodeExecutable(executable)
	return err
}

func hermeticOpenCodeRuntimeEnv(parent, result string) ([]string, error) {
	parentData, err := hermeticParentXDG("XDG_DATA_HOME")
	if err != nil {
		return nil, err
	}
	parentConfig, err := hermeticParentXDG("XDG_CONFIG_HOME")
	if err != nil {
		return nil, err
	}
	paths := map[string]string{
		"HOME":            filepath.Join(parent, "home"),
		"TMPDIR":          filepath.Join(parent, "tmp"),
		"XDG_CONFIG_HOME": filepath.Join(parent, "xdg-config"),
		"XDG_DATA_HOME":   filepath.Join(parent, "xdg-data"),
		"XDG_CACHE_HOME":  filepath.Join(parent, "xdg-cache"),
		"XDG_STATE_HOME":  filepath.Join(parent, "xdg-state"),
		"XDG_RUNTIME_DIR": filepath.Join(parent, "xdg-runtime"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("execution: create hermetic OpenCode runtime directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, fmt.Errorf("execution: secure hermetic OpenCode runtime directory: %w", err)
		}
	}

	targetData := filepath.Join(paths["XDG_DATA_HOME"], "opencode")
	if err := os.MkdirAll(targetData, 0o700); err != nil {
		return nil, fmt.Errorf("execution: create hermetic OpenCode data directory: %w", err)
	}
	auth, err := readHermeticProfileFile(filepath.Join(parentData, "opencode", "auth.json"), true)
	if err != nil {
		return nil, fmt.Errorf("execution: read parent OpenCode auth: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetData, "auth.json"), auth, 0o600); err != nil {
		return nil, fmt.Errorf("execution: write hermetic OpenCode auth: %w", err)
	}

	config, err := readHermeticProfileFile(filepath.Join(parentConfig, "opencode", "opencode.json"), false)
	if err != nil {
		return nil, fmt.Errorf("execution: read parent OpenCode config: %w", err)
	}
	projected, err := projectHermeticOpenCodeConfig(config)
	if err != nil {
		return nil, fmt.Errorf("execution: project parent OpenCode config: %w", err)
	}
	targetConfig := filepath.Join(paths["XDG_CONFIG_HOME"], "opencode")
	if err := os.MkdirAll(targetConfig, 0o700); err != nil {
		return nil, fmt.Errorf("execution: create hermetic OpenCode config directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetConfig, "opencode.json"), projected, 0o600); err != nil {
		return nil, fmt.Errorf("execution: write hermetic OpenCode config: %w", err)
	}

	pathValue := os.Getenv("PATH")
	if pathValue == "" || strings.IndexByte(pathValue, 0) >= 0 {
		return nil, errors.New("execution: hermetic OpenCode requires PATH")
	}
	return []string{
		"PATH=" + pathValue,
		"HOME=" + paths["HOME"],
		"TMPDIR=" + paths["TMPDIR"],
		"XDG_CONFIG_HOME=" + paths["XDG_CONFIG_HOME"],
		"XDG_DATA_HOME=" + paths["XDG_DATA_HOME"],
		"XDG_CACHE_HOME=" + paths["XDG_CACHE_HOME"],
		"XDG_STATE_HOME=" + paths["XDG_STATE_HOME"],
		"XDG_RUNTIME_DIR=" + paths["XDG_RUNTIME_DIR"],
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"SKYNEX_RESULT_FILE=" + result,
	}, nil
}

func hermeticParentXDG(name string) (string, error) {
	value, ok := os.LookupEnv(name)
	if !ok || value == "" || strings.TrimSpace(value) != value || strings.IndexByte(value, 0) >= 0 ||
		!filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", fmt.Errorf("execution: hermetic OpenCode requires an exact parent %s", name)
	}
	info, err := os.Lstat(value)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("execution: hermetic OpenCode parent %s is unavailable", name)
	}
	return value, nil
}

func readHermeticProfileFile(path string, private bool) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > maxHermeticOpenCodeProfileFile {
		return nil, errors.New("profile file is not a bounded regular file")
	}
	if private && before.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("profile credential permissions are too broad")
	}
	raw, err := safefs.ReadFileAbsoluteVerified(path, maxHermeticOpenCodeProfileFile)
	if err != nil {
		return nil, err
	}
	after, afterErr := os.Lstat(path)
	if afterErr != nil {
		return nil, errors.New("read profile file")
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() ||
		before.Mode().Perm() != after.Mode().Perm() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("profile file changed while it was read")
	}
	if private && after.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("profile credential permissions are too broad")
	}
	return raw, nil
}

func projectHermeticOpenCodeConfig(raw []byte) ([]byte, error) {
	var config map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil || config == nil {
		return nil, errors.New("config must be one JSON object")
	}
	if trailing := decoder.Decode(new(any)); !errors.Is(trailing, io.EOF) {
		return nil, errors.New("config contains trailing JSON")
	}
	config["plugin"] = []any{}
	config["mcp"] = map[string]any{}
	return json.Marshal(config)
}
