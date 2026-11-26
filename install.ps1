# Install the a2p2 binary from a GitHub release (Windows PowerShell 5.1 or later).
# UNTRIED: this script has not been run on Windows.
# It downloads a2p2_windows_<arch>.exe and SHA256SUMS from one release, checks
# the SHA-256, and only then copies the file to the install directory.
# It never edits PATH or the registry, configures a harness or starts anything.
# Read it first:  irm https://raw.githubusercontent.com/wallter/a2p2/main/install.ps1 -OutFile install.ps1
# Settings: $env:A2P2_VERSION (default latest), $env:A2P2_INSTALL_DIR
# (default $env:LOCALAPPDATA\a2p2\bin), $env:A2P2_VERIFY_ATTESTATION = '1'
# to also require `gh attestation verify` from this repository's release.yml,
# for the tag of the release, on a GitHub-hosted runner (releases up to
# v0.2.3 have none). Then "latest" is asked of gh once, and that tag is
# what is downloaded and checked.
# The checksum shows the download is intact, not who built it.
# A directory it creates is made with an access list of its own: you,
# SYSTEM and Administrators, nothing inherited. It refuses an install
# directory, or a directory above it, that is a junction or symbolic link,
# that is owned by another account than you, SYSTEM, Administrators or
# TrustedInstaller, or that another account may change (such as a folder
# made by hand under C:\, which every user may write). It checks the whole
# path before the download and again before it copies the file.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Get-Https($uri, $out) {
  $r = Invoke-WebRequest -UseBasicParsing -Uri $uri -OutFile $out -PassThru
  # The final address after any redirect (5.1 and 7 keep it in different places).
  $final = if ($r.BaseResponse.ResponseUri) { $r.BaseResponse.ResponseUri } else { $r.BaseResponse.RequestMessage.RequestUri }
  if ($final.Scheme -ne 'https') { throw "refusing a download that ended on $($final.Scheme): $uri" }
}

