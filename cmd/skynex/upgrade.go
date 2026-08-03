package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/binaryinstall"
)

// GitHubRelease represents the minimal structure we need from the GitHub API
type GitHubRelease struct {
	TagName string `json:"tag_name"`
}

const releaseAllowedSigner = "skynex-release ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINUht44Rk/nWIXqcKizh8SWdnECJZOQ5yuPjaxaWxAAF skynex release signing\n"

const (
	upgradeHTTPTimeout = 30 * time.Second
	maxArchiveBytes    = 100 * 1024 * 1024
	maxManifestBytes   = 2 * 1024 * 1024
	maxBinaryBytes     = 250 * 1024 * 1024
)

// selfUpgrade downloads and installs the latest skynex binary if a newer version is available.
// Returns nil if already up-to-date or if the current version is "dev" (running from source).
// Non-fatal errors log warnings and allow the package update to continue.
func selfUpgrade() error {
	// Dev builds skip upgrade
	if version == "dev" {
		fmt.Println("    Running dev build, skipping binary upgrade.")
		return nil
	}

	// Fetch latest release from GitHub
	latestTag, err := getLatestGitHubTag()
	if err != nil {
		return fmt.Errorf("failed to fetch latest GitHub release: %w", err)
	}

	// Strip 'v' prefix from tag (e.g., "v1.5.0" -> "1.5.0")
	latestVersion := strings.TrimPrefix(latestTag, "v")
	if !validReleaseTag(latestTag) || !validSemver(latestVersion) {
		return fmt.Errorf("latest release tag is not valid semver")
	}
	if !validSemver(version) {
		return fmt.Errorf("current version %q is not valid semver", version)
	}

	// Compare versions
	comparison, err := compareSemver(version, latestVersion)
	if err != nil {
		return err
	}
	if comparison >= 0 {
		fmt.Printf("    Already up to date (v%s); refusing downgrade or equal version.\n", version)
		return nil
	}

	// Detect platform
	osType := runtime.GOOS
	arch := runtime.GOARCH

	// Download the release archive
	tmpDir, err := os.MkdirTemp("", "skynex-upgrade-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Determine archive name and URL
	var archiveName, downloadURL string
	if osType == "windows" {
		archiveName = fmt.Sprintf("skynex_%s_%s_%s.zip", latestVersion, osType, arch)
	} else {
		archiveName = fmt.Sprintf("skynex_%s_%s_%s.tar.gz", latestVersion, osType, arch)
	}
	downloadURL = fmt.Sprintf("https://github.com/joeldevz/skynex/releases/download/v%s/%s", latestVersion, archiveName)

	// Download archive
	archivePath := filepath.Join(tmpDir, archiveName)
	if err := downloadFile(downloadURL, archivePath); err != nil {
		return fmt.Errorf("failed to download archive: %w", err)
	}

	// Verify checksum (optional but preferred)
	checksumURL := fmt.Sprintf("https://github.com/joeldevz/skynex/releases/download/v%s/checksums.txt", latestVersion)
	if err := verifyChecksum(archivePath, archiveName, checksumURL); err != nil {
		return fmt.Errorf("release authenticity verification failed: %w", err)
	}

	// Extract binary from archive
	binaryPath := filepath.Join(tmpDir, "skynex")
	if osType == "windows" {
		binaryPath = filepath.Join(tmpDir, "skynex.exe")
	}

	if err := extractBinary(archivePath, binaryPath, osType); err != nil {
		return fmt.Errorf("failed to extract binary: %w", err)
	}

	// Get current binary path
	currentBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to determine current binary path: %w", err)
	}

	// Resolve symlinks
	currentBinary, err = filepath.EvalSymlinks(currentBinary)
	if err != nil {
		return fmt.Errorf("failed to resolve symlinks: %w", err)
	}

	// Replace binary atomically
	if err := binaryinstall.Install(binaryPath, currentBinary); err != nil {
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	fmt.Printf("    Upgraded skynex from v%s to v%s — restart or re-run for changes to take effect.\n", version, latestVersion)
	return nil
}

// getLatestGitHubTag fetches the latest release tag from GitHub API
func getLatestGitHubTag() (string, error) {
	url := "https://api.github.com/repos/joeldevz/skynex/releases/latest"
	resp, err := upgradeHTTPClient().Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	var release GitHubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestBytes)).Decode(&release); err != nil {
		return "", err
	}

	if release.TagName == "" {
		return "", fmt.Errorf("no tag_name found in response")
	}

	return release.TagName, nil
}

