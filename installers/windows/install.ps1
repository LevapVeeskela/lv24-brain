#Requires -Version 5.1
<#
.SYNOPSIS
Installs the LV24 Brain engine on Windows.

.DESCRIPTION
Installs the brain.exe binary from a GitHub release (or a local file), verifies
its SHA-256 checksum, writes brain.yaml, optionally adds the install directory
to the user PATH, and optionally installs the OpenCode handoff hook.

The installer never asks for, writes, or logs secrets. Git mode uses the
existing Git credential helper; server mode uses `brain login`, BRAIN_TOKEN, or
`brain secret set server-token`.

.PARAMETER Mode
Install mode: local, git, or server. Prompted when omitted unless -Yes is set.

.PARAMETER DataDir
Brain data directory. Defaults to %USERPROFILE%\brain (local) or
%USERPROFILE%\brain-data (git). Not written for server mode.

.PARAMETER RepoUrl
Private data repository URL for git mode. The installer runs git clone when the
target data directory is empty or absent.

.PARAMETER ServerUrl
Brain server base URL for server mode.

.PARAMETER Channel
Update channel recorded in brain.yaml: stable or beta.

.PARAMETER Version
Release version to install (for example 1.2.3) or latest.

.PARAMETER SourceBinary
Install a local brain.exe instead of downloading a release.

.PARAMETER Sha256
Expected SHA-256 for -SourceBinary or as an override for the release asset.

.EXAMPLE
.\install.ps1 -Mode local -InstallOpenCodeHook

.EXAMPLE
.\install.ps1 -Mode git -RepoUrl git@github.com:example/brain-data.git

.EXAMPLE
.\install.ps1 -Mode server -ServerUrl https://brain.example.com

.NOTES
No secrets are stored by this script. Keep .env files and tokens out of any
brain data repository.
#>
[CmdletBinding()]
param(
    [ValidateSet('local', 'git', 'server')]
    [string]$Mode,

    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\lv24-brain'),

    [string]$DataDir,

    [string]$RepoUrl,

    [string]$ServerUrl,

    [ValidateSet('stable', 'beta')]
    [string]$Channel = 'stable',

    [string]$Version = 'latest',

    [string]$Repo = 'LevapVeeskela/lv24-brain',

    [string]$SourceBinary,

    [string]$Sha256,

    [switch]$InstallOpenCodeHook,

    [switch]$NoPath,

    [switch]$SkipChecksum,

    [switch]$Yes
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {
    # Older hosts keep their configured protocol.
}

$script:Headers = @{
    'User-Agent' = 'lv24-brain-installer'
    'Accept'     = 'application/vnd.github+json'
}
$script:StageRoot = Join-Path $env:TEMP ('lv24-brain-install-' + [Guid]::NewGuid().ToString('N'))
$script:CreatedStageRoot = $false

function Write-Step {
    param([Parameter(Mandatory = $true)][string]$Text)
    Write-Host "==> $Text"
}

function Write-Note {
    param([Parameter(Mandatory = $true)][string]$Text)
    Write-Host "    $Text"
}

function Get-Field {
    param(
        [Parameter(Mandatory = $true)]$Object,
        [Parameter(Mandatory = $true)][string]$Name
    )
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }
    return $property.Value
}

function Get-PlatformArchitecture {
    $architecture = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) {
        $architecture = $env:PROCESSOR_ARCHITEW6432
    }
    switch ($architecture.ToUpperInvariant()) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { throw "Unsupported Windows architecture: $architecture. Releases cover amd64 and arm64." }
    }
}

function Resolve-InstallMode {
    param(
        [string]$RequestedMode,
        [switch]$NonInteractive
    )
    if ($RequestedMode) {
        return $RequestedMode
    }
    if ($NonInteractive) {
        return 'local'
    }
    Write-Host 'Choose an install mode:'
    Write-Host '  1) local  - this machine only, Markdown files'
    Write-Host '  2) git    - shared private data repository (team default)'
    Write-Host '  3) server - connect to a team brain server'
    $choice = Read-Host 'Mode [1]'
    switch ($choice.Trim().ToLowerInvariant()) {
        '' { return 'local' }
        '1' { return 'local' }
        'local' { return 'local' }
        '2' { return 'git' }
        'git' { return 'git' }
        '3' { return 'server' }
        'server' { return 'server' }
        default { throw "Unknown mode choice: $choice" }
    }
}

