package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEnvironmentDefaultsToPrivateIdentityDirectories(t *testing.T) {
	runDir := t.TempDir()
	t.Setenv("HOME", "/host/home/must-not-leak")
	t.Setenv("XDG_CONFIG_HOME", "/host/config/must-not-leak")
	env, err := buildEnvironment(nil, map[string]string{"EVAL_TOKEN": "explicit"}, runDir)
	if err != nil {
		t.Fatal(err)
	}
	values := envMap(env)
	if values["EVAL_TOKEN"] != "explicit" {
		t.Fatalf("explicit override missing: %v", values)
	}
	for _, key := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		directory := values[key]
		relative, relErr := filepath.Rel(runDir, directory)
		if directory == "" || relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("%s = %q is not private to %q", key, directory, runDir)
		}
		info, statErr := os.Stat(directory)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o", key, info.Mode().Perm())
		}
	}
}

func TestExtraArgsCannotOverrideIsolation(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"--port", "--port=9999", "--hostname", "--hostname=0.0.0.0", "--mdns"} {
		if err := validateExtraArgs([]string{arg}); err == nil {
			t.Errorf("argument %q was accepted", arg)
		}
	}
	if err := validateExtraArgs([]string{"--print-logs", "--log-level", "ERROR"}); err != nil {
		t.Fatalf("safe extra args rejected: %v", err)
	}
}

func TestBuildEnvironmentRejectsLoaderShellAndProxyInjection(t *testing.T) {
	for _, key := range []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "BASH_ENV", "ENV",
		"PYTHONPATH", "RUBYOPT", "PERL5OPT", "HTTP_PROXY", "HTTPS_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "LANG", "LC_ALL", "TZ",
	} {
		t.Run(key, func(t *testing.T) {
			if _, err := buildEnvironment([]string{key}, nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("allowlisted injection variable %q accepted: %v", key, err)
			}
			if _, err := buildEnvironment(nil, map[string]string{key: "injected"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("overridden injection variable %q accepted: %v", key, err)
			}
		})
	}
}

func TestBuildEnvironmentFixesLocaleTimezoneAndDropsAmbientTLSOverrides(t *testing.T) {
	t.Setenv("LANG", "lt_LT.UTF-8")
	t.Setenv("LC_ALL", "lt_LT.UTF-8")
	t.Setenv("TZ", "Europe/Vilnius")
	t.Setenv("SSL_CERT_FILE", "/tmp/ambient-ca.pem")
	t.Setenv("SSL_CERT_DIR", "/tmp/ambient-ca-dir")
	env, err := buildEnvironment(nil, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	values := envMap(env)
	if values["LANG"] != "C" || values["LC_ALL"] != "C" || values["TZ"] != "UTC" {
		t.Fatalf("locale/timezone are not deterministic: %#v", values)
	}
	if _, exists := values["SSL_CERT_FILE"]; exists {
		t.Fatalf("ambient SSL_CERT_FILE leaked: %#v", values)
	}
	if _, exists := values["SSL_CERT_DIR"]; exists {
		t.Fatalf("ambient SSL_CERT_DIR leaked: %#v", values)
	}
}

func TestBuildEnvironmentRejectsEmptyAndRelativePATHSegments(t *testing.T) {
	absolute := t.TempDir()
	for index, value := range []string{
		"", ".", "relative" + string(os.PathListSeparator) + absolute,
		absolute + string(os.PathListSeparator), string(os.PathListSeparator) + absolute,
	} {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			t.Setenv("PATH", value)
			if _, err := buildEnvironment(nil, nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "PATH") {
				t.Fatalf("unsafe PATH %q was accepted: %v", value, err)
			}
		})
	}
}

func envMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, item := range env {
		key, value, found := strings.Cut(item, "=")
		if found {
			result[key] = value
		}
	}
	return result
}
