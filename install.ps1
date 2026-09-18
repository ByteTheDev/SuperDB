#requires -Version 5.1
[CmdletBinding()]
param(
    [string]$Version = $env:SUPERDB_VERSION,
    [string]$Repository = $(if ($env:SUPERDB_REPOSITORY) { $env:SUPERDB_REPOSITORY } else { "ByteTheDev/SuperDB" }),
    [string]$InstallDir = $(Join-Path $env:LOCALAPPDATA "SuperDB\bin"),
    [switch]$NoPath
)

$ErrorActionPreference = "Stop"
$headers = @{ "User-Agent" = "SuperDB-Installer" }
if (-not $Version) {
    $release = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/$Repository/releases/latest"
    $Version = $release.tag_name
}
$Version = $Version.TrimStart("v")

$architecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($architecture.ToUpperInvariant()) {
    "AMD64" { $arch = "amd64" }
    "ARM64" { $arch = "arm64" }
    default { throw "Unsupported Windows architecture: $architecture" }
}

$archive = "superdb_${Version}_windows_${arch}.zip"
$baseUrl = "https://github.com/$Repository/releases/download/v$Version"
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("superdb-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force -Path $tempDir | Out-Null
try {
    $archivePath = Join-Path $tempDir $archive
    $checksumsPath = Join-Path $tempDir "SHA256SUMS"
    Invoke-WebRequest -Headers $headers -Uri "$baseUrl/$archive" -OutFile $archivePath
    Invoke-WebRequest -Headers $headers -Uri "$baseUrl/SHA256SUMS" -OutFile $checksumsPath

    $line = Get-Content $checksumsPath | Where-Object { $_ -match [regex]::Escape($archive) } | Select-Object -First 1
    if (-not $line) { throw "Checksum missing for $archive" }
    $expected = ($line -split "\s+")[0].ToLowerInvariant()
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
    if ($expected -ne $actual) { throw "Checksum verification failed" }

    $extractDir = Join-Path $tempDir "extract"
    Expand-Archive -LiteralPath $archivePath -DestinationPath $extractDir -Force
    $packageDir = Join-Path $extractDir "superdb_${Version}_windows_${arch}"
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -LiteralPath (Join-Path $packageDir "superdb.exe") -Destination (Join-Path $InstallDir "superdb.exe") -Force
    Copy-Item -LiteralPath (Join-Path $packageDir "superdb-cli.exe") -Destination (Join-Path $InstallDir "superdb-cli.exe") -Force

    if (-not $NoPath) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @($userPath -split ";" | Where-Object { $_ })
        if ($entries -notcontains $InstallDir) {
            $newPath = (($entries + $InstallDir) -join ";")
            [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
            Write-Host "Added $InstallDir to your user PATH. Open a new terminal to use it."
        }
    }
    Write-Host "Installed SuperDB $Version to $InstallDir"
} finally {
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