// downloadFile downloads a file from URL to destination
func downloadFile(url, dest string) error {
	resp, err := upgradeHTTPClient().Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	written, err := io.Copy(f, io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return err
	}
	if written == 0 || written > maxArchiveBytes {
		return fmt.Errorf("download exceeds %d-byte limit or is empty", maxArchiveBytes)
	}
	return f.Close()
}

func upgradeHTTPClient() *http.Client { return &http.Client{Timeout: upgradeHTTPTimeout} }

// verifyChecksum downloads checksums.txt and verifies the archive
func verifyChecksum(archivePath, archiveName, checksumURL string) error {
	// Download checksums.txt
	checksumResp, err := upgradeHTTPClient().Get(checksumURL)
	if err != nil {
		return fmt.Errorf("failed to download checksums.txt: %w", err)
	}
	defer checksumResp.Body.Close()

	if checksumResp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksums.txt not found (HTTP %d)", checksumResp.StatusCode)
	}

	checksumData, err := io.ReadAll(io.LimitReader(checksumResp.Body, maxManifestBytes+1))
	if err != nil {
		return err
	}
	if len(checksumData) > maxManifestBytes {
		return fmt.Errorf("checksums.txt exceeds size limit")
	}

	signatureResp, err := upgradeHTTPClient().Get(checksumURL + ".sig")
	if err != nil {
		return fmt.Errorf("failed to download checksums signature: %w", err)
	}
	defer signatureResp.Body.Close()
	if signatureResp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksums.txt.sig not found (HTTP %d)", signatureResp.StatusCode)
	}
	signature, err := io.ReadAll(io.LimitReader(signatureResp.Body, maxManifestBytes+1))
	if err != nil {
		return fmt.Errorf("read checksums signature: %w", err)
	}
	if len(signature) > maxManifestBytes {
		return fmt.Errorf("checksums signature exceeds size limit")
	}
	if err := verifySSHSignature(checksumData, signature, []byte(releaseAllowedSigner)); err != nil {
		return err
	}

	// Find the checksum line for this archive
	var expectedChecksum string
	for _, line := range strings.Split(string(checksumData), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == archiveName {
			expectedChecksum = fields[0]
			break
		}
	}

	if expectedChecksum == "" {
		return fmt.Errorf("archive %s not found in checksums.txt", archiveName)
	}

	// Compute SHA256 of the archive
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}

	actualChecksum := fmt.Sprintf("%x", hash.Sum(nil))

	if !strings.EqualFold(actualChecksum, expectedChecksum) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
	}

	return nil
}

