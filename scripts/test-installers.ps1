$ErrorActionPreference = 'Stop'
# This suite invokes the installer expecting non-zero exits, so keep native exit
# codes as data ($LASTEXITCODE) instead of terminating errors. PowerShell 7.4+
# would otherwise abort those deliberate-failure cases before they are asserted.
$PSNativeCommandUseErrorActionPreference = $false
$root = Split-Path -Parent $PSScriptRoot
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('skynex-installer-test-' + [IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  $release = Join-Path $tmp 'v1.2.3'; New-Item -ItemType Directory -Path $release | Out-Null
  $key = Join-Path $tmp 'release-key'
  & ssh-keygen -q -t ed25519 -N '' -f $key
  $pub = (& ssh-keygen -y -f $key).Trim()
  $pubKey = ($pub -split '\s+')[1]
  $trustedPubKey = ((Get-Content -Raw (Join-Path $root 'release/trust/skynex-release-signing-key.pub')).Trim() -split '\s+')[1]
  foreach ($installerPath in @((Join-Path $root 'scripts/install.sh'), (Join-Path $root 'scripts/install.ps1'))) {
    if (-not (Get-Content -Raw $installerPath).Contains($trustedPubKey)) {
      throw "installer trust pin is not current: $installerPath"
    }
  }
  $allowed = Join-Path $tmp 'allowed_signers'
  "skynex-release $pub fixture" | Set-Content -NoNewline -Encoding ascii $allowed

  # PowerShell 7 dropped `Add-Type -OutputAssembly`/`-OutputType`, so the fake
  # binary is compiled through Windows PowerShell, which still supports it.
  $fakeExe = Join-Path $tmp 'skynex.exe'
  $fakeBuilder = Join-Path $tmp 'build-fake-skynex.ps1'
  @'
param([string]$OutputPath)
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @"
using System; using System.IO;
class FakeSkynex { public static void Main(string[] a) { if (a.Length == 3 && a[0] == "internal-install-binary") { File.Copy(a[1], a[2], true); return; } } }
"@ -OutputAssembly $OutputPath -OutputType ConsoleApplication
'@ | Set-Content -Encoding utf8 $fakeBuilder
  & powershell -NoProfile -File $fakeBuilder -OutputPath $fakeExe
  if ($LASTEXITCODE -ne 0 -or -not (Test-Path $fakeExe)) { throw 'could not compile the fake skynex.exe fixture' }
  # The installer rejects archives under 1000 bytes, so pad the fixture with an
  # incompressible PE overlay (ignored by the loader) like the Unix suite does.
  $pad = New-Object byte[] 4096
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($pad)
  $padStream = [IO.File]::Open($fakeExe, [IO.FileMode]::Append, [IO.FileAccess]::Write)
  try { $padStream.Write($pad, 0, $pad.Length) } finally { $padStream.Dispose() }
  $archive = Join-Path $release 'skynex_1.2.3_windows_amd64.zip'
  Add-Type -AssemblyName System.IO.Compression.FileSystem
  $zip = [IO.Compression.ZipFile]::Open($archive, [IO.Compression.ZipArchiveMode]::Create)
  try { [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, (Join-Path $tmp 'skynex.exe'), 'skynex.exe') | Out-Null } finally { $zip.Dispose() }
  $hash = (Get-FileHash $archive -Algorithm SHA256).Hash.ToLower()
  "$hash  skynex_1.2.3_windows_amd64.zip" | Set-Content -Encoding ascii (Join-Path $release 'checksums.txt')
  & ssh-keygen -q -Y sign -n file -f $key (Join-Path $release 'checksums.txt') | Out-Null

  $server = Join-Path $tmp 'server.ps1'
  @'
param($prefix, $release)
$l = [Net.HttpListener]::new(); $l.Prefixes.Add($prefix); $l.Start()
while ($true) { $c = $l.GetContext(); if ($c.Request.Url.AbsolutePath -eq '/api/repos/joeldevz/skynex/releases/latest') { $b = [Text.Encoding]::UTF8.GetBytes('{"tag_name":"v1.2.3"}'); $c.Response.StatusCode=200; $c.Response.ContentType='application/json' } else { $p = Join-Path $release ([IO.Path]::GetFileName($c.Request.Url.AbsolutePath)); $b=[IO.File]::ReadAllBytes($p) }; $c.Response.OutputStream.Write($b,0,$b.Length); $c.Response.Close() }
'@ | Set-Content -Encoding utf8 $server
  $port = Get-Random -Minimum 18000 -Maximum 28000; $prefix = "http://127.0.0.1:$port/"
  $serverProc = Start-Process pwsh -ArgumentList '-NoProfile','-File',$server,$prefix,$release -PassThru
  Start-Sleep -Milliseconds 300
  # install.ps1 builds both endpoints by interpolating $GITHUB_OWNER/$GITHUB_REPO, so the
  # fixture must rewrite that literal source text (including the release API endpoint),
  # or the run escapes to the real GitHub API and release CDN instead of this listener.
  function New-FixtureInstaller {
    param([string]$Path, [switch]$FixtureSigner)
    $text = Get-Content -Raw (Join-Path $root 'scripts/install.ps1')
    $text = $text.Replace('https://api.github.com/repos/$GITHUB_OWNER/$GITHUB_REPO/releases/latest', "${prefix}api/repos/joeldevz/skynex/releases/latest")
    $text = $text.Replace('https://github.com/$GITHUB_OWNER/$GITHUB_REPO/releases/download', "${prefix}release")
    if ($FixtureSigner) { $text = $text.Replace($trustedPubKey, $pubKey) }
    if ($text -match 'https://api\.github\.com' -or $text -match 'https://github\.com/\$GITHUB_OWNER/\$GITHUB_REPO/releases/download') { throw 'installer fixture failed to redirect a GitHub endpoint' }
    $text | Set-Content -Encoding utf8 $Path
  }

  $installer = Join-Path $tmp 'install.ps1'
  New-FixtureInstaller -Path $installer -FixtureSigner
  $dest = Join-Path $tmp 'destination'; & powershell -NoProfile -File $installer -Method binary -InstallDir $dest
  if (-not (Test-Path (Join-Path $dest 'skynex.exe'))) { throw 'Windows installer did not install the fake binary' }

  # The production script must not accept the removed test-only signer overrides.
  $env:SKYNEX_TEST_MODE = '1'; $env:SKYNEX_TEST_ALLOWED_SIGNERS = $allowed
  $bypassDest = Join-Path $tmp 'production-env-bypass'
  $controlledProduction = Join-Path $tmp 'controlled-production.ps1'
  New-FixtureInstaller -Path $controlledProduction
  $bypassOutput = & powershell -NoProfile -File $controlledProduction -Method binary -InstallDir $bypassDest 2>&1
  if ($LASTEXITCODE -eq 0 -or ($bypassOutput -join "`n") -notmatch 'Invalid checksums\.txt signature' -or (Test-Path $bypassDest)) { throw 'production Windows installer accepted test signer overrides or changed the destination' }

  # Overwrite the armored signature like the Unix suite does: appending to it
  # only adds trailing text after the END marker, which ssh-keygen ignores.
  Set-Content -Encoding ascii -Path (Join-Path $release 'checksums.txt.sig') -Value 'tampered'
  & powershell -NoProfile -File $installer -Method binary -InstallDir (Join-Path $tmp 'bad')
  if ($LASTEXITCODE -eq 0) { throw 'tampered Windows signature was accepted' }
  Write-Output 'Windows installer runtime acceptance passed'
} finally {
  if ($serverProc) { Stop-Process -Id $serverProc.Id -Force -ErrorAction SilentlyContinue }
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  Remove-Item Env:SKYNEX_TEST_MODE,Env:SKYNEX_TEST_ALLOWED_SIGNERS -ErrorAction SilentlyContinue
}

# The last native command is deliberately expected to fail for the tampered
# signature case. GitHub Actions' pwsh wrapper propagates that stale native exit
# code even though every assertion above passed, so explicitly mark the suite's
# successful completion after cleanup.
exit 0
