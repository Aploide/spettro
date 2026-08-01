<#
.SYNOPSIS
    Installs spettro on Windows.

.DESCRIPTION
    Downloads the release archive matching this machine's architecture, verifies
    its SHA-256 against the release's checksums.txt, and installs the executable
    into a per-user directory. Nothing is written outside the user profile and
    no elevation is required, so the in-place self-update (/update in the TUI)
    works later without an administrator prompt either.

.PARAMETER Version
    Release tag to install, e.g. v1.2.3. Defaults to the latest release.

.PARAMETER InstallDir
    Destination directory. Defaults to %LOCALAPPDATA%\Programs\spettro.

.PARAMETER NoPathUpdate
    Leave the user PATH untouched. By default InstallDir is added to it when
    missing.

.PARAMETER BaseUrl
    Base URL the release assets are fetched from, for mirrors and offline
    installs. Defaults to the GitHub release download URL for Version. The
    archive and checksums.txt must both live directly under it.

.EXAMPLE
    irm https://raw.githubusercontent.com/aploide/spettro/main/install.ps1 | iex

.EXAMPLE
    .\install.ps1 -Version v1.2.3 -InstallDir C:\tools\spettro -NoPathUpdate
#>
[CmdletBinding()]
param(
    [string] $Version,
    [string] $InstallDir,
    [string] $BaseUrl,
    # A switch rather than a [bool]: `-Flag:$false` is not parsed as a boolean
    # when the script is run through `powershell.exe -File`, which is exactly
    # how an installer tends to be invoked.
    [switch] $NoPathUpdate
)

# Stop on the first error, and make non-terminating errors terminating so a
# failed download can never fall through to installing a partial file.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo = 'aploide/spettro'

function Write-Step { param([string] $Message) Write-Host "==> $Message" }

# ── preflight ────────────────────────────────────────────────────────────────
# PowerShell 5.1 defaults to TLS 1.0/1.1, which GitHub refuses. Requesting
# TLS 1.2 explicitly is what makes this script work on a stock Windows 10/11.
if ([Net.ServicePointManager]::SecurityProtocol -notmatch 'Tls12') {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}

# ── detect architecture ──────────────────────────────────────────────────────
# PROCESSOR_ARCHITECTURE reports the *process* architecture, which is x86 for a
# 32-bit PowerShell on a 64-bit OS; PROCESSOR_ARCHITEW6432 is set only in that
# case and names the real machine.
$rawArch = $env:PROCESSOR_ARCHITEW6432
if ([string]::IsNullOrWhiteSpace($rawArch)) { $rawArch = $env:PROCESSOR_ARCHITECTURE }

switch ($rawArch.ToUpperInvariant()) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default {
        throw "Unsupported architecture: $rawArch. spettro ships windows/amd64 and windows/arm64 builds."
    }
}

# ── resolve version ──────────────────────────────────────────────────────────
if ([string]::IsNullOrWhiteSpace($Version)) {
    Write-Step 'Fetching latest release...'
    try {
        $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
            -Headers @{ 'User-Agent' = 'spettro-installer'; 'Accept' = 'application/vnd.github+json' }
        $Version = $release.tag_name
    } catch {
        throw "Could not determine the latest release: $($_.Exception.Message)`nPass one explicitly: .\install.ps1 -Version v1.0.0"
    }
}
if ([string]::IsNullOrWhiteSpace($Version)) {
    throw "Could not determine the latest release version. Pass one explicitly: .\install.ps1 -Version v1.0.0"
}

if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\spettro'
}

Write-Step "Installing spettro $Version (windows/$arch)"

# ── download ─────────────────────────────────────────────────────────────────
if ([string]::IsNullOrWhiteSpace($BaseUrl)) {
    $BaseUrl = "https://github.com/$Repo/releases/download/$Version"
}
$BaseUrl = $BaseUrl.TrimEnd('/')

