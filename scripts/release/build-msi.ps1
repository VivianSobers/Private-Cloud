<#
.SYNOPSIS
    Build the pcsync Windows installer (.msi) from an already-built pcsync.exe.

.DESCRIPTION
    WHAT IS AUTOMATED AND WHAT IS NOT — the honest version.

    Building the .msi is fully automated and needs no secrets. WiX v5 installs as
    a dotnet global tool, so this runs on a stock GitHub windows-latest runner
    with nothing preinstalled beyond the .NET SDK that is already there.

    AUTHENTICODE SIGNING is not automated, and cannot be, because it needs a paid
    code-signing certificate from a commercial CA (an OV certificate is a few
    hundred dollars a year; an EV one, which is what actually clears SmartScreen
    reputation quickly, needs a hardware token). There is no keyless equivalent
    for Authenticode the way Sigstore is for artifacts, which is exactly why the
    binaries are signed with cosign instead and the .msi is not.

      * PC_WINDOWS_PFX_BASE64 unset -> an UNSIGNED .msi. Windows SmartScreen will
        warn on first run. The cosign signature over SHA256SUMS still covers this
        file, so it can be verified — just not by Windows itself.
      * PC_WINDOWS_PFX_BASE64 set   -> signtool signs and timestamps it.

.PARAMETER Version
    The release tag, e.g. v1.3.0. A leading v is stripped for the MSI version.

.PARAMETER BinaryPath
    Path to the built pcsync-windows-amd64.exe.

.PARAMETER OutputPath
    Where to write the .msi.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$BinaryPath,
    [Parameter(Mandatory = $true)][string]$OutputPath
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path $BinaryPath)) {
    throw "build-msi: $BinaryPath not found"
}

# MSI ProductVersion is strictly numeric (a.b.c), so a tag like v1.3.0-rc.1 has
# to be reduced. The pre-release part is dropped rather than encoded: Windows
# compares these numbers to decide what upgrades what, and inventing a fourth
# field would make 1.3.0-rc.1 look newer than 1.3.0.
$bare = $Version -replace '^v', ''
$msiVersion = ($bare -split '-')[0]
if ($msiVersion -notmatch '^\d+\.\d+\.\d+$') {
    throw "build-msi: cannot turn version '$Version' into an MSI version"
}
Write-Host "build-msi: pcsync $Version (MSI ProductVersion $msiVersion)"

# WiX v5 as a dotnet global tool. Idempotent: a second run on a warm machine
# reports "already installed" and carries on.
$wix = Get-Command wix -ErrorAction SilentlyContinue
if (-not $wix) {
    Write-Host "build-msi: installing the WiX toolset"
    dotnet tool install --global wix --version 5.0.2
    $env:PATH = "$env:PATH;$env:USERPROFILE\.dotnet\tools"
    $wix = Get-Command wix -ErrorAction SilentlyContinue
    if (-not $wix) { throw "build-msi: wix is still not on PATH after install" }
}

$wxs = Join-Path $PSScriptRoot '..\..\client\deploy\windows\pcsync.wxs'
$wxs = (Resolve-Path $wxs).Path
$binFull = (Resolve-Path $BinaryPath).Path

$outDir = Split-Path -Parent $OutputPath
if ($outDir -and -not (Test-Path $outDir)) { New-Item -ItemType Directory -Path $outDir | Out-Null }

& wix build $wxs -arch x64 `
    -d "Version=$msiVersion" `
    -d "BinaryPath=$binFull" `
    -o $OutputPath
if ($LASTEXITCODE -ne 0) { throw "build-msi: wix build failed with exit code $LASTEXITCODE" }
Write-Host "build-msi: built $OutputPath"

if ([string]::IsNullOrWhiteSpace($env:PC_WINDOWS_PFX_BASE64)) {
    Write-Host @'
build-msi: PC_WINDOWS_PFX_BASE64 is not set - the installer is UNSIGNED.
  SmartScreen will warn on first run. Authenticode needs a paid certificate from
  a commercial CA; there is no keyless equivalent, which is why the release signs
  its artifacts with cosign instead. docs/install.md tells users how to verify.
'@
    exit 0
}

# The PFX lands in a temp file, is used, and is deleted in the finally block —
# never written next to the build output where an artifact upload could catch it.
$pfx = Join-Path ([System.IO.Path]::GetTempPath()) "pcsync-signing-$([guid]::NewGuid()).pfx"
try {
    [IO.File]::WriteAllBytes($pfx, [Convert]::FromBase64String($env:PC_WINDOWS_PFX_BASE64))
    $signtool = Get-ChildItem -Path "${env:ProgramFiles(x86)}\Windows Kits\10\bin" -Filter signtool.exe -Recurse -ErrorAction SilentlyContinue |
        Where-Object { $_.FullName -match 'x64' } | Select-Object -First 1
    if (-not $signtool) { throw "build-msi: PC_WINDOWS_PFX_BASE64 is set but signtool.exe was not found" }

    $args = @('sign', '/fd', 'SHA256', '/f', $pfx,
              '/tr', 'http://timestamp.digicert.com', '/td', 'SHA256')
    if ($env:PC_WINDOWS_PFX_PASSWORD) { $args += @('/p', $env:PC_WINDOWS_PFX_PASSWORD) }
    $args += $OutputPath

    & $signtool.FullName @args
    if ($LASTEXITCODE -ne 0) { throw "build-msi: signtool failed with exit code $LASTEXITCODE" }
    # Timestamped on purpose: without /tr the signature stops validating the day
    # the certificate expires, and a released installer outlives its certificate.
    Write-Host "build-msi: signed and timestamped $OutputPath"
}
finally {
    if (Test-Path $pfx) { Remove-Item $pfx -Force }
}
