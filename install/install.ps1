# Installs forgo (https://github.com/lsegal/forgo) on Windows.
#
# Usage:
#   irm https://raw.githubusercontent.com/lsegal/forgo/release-branch.go1.26/install/install.ps1 | iex
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/lsegal/forgo/release-branch.go1.26/install/install.ps1))) -Version v0.2.0
#
# Env vars:
#   FORGO_REPO         "owner/repo" to install from (default: lsegal/forgo)
#   FORGO_INSTALL_DIR  install directory (default: $HOME\.forgo)
param(
    [string]$Version = "latest"
)

$ErrorActionPreference = "Stop"

$Repo = if ($env:FORGO_REPO) { $env:FORGO_REPO } else { "lsegal/forgo" }
$InstallDir = if ($env:FORGO_INSTALL_DIR) { $env:FORGO_INSTALL_DIR } else { Join-Path $HOME ".forgo" }

$goos = "windows"
# $env:PROCESSOR_ARCHITEW6432 is set instead of PROCESSOR_ARCHITECTURE when running a
# 32-bit process (e.g. 32-bit PowerShell) under WOW64 on 64-bit Windows.
$archRaw = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($archRaw) {
    "AMD64" { $goarch = "amd64" }
    "ARM64" { $goarch = "arm64" }
    default {
        Write-Error "unsupported architecture: $archRaw"
        exit 1
    }
}

$asset = "forgo-$goos-$goarch.zip"

if ($Version -eq "latest") {
    $url = "https://github.com/$Repo/releases/latest/download/$asset"
} else {
    $tag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
    $url = "https://github.com/$Repo/releases/download/$tag/$asset"
}

Write-Host "Installing forgo ($Version) for $goos/$goarch..."
Write-Host "  from: $url"
Write-Host "  to:   $InstallDir"

$tmpZip = Join-Path ([System.IO.Path]::GetTempPath()) ("forgo-install-" + [System.Guid]::NewGuid() + ".zip")
$tmpExtract = Join-Path ([System.IO.Path]::GetTempPath()) ("forgo-install-" + [System.Guid]::NewGuid())

try {
    # GitHub occasionally serves a transient error (e.g. 503) for a release
    # that's still propagating, so retry a few times with backoff before
    # giving up.
    $maxAttempts = 5
    $downloaded = $false
    for ($attempt = 1; $attempt -le $maxAttempts; $attempt++) {
        try {
            $ProgressPreference = "SilentlyContinue" # large-file downloads are much faster without the progress UI
            Invoke-WebRequest -Uri $url -OutFile $tmpZip -UseBasicParsing
            $downloaded = $true
            break
        } catch {
            if ($attempt -eq $maxAttempts) {
                Write-Error "failed to download $url after $maxAttempts attempts`nCheck that '$Version' is a real forgo release for $goos/$goarch."
                exit 1
            }
            Write-Host "warning: download attempt $attempt/$maxAttempts failed, retrying..."
            Start-Sleep -Seconds ($attempt * 2)
        }
    }
    if (-not $downloaded) { exit 1 }

    if (-not (Test-Path $tmpZip) -or (Get-Item $tmpZip).Length -eq 0) {
        Write-Error "download of $url produced an empty file; try again"
        exit 1
    }

    New-Item -ItemType Directory -Force $tmpExtract | Out-Null
    try {
        Expand-Archive -Path $tmpZip -DestinationPath $tmpExtract -Force
    } catch {
        Write-Error "downloaded file wasn't a valid zip (got $((Get-Item $tmpZip).Length) bytes) -- the download may have been interrupted; try again"
        exit 1
    }

    $inner = Get-ChildItem $tmpExtract | Select-Object -First 1
    if (-not $inner) {
        Write-Error "downloaded archive was empty"
        exit 1
    }

    if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
    New-Item -ItemType Directory -Force (Split-Path $InstallDir -Parent) | Out-Null
    Move-Item $inner.FullName $InstallDir
} finally {
    Remove-Item -Force $tmpZip -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $tmpExtract -ErrorAction SilentlyContinue
}

$installedVersion = "(unknown)"
$versionFile = Join-Path $InstallDir "FORGO_VERSION"
if (Test-Path $versionFile) { $installedVersion = (Get-Content $versionFile -Raw).Trim() }

$binDir = Join-Path $InstallDir "bin"

# Persist GOROOT for the current user.
[Environment]::SetEnvironmentVariable("GOROOT", $InstallDir, "User")

# Persist PATH for the current user, without duplicating the entry on reinstall.
$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if (-not $userPath) { $userPath = "" }
$pathEntries = $userPath.Split(';') | Where-Object { $_ -and ($_ -ne $binDir) }
$newUserPath = (@($binDir) + $pathEntries) -join ';'
[Environment]::SetEnvironmentVariable("PATH", $newUserPath, "User")

# Reflect the change in the current session too.
$env:GOROOT = $InstallDir
$env:PATH = "$binDir;$env:PATH"

Write-Host ""
Write-Host "forgo $installedVersion installed to $InstallDir"
Write-Host ""
Write-Host "GOROOT and PATH have been set permanently for your user account."
Write-Host "Open a new terminal to pick them up there, or use this one now -- it's already updated."
Write-Host ""
Write-Host "Then verify with:"
Write-Host "  forgo version"