$archiveName = "spettro_${Version}_windows_${arch}.zip"
$archiveUrl  = "$BaseUrl/$archiveName"
$sumsUrl     = "$BaseUrl/checksums.txt"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("spettro-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    $archivePath = Join-Path $tmp $archiveName
    $sumsPath    = Join-Path $tmp 'checksums.txt'

    Write-Step "Downloading $archiveUrl"
    try {
        # -UseBasicParsing keeps this working when Internet Explorer's engine is
        # absent or unconfigured, which is the norm on Server and on fresh
        # accounts; without it PowerShell 5.1 throws before the download starts.
        Invoke-WebRequest -Uri $archiveUrl -OutFile $archivePath -UseBasicParsing `
            -Headers @{ 'User-Agent' = 'spettro-installer' }
    } catch {
        throw "Download failed: $($_.Exception.Message)`nCheck that $Version exists at https://github.com/$Repo/releases"
    }

    # ── verify checksum ──────────────────────────────────────────────────────
    Write-Step 'Downloading checksums...'
    try {
        Invoke-WebRequest -Uri $sumsUrl -OutFile $sumsPath -UseBasicParsing `
            -Headers @{ 'User-Agent' = 'spettro-installer' }
    } catch {
        throw "Could not download checksums.txt for $Version. Refusing to install without integrity verification."
    }

    $expected = $null
    foreach ($line in Get-Content -LiteralPath $sumsPath) {
        # @() so a line that splits to a single token is still an array:
        # under Set-StrictMode, .Count on a bare string is an error.
        $fields = @($line -split '\s+' | Where-Object { $_ -ne '' })
        if ($fields.Count -ge 2 -and $fields[-1] -eq $archiveName) {
            $expected = $fields[0]
            break
        }
    }
    if ([string]::IsNullOrWhiteSpace($expected)) {
        throw "No checksum for $archiveName in checksums.txt. Refusing to install without integrity verification."
    }

    $actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash
    if ($actual -ne $expected.ToUpperInvariant() -and $actual.ToLowerInvariant() -ne $expected.ToLowerInvariant()) {
        throw "Checksum verification failed.`n  expected: $expected`n  actual:   $actual`nThe download may be corrupted or tampered with. Refusing to install."
    }
    Write-Step 'Checksum verified.'

    # ── extract ──────────────────────────────────────────────────────────────
    $extractDir = Join-Path $tmp 'extracted'
    New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
    Expand-Archive -LiteralPath $archivePath -DestinationPath $extractDir -Force

    $binary = Get-ChildItem -LiteralPath $extractDir -Filter 'spettro.exe' -Recurse -File |
        Select-Object -First 1
    if ($null -eq $binary) {
        throw "The release archive does not contain spettro.exe."
    }

    # ── install ──────────────────────────────────────────────────────────────
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $dest = Join-Path $InstallDir 'spettro.exe'

    # Windows refuses to overwrite the image of a running process but does allow
    # renaming it, which is also how the in-place self-update works. Move any
    # current build aside so re-running this over a live spettro still succeeds.
    if (Test-Path -LiteralPath $dest) {
        $backup = "$dest.old"
        Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
        try {
            Rename-Item -LiteralPath $dest -NewName (Split-Path -Leaf $backup) -Force
        } catch {
            throw "Could not replace $dest : $($_.Exception.Message)`nClose any running spettro and try again."
        }
    }

    Copy-Item -LiteralPath $binary.FullName -Destination $dest -Force
    # Clear the mark-of-the-web so SmartScreen does not block the first launch.
    Unblock-File -LiteralPath $dest -ErrorAction SilentlyContinue
    # Best effort: fails while the previous build is still running, and the
    # next install or update clears it.
    Remove-Item -LiteralPath "$dest.old" -Force -ErrorAction SilentlyContinue

    # ── PATH ─────────────────────────────────────────────────────────────────
    # The user PATH is read and written whole, so compare against the stored
    # value rather than $env:PATH, which is the merged machine+user copy this
    # process inherited.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($null -eq $userPath) { $userPath = '' }
    $onPath = @($userPath -split ';' | Where-Object { $_.TrimEnd('\') -ieq $InstallDir.TrimEnd('\') }).Count -gt 0

    if (-not $onPath -and -not $NoPathUpdate) {
        $updated = if ([string]::IsNullOrWhiteSpace($userPath)) { $InstallDir } else { "$userPath;$InstallDir" }
        [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
        # Also update this session so the verification below can run.
        $env:Path = "$env:Path;$InstallDir"
        $onPath = $true
        Write-Step "Added $InstallDir to your user PATH."
        Write-Host "   Open a new terminal for it to take effect in existing sessions."
    }

    # ── report ───────────────────────────────────────────────────────────────
    Write-Host ''
    Write-Host "spettro $Version installed to $dest"

    if ($onPath) {
        Write-Host "Run 'spettro' to get started."
    } else {
        Write-Host ''
        Write-Host "$InstallDir is not on your PATH. Add it with:"
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';$InstallDir', 'User')"
    }
} finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