function Resolve-ServerUrl {
    param(
        [string]$Value,
        [switch]$NonInteractive
    )
    if (-not $Value -and -not $NonInteractive) {
        $Value = Read-Host 'Brain server URL (https://...)'
    }
    if (-not $Value) {
        throw 'Server mode requires -ServerUrl.'
    }
    $uri = $null
    if (-not [Uri]::TryCreate($Value, [UriKind]::Absolute, [ref]$uri)) {
        throw "Invalid server URL: $Value"
    }
    if ($uri.Scheme -ne 'https' -and $uri.Scheme -ne 'http') {
        throw 'Server URL must use http or https.'
    }
    return $Value.TrimEnd('/')
}

function Resolve-DataDir {
    param(
        [string]$Requested,
        [string]$SelectedMode
    )
    if ($Requested) {
        return [IO.Path]::GetFullPath($Requested)
    }
    if ($SelectedMode -eq 'git') {
        return [IO.Path]::GetFullPath((Join-Path $env:USERPROFILE 'brain-data'))
    }
    return [IO.Path]::GetFullPath((Join-Path $env:USERPROFILE 'brain'))
}

function Invoke-Download {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Destination
    )
    if (-not $Uri.StartsWith('https://', [StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing non-HTTPS download URL: $Uri"
    }
    Invoke-WebRequest -Uri $Uri -OutFile $Destination -Headers $script:Headers -UseBasicParsing
}

function Get-Release {
    param(
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][string]$ReleaseVersion,
        [Parameter(Mandatory = $true)][string]$ReleaseChannel
    )
    $uri = "https://api.github.com/repos/$Repository/releases?per_page=30"
    $releases = @(Invoke-RestMethod -Uri $uri -Headers $script:Headers -Method Get)
    $filtered = @($releases | Where-Object { -not $_.draft })
    if ($ReleaseChannel -eq 'stable') {
        $filtered = @($filtered | Where-Object { -not $_.prerelease })
    }
    if ($ReleaseVersion -ne 'latest') {
        $tag = $ReleaseVersion
        if (-not $tag.StartsWith('v')) {
            $tag = 'v' + $tag
        }
        $match = @($filtered | Where-Object { $_.tag_name -eq $tag -or $_.tag_name -eq $ReleaseVersion }) | Select-Object -First 1
        if (-not $match) {
            throw "Release $ReleaseVersion was not found on channel $ReleaseChannel."
        }
        return $match
    }
    $latest = $filtered | Sort-Object { [DateTime]$_.published_at } -Descending | Select-Object -First 1
    if (-not $latest) {
        throw "No releases found on channel $ReleaseChannel."
    }
    return $latest
}

function Get-ReleaseManifest {
    param(
        [Parameter(Mandatory = $true)]$Release
    )
    $manifestAsset = @((Get-Field $Release 'assets') | Where-Object { (Get-Field $_ 'name') -eq 'release.json' }) | Select-Object -First 1
    if (-not $manifestAsset) {
        throw 'The selected release has no release.json manifest.'
    }
    if (-not (Test-Path -LiteralPath $script:StageRoot)) {
        New-Item -ItemType Directory -Path $script:StageRoot -Force | Out-Null
        $script:CreatedStageRoot = $true
    }
    $manifestPath = Join-Path $script:StageRoot 'release.json'
    Invoke-Download -Uri $manifestAsset.browser_download_url -Destination $manifestPath
    return Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
}

function Get-ManifestAsset {
    param(
        [Parameter(Mandatory = $true)]$Manifest,
        [Parameter(Mandatory = $true)][string]$Architecture
    )
    $candidates = @((Get-Field $Manifest 'assets') | Where-Object {
            (Get-Field $_ 'os') -eq 'windows' -and
            (Get-Field $_ 'arch') -eq $Architecture -and
            ([string]::IsNullOrEmpty((Get-Field $_ 'kind')) -or (Get-Field $_ 'kind') -eq 'cli' -or (Get-Field $_ 'kind') -eq 'brain')
        })
    if ($candidates.Count -eq 0) {
        throw "Release $($Manifest.version) has no Windows $Architecture CLI asset."
    }
    return $candidates[0]
}

