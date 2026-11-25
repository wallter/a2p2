# Install the a2p2 binary from a GitHub release (Windows PowerShell 5.1 or later).
# It downloads a2p2_windows_<arch>.exe and SHA256SUMS from one release, checks
# the SHA-256, and only then copies the file to the install directory.
# It never edits PATH or the registry, configures a harness or starts anything.
# Read it first:  irm https://raw.githubusercontent.com/wallter/a2p2/main/install.ps1 -OutFile install.ps1
# Settings: $env:A2P2_VERSION (default latest), $env:A2P2_INSTALL_DIR
# (default $env:LOCALAPPDATA\a2p2\bin).
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Main {
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

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Write-Host "downloading $asset"
    Invoke-WebRequest -UseBasicParsing -Uri "$url/$asset" -OutFile (Join-Path $tmp $asset)
    Invoke-WebRequest -UseBasicParsing -Uri "$url/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
    $lines = @(Get-Content (Join-Path $tmp 'SHA256SUMS') | Where-Object { ($_ -split '\s+')[1] -in @($asset, "*$asset") })
    if ($lines.Count -ne 1) { throw "SHA256SUMS needs exactly one line for $asset; nothing was installed" }
    $want = ($lines[0] -split '\s+')[0].ToLower()
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $asset)).Hash.ToLower()
    if ($got -ne $want) { throw "checksum mismatch for $asset (expected $want, got $got); nothing was installed" }
    Write-Host "checksum ok: $got"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $target = Join-Path $dir 'a2p2.exe'
    $new = "$target.new"
    Copy-Item (Join-Path $tmp $asset) $new -Force
    Move-Item $new $target -Force
    Write-Host "installed $target"
    & $target version
  } finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
  if (($env:Path -split ';') -notcontains $dir) {
    Write-Host "$dir is not on your PATH. To add it for this user, run:"
    Write-Host "  [Environment]::SetEnvironmentVariable('Path', `"`$([Environment]::GetEnvironmentVariable('Path','User'));$dir`", 'User')"
  }
  Write-Host 'Next: connect a harness with  a2p2 install claude  (or codex, opencode, hermes, pi),'
  Write-Host 'then run  a2p2 doctor'
}
Main
