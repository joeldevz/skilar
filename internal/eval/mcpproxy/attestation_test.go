package mcpproxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testNonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestAttestationIsCanonicalProtectedAndFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := writeAttestationAtomically(path, "worker", testNonce, []string{"zeta", "alpha"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("attestation mode = %v, want 0600 regular", info.Mode())
	}
	attestation, err := VerifyAttestation(path, "worker", testNonce, []string{"alpha", "zeta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(attestation.RawTools) != 2 || attestation.RawTools[0] != "alpha" || attestation.RawTools[1] != "zeta" {
		t.Fatalf("raw tools = %#v", attestation.RawTools)
	}
	if _, err := VerifyAttestation(path, "worker", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", []string{"alpha", "zeta"}); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("stale nonce error = %v", err)
	}
}

func TestAttestationRejectsDuplicateMembersAndUnsafeMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.json")
	raw := []byte(`{"schema_version":1,"mcp_name":"worker","nonce":"` + testNonce + `","raw_tools":["alpha"],"raw_tools":["alpha"]}` + "\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAttestation(path, "worker", testNonce, []string{"alpha"}); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("duplicate member error = %v", err)
	}
	if err := writeAttestationAtomically(path, "worker", testNonce, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAttestation(path, "worker", testNonce, []string{"alpha"}); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("unsafe mode error = %v", err)
	}
}

func TestRuntimeManifestBindsFreshMaterialWithoutReplacingDeclaredCommand(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manifest.json")
	attestationPath := filepath.Join(root, "attestation.json")
	bindings := []RuntimeBinding{{MCPName: "worker", AttestationPath: attestationPath, Nonce: testNonce}}
	if err := WriteRuntimeManifest(path, bindings); err != nil {
		t.Fatal(err)
	}
	declared := Config{
		MCPName: "worker", ExpectedTools: []string{"worker_result"},
		Environment: map[string]string{"SKX_SCENARIO": "one"}, Command: []string{"/bin/false", "fixed"},
	}
	bound, err := BindRuntimeManifest(declared, path)
	if err != nil {
		t.Fatal(err)
	}
	if bound.AttestationPath != attestationPath || bound.Nonce != testNonce || bound.Command[0] != "/bin/false" || bound.Environment["SKX_SCENARIO"] != "one" {
		t.Fatalf("bound config = %#v", bound)
	}
}

func TestAttestationRejectsMultipleHardLinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tools.json")
	if err := writeAttestationAtomically(path, "worker", testNonce, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(root, "second-link.json")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := VerifyAttestation(path, "worker", testNonce, []string{"alpha"}); !errors.Is(err, ErrInvalidAttestation) {
		t.Fatalf("multiply linked attestation error = %v", err)
	}
}
