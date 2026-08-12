package baseline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestArtifactRoundTripIsPrivateCanonicalAndIntegrityChecked(t *testing.T) {
	samples := []json.RawMessage{json.RawMessage(`{ "z": 2, "a": 1 }`)}
	artifact, err := NewArtifact("current", "suite", time.Unix(123, 456), validFingerprint(), samples, map[string]json.RawMessage{"metric": json.RawMessage(`{"n": 1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact.Samples[0]) != `{"a":1,"z":2}` {
		t.Fatalf("sample not canonicalized: %s", artifact.Samples[0])
	}
	path := filepath.Join(t.TempDir(), "nested", "baseline.json")
	if err := artifact.Save(path, IOOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := Load(path, IOOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Integrity.Digest != artifact.Integrity.Digest || string(loaded.Samples[0]) != `{"a":1,"z":2}` {
		t.Fatalf("round trip changed artifact: %+v", loaded)
	}

	var tampered map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["label"] = "tampered"
	data, _ = json.Marshal(tampered)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, IOOptions{}); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("tampered artifact accepted: %v", err)
	}
}

func TestLoadJSONRejectsOversizeDuplicateUnknownAndSymlink(t *testing.T) {
	directory := t.TempDir()
	oversize := filepath.Join(directory, "oversize.json")
	if err := os.WriteFile(oversize, []byte(`{"value":"123456789"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var destination struct {
		Value string `json:"value"`
	}
	if err := LoadJSON(oversize, &destination, IOOptions{MaxBytes: 5}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize input accepted: %v", err)
	}
	duplicate := filepath.Join(directory, "duplicate.json")
	os.WriteFile(duplicate, []byte(`{"value":"a","value":"b"}`), 0o600)
	if err := LoadJSON(duplicate, &destination, IOOptions{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate key accepted: %v", err)
	}
	unknown := filepath.Join(directory, "unknown.json")
	os.WriteFile(unknown, []byte(`{"value":"a","extra":true}`), 0o600)
	if err := LoadJSON(unknown, &destination, IOOptions{Strict: true}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field accepted: %v", err)
	}
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(unknown, link); err == nil {
		if err := LoadJSON(link, &destination, IOOptions{}); err == nil || !strings.Contains(err.Error(), "non-regular") {
			t.Fatalf("symlink accepted: %v", err)
		}
	}
}

func TestSaveJSONDoesNotClobberOnMarshalFailureAndRejectsSymlinkTarget(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "result.json")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(path, map[string]any{"bad": make(chan int)}, IOOptions{}); err == nil {
		t.Fatal("unmarshalable value was saved")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Fatalf("existing result was clobbered: %q", data)
	}
	target := filepath.Join(directory, "target.json")
	os.WriteFile(target, []byte(`{}`), 0o600)
	link := filepath.Join(directory, "result-link.json")
	if err := os.Symlink(target, link); err == nil {
		if err := SaveJSON(link, map[string]bool{"ok": true}, IOOptions{}); err == nil || !strings.Contains(err.Error(), "non-regular") {
			t.Fatalf("symlink result target accepted: %v", err)
		}
	}
}

func TestArtifactRejectsDuplicateKeysInsideRawSamples(t *testing.T) {
	_, err := NewArtifact("label", "suite", time.Now(), validFingerprint(), []json.RawMessage{json.RawMessage(`{"x":1,"x":2}`)}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate sample key accepted: %v", err)
	}
}

func TestArtifactRequiresObjectSamples(t *testing.T) {
	_, err := NewArtifact("label", "suite", time.Now(), validFingerprint(), []json.RawMessage{json.RawMessage(`null`)}, nil)
	if err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Fatalf("non-object sample accepted: %v", err)
	}
}