$Me = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
# You, SYSTEM, Administrators, TrustedInstaller.
$Trusted = @($Me, 'S-1-5-18', 'S-1-5-32-544', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
# Rights that let an account change what is in a directory: write data or
# add files and folders, extended attributes and attributes, delete a child,
# delete, change permissions, take ownership, GENERIC_ALL, GENERIC_WRITE.
$WriteMask = [int64](0x2 -bor 0x4 -bor 0x10 -bor 0x40 -bor 0x100 -bor 0x10000 -bor 0x40000 -bor 0x80000 -bor 0x10000000 -bor 0x40000000)
# Rights on a directory above it that let an account rename what is below.
$AboveMask = [int64](0x40 -bor 0x10000 -bor 0x40000 -bor 0x80000 -bor 0x10000000)

# Get-Writers PATH MASK ABOVE: the accounts other than $Trusted with an
# allow entry in MASK. For the install directory (ABOVE false) entries that
# only pass to new files count, since a2p2.exe gets them; for a directory
# above it they do not. CREATOR OWNER (S-1-3-0) stands for whoever makes a
# file, here you.
function Get-Writers($path, $mask, $above) {
  $acl = Get-Acl -LiteralPath $path
  $out = @()
  $owner = $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value
  if ($Trusted -notcontains $owner) { $out += [pscustomobject]@{ Sid = $owner; What = 'owner' } }
  foreach ($r in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
    if ($r.AccessControlType -ne 'Allow') { continue }
    $sid = $r.IdentityReference.Value
    if (($Trusted -contains $sid) -or $sid -eq 'S-1-3-0') { continue }
    if ($above -and ([int]($r.PropagationFlags -band [Security.AccessControl.PropagationFlags]::InheritOnly) -ne 0)) { continue }
    if (([int64]$r.FileSystemRights -band $mask) -ne 0) { $out += [pscustomobject]@{ Sid = $sid; What = 'write' } }
  }
  $out
}

function Get-Name($sid) {
  try { (New-Object Security.Principal.SecurityIdentifier $sid).Translate([Security.Principal.NTAccount]).Value } catch { $sid }
}

# Get-Attributes PATH: the attributes of PATH itself, not of what a
# junction or symbolic link there leads to (GetFileAttributes does not
# follow one), or $null when nothing is there.
function Get-Attributes($path) {
  try { [IO.File]::GetAttributes($path) }
  catch {
    $e = $_.Exception
    if ($e.InnerException) { $e = $e.InnerException }
    if (($e -is [IO.FileNotFoundException]) -or ($e -is [IO.DirectoryNotFoundException])) { return $null }
    throw
  }
}

# Assert-NotReparse PATH: throws if PATH is a junction or symbolic link.
# Whoever made the link could later point it elsewhere or put a folder of
# their own in its place, and Get-Acl would judge the folder it leads to.
function Assert-NotReparse($path) {
  $a = Get-Attributes $path
  if ($null -eq $a) { throw "$path is gone; nothing was installed" }
  if (($a -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw "$path is a junction or symbolic link; nothing was installed. Use a path made of real folders, such as the default directory."
  }
}

# Assert-Safe PATH ABOVE: throws if PATH is a junction or symbolic link, if
# its access list cannot be read, if its owner is not in $Trusted (an owner
# may always change the access list), or if any other account may change
# it.
function Assert-Safe($path, $above) {
  Assert-NotReparse $path
  $mask = if ($above) { $AboveMask } else { $WriteMask }
  try { $ws = @(Get-Writers $path $mask $above) }
  catch { throw "could not read the access list of $path ($($_.Exception.Message)); nothing was installed" }
  foreach ($w in $ws) {
    $n = Get-Name $w.Sid
    if ($w.What -eq 'owner') {
      throw "$path is owned by $n, who may change its access list, so another user could replace a2p2.exe; nothing was installed. Use the default directory or one under your user profile."
    }
    throw "$n may change $path, so another user could replace a2p2.exe; nothing was installed. Use the default directory or one under your user profile."
  }
}

# Assert-Path DIR: checks the deepest part of DIR that exists and every
# directory above it, and returns the parts that are missing, outermost
# first.
function Assert-Path($dir) {
  $missing = @()
  $p = $dir
  while ($null -eq (Get-Attributes $p)) {
    $missing = @($p) + $missing
    $p = Split-Path -Parent $p
    if (-not $p) { throw "no part of $dir exists" }
  }
  if (((Get-Attributes $p) -band [IO.FileAttributes]::Directory) -eq 0) { throw "$p is not a directory; nothing was installed" }
  if ($missing.Count -eq 0) { Assert-Safe $dir $false }
  $q = if ($missing.Count -eq 0) { Split-Path -Parent $dir } else { $p }
  while ($q) {
    Assert-Safe $q $true
    $q = Split-Path -Parent $q
  }
  , $missing
}

# New-PrivateDir PATH: makes PATH with an access list of its own (you,
# SYSTEM, Administrators; nothing inherited) already in place, so it is
# never open in between, and checks afterwards that it is a real folder,
# still empty and yours.
function New-PrivateDir($path) {
  if ($null -ne (Get-Attributes $path)) { throw "$path appeared while this script ran; nothing was installed" }
  $acl = New-Object Security.AccessControl.DirectorySecurity
  $acl.SetAccessRuleProtection($true, $false)
  foreach ($sid in @($Me, 'S-1-5-18', 'S-1-5-32-544')) {
    $id = New-Object Security.Principal.SecurityIdentifier $sid
    $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($id, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
  }
  if ($PSVersionTable.PSEdition -eq 'Core') {
    [IO.FileSystemAclExtensions]::Create((New-Object IO.DirectoryInfo $path), $acl)
  } else {
    [IO.Directory]::CreateDirectory($path, $acl) | Out-Null
  }
  Assert-NotReparse $path
  $owner = (Get-Acl -LiteralPath $path).GetOwner([Security.Principal.SecurityIdentifier]).Value
  if (@($Me, 'S-1-5-32-544') -notcontains $owner) { throw "$path was made by another account ($(Get-Name $owner)) while this script ran; nothing was installed" }
  if (@(Get-ChildItem -LiteralPath $path -Force).Count -ne 0) { throw "$path is not empty just after it was made; nothing was installed" }
  if (@(Get-Writers $path $WriteMask $false).Count -ne 0) { throw "could not give $path a private access list; nothing was installed" }
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
  # A full path, relative to this session's location, not the process's.
  $dir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($dir)
  $attest = $env:A2P2_VERIFY_ATTESTATION
  if ($attest -and $attest -ne '0' -and $attest -ne '1') { throw "A2P2_VERIFY_ATTESTATION must be 1 or 0, got '$attest'" }
  $attest = ($attest -eq '1')
  if ($attest) {
    if ($ver -and $ver -match '^v0\.(1\.|2\.[0-3]($|-))') { throw "release $ver has no attestation (releases up to v0.2.3 have none); unset A2P2_VERIFY_ATTESTATION or pick a later release" }
    if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { throw 'A2P2_VERIFY_ATTESTATION=1 needs the GitHub CLI (gh); nothing was installed' }
    # The attestation must name the tag the files come from, so "latest"
    # is resolved once here and both files are fetched from that tag.
    if (-not $ver) {
      $ver = (& gh release view --repo wallter/a2p2 --json tagName --jq .tagName)
      if ($LASTEXITCODE -ne 0) { throw 'could not ask gh for the latest release; set A2P2_VERSION' }
      $ver = "$ver".Trim()
      if ($ver -notmatch '^v\d+\.\d+\.\d+[A-Za-z0-9._-]*$') { throw "gh named the latest release '$ver', which is not a version tag; set A2P2_VERSION" }
      if ($ver -match '^v0\.(1\.|2\.[0-3]($|-))') { throw "the latest release $ver has no attestation (releases up to v0.2.3 have none); unset A2P2_VERIFY_ATTESTATION" }
      $url = "$base/download/$ver"
    }
  }
  # The directories that exist are checked before anything is downloaded;
  # the missing ones are made later, each with a private access list, and
  # the whole path is checked again before the copy.
  $missing = Assert-Path $dir

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
    if ($attest) {
      & gh attestation verify (Join-Path $tmp $asset) --repo wallter/a2p2 --signer-workflow wallter/a2p2/.github/workflows/release.yml --source-ref "refs/tags/$ver" --deny-self-hosted-runners
      if ($LASTEXITCODE -ne 0) { throw "$asset has no valid attestation from wallter/a2p2/.github/workflows/release.yml for $ver; nothing was installed" }
      Write-Host "attestation ok: built by wallter/a2p2/.github/workflows/release.yml from $ver"
    } else {
      Write-Host 'note: the checksum shows the download is intact, not who built it.'
      Write-Host '  To check that too, run this again with $env:A2P2_VERIFY_ATTESTATION = ''1'' (needs gh).'
    }
    foreach ($m in $missing) { New-PrivateDir $m }
    if ((Assert-Path $dir).Count -ne 0) { throw "$dir is missing after it was made; nothing was installed" }
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
