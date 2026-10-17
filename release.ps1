# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 jdominick05
<#
  Local release: .\release.ps1 0.2.0 [-InstallTo C:\tools\llm-top.exe]
  Clean tree, a "## <version> - <date>" heading in CHANGELOG.md and a new tag required. Runs go vet, builds
  windows/amd64 with the version stamped in, writes dist\llm-top-<v>-windows-amd64.zip and dist\SHA256SUMS,
  tags v<v> (annotated) and copies the exe to -InstallTo (default $env:LLMTOP_INSTALL; neither = no install).
  Nothing is pushed or published.

  -SkipInstall leaves the installed exe alone (e.g. while the TUI is open).
#>
param([Parameter(Mandatory = $true)][string]$Version, [string]$InstallTo = $env:LLMTOP_INSTALL, [switch]$SkipInstall)

$ErrorActionPreference = 'Stop'
$install = $InstallTo
if (-not $install) { $SkipInstall = $true }

function Fail($msg) { Write-Host "release: $msg" -ForegroundColor Red; exit 1 }
function Check($what) { if ($LASTEXITCODE -ne 0) { Fail "$what failed (exit $LASTEXITCODE)" } }

if ($Version -notmatch '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?$') {
    Fail "'$Version' is not a semver like 0.1.0 (no leading v)"
}
$tag = "v$Version"
$name = "llm-top-$Version-windows-amd64"

Push-Location $PSScriptRoot
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    $dirty = git status --porcelain
    Check 'git status'
    if ($dirty) { Fail "working tree is dirty; commit or stash first:`n$($dirty -join "`n")" }

    git rev-parse -q --verify "refs/tags/$tag" | Out-Null
    if ($LASTEXITCODE -eq 0) { Fail "tag $tag already exists" }

    # release.yml takes the notes from this section and refuses without it
    if (-not (Select-String -Path CHANGELOG.md -Pattern "^## $([regex]::Escape($Version)) " -Quiet)) {
        Fail "CHANGELOG.md has no '## $Version - <date>' heading"
    }

    $running = $null
    if ($install) { $running = Get-Process llm-top -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $install } }
    if ($running -and -not $SkipInstall) { Fail "$install is running (pid $($running.Id -join ', ')); close it first or use -SkipInstall" }

    Write-Host "go vet"
    go vet ./...
    Check 'go vet'

    $stage = Join-Path 'dist' $name
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory -Force $stage | Out-Null
    $exe = Join-Path $stage 'llm-top.exe'

    Write-Host "go build $Version"
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $exe ./src
    Check 'go build'

    $got = & $exe -version
    if ($got -ne "llm-top $Version") { Fail "built binary reports '$got'" }

    Copy-Item README.md, CHANGELOG.md, LICENSE $stage
    $zip = Join-Path 'dist' "$name.zip"
    if (Test-Path $zip) { Remove-Item -Force $zip }
    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip

    # sha256sum format, one line per release zip in dist\, ASCII + LF
    $lines = Get-ChildItem dist -Filter 'llm-top-*.zip' | Sort-Object Name | ForEach-Object {
        (Get-FileHash -Algorithm SHA256 $_.FullName).Hash.ToLower() + '  ' + $_.Name
    }
    [IO.File]::WriteAllText((Join-Path $PSScriptRoot 'dist\SHA256SUMS'), (($lines -join "`n") + "`n"), [Text.Encoding]::ASCII)

    git tag -a $tag -m "llm-top $Version"
    Check 'git tag'

    if (-not $SkipInstall) { Copy-Item -Force $exe $install }
    Remove-Item -Recurse -Force $stage

    Write-Host "released $tag"
    Write-Host "  $zip"
    Get-Content dist\SHA256SUMS | ForEach-Object { Write-Host "  $_" }
    if ($SkipInstall) { Write-Host "  not installed (-InstallTo or LLMTOP_INSTALL); the exe is in $zip" }
    else { Write-Host "  installed: $(& $install -version)" }
}
finally {
    foreach ($k in $saved.Keys) {
        if ($null -eq $saved[$k]) { Remove-Item "env:$k" -ErrorAction SilentlyContinue }
        else { Set-Item "env:$k" -Value $saved[$k] }
    }
    Pop-Location
}
