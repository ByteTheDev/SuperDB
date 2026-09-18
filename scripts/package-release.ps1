param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$Version,
    [Parameter(Mandatory = $false, Position = 1)]
    [ValidateSet("windows", "linux", "darwin")]
    [string]$GOOS = "windows",
    [Parameter(Mandatory = $false, Position = 2)]
    [ValidateSet("amd64", "arm64")]
    [string]$GOARCH = "amd64",
    [Parameter(Mandatory = $false, Position = 3)]
    [string]$OutputDir = "dist"
)

$ErrorActionPreference = "Stop"
$Version = $Version.TrimStart("v")
$RootDir = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$PackageName = "superdb_${Version}_${GOOS}_${GOARCH}"
$PackageDir = Join-Path (Join-Path $RootDir $OutputDir) $PackageName
$ArchiveDir = Join-Path $RootDir $OutputDir
$Extension = if ($GOOS -eq "windows") { ".exe" } else { "" }

if (Test-Path $PackageDir) {
    Remove-Item -LiteralPath $PackageDir -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $PackageDir | Out-Null
New-Item -ItemType Directory -Force -Path $ArchiveDir | Out-Null

$oldCGOEnabled = $env:CGO_ENABLED
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$ldflags = "-s -w -X main.version=$Version"
$env:CGO_ENABLED = "0"
$env:GOOS = $GOOS
$env:GOARCH = $GOARCH
Push-Location $RootDir
try {
	& go build -trimpath -ldflags $ldflags -o (Join-Path $PackageDir "superdb$Extension") ./cmd/superdb
	if ($LASTEXITCODE -ne 0) { throw "go build failed for superdb" }
	& go build -trimpath -ldflags $ldflags -o (Join-Path $PackageDir "superdb-cli$Extension") ./cmd/superdb-cli
	if ($LASTEXITCODE -ne 0) { throw "go build failed for superdb-cli" }
} finally {
	Pop-Location
	$env:CGO_ENABLED = $oldCGOEnabled
	$env:GOOS = $oldGOOS
	$env:GOARCH = $oldGOARCH
}
Copy-Item -LiteralPath (Join-Path $RootDir "README.md") -Destination $PackageDir

if ($GOOS -eq "windows") {
    Compress-Archive -Path $PackageDir -DestinationPath (Join-Path $ArchiveDir "$PackageName.zip") -Force
} else {
    & tar -C (Split-Path -Parent $PackageDir) -czf (Join-Path $ArchiveDir "$PackageName.tar.gz") $PackageName
    if ($LASTEXITCODE -ne 0) { throw "tar failed for $PackageName" }
}
Remove-Item -LiteralPath $PackageDir -Recurse -Force
