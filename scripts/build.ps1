param(
  [ValidateSet("amd64", "arm64")]
  [string]$Arch = "amd64",
  [string]$FnpackVersion = "1.2.3"
)

$ErrorActionPreference = "Stop"
$RootDir = Split-Path -Parent $PSScriptRoot
Set-Location $RootDir

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  $env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path", "User")
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw "Go compiler not found. Please install Go from https://go.dev/dl/ or run scripts/build.sh on Linux."
}

if (-not $env:GOPROXY) {
  $env:GOPROXY = "https://goproxy.cn,direct"
}

$FnPlatform = if ($Arch -eq "arm64") { "arm" } else { "x86" }
$manifestPath = Join-Path $RootDir "fpk\manifest"
$manifestRaw = [System.IO.File]::ReadAllText($manifestPath)
$AppVersion = "0.0.0"
if ($manifestRaw -match '(?m)^version\s*=\s*([^\r\n]+)') {
  $AppVersion = $matches[1].Trim()
}

Write-Host "[1/5] Cross-compiling Go binary (linux/$Arch v$AppVersion -> platform=$FnPlatform)..."
New-Item -ItemType Directory -Force -Path "fpk\app\bin", "dist", "tools" | Out-Null
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = $Arch
go mod tidy
go build -ldflags="-s -w -X fn-nspawn/internal/nspawn.AppVersion=$AppVersion" -o "fpk\app\bin\fn-nspawn" .

Write-Host "[2/5] Updating manifest platform=$FnPlatform..."
$manifestUpdated = [System.Text.RegularExpressions.Regex]::Replace($manifestRaw, '(?m)^platform\s*=.*$', "platform=$FnPlatform")
[System.IO.File]::WriteAllText($manifestPath, $manifestUpdated, (New-Object System.Text.UTF8Encoding($false)))

Write-Host "[3/5] Normalizing UTF-8 (no BOM) and LF line endings in fpk text files..."
$textFiles = @(
  Get-Item "fpk\manifest"
  Get-Item "fpk\app\ui\config"
  Get-ChildItem "fpk\cmd\*" -File
  Get-ChildItem "fpk\config\*" -File
  Get-ChildItem "fpk\wizard\*" -File -ErrorAction SilentlyContinue
)
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
foreach ($f in $textFiles) {
  $raw = [System.IO.File]::ReadAllText($f.FullName)
  $lf = $raw -replace "`r`n", "`n"
  [System.IO.File]::WriteAllText($f.FullName, $lf, $utf8NoBom)
}

Write-Host "[4/5] Checking fnpack.exe..."
$fnpackCmd = Get-Command fnpack -ErrorAction SilentlyContinue
$fnpackExe = if ($fnpackCmd) { $fnpackCmd.Source } else { Join-Path $RootDir "tools\fnpack.exe" }
if (-not (Test-Path $fnpackExe)) {
  $url = "https://static2.fnnas.com/fnpack/fnpack-$FnpackVersion-windows-amd64"
  Write-Host "Downloading fnpack: $url"
  Invoke-WebRequest -Uri $url -OutFile $fnpackExe
}

Write-Host "[5/5] Building .fpk package..."
Remove-Item -Force "fpk\*.fpk" -ErrorAction SilentlyContinue
Push-Location "fpk"
try {
  & $fnpackExe build
} finally {
  Pop-Location
}

$outName = "fn-nspawn-v$AppVersion-$FnPlatform.fpk"
Move-Item -Force "fpk\*.fpk" "dist\$outName"
Get-ChildItem "dist\*.fpk"
Write-Host "[OK] Build complete! Upload dist\$outName to fnOS App Center -> Manual Install."
