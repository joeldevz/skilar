#Requires -Version 5.1
<#
.SYNOPSIS
  skynex — Install Script for Windows
  AI agent skills installer for OpenCode and Claude Code.

.DESCRIPTION
  Downloads and installs the skynex binary for Windows.
  Supports installation via a pre-built binary from GitHub Releases.

.EXAMPLE
  Download install.ps1 from a tagged release, verify its detached signature,
  then run the local file. Never pipe an unverified network response to iex.

  # Force a specific method:
  .\install.ps1 -Method binary
#>

[CmdletBinding()]
param(
  [ValidateSet("auto", "binary")]
  [string]$Method = "auto",

  [string]$InstallDir = ""
)

$ErrorActionPreference = "Stop"

$GITHUB_OWNER = "joeldevz"
$GITHUB_REPO  = "skynex"
$BINARY_NAME  = "skynex"
$MAX_COMPRESSED_BYTES = 100MB
$MAX_EXTRACTED_BYTES = 250MB
$RELEASE_BASE_URL = "https://github.com/$GITHUB_OWNER/$GITHUB_REPO/releases/download"

# ============================================================================
# Logging
# ============================================================================

function Write-Info    { param([string]$Message) Write-Host "[info] $Message" -ForegroundColor Blue }
function Write-Success { param([string]$Message) Write-Host "[ok] $Message"   -ForegroundColor Green }
function Write-Warn    { param([string]$Message) Write-Host "[warn] $Message"  -ForegroundColor Yellow }
function Write-Err     { param([string]$Message) Write-Host "[error] $Message" -ForegroundColor Red }
function Write-Step    { param([string]$Message) Write-Host "`n==> $Message"   -ForegroundColor Cyan }

function Stop-WithError {
  param([string]$Message)
  Write-Err $Message
  exit 1
}

function New-PrivateTempDirectory {
  $root = [IO.Path]::GetTempPath()
  for ($attempt = 0; $attempt -lt 10; $attempt++) {
    $candidate = Join-Path $root ("skynex-install-" + [IO.Path]::GetRandomFileName())
    try {
      [IO.Directory]::CreateDirectory($candidate) | Out-Null
      $acl = New-Object System.Security.AccessControl.DirectorySecurity
      $acl.SetAccessRuleProtection($true, $false)
      $identity = [Security.Principal.WindowsIdentity]::GetCurrent().User
      $acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule($identity, "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")))
      [IO.Directory]::SetAccessControl($candidate, $acl)
      return $candidate
    } catch [IO.IOException] {
      continue
    }
  }
  Stop-WithError "Could not create a private temporary directory"
}

