# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 jdominick05
<#
  llm-top installer for Windows: downloads a release, checks it against SHA256SUMS, installs llm-top.exe and adds
  the folder to the user PATH. Works in Windows PowerShell 5.1 and PowerShell 7.

    irm https://raw.githubusercontent.com/jdominick05/llm-top/main/install.ps1 | iex
    .\install.ps1 [-Version 0.2.0] [-Dir C:\tools\llm-top] [-NoPath]

  Default dir: %LOCALAPPDATA%\Programs\llm-top. Env (for the irm | iex form): LLMTOP_VERSION, LLMTOP_DIR.
  Optional: GITHUB_TOKEN (higher API rate limit), LLMTOP_GH=1 (download with a logged-in gh instead).
  The config is llm-top.json next to the exe if one is there, else %APPDATA%\llm-top\llm-top.json.
  Uninstall: delete the folder and remove it from the user PATH.
#>
param(
    [string]$Version = $(if ($env:LLMTOP_VERSION) { $env:LLMTOP_VERSION } else { 'latest' }),
    [string]$Dir = $(if ($env:LLMTOP_DIR) { $env:LLMTOP_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\llm-top' }),
    [switch]$NoPath
)

# under irm | iex this runs in the caller's session: put its preferences back afterwards
$llmtopSaved = @($ErrorActionPreference, $ProgressPreference)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$tmp = $null
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $repo = 'jdominick05/llm-top'
    $Version = $Version.TrimStart('v')
    # relative to the PowerShell location: .NET's current directory doesn't follow Set-Location
    $Dir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Dir).TrimEnd('\')
    function Fail($msg) { throw "install.ps1: $msg" }

    if ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64' -and $env:PROCESSOR_ARCHITEW6432 -ne 'AMD64') {
        Fail "no prebuilt binary for $env:PROCESSOR_ARCHITECTURE; build from source: go build -o llm-top.exe ./src"
    }
    $exe = Join-Path $Dir 'llm-top.exe'
    $running = Get-Process llm-top -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe }
    if ($running) { Fail "$exe is running (pid $($running.Id -join ', ')); close it first" }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("llm-top-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory $tmp | Out-Null

    if ($env:LLMTOP_GH -and (Get-Command gh -ErrorAction SilentlyContinue)) {
        $a = @('release', 'download')
        if ($Version -ne 'latest') { $a += "v$Version" }
        & gh @a -R $repo -p 'llm-top-*-windows-amd64.zip' -p SHA256SUMS -D $tmp
        if ($LASTEXITCODE -ne 0) { Fail 'gh release download failed' }
    }
    else {
        $api = "https://api.github.com/repos/$repo/releases/latest"
        if ($Version -ne 'latest') { $api = "https://api.github.com/repos/$repo/releases/tags/v$Version" }
        $hdr = @{ Accept = 'application/vnd.github+json'; 'User-Agent' = 'llm-top-install' }
        $rel = $null
        foreach ($useToken in @([bool]$env:GITHUB_TOKEN, $false) | Select-Object -Unique) {
            $h = $hdr.Clone()
            if ($useToken) { $h.Authorization = "Bearer $env:GITHUB_TOKEN" }
            try { $rel = Invoke-RestMethod -Headers $h $api; break }
            catch {
                $code = 0
                if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
                if ($code -eq 401 -and $useToken) { continue } # a stale token: the repo is public, try without
                if ($code -eq 404) { Fail "no release $(if ($Version -eq 'latest') { 'yet' } else { "v$Version" }) (see https://github.com/$repo/releases)" }
                if ($code -eq 403 -or $code -eq 429) { Fail 'GitHub API rate limit reached; try again in a while or set GITHUB_TOKEN' }
                Fail "cannot read ${api}: $($_.Exception.Message)"
            }
        }
        foreach ($n in @("llm-top-$($rel.tag_name.TrimStart('v'))-windows-amd64.zip", 'SHA256SUMS')) {
            $asset = $rel.assets | Where-Object { $_.name -eq $n }
            if (-not $asset) { Fail "release $($rel.tag_name) has no $n" }
            Invoke-WebRequest -UseBasicParsing -Headers @{ 'User-Agent' = 'llm-top-install' } $asset.browser_download_url -OutFile (Join-Path $tmp $n)
        }
    }

    $zip = Get-ChildItem $tmp -Filter 'llm-top-*-windows-amd64.zip' | Select-Object -First 1
    if (-not $zip) { Fail 'download incomplete' }
    $line = Get-Content (Join-Path $tmp 'SHA256SUMS') | Where-Object { $_ -match "^([0-9a-f]{64})  $([regex]::Escape($zip.Name))$" } | Select-Object -First 1
    if (-not $line) { Fail "SHA256SUMS has no entry for $($zip.Name)" }
    $want = $line.Substring(0, 64)
    $sum = (Get-FileHash -Algorithm SHA256 $zip.FullName).Hash.ToLower()
    if ($sum -ne $want) { Fail "checksum mismatch for $($zip.Name)" }

    Expand-Archive -Path $zip.FullName -DestinationPath (Join-Path $tmp 'x')
    New-Item -ItemType Directory -Force $Dir | Out-Null
    Copy-Item -Force (Join-Path $tmp 'x\llm-top.exe') $exe
    Write-Host "installed $(& $exe -version) to $exe"

    if (-not $NoPath) {
        # read and write the raw value: entries like %USERPROFILE%\bin must stay unexpanded (REG_EXPAND_SZ)
        $key = Get-Item 'HKCU:\Environment'
        $userPath = [string]$key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
        $parts = @($userPath -split ';' | Where-Object { $_ })
        if (($parts | ForEach-Object { $_.TrimEnd('\') }) -notcontains $Dir) {
            Set-ItemProperty -Path 'HKCU:\Environment' -Name Path -Value (($parts + $Dir) -join ';') -Type ExpandString
            # tell Explorer the environment changed, so new terminals see it
            [Environment]::SetEnvironmentVariable('LLMTOP_INSTALL_PING', '1', 'User')
            [Environment]::SetEnvironmentVariable('LLMTOP_INSTALL_PING', $null, 'User')
            Write-Host "added $Dir to the user PATH (new terminals pick it up)"
        }
    }
    $cfg = Join-Path $Dir 'llm-top.json'
    if (-not (Test-Path $cfg)) { $cfg = Join-Path $env:APPDATA 'llm-top\llm-top.json' }
    Write-Host "config: $cfg (written with defaults on first run; set server_url / api_key there)"
}
finally {
    if ($tmp) { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
    $ErrorActionPreference, $ProgressPreference = $llmtopSaved
}