function Get-ChecksumFromFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$FileName
    )
    foreach ($line in Get-Content -LiteralPath $Path -Encoding UTF8) {
        $trimmed = $line.Trim()
        if (-not $trimmed -or $trimmed.StartsWith('#')) {
            continue
        }
        $parts = $trimmed -split '\s+', 2
        if ($parts.Count -ne 2) {
            continue
        }
        $name = $parts[1].TrimStart('*')
        if ($name -eq $FileName -and $parts[0] -match '^[0-9a-fA-F]{64}$') {
            return $parts[0].ToLowerInvariant()
        }
    }
    return ''
}

function Assert-FileChecksum {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Expected
    )
    if (-not $Expected) {
        throw 'No SHA-256 checksum is available for the selected binary.'
    }
    $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $Expected.ToLowerInvariant()) {
        throw "SHA-256 mismatch: got $actual, expected $($Expected.ToLowerInvariant())."
    }
}

function Install-BrainBinary {
    param(
        [Parameter(Mandatory = $true)][string]$TargetDir,
        [Parameter(Mandatory = $true)][string]$Architecture
    )
    if (-not (Test-Path -LiteralPath $script:StageRoot)) {
        New-Item -ItemType Directory -Path $script:StageRoot -Force | Out-Null
        $script:CreatedStageRoot = $true
    }
    $downloadPath = Join-Path $script:StageRoot 'brain-download.exe'
    $expected = $Sha256

    if ($SourceBinary) {
        $source = (Resolve-Path -LiteralPath $SourceBinary).Path
        Copy-Item -LiteralPath $source -Destination $downloadPath -Force
        Write-Step "Using local binary $source"
    }
    else {
        Write-Step "Looking up release ($Channel, $Version)"
        $release = Get-Release -Repository $Repo -ReleaseVersion $Version -ReleaseChannel $Channel
        $manifest = Get-ReleaseManifest -Release $release
        $asset = Get-ManifestAsset -Manifest $manifest -Architecture $Architecture
        if (-not $expected) {
            $expected = Get-Field $asset 'sha256'
        }
        $checksumsUrl = Get-Field $manifest 'checksums_url'
        if (-not $expected -and $checksumsUrl) {
            $checksumsPath = Join-Path $script:StageRoot 'checksums.txt'
            Invoke-Download -Uri $checksumsUrl -Destination $checksumsPath
            $expected = Get-ChecksumFromFile -Path $checksumsPath -FileName (Get-Field $asset 'name')
        }
        Write-Step "Downloading brain $($manifest.version) for windows/$Architecture"
        Invoke-Download -Uri (Get-Field $asset 'url') -Destination $downloadPath
    }

    if ($SkipChecksum) {
        Write-Warning 'Checksum verification was skipped by explicit request.'
    }
    else {
        Assert-FileChecksum -Path $downloadPath -Expected $expected
    }

    if (-not (Test-Path -LiteralPath $TargetDir)) {
        New-Item -ItemType Directory -Path $TargetDir -Force | Out-Null
    }
    $brainExe = Join-Path $TargetDir 'brain.exe'
    $staged = Join-Path $TargetDir 'brain.exe.new'
    Copy-Item -LiteralPath $downloadPath -Destination $staged -Force
    try {
        Move-Item -LiteralPath $staged -Destination $brainExe -Force
    }
    catch {
        Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
        throw "Could not replace $brainExe. Stop any running brain process and retry. $($_.Exception.Message)"
    }
    return $brainExe
}