function Test-SafeZip {
  param([string]$Path)
  Add-Type -AssemblyName System.IO.Compression.FileSystem
  $archive = [IO.Compression.ZipFile]::OpenRead($Path)
  try {
    $entries = @($archive.Entries)
    if ($entries.Count -ne 1) { Stop-WithError "Archive must contain exactly one entry: $BINARY_NAME.exe" }
    foreach ($entry in $entries) {
      $name = $entry.FullName.Replace('\', '/')
      if ([IO.Path]::IsPathRooted($name) -or $name -match '(^|/)\.\.?(/|$)' -or $name -ne "$BINARY_NAME.exe") {
        Stop-WithError "Unsafe archive member: $($entry.FullName)"
      }
      if ($entry.FullName.EndsWith('/') -or $entry.Length -lt 1) {
        Stop-WithError "Archive member is not a non-empty regular file"
      }
      $unixType = ($entry.ExternalAttributes -shr 16) -band 0xF000
      if ($unixType -eq 0xA000 -or $unixType -eq 0x4000) {
        Stop-WithError "Archive member is a link or special file"
      }
    }
  } finally {
    $archive.Dispose()
  }
}

# ============================================================================
# Banner
# ============================================================================

function Show-Banner {
  Write-Host ""
  Write-Host "   ____           _                    ____  _    _ _ _     " -ForegroundColor Cyan
  Write-Host "  / ___| ___  ___| | ___ ___  ___     / ___|| | _(_) | |___ " -ForegroundColor Cyan
  Write-Host " | |   / _ \/ __| |/ / '__\ \/ /     \___ \| |/ / | | / __|" -ForegroundColor Cyan
  Write-Host " | |__| (_) \__ \   <| |   >  <       ___) |   <| | | \__ \" -ForegroundColor Cyan
  Write-Host "  \____\___/|___/_|\_\_|  /_/\_\     |____/|_|\_\_|_|_|___/" -ForegroundColor Cyan
  Write-Host ""
  Write-Host "  AI agent skills installer for OpenCode and Claude Code" -ForegroundColor DarkGray
  Write-Host ""
}

# ============================================================================
# Platform detection
# ============================================================================

function Get-Platform {
  Write-Step "Detecting platform"

  $arch = if ([Environment]::Is64BitOperatingSystem) {
    if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
  } else {
    Stop-WithError "32-bit Windows is not supported."
  }

  Write-Success "Platform: Windows ($arch)"
  return $arch
}

# ============================================================================
# Prerequisites
# ============================================================================

function Test-Prerequisites {
  Write-Step "Checking prerequisites"

  $missing = @()
  if (-not (Get-Command "curl" -ErrorAction SilentlyContinue)) { $missing += "curl" }

  if ($missing.Count -gt 0) {
    Stop-WithError "Missing required tools: $($missing -join ', '). Please install them and try again."
  }

  Write-Success "curl is available"
}

# ============================================================================
# Install method detection
# ============================================================================

function Get-InstallMethod {
  param([string]$Forced)

  if ($Forced -ne "auto") {
    Write-Info "Using forced method: $Forced"
    return $Forced
  }

  Write-Step "Detecting best install method"
  Write-Info "Will download pre-built binary from GitHub Releases"
  return "binary"
}

# ============================================================================
# ============================================================================
# Install via binary download
# ============================================================================

function Get-LatestVersion {
  Write-Info "Fetching latest release from GitHub..."

  $url = "https://api.github.com/repos/$GITHUB_OWNER/$GITHUB_REPO/releases/latest"

  try {
    $response = Invoke-RestMethod -Uri $url -Headers @{ "User-Agent" = "skynex-installer" } -TimeoutSec 60 -UseBasicParsing
  } catch {
    Stop-WithError "Failed to fetch latest release. Rate limited? Try again later or use a signed release asset"
  }

  $version = $response.tag_name
  if (-not $version) {
    Stop-WithError "Could not determine latest version from GitHub API response"
  }

  if ($version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$') { Stop-WithError "Release tag is not valid semver" }
  Write-Success "Latest version: $version"
  return $version
}

function Install-ViaBinary {
  param([string]$Arch)

  Write-Step "Installing pre-built binary"

  $version = Get-LatestVersion
  $versionNumber = $version.TrimStart("v")

  $archiveName = "${BINARY_NAME}_${versionNumber}_windows_${Arch}.zip"
  $downloadUrl  = "$RELEASE_BASE_URL/$version/$archiveName"
  $checksumsUrl = "$RELEASE_BASE_URL/$version/checksums.txt"
  $signatureUrl = "$checksumsUrl.sig"

  $tmpDir = New-PrivateTempDirectory
  $tempDest = $null

  try {
    # Download archive
    Write-Info "Downloading $archiveName..."
    $archivePath = Join-Path $tmpDir $archiveName
    Invoke-WebRequest -Uri $downloadUrl -OutFile $archivePath -TimeoutSec 120 -UseBasicParsing

    $fileSize = (Get-Item $archivePath).Length
    if ($fileSize -lt 1000) {
      Stop-WithError "Downloaded file is suspiciously small ($fileSize bytes). Archive may not exist for this platform."
    }
    $compressedLimit = if ($env:SKYNEX_MAX_COMPRESSED_BYTES) { [long]$env:SKYNEX_MAX_COMPRESSED_BYTES } else { $MAX_COMPRESSED_BYTES }
    if ($fileSize -gt $compressedLimit) { Stop-WithError "Downloaded archive exceeds compressed size limit" }
    Write-Success "Downloaded $archiveName ($fileSize bytes)"

    # Verify checksum
    Write-Info "Verifying checksum..."
    $checksumsPath = Join-Path $tmpDir "checksums.txt"
    try { Invoke-WebRequest -Uri $checksumsUrl -OutFile $checksumsPath -TimeoutSec 60 -UseBasicParsing } catch { Stop-WithError "Could not download checksums.txt; refusing unverified archive" }
    $signaturePath = Join-Path $tmpDir "checksums.txt.sig"
    try { Invoke-WebRequest -Uri $signatureUrl -OutFile $signaturePath -TimeoutSec 60 -UseBasicParsing } catch { Stop-WithError "Could not download checksums.txt.sig; refusing unverified archive" }
    $sshKeygen = Get-Command "ssh-keygen" -ErrorAction SilentlyContinue
    if (-not $sshKeygen) { Stop-WithError "ssh-keygen is required to verify release authenticity" }
    $allowedSigners = Join-Path $tmpDir "allowed_signers"
    $signer = "skynex-release ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINUht44Rk/nWIXqcKizh8SWdnECJZOQ5yuPjaxaWxAAF skynex release signing`n"
    Set-Content -Path $allowedSigners -NoNewline -Encoding ascii -Value $signer
    # The signature covers checksums.txt byte for byte, so feed the file to
    # ssh-keygen verbatim. Piping `Get-Content` re-encodes the text and appends
    # a trailing newline, which makes every valid signature fail to verify.
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $sshKeygen.Source
    $psi.Arguments = '-Y verify -f "{0}" -I skynex-release -n file -s "{1}"' -f $allowedSigners, $signaturePath
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $verify = [Diagnostics.Process]::Start($psi)
    $checksumsBytes = [IO.File]::ReadAllBytes($checksumsPath)
    $verify.StandardInput.BaseStream.Write($checksumsBytes, 0, $checksumsBytes.Length)
    $verify.StandardInput.BaseStream.Flush()
    $verify.StandardInput.Close()
    $verify.StandardOutput.ReadToEnd() | Out-Null
    $verify.StandardError.ReadToEnd() | Out-Null
    $verify.WaitForExit()
    if ($verify.ExitCode -ne 0) { Stop-WithError "Invalid checksums.txt signature" }
    Write-Success "Release signature verified"
    $matches = @(Get-Content $checksumsPath | Where-Object {
      $parts = $_ -split "\s+"
      $parts.Count -ge 2 -and ($parts[1] -eq $archiveName -or $parts[1] -eq "*$archiveName")
    })
    if ($matches.Count -ne 1) { Stop-WithError "checksums.txt must contain exactly one entry for $archiveName" }
    $expectedChecksum = (($matches[0] -split "\s+")[0]).ToLower()
    if ($expectedChecksum -notmatch '^[0-9a-f]{64}$') { Stop-WithError "Malformed checksum for $archiveName" }
    $actualChecksum = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()
    if ($actualChecksum -ne $expectedChecksum) { Stop-WithError "Checksum mismatch!`n  Expected: $expectedChecksum`n  Got:      $actualChecksum" }
    Write-Success "Checksum verified"

    # Validate every entry before extracting anything.
    Test-SafeZip -Path $archivePath
    $recheck = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()
    if ($recheck -ne $expectedChecksum) { Stop-WithError "Archive changed after checksum verification" }

    # Extract only the expected regular binary.
    Write-Info "Extracting $BINARY_NAME..."
    $binaryPath = Join-Path $tmpDir "$BINARY_NAME.exe"
    $archive = [IO.Compression.ZipFile]::OpenRead($archivePath)
    try {
      $entry = $archive.Entries[0]
      $input = $entry.Open()
      try {
        $output = [IO.File]::Open($binaryPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
        try {
          $buffer = New-Object byte[] 65536
          [long]$total = 0
          while (($read = $input.Read($buffer, 0, $buffer.Length)) -gt 0) {
            $total += $read
            $extractedLimit = if ($env:SKYNEX_MAX_EXTRACTED_BYTES) { [long]$env:SKYNEX_MAX_EXTRACTED_BYTES } else { $MAX_EXTRACTED_BYTES }
            if ($total -gt $extractedLimit) { Stop-WithError "Extracted binary exceeds size limit" }
            $output.Write($buffer, 0, $read)
          }
        } finally { $output.Dispose() }
      } finally { $input.Dispose() }
    } finally { $archive.Dispose() }
    $extractedSize = (Get-Item $binaryPath).Length
    if ($extractedSize -lt 1 -or $extractedSize -gt $extractedLimit) { Stop-WithError "Extracted binary exceeds size limit" }

    # Determine install directory
    $installDir = $InstallDir
    if (-not $installDir) {
      $installDir = Join-Path $env:LOCALAPPDATA "skynex\bin"
    }

    if (Test-Path -LiteralPath $installDir) {
      $dirItem = Get-Item -LiteralPath $installDir -Force
      if (($dirItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or -not $dirItem.PSIsContainer) {
        Stop-WithError "Install directory is not a safe directory"
      }
    } else {
      New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    }

    $destPath = Join-Path $installDir "$BINARY_NAME.exe"
    Write-Info "Installing to $destPath..."
    & $binaryPath internal-install-binary $binaryPath $destPath
    if ($LASTEXITCODE -ne 0) { Stop-WithError "Verified binary could not perform installation" }

    Write-Success "Installed $BINARY_NAME to $destPath"

    # Check PATH
    if ($env:PATH -notlike "*$installDir*") {
      Write-Warn "$installDir is not in your PATH"
      Write-Host ""
      Write-Warn "Run this to add it permanently:"
      Write-Host "  [Environment]::SetEnvironmentVariable('PATH', `$env:PATH + ';$installDir', 'User')" -ForegroundColor DarkGray
      Write-Host ""
    }

  } finally {
    Remove-Item -Path $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# ============================================================================
# Verify installation
# ============================================================================

function Test-Installation {
  Write-Step "Verifying installation"

  # Refresh PATH for current session
  $env:PATH = [Environment]::GetEnvironmentVariable("PATH", "Machine") + ";" +
              [Environment]::GetEnvironmentVariable("PATH", "User")

  $cmd = Get-Command $BINARY_NAME -ErrorAction SilentlyContinue
  if ($cmd) {
    Write-Success "$BINARY_NAME is installed and ready"
    return
  }

  $locations = @(
    (Join-Path $env:LOCALAPPDATA "skynex\bin\$BINARY_NAME.exe")
  )
  foreach ($loc in $locations) {
    if ($loc -and (Test-Path $loc)) {
      Write-Success "Found $BINARY_NAME at $loc"
      Write-Warn "Binary location is not in your PATH. Add it to use '$BINARY_NAME' directly."
      return
    }
  }

  Write-Warn "Could not verify installation. You may need to restart your terminal."
}

# ============================================================================
# Next steps
# ============================================================================

function Show-NextSteps {
  Write-Host ""
  Write-Host "Installation complete!" -ForegroundColor Green
  Write-Host ""
  Write-Host "Next steps:" -ForegroundColor White
  Write-Host "  1. Run '$BINARY_NAME' to start the interactive installer" -ForegroundColor Cyan
  Write-Host "  2. Select your AI tool(s): Claude Code, OpenCode"         -ForegroundColor Cyan
  Write-Host "  3. Follow the prompts"                                      -ForegroundColor Cyan
  Write-Host ""
  Write-Host "For help: $BINARY_NAME --help"                                -ForegroundColor DarkGray
  Write-Host "Docs: https://github.com/$GITHUB_OWNER/$GITHUB_REPO"         -ForegroundColor DarkGray
  Write-Host ""
}

# ============================================================================
# Main
# ============================================================================

function Main {
  Show-Banner

  $arch = Get-Platform
  Test-Prerequisites

  $installMethod = Get-InstallMethod -Forced $Method

  switch ($installMethod) {
    "binary" { Install-ViaBinary -Arch $arch }
  }

  Test-Installation
  Show-NextSteps
}

Main
