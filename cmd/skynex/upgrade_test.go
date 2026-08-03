package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifySSHSignatureValidAndRejectsTampering(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen unavailable")
	}
	dir := t.TempDir()
	private := filepath.Join(dir, "test-key")
	if output, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "test", "-f", private).CombinedOutput(); err != nil {
		t.Fatalf("generate fixture key: %v (%s)", err, output)
	}
	public, err := os.ReadFile(private + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := []byte("skynex-release " + strings.TrimSpace(string(public)) + "\n")
	data := []byte("sha256  skynex_linux_amd64.tar.gz\n")
	manifest := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("ssh-keygen", "-Y", "sign", "-n", "file", "-f", private, manifest).CombinedOutput(); err != nil {
		t.Fatalf("sign fixture: %v (%s)", err, output)
	}
	signature, err := os.ReadFile(manifest + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySSHSignature(data, signature, allowed); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := verifySSHSignature([]byte("tampered\n"), signature, allowed); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	badSignature := append([]byte(nil), signature...)
	badSignature[len(badSignature)/2] ^= 1
	if err := verifySSHSignature(data, badSignature, allowed); err == nil {
		t.Fatal("tampered signature accepted")
	}
	wrongPrivate := filepath.Join(dir, "wrong-key")
	if output, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "wrong", "-f", wrongPrivate).CombinedOutput(); err != nil {
		t.Fatalf("generate wrong fixture key: %v (%s)", err, output)
	}
	wrongManifest := filepath.Join(dir, "wrong-checksums")
	if err := os.WriteFile(wrongManifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("ssh-keygen", "-Y", "sign", "-n", "file", "-f", wrongPrivate, wrongManifest).CombinedOutput(); err != nil {
		t.Fatalf("sign with wrong fixture key: %v (%s)", err, output)
	}
	wrongSignature, err := os.ReadFile(wrongManifest + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySSHSignature(data, wrongSignature, allowed); err == nil {
		t.Fatal("signature from wrong key accepted")
	}
	if err := verifySSHSignature(data, signature, []byte("malformed signer\n")); err == nil {
		t.Fatal("malformed allowed signer accepted")
	}
}
