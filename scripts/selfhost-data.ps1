# Offline-consistent local DB/uploads backup; restore only into a NEW test project.
# Requires PowerShell 7 and the three self-host Compose files. No host bind mounts.
#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('Backup', 'Restore')][string]$Action,
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9][a-z0-9-]+$')][string]$Project,
    [Parameter(Mandatory)][string]$EnvFile,
    [Parameter(Mandatory)][string]$ReleaseEnv,
    [Parameter(Mandatory)][string]$Directory
)
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
function Native([string[]]$Arguments) {
    $result = & docker @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Docker command failed ($($Arguments[0])); exit $LASTEXITCODE" }
    return $result
}
function Container([string]$Service) {
    $id = (Native ($compose + @('ps', '-aq', $Service))) -join ''
    if (-not $id) { throw "Missing $Service container" }
    return $id.Trim()
}
function Run-Helper {
    [void](Native @('start', '--attach', $helper))
    if ((Native @('inspect', '--format', '{{.State.ExitCode}}', $helper)) -ne '0') { throw 'Upload archive operation failed' }
}
# Resolve Windows paths before moving into the checkout; docker cp handles spaces.
$EnvFile = (Resolve-Path -LiteralPath $EnvFile).Path
$ReleaseEnv = (Resolve-Path -LiteralPath $ReleaseEnv).Path
$Directory = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Directory)
Push-Location (Split-Path $PSScriptRoot -Parent)
$helper = $null
$resume = @()
$db = $null
$dumpPath = "/tmp/acco53-$([Guid]::NewGuid().ToString('N')).dump"
try {
    $compose = @('compose', '--project-name', $Project, '--env-file', $EnvFile,
        '--env-file', $ReleaseEnv, '-f', 'docker-compose.selfhost.yml',
        '-f', 'docker-compose.selfhost.release.yml', '-f', 'docker-compose.selfhost.windows.yml')
    # Parse in memory only: interpolated config contains credentials. Never log it.
    $config = (Native ($compose + @('config', '--format', 'json'))) -join "`n" | ConvertFrom-Json
    foreach ($volume in 'pgdata', 'backend_uploads') {
        if ($config.volumes.$volume.name -ne "${Project}_$volume" -or $config.volumes.$volume.external) {
            throw 'Expected isolated project-scoped named volumes'
        }
    }
    if ($config.services.backend.environment.S3_BUCKET) { throw 'This runbook backs up local uploads only' }
    if ($Action -eq 'Restore') {
        if ($Project -notmatch '^multica-restore-[a-z0-9-]+$') { throw 'Restore requires a new multica-restore-* test project' }
        $existing = Native @('volume', 'ls', '--format', '{{.Name}}')
        if ($existing -contains "${Project}_pgdata" -or $existing -contains "${Project}_backend_uploads" -or
            (Native ($compose + @('ps', '-aq')))) { throw 'Restore target already exists; choose a NEW test project' }
        $meta = Get-Content (Join-Path $Directory 'backup.json') -Raw | ConvertFrom-Json
        foreach ($file in 'database.dump', 'uploads.tar.gz') {
            if ((Get-FileHash (Join-Path $Directory $file) -Algorithm SHA256).Hash.ToLowerInvariant() -ne $meta.sha256.$file) {
                throw "Backup checksum mismatch: $file"
            }
        }
        # Refuse a different release before creating containers or touching data.
        foreach ($service in 'postgres', 'backend', 'frontend') {
            $image = (Native @('image', 'inspect', $config.services.$service.image)) -join "`n" | ConvertFrom-Json
            if ($image[0].Id -ne $meta.images.$service) { throw "Restore image mismatch: $service" }
        }
        [void](Native ($compose + @('up', '-d', '--no-build', '--pull', 'never', '--wait', '--wait-timeout', '180', 'postgres')))
        $db = Container 'postgres'
        [void](Native @('cp', (Join-Path $Directory 'database.dump'), "${db}:$dumpPath"))
        [void](Native ($compose + @('exec', '-T', 'postgres', 'sh', '-c',
            'pg_restore --exit-on-error --single-transaction --no-owner --no-acl -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$1"', 'sh', $dumpPath)))
        [void](Native ($compose + @('create', '--no-build', '--pull', 'never', 'backend')))
        $backend = Container 'backend'
        $helper = (Native @('create', '--pull=never', '--platform', $config.services.backend.platform,
            '--volumes-from', "${backend}:rw", '--entrypoint', 'sh', $config.services.backend.image,
            '-c', 'tar -xzf /tmp/uploads.tar.gz -C /app/data/uploads')) -join ''
        [void](Native @('cp', (Join-Path $Directory 'uploads.tar.gz'), "${helper}:/tmp/uploads.tar.gz"))
        Run-Helper
        Write-Host "Restored into $Project. API/UI are stopped; start and verify this test project explicitly."
    } else {
        if (Test-Path -LiteralPath $Directory) { throw 'Backup directory exists; choose a new path' }
        $images = [ordered]@{}
        foreach ($service in 'postgres', 'backend', 'frontend') {
            $id = Container $service
            $info = (Native @('inspect', $id)) -join "`n" | ConvertFrom-Json
            if (-not $info[0].State.Running) { throw "Backup requires running $service" }
            $images[$service] = $info[0].Image
        }
        [void](New-Item -ItemType Directory -Path $Directory)
        # Stop API/UI writes, retain DB and all volumes. Always resume these services.
        $resume = @('backend', 'frontend')
        [void](Native ($compose + @('stop', 'frontend', 'backend')))
        $db = Container 'postgres'
        [void](Native ($compose + @('exec', '-T', 'postgres', 'sh', '-c',
            'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc -f "$1"', 'sh', $dumpPath)))
        [void](Native @('cp', "${db}:$dumpPath", (Join-Path $Directory 'database.dump')))
        $backend = Container 'backend'
        $helper = (Native @('create', '--pull=never', '--platform', $config.services.backend.platform,
            '--volumes-from', "${backend}:ro", '--entrypoint', 'sh', $images.backend,
            '-c', 'tar -czf /tmp/uploads.tar.gz -C /app/data/uploads .')) -join ''
        Run-Helper
        [void](Native @('cp', "${helper}:/tmp/uploads.tar.gz", (Join-Path $Directory 'uploads.tar.gz')))
        $hashes = [ordered]@{}
        foreach ($file in 'database.dump', 'uploads.tar.gz') {
            $hashes[$file] = (Get-FileHash (Join-Path $Directory $file) -Algorithm SHA256).Hash.ToLowerInvariant()
        }
        $meta = @{ schema_version = 1; created_at = [DateTime]::UtcNow.ToString('o');
            source_project = $Project; images = $images; sha256 = $hashes }
        [IO.File]::WriteAllText((Join-Path $Directory 'backup.json'), ($meta | ConvertTo-Json -Depth 10), [Text.UTF8Encoding]::new($false))
        Write-Host "Backup written to $Directory. Keep it private; verify by restoring to a NEW test project."
    }
} finally {
    try {
        if ($helper) { [void](Native @('rm', '-f', $helper)) }
        if ($db) { [void](Native @('exec', $db, 'rm', '-f', $dumpPath)) }
    } finally {
        try {
            if ($resume.Count) { [void](Native ($compose + @('start') + $resume)) }
        } finally { Pop-Location }
    }
}
