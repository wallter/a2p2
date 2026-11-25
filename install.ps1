# Install the a2p2 binary from a GitHub release (Windows PowerShell 5.1 or later).
# UNTRIED: this script has not been run on Windows.
# It downloads a2p2_windows_<arch>.exe and SHA256SUMS from one release, checks
# the SHA-256, and only then copies the file to the install directory.
# It never edits PATH or the registry, configures a harness or starts anything.
# Read it first:  irm https://raw.githubusercontent.com/wallter/a2p2/main/install.ps1 -OutFile install.ps1
# Settings: $env:A2P2_VERSION (default latest), $env:A2P2_INSTALL_DIR
# (default $env:LOCALAPPDATA\a2p2\bin).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Get-Https($uri, $out) {
  $r = Invoke-WebRequest -UseBasicParsing -Uri $uri -OutFile $out -PassThru
  # The final address after any redirect (5.1 and 7 keep it in different places).
  $final = if ($r.BaseResponse.ResponseUri) { $r.BaseResponse.ResponseUri } else { $r.BaseResponse.RequestMessage.RequestUri }
  if ($final.Scheme -ne 'https') { throw "refusing a download that ended on $($final.Scheme): $uri" }
}

function Main {
  # An x64 PowerShell under emulation on ARM64 Windows reports AMD64 here and
  # gets the amd64 build, which runs there. PROCESSOR_ARCHITEW6432 is set for
  # a 32-bit process on 64-bit Windows and names the real architecture.
  $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
  switch ($arch) {
    'AMD64' { $a = 'amd64' }
    'ARM64' { $a = 'arm64' }
    default { throw "unsupported architecture '$arch'. Download a binary and SHA256SUMS from https://github.com/wallter/a2p2/releases and check it by hand." }
  }
  $asset = "a2p2_windows_$a.exe"
  $ver = $env:A2P2_VERSION
  if ($ver -and $ver -notmatch '^v\d+\.\d+\.\d+[A-Za-z0-9._-]*$') { throw "A2P2_VERSION must look like v0.2.1, got '$ver'" }
  $base = 'https://github.com/wallter/a2p2/releases'
  $url = if ($ver) { "$base/download/$ver" } else { "$base/latest/download" }
  $dir = if ($env:A2P2_INSTALL_DIR) { $env:A2P2_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'a2p2\bin' }

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
  New-Item -ItemType Directory -Path $tmp | Out-Null
  $stage = $null
  try {
    Write-Host "downloading $asset"
    Get-Https "$url/$asset" (Join-Path $tmp $asset)
    Get-Https "$url/SHA256SUMS" (Join-Path $tmp 'SHA256SUMS')
    $pat = '^([0-9a-fA-F]{64})\s+\*?' + [regex]::Escape($asset) + '\s*$'
    $lines = @(Get-Content -LiteralPath (Join-Path $tmp 'SHA256SUMS') | Where-Object { $_ -match $pat })
    if ($lines.Count -ne 1) { throw "SHA256SUMS needs exactly one line for $asset; nothing was installed" }
    $null = $lines[0] -match $pat
    $want = $Matches[1].ToLower()
    $got = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $asset)).Hash.ToLower()
    if ($got -ne $want) { throw "checksum mismatch for $asset (expected $want, got $got); nothing was installed" }
    Write-Host "checksum ok: $got"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $target = Join-Path $dir 'a2p2.exe'
    $stage = Join-Path $dir (".a2p2.$([guid]::NewGuid()).new")
    Copy-Item -LiteralPath (Join-Path $tmp $asset) -Destination $stage
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $stage).Hash.ToLower() -ne $want) { throw "the copy in $dir does not match the checksum; nothing was installed" }
    try { Move-Item -LiteralPath $stage -Destination $target -Force }
    catch { throw "could not replace $target ($($_.Exception.Message)). If a2p2 is running, stop a2p2 first (a2p2 stop), then run this again." }
    Write-Host "installed $target"
    & $target version
    if ($LASTEXITCODE -ne 0) { throw "installed $target but 'version' failed (exit $LASTEXITCODE)" }
  } finally {
    if ($stage -and (Test-Path -LiteralPath $stage)) { Remove-Item -LiteralPath $stage -Force -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
  if (($env:Path -split ';') -notcontains $dir) {
    Write-Host "$dir is not on your PATH. Add this directory to your PATH."
  }
  Write-Host 'Next: connect a harness with  a2p2 install claude  (or codex, opencode, hermes, pi),'
  Write-Host 'then run  a2p2 doctor'
}
Main