func verifySSHSignature(data, signature, allowedSigner []byte) error {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return fmt.Errorf("ssh-keygen unavailable: %w", err)
	}
	allowedFile, err := os.CreateTemp("", "skynex-allowed-signers-")
	if err != nil {
		return err
	}
	allowedPath := allowedFile.Name()
	defer os.Remove(allowedPath)
	if err := allowedFile.Chmod(0o600); err != nil {
		allowedFile.Close()
		return err
	}
	if _, err := allowedFile.Write(allowedSigner); err != nil {
		allowedFile.Close()
		return err
	}
	if err := allowedFile.Close(); err != nil {
		return err
	}
	signatureFile, err := os.CreateTemp("", "skynex-checksums-signature-")
	if err != nil {
		return err
	}
	signaturePath := signatureFile.Name()
	defer os.Remove(signaturePath)
	if _, err := signatureFile.Write(signature); err != nil {
		signatureFile.Close()
		return err
	}
	if err := signatureFile.Close(); err != nil {
		return err
	}
	verify := exec.Command("ssh-keygen", "-Y", "verify", "-f", allowedPath, "-I", "skynex-release", "-n", "file", "-s", signaturePath)
	verify.Stdin = bytes.NewReader(data)
	if output, err := verify.CombinedOutput(); err != nil {
		return fmt.Errorf("invalid checksums.txt signature: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// extractBinary extracts the skynex binary from the archive (tar.gz or zip)
func extractBinary(archivePath, outputPath string, osType string) error {
	if osType == "windows" {
		return extractBinaryFromZip(archivePath, outputPath)
	}
	return extractBinaryFromTarGz(archivePath, outputPath)
}

// extractBinaryFromTarGz extracts the skynex binary from a tar.gz archive
func extractBinaryFromTarGz(archivePath, outputPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	count := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		count++
		if count > 1 || header.Name != "skynex" || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maxBinaryBytes {
			return fmt.Errorf("archive must contain exactly one regular skynex member")
		}
		out, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
		if err != nil {
			return err
		}
		written, copyErr := io.CopyN(out, tr, header.Size)
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil || written != header.Size {
			return fmt.Errorf("read binary member: %w", copyErr)
		}
	}
	if count != 1 {
		return fmt.Errorf("skynex binary not found in archive")
	}
	return os.Chmod(outputPath, 0o755)
}

// extractBinaryFromZip extracts the skynex.exe binary from a zip archive
func extractBinaryFromZip(archivePath, outputPath string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	if len(r.File) != 1 {
		return fmt.Errorf("archive must contain exactly one member")
	}
	for _, zf := range r.File {
		if zf.Name == "skynex.exe" && zf.Mode().IsRegular() && !strings.HasSuffix(zf.Name, "/") && zf.UncompressedSize64 > 0 && zf.UncompressedSize64 <= maxBinaryBytes {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			out, err := os.Create(outputPath)
			if err != nil {
				return err
			}
			defer out.Close()

			if _, err := io.Copy(out, io.LimitReader(rc, maxBinaryBytes+1)); err != nil {
				return err
			}
			if info, err := os.Stat(outputPath); err != nil || info.Size() > maxBinaryBytes || info.Size() == 0 {
				return fmt.Errorf("extracted binary exceeds size limit")
			}
			return nil
		}
	}

	return fmt.Errorf("skynex.exe not found in archive")
}

func validReleaseTag(tag string) bool {
	return len(tag) > 1 && (tag[0] != 'v' || len(tag) > 1) && validSemver(strings.TrimPrefix(tag, "v"))
}

func validSemver(value string) bool {
	parts := strings.SplitN(value, "+", 2)
	corePre := strings.SplitN(parts[0], "-", 2)
	core := strings.Split(corePre[0], ".")
	if len(core) != 3 {
		return false
	}
	for _, p := range core {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return false
		}
		if _, err := strconv.ParseUint(p, 10, 64); err != nil {
			return false
		}
	}
	preRelease := len(corePre) == 2
	for sectionIndex, section := range append(corePre[1:], parts[1:]...) {
		if section == "" {
			continue
		}
		for _, p := range strings.Split(section, ".") {
			if p == "" {
				return false
			}
			if preRelease && sectionIndex == 0 && len(p) > 1 && p[0] == '0' {
				allDigits := true
				for _, r := range p {
					if r < '0' || r > '9' {
						allDigits = false
						break
					}
				}
				if allDigits {
					return false
				}
			}
			for _, r := range p {
				if !(r == '-' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
					return false
				}
			}
		}
	}
	return true
}

func compareSemver(a, b string) (int, error) {
	parse := func(v string) ([]uint64, []string, error) {
		parts := strings.SplitN(v, "-", 2)
		core := strings.Split(parts[0], ".")
		if !validSemver(v) {
			return nil, nil, fmt.Errorf("invalid semver %q", v)
		}
		nums := make([]uint64, 3)
		for i := range nums {
			n, _ := strconv.ParseUint(core[i], 10, 64)
			nums[i] = n
		}
		if len(parts) == 1 {
			return nums, nil, nil
		}
		return nums, strings.Split(parts[1], "."), nil
	}
	an, ap, err := parse(a)
	if err != nil {
		return 0, err
	}
	bn, bp, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := range an {
		if an[i] < bn[i] {
			return -1, nil
		}
		if an[i] > bn[i] {
			return 1, nil
		}
	}
	if len(ap) == 0 && len(bp) > 0 {
		return 1, nil
	}
	if len(ap) > 0 && len(bp) == 0 {
		return -1, nil
	}
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] == bp[i] {
			continue
		}
		an, ae := strconv.ParseUint(ap[i], 10, 64)
		bn, be := strconv.ParseUint(bp[i], 10, 64)
		if ae == nil && be == nil {
			if an < bn {
				return -1, nil
			}
			return 1, nil
		}
		if ae == nil {
			return -1, nil
		}
		if be == nil {
			return 1, nil
		}
		if ap[i] < bp[i] {
			return -1, nil
		}
		return 1, nil
	}
	if len(ap) < len(bp) {
		return -1, nil
	}
	if len(ap) > len(bp) {
		return 1, nil
	}
	return 0, nil
}

// replaceBinary replaces the current binary with a new one atomically
func replaceBinary(currentPath, newPath string) error {
	return binaryinstall.Install(newPath, currentPath)
}
