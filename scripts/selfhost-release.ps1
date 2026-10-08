# Build an offline fork release from one clean commit. PowerShell 7 + Docker only.
[CmdletBinding()]
param(
    [ValidateSet('linux/amd64', 'linux/arm64')][string]$Platform = 'linux/amd64',
    [string]$OutputDirectory = 'release-artifacts',
    [switch]$ExportImages,
    [switch]$Smoke
)
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

function Invoke-Native([string]$File, [string[]]$Arguments) {
    $result = & $File @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$File failed with exit code $LASTEXITCODE" }
    return $result
}
function Inspect-Image([string]$Reference) {
    $items = (Invoke-Native docker @('image', 'inspect', '--platform', $Platform, $Reference)) -join "`n" | ConvertFrom-Json
    return $items[0]
}
function Write-Utf8([string]$Path, [string]$Content) {
    [IO.File]::WriteAllText($Path, $Content, [Text.UTF8Encoding]::new($false))
}
function Get-FreePort {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { return $listener.LocalEndpoint.Port } finally { $listener.Stop() }
}
function Wait-Http([string]$Url) {
    $deadline = [DateTime]::UtcNow.AddMinutes(3)
    do {
        try {
            $response = Invoke-WebRequest $Url -TimeoutSec 5
            if ($response.StatusCode -eq 200) { return }
        } catch { }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "HTTP smoke failed: $Url"
}

Push-Location (Split-Path $PSScriptRoot -Parent)
$smokeStarted = $false
$savedEnvironment = @{}
try {
    $sha = (Invoke-Native git @('rev-parse', 'HEAD')).Trim()
    if (Invoke-Native git @('status', '--porcelain')) { throw 'Build requires a clean checkout (including untracked source).' }
    [void](Invoke-Native git @('fetch', 'origin', 'main'))
    $version = "0.6.2-accordlens.$($sha.Substring(0,12))"
    $date = (Invoke-Native git @('show', '-s', '--format=%cI', 'HEAD')).Trim()
    $out = [IO.Path]::GetFullPath($OutputDirectory)
    if (Test-Path $out) { throw "Output already exists: $out. Choose a new directory; keep previous evidence." }
    [void](New-Item -ItemType Directory -Path $out)
    $images = [ordered]@{}
    $buildArgs = @('--build-arg', "VERSION=$version", '--build-arg', "COMMIT=$sha", '--build-arg', "DATE=$date")
    foreach ($component in @('backend', 'web')) {
        $tag = "accordlens/multica-${component}:sha-$sha"
        $file = if ($component -eq 'backend') { 'Dockerfile' } else { 'Dockerfile.web' }
        $metadataFile = Join-Path $out "$component-build.json"
        $argsForBuild = @('buildx', 'build', '--platform', $Platform, '--load', '--file', $file,
            '--tag', $tag, '--metadata-file', $metadataFile, '--provenance=false') + $buildArgs
        if ($component -eq 'web') { $argsForBuild += @('--build-arg', "NEXT_PUBLIC_APP_VERSION=$version") }
        [void](Invoke-Native docker ($argsForBuild + @('.')))
        $info = Inspect-Image $tag
        if ("$($info.Os)/$($info.Architecture)" -ne $Platform) { throw "Wrong platform: $tag" }
        if ($info.Config.Labels.'org.opencontainers.image.revision' -ne $sha) { throw "Wrong revision: $tag" }
        $metadata = Get-Content $metadataFile -Raw | ConvertFrom-Json
        $images[$component] = [ordered]@{
            tag = $tag; image_id = $info.Id; manifest_digest = $metadata.'containerimage.digest'
            config_digest = $metadata.'containerimage.config.digest'
            repo_digests = @($info.RepoDigests); platform = $Platform
            deploy_reference = $info.Id; digest_kind = 'docker-image-id'
        }
    }
    # Resolve the upstream database once, then freeze its local image ID too.
    [void](Invoke-Native docker @('pull', '--platform', $Platform, 'pgvector/pgvector:pg17'))
    $db = Inspect-Image 'pgvector/pgvector:pg17'
    if ("$($db.Os)/$($db.Architecture)" -ne $Platform) { throw 'Wrong database platform' }
    $images['database'] = [ordered]@{
        tag = 'pgvector/pgvector:pg17'; image_id = $db.Id; repo_digests = @($db.RepoDigests)
        platform = $Platform; deploy_reference = $db.Id; digest_kind = 'docker-image-id'
    }
    # Reuse the backend builder's source/toolchain for the native Windows runtime.
    $cliDirectory = Join-Path $out 'cli-windows-amd64'
    [void](Invoke-Native docker (@('buildx', 'build', '--platform', $Platform, '--file', 'Dockerfile',
        '--target', 'windows-cli', '--output', "type=local,dest=$cliDirectory") + $buildArgs + @('.')))
    $cliPath = Join-Path $cliDirectory 'multica.exe'
    if (-not (Test-Path $cliPath)) { throw 'Windows CLI export missing' }
    $linuxCliVersion = (Invoke-Native docker @('run', '--rm', '--platform', $Platform,
        '--entrypoint', '/app/multica', $images.backend.image_id, '--version')) -join "`n"
    if (-not $linuxCliVersion.Contains($sha) -or -not $linuxCliVersion.Contains($version)) { throw 'CLI version mismatch' }
    $releaseEnv = @(
        "MULTICA_RELEASE_PLATFORM=$Platform",
        "MULTICA_RELEASE_BACKEND_IMAGE=$($images.backend.deploy_reference)",
        "MULTICA_RELEASE_WEB_IMAGE=$($images.web.deploy_reference)",
        "MULTICA_RELEASE_DATABASE_IMAGE=$($images.database.deploy_reference)"
    ) -join "`n"
    Write-Utf8 (Join-Path $out 'release.env') ($releaseEnv + "`n")
    $manifest = [ordered]@{
        schema_version = 1; repository = 'https://github.com/accordlens/multica'; source_sha = $sha
        observed_main_sha = (Invoke-Native git @('rev-parse', 'origin/main')).Trim()
        upstream_fork_base = 'e0f84dda47d42f1826af6b8f596de0b38c3d90a6'
        version = $version; source_date = $date; built_at = [DateTime]::UtcNow.ToString('o')
        platform = $Platform; images = $images
        cli = @{ source_sha = $sha; version = $version; linux_version_output = $linuxCliVersion
            windows_amd64_sha256 = (Get-FileHash $cliPath -Algorithm SHA256).Hash.ToLowerInvariant() }
        tools = @{ docker = (Invoke-Native docker @('version', '--format', '{{json .}}')) -join "`n"
            compose = (Invoke-Native docker @('compose', 'version')) -join "`n"
            buildx = (Invoke-Native docker @('buildx', 'version')) -join "`n"
            powershell = $PSVersionTable.PSVersion.ToString()
            node = (Invoke-Native docker @('run', '--rm', '--platform', $Platform, '--entrypoint', 'node', $images.web.image_id, '--version')) -join "`n"
            pnpm = (Get-Content package.json -Raw | ConvertFrom-Json).packageManager }
        attribution = @{ LICENSE_sha256 = (Get-FileHash LICENSE).Hash.ToLowerInvariant()
            NOTICE_sha256 = (Get-FileHash NOTICE).Hash.ToLowerInvariant() }
        smoke = 'NOT_RUN'; windows_host_validation = 'NOT_RUN'; registry_publication = 'NOT_PERFORMED'
    }
    if ($IsWindows) {
        $windowsVersion = (Invoke-Native $cliPath @('--version')) -join "`n"
        if (-not $windowsVersion.Contains($sha) -or -not $windowsVersion.Contains($version)) { throw 'Windows CLI version mismatch' }
        $manifest.windows_host_validation = 'PASS: native Windows CLI --version (Docker/reboot/LTE acceptance separate)'
    }
    if ($ExportImages) {
        $archive = Join-Path $out 'images.tar'
        [void](Invoke-Native docker @('save', '--platform', $Platform, '--output', $archive, $images.backend.tag, $images.web.tag, $images.database.tag))
        $manifest['archive'] = @{ file = 'images.tar'; sha256 = (Get-FileHash $archive).Hash.ToLowerInvariant() }
    }
    if ($Smoke) {
        $project = "acco52-smoke-$($sha.Substring(0,8))-$([Guid]::NewGuid().ToString('N').Substring(0,8))"
        $backendPort = Get-FreePort
        $webPort = Get-FreePort
        while ($webPort -eq $backendPort) { $webPort = Get-FreePort }
        $smokeEnv = Join-Path $out 'smoke.env'
        $key = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
        Write-Utf8 $smokeEnv ($releaseEnv + "`nJWT_SECRET=$key`nPOSTGRES_PASSWORD=$key`nBACKEND_PORT=$backendPort`nFRONTEND_PORT=$webPort`nFRONTEND_ORIGIN=http://localhost:$webPort`nMULTICA_APP_URL=http://localhost:$webPort`nDO_NOT_TRACK=true`nALLOW_SIGNUP=false`n")
        # Compose gives the process environment precedence over --env-file.
        # Clear only variables referenced by these files, then restore them.
        $composeText = (Get-Content docker-compose.selfhost.yml -Raw) + (Get-Content docker-compose.selfhost.release.yml -Raw)
        foreach ($match in [regex]::Matches($composeText, '\$\{([A-Z][A-Z0-9_]*)')) {
            $name = $match.Groups[1].Value
            if (-not $savedEnvironment.ContainsKey($name)) {
                $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
                Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
            }
        }
        $composeArgs = @('compose', '--project-name', $project, '--env-file', $smokeEnv,
            '-f', 'docker-compose.selfhost.yml', '-f', 'docker-compose.selfhost.release.yml')
        $smokeStarted = $true
        [void](Invoke-Native docker ($composeArgs + @('up', '-d', '--no-build', '--pull', 'never', '--wait', '--wait-timeout', '180')))
        Wait-Http "http://127.0.0.1:$backendPort/health"
        Wait-Http "http://127.0.0.1:$backendPort/readyz"
        Wait-Http "http://127.0.0.1:$webPort/"
        foreach ($service in @('backend', 'frontend', 'postgres')) {
            $component = @{ backend = 'backend'; frontend = 'web'; postgres = 'database' }[$service]
            $container = (Invoke-Native docker ($composeArgs + @('ps', '-q', $service))).Trim()
            $actual = (Invoke-Native docker @('inspect', '--format', '{{.Image}}', $container)).Trim()
            if ($actual -ne $images[$component].image_id) { throw "Running image mismatch: $service" }
        }
        $manifest.smoke = 'PASS: /health, /readyz, frontend HTTP 200; all running image IDs match'
    }
    Write-Utf8 (Join-Path $out 'manifest.json') ($manifest | ConvertTo-Json -Depth 20)
    Write-Host "Release ready: $sha ($Platform); manifest: $out/manifest.json"
} finally {
    try {
        if ($smokeStarted) {
            # Only this script's unique disposable project and volumes are removed.
            try {
                [void](Invoke-Native docker ($composeArgs + @('down', '--volumes', '--remove-orphans')))
            } finally { Remove-Item $smokeEnv -ErrorAction SilentlyContinue }
        }
    } finally {
        foreach ($name in $savedEnvironment.Keys) {
            if ($null -eq $savedEnvironment[$name]) {
                Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
            } else {
                [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
            }
        }
        Pop-Location
    }
}
