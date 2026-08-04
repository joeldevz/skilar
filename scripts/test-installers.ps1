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
while ($true) { $c = $l.GetContext(); if ($c.Request.Url.AbsolutePath -eq '/api/repos/joeldevz/skynex/releases/latest') { $b = [Text.Encoding]::UTF8.GetBytes('{"tag_name":"v1.2.3"}'); $c.Response.StatusCode=200 } else { $p = Join-Path $release ([IO.Path]::GetFileName($c.Request.Url.AbsolutePath)); $b=[IO.File]::ReadAllBytes($p) }; $c.Response.OutputStream.Write($b,0,$b.Length); $c.Response.Close() }
'@ | Set-Content -Encoding utf8 $server
  $port = Get-Random -Minimum 18000 -Maximum 28000; $prefix = "http://127.0.0.1:$port/"
  $serverProc = Start-Process pwsh -ArgumentList '-NoProfile','-File',$server,$prefix,$release -PassThru
  Start-Sleep -Milliseconds 300
  $installer = Join-Path $tmp 'install.ps1'
  (Get-Content -Raw (Join-Path $root 'scripts/install.ps1')).Replace('AAAAC3NzaC1lZDI1NTE5AAAAINUht44Rk/nWIXqcKizh8SWdnECJZOQ5yuPjaxaWxAAF', $pubKey).Replace('https://github.com/joeldevz/skynex/releases/download', "${prefix}release") | Set-Content -Encoding utf8 $installer
  $dest = Join-Path $tmp 'destination'; & powershell -NoProfile -File $installer -Method binary -InstallDir $dest
  if (-not (Test-Path (Join-Path $dest 'skynex.exe'))) { throw 'Windows installer did not install the fake binary' }

  # The production script must not accept the removed test-only signer overrides.
  $env:SKYNEX_TEST_MODE = '1'; $env:SKYNEX_TEST_ALLOWED_SIGNERS = $allowed
  $bypassDest = Join-Path $tmp 'production-env-bypass'
  $controlledProduction = Join-Path $tmp 'controlled-production.ps1'
  (Get-Content -Raw (Join-Path $root 'scripts/install.ps1')).Replace('https://github.com/joeldevz/skynex/releases/download', "${prefix}release") | Set-Content -Encoding utf8 $controlledProduction
  $bypassOutput = & powershell -NoProfile -File $controlledProduction -Method binary -InstallDir $bypassDest 2>&1
  if ($LASTEXITCODE -eq 0 -or ($bypassOutput -join "`n") -notmatch 'Invalid checksums\.txt signature' -or (Test-Path $bypassDest)) { throw 'production Windows installer accepted test signer overrides or changed the destination' }

  Add-Content -Path (Join-Path $release 'checksums.txt.sig') -Value 'tampered'
  & powershell -NoProfile -File $installer -Method binary -InstallDir (Join-Path $tmp 'bad')
  if ($LASTEXITCODE -eq 0) { throw 'tampered Windows signature was accepted' }
  Write-Output 'Windows installer runtime acceptance passed'
} finally {
  if ($serverProc) { Stop-Process -Id $serverProc.Id -Force -ErrorAction SilentlyContinue }
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  Remove-Item Env:SKYNEX_TEST_MODE,Env:SKYNEX_TEST_ALLOWED_SIGNERS -ErrorAction SilentlyContinue
}