function Add-UserPath {
    param([Parameter(Mandatory = $true)][string]$Directory)
    $current = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($null -eq $current) {
        $current = ''
    }
    $entries = @($current -split ';' | Where-Object { $_ })
    $present = $false
    foreach ($entry in $entries) {
        if ($entry.TrimEnd('\') -ieq $Directory.TrimEnd('\')) {
            $present = $true
            break
        }
    }
    if (-not $present) {
        $updated = (($entries + $Directory.TrimEnd('\')) -join ';')
        [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
        Write-Step "Added $Directory to the user PATH (open a new terminal to pick it up)"
    }
    $env:Path = $Directory + ';' + $env:Path
}

function Write-BrainConfig {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$SelectedMode,
        [string]$BrainDataDir,
        [string]$BrainServerUrl,
        [Parameter(Mandatory = $true)][string]$UpdateChannel
    )
    $lines = @(
        '# lv24-brain instance configuration',
        "mode: $SelectedMode"
    )
    if ($BrainDataDir) {
        $escaped = $BrainDataDir.Replace("'", "''")
        $lines += "data_dir: '$escaped'"
    }
    if ($BrainServerUrl) {
        $escaped = $BrainServerUrl.Replace("'", "''")
        $lines += "server_url: '$escaped'"
    }
    $lines += 'schema_version: 1'
    $lines += "update_channel: $UpdateChannel"
    $parent = Split-Path -Parent $Path
    if ($parent -and -not (Test-Path -LiteralPath $parent)) {
        New-Item -ItemType Directory -Path $parent -Force | Out-Null
    }
    $encoding = New-Object System.Text.UTF8Encoding($false)
    [IO.File]::WriteAllText($Path, (($lines -join [Environment]::NewLine) + [Environment]::NewLine), $encoding)
}

function Initialize-BrainInstance {
    param(
        [Parameter(Mandatory = $true)][string]$BrainExe,
        [Parameter(Mandatory = $true)][string]$ConfigPath,
        [Parameter(Mandatory = $true)][string]$SelectedMode,
        [string]$BrainDataDir,
        [string]$BrainServerUrl,
        [Parameter(Mandatory = $true)][string]$UpdateChannel
    )
    if ($BrainDataDir -and -not (Test-Path -LiteralPath $BrainDataDir)) {
        New-Item -ItemType Directory -Path $BrainDataDir -Force | Out-Null
    }
    $initArgs = @('init', '--mode', $SelectedMode, '--config', $ConfigPath)
    if ($BrainDataDir) {
        $initArgs += @('--data-dir', $BrainDataDir)
    }
    if ($BrainServerUrl) {
        $initArgs += @('--server-url', $BrainServerUrl)
    }
    try {
        & $BrainExe @initArgs | Out-Null
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "brain init exited with code $LASTEXITCODE; writing brain.yaml directly."
        }
    }
    catch {
        Write-Warning "brain init is not available yet; writing brain.yaml directly."
    }
    Write-BrainConfig -Path $ConfigPath -SelectedMode $SelectedMode -BrainDataDir $BrainDataDir -BrainServerUrl $BrainServerUrl -UpdateChannel $UpdateChannel
    return $ConfigPath
}

function Initialize-GitDataDir {
    param(
        [Parameter(Mandatory = $true)][string]$BrainDataDir,
        [string]$RepositoryUrl
    )
    if (-not $RepositoryUrl) {
        Write-Note "No -RepoUrl supplied; using $BrainDataDir as the data directory."
        if (-not (Test-Path -LiteralPath $BrainDataDir)) {
            New-Item -ItemType Directory -Path $BrainDataDir -Force | Out-Null
        }
        return
    }
    $existing = @()
    if (Test-Path -LiteralPath $BrainDataDir) {
        $existing = @(Get-ChildItem -LiteralPath $BrainDataDir -Force)
    }
    if ($existing.Count -gt 0) {
        Write-Note "Data directory already exists and is not empty; skipping git clone."
        return
    }
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
        throw 'Git is required for git mode. Install Git for Windows and retry.'
    }
    Write-Step 'Cloning the private data repository'
    & git clone $RepositoryUrl $BrainDataDir
    if ($LASTEXITCODE -ne 0) {
        throw 'git clone failed. Check your credential helper or SSH key; never embed tokens in the URL.'
    }
}

function Install-OpenCodeHook {
    param(
        [Parameter(Mandatory = $true)][string]$BrainExe,
        [Parameter(Mandatory = $true)][string]$ConfigPath
    )
    $candidates = @(
        (Join-Path $PSScriptRoot 'brain-handoff.js'),
        (Join-Path $PSScriptRoot '..\..\integrations\opencode\brain-handoff.js')
    )
    $template = $null
    foreach ($candidate in $candidates) {
        if (Test-Path -LiteralPath $candidate) {
            $template = (Resolve-Path -LiteralPath $candidate).Path
            break
        }
    }
    if (-not $template) {
        Write-Warning 'brain-handoff.js template was not found next to the installer; skipping the OpenCode hook.'
        return
    }
    $pluginDir = Join-Path $env:USERPROFILE '.config\opencode\plugins'
    if (-not (Test-Path -LiteralPath $pluginDir)) {
        New-Item -ItemType Directory -Path $pluginDir -Force | Out-Null
    }
    $target = Join-Path $pluginDir 'brain-handoff.js'
    if (Test-Path -LiteralPath $target) {
        $backup = $target + '.bak-' + (Get-Date -Format 'yyyyMMddHHmmss')
        Copy-Item -LiteralPath $target -Destination $backup -Force
        Write-Note "Backed up the previous hook to $backup"
    }
    $text = [IO.File]::ReadAllText($template)
    $text = $text.Replace('__BRAIN_BIN__', $BrainExe.Replace('\', '\\'))
    $text = $text.Replace('__BRAIN_CONFIG__', $ConfigPath.Replace('\', '\\'))
    $text = $text.Replace('__HANDOFF_SUMMARY__', 'Auto handoff: OpenCode session checkpoint')
    $encoding = New-Object System.Text.UTF8Encoding($false)
    [IO.File]::WriteAllText($target, $text, $encoding)
    Write-Step "Installed the OpenCode handoff hook at $target"
}

try {
    $Mode = Resolve-InstallMode -RequestedMode $Mode -NonInteractive:$Yes
    if ($Mode -eq 'server') {
        $ServerUrl = Resolve-ServerUrl -Value $ServerUrl -NonInteractive:$Yes
    }

    $installMode = $Mode
    $dataDirectory = $null
    if ($installMode -ne 'server') {
        $dataDirectory = Resolve-DataDir -Requested $DataDir -SelectedMode $installMode
    }
    elseif ($DataDir) {
        $dataDirectory = Resolve-DataDir -Requested $DataDir -SelectedMode $installMode
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $architecture = Get-PlatformArchitecture
    $brainExe = Install-BrainBinary -TargetDir $InstallDir -Architecture $architecture

    if ($installMode -eq 'git') {
        Initialize-GitDataDir -BrainDataDir $dataDirectory -RepositoryUrl $RepoUrl
    }
    $configDir = $dataDirectory
    if (-not $configDir) {
        $configDir = Join-Path $env:USERPROFILE 'brain'
    }
    $configPath = Join-Path $configDir 'brain.yaml'
    Initialize-BrainInstance -BrainExe $brainExe -ConfigPath $configPath -SelectedMode $installMode -BrainDataDir $dataDirectory -BrainServerUrl $ServerUrl -UpdateChannel $Channel | Out-Null

    if (-not $NoPath) {
        Add-UserPath -Directory $InstallDir
    }

    if (-not $InstallOpenCodeHook -and -not $Yes) {
        $answer = Read-Host 'Install the OpenCode handoff hook? [y/N]'
        if ($answer -match '^(y|yes)$') {
            $InstallOpenCodeHook = $true
        }
    }
    if ($InstallOpenCodeHook) {
        Install-OpenCodeHook -BrainExe $brainExe -ConfigPath $configPath
    }

    Write-Step 'Verifying the installed binary'
    try {
        & $brainExe version | Out-Null
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "brain version exited with code $LASTEXITCODE."
        }
    }
    catch {
        Write-Warning "Could not run brain version: $($_.Exception.Message)"
    }

    Write-Host ''
    Write-Step 'Install complete'
    Write-Note "binary:  $brainExe"
    Write-Note "config:  $configPath"
    if ($dataDirectory) {
        Write-Note "data:    $dataDirectory"
    }
    Write-Note "channel: $Channel"
    Write-Host ''
    if ($installMode -eq 'server') {
        Write-Note "Next: set BRAIN_TOKEN or run: brain secret set server-token; then run brain status."
    }
    else {
        Write-Note "Next: brain status --config `"$configPath`""
        Write-Note "      brain doctor --config `"$configPath`""
    }
    Write-Note 'No secrets were stored. Keep tokens and .env values out of the data repository.'
    exit 0
}
catch {
    Write-Host "Install failed: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
finally {
    if ($script:CreatedStageRoot -and (Test-Path -LiteralPath $script:StageRoot)) {
        Remove-Item -LiteralPath $script:StageRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}
