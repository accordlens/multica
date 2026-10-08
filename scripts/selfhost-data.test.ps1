# Real Docker replay with synthetic DB/binary file data; never runs user agents.
#Requires -Version 7.0
[CmdletBinding()]
param([string]$ReleaseEnv = 'release-artifacts/release.env')
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
function D {
    $result = & docker @args
    if ($LASTEXITCODE -ne 0) { throw "Docker test command failed: $($args[0])" }
    return $result
}
function Assert([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
function Http([string]$Url) {
    $deadline = [DateTime]::UtcNow.AddMinutes(3)
    do {
        try { if ((Invoke-WebRequest $Url -TimeoutSec 5).StatusCode -eq 200) { return } } catch { }
        Start-Sleep -Seconds 2
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Not ready: $Url"
}
function Free-Port {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { return $listener.LocalEndpoint.Port } finally { $listener.Stop() }
}
# Validate input before changing the caller's environment.
$release = (Resolve-Path $ReleaseEnv).Path
Push-Location (Split-Path $PSScriptRoot -Parent)
$savedEnvironment = @{}
$composeText = (Get-Content docker-compose.selfhost.yml -Raw) +
    (Get-Content docker-compose.selfhost.release.yml -Raw) + (Get-Content docker-compose.selfhost.windows.yml -Raw)
foreach ($match in [regex]::Matches($composeText, '\$\{([A-Z][A-Z0-9_]*)')) {
    $name = $match.Groups[1].Value
    if (-not $savedEnvironment.ContainsKey($name)) {
        $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
    }
}
$suffix = [Guid]::NewGuid().ToString('N').Substring(0,12)
$source = "acco53-data-$suffix"
$target = "multica-restore-$suffix"
$out = Join-Path (Get-Location) "release-artifacts/data test $suffix"
[void](New-Item -ItemType Directory -Path $out)
$backup = Join-Path $out 'backup with spaces'
$sourceEnv = Join-Path $out 'source.env'
$targetEnv = Join-Path $out 'target.env'
$apiPort = Free-Port
$webPort = Free-Port
$restoreApi = Free-Port
$restoreWeb = Free-Port
Assert (@($apiPort,$webPort,$restoreApi,$restoreWeb | Select-Object -Unique).Count -eq 4) 'Ports must differ'
$composeTail = @('--env-file', $release, '-f', 'docker-compose.selfhost.yml',
    '-f', 'docker-compose.selfhost.release.yml', '-f', 'docker-compose.selfhost.windows.yml')
$a = @('compose','--project-name',$source,'--env-file',$sourceEnv) + $composeTail
$b = @('compose','--project-name',$target,'--env-file',$targetEnv) + $composeTail
try {
    foreach ($pair in @(@($sourceEnv,$apiPort,$webPort),@($targetEnv,$restoreApi,$restoreWeb))) {
        $secret = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
        [IO.File]::WriteAllText($pair[0], "JWT_SECRET=$secret`nPOSTGRES_PASSWORD=$secret`nMULTICA_WINDOWS_API_PORT=$($pair[1])`nMULTICA_WINDOWS_WEB_PORT=$($pair[2])`nALLOW_SIGNUP=false`nDO_NOT_TRACK=true`n", [Text.UTF8Encoding]::new($false))
    }
    [void](D @a up -d --no-build --pull never --wait --wait-timeout 180)
    Http "http://127.0.0.1:$apiPort/health"
    Http "http://127.0.0.1:$apiPort/readyz"
    Http "http://127.0.0.1:$webPort/"
    [void](D @a exec -T postgres psql -U multica -d multica -v ON_ERROR_STOP=1 -c "CREATE TABLE acco53_probe (value text); INSERT INTO acco53_probe VALUES ('before-backup');")
    $binary = Join-Path $out 'probe.bin'
    [IO.File]::WriteAllBytes($binary, [byte[]](0..255))
    $backend = (D @a ps -q backend).Trim()
    [void](D cp $binary "${backend}:/app/data/uploads/probe.bin")
    $dataScript = Join-Path $PSScriptRoot 'selfhost-data.ps1'
    Push-Location $out
    try {
        & $dataScript -Action Backup -Project $source -EnvFile $sourceEnv -ReleaseEnv $release -Directory 'backup with spaces'
    } finally { Pop-Location }
    Assert (Test-Path (Join-Path $backup 'backup.json')) 'Relative backup path ignored PowerShell location'
    # down/up keeps both volumes; API, schema marker and binary attachment survive.
    [void](D @a down)
    [void](D @a up -d --no-build --pull never --wait --wait-timeout 180)
    Http "http://127.0.0.1:$apiPort/readyz"
    $backend = (D @a ps -q backend).Trim()
    $restartedFile = Join-Path $out 'after-recreate.bin'
    [void](D cp "${backend}:/app/data/uploads/probe.bin" $restartedFile)
    Assert ((Get-FileHash $binary).Hash -eq (Get-FileHash $restartedFile).Hash) 'Binary changed after recreate'
    [void](D @a exec -T postgres psql -U multica -d multica -v ON_ERROR_STOP=1 -c "UPDATE acco53_probe SET value='source-after-backup';")
    & ./scripts/selfhost-data.ps1 -Action Restore -Project $target -EnvFile $targetEnv -ReleaseEnv $release -Directory $backup
    [void](D @b up -d --no-build --pull never --wait --wait-timeout 180)
    Http "http://127.0.0.1:$restoreApi/health"
    Http "http://127.0.0.1:$restoreApi/readyz"
    Http "http://127.0.0.1:$restoreWeb/"
    $sourceValue = (D @a exec -T postgres psql -U multica -d multica -Atc 'SELECT value FROM acco53_probe').Trim()
    $restoredValue = (D @b exec -T postgres psql -U multica -d multica -Atc 'SELECT value FROM acco53_probe').Trim()
    Assert ($sourceValue -eq 'source-after-backup' -and $restoredValue -eq 'before-backup') 'Restore touched source / lost snapshot'
    $restoredBackend = (D @b ps -q backend).Trim()
    $restoredFile = Join-Path $out 'restored.bin'
    [void](D cp "${restoredBackend}:/app/data/uploads/probe.bin" $restoredFile)
    Assert ((Get-FileHash $binary).Hash -eq (Get-FileHash $restoredFile).Hash) 'Binary restore mismatch'
    [void](D @b exec -T postgres psql -U multica -d multica -v ON_ERROR_STOP=1 -c "UPDATE acco53_probe SET value='recovery-only';")
    Assert ((D @a exec -T postgres psql -U multica -d multica -Atc 'SELECT value FROM acco53_probe').Trim() -eq 'source-after-backup') 'Recovery mutation touched source'
    $ids = @(D @a ps -q) + @(D @b ps -q)
    $stats = @(D stats --no-stream --format '{{json .}}' @ids)
    $states = @()
    foreach ($id in $ids) {
        $info = ((D inspect $id) -join "`n" | ConvertFrom-Json)[0]
        Assert (-not $info.State.OOMKilled -and $info.RestartCount -eq 0) 'OOM or unexpected restart'
        if ($info.Config.Labels.'com.docker.compose.service' -eq 'postgres') {
            Assert (@($info.HostConfig.PortBindings.PSObject.Properties).Count -eq 0) 'Postgres published a port'
        } else {
            foreach ($port in $info.HostConfig.PortBindings.PSObject.Properties.Value) {
                Assert ($port[0].HostIp -eq '127.0.0.1') 'Raw app port escaped loopback'
            }
        }
        $states += @{ service = $info.Config.Labels.'com.docker.compose.service'; image_id = $info.Image;
            project = $info.Config.Labels.'com.docker.compose.project'; oom = $info.State.OOMKilled; restart_count = $info.RestartCount }
    }
    $rejected = $false
    try { & ./scripts/selfhost-data.ps1 -Action Restore -Project $target -EnvFile $targetEnv -ReleaseEnv $release -Directory $backup } catch { $rejected = $_.Exception.Message -match 'already exists' }
    Assert $rejected 'Must reject an existing restore target'
    $rejected = $false
    try { & ./scripts/selfhost-data.ps1 -Action Restore -Project 'multica-pilot' -EnvFile $sourceEnv -ReleaseEnv $release -Directory $backup } catch { $rejected = $_.Exception.Message -match 'NEW|new multica-restore' }
    Assert $rejected 'Must reject pilot as restore target'
    $mismatchRelease = Join-Path $out 'mismatch.env'
    $releaseText = Get-Content $release -Raw
    $wrongImage = ((D inspect $restoredBackend) -join "`n" | ConvertFrom-Json)[0].Image
    [IO.File]::WriteAllText($mismatchRelease, [regex]::Replace($releaseText, 'MULTICA_RELEASE_WEB_IMAGE=.*', "MULTICA_RELEASE_WEB_IMAGE=$wrongImage"), [Text.UTF8Encoding]::new($false))
    $mismatchTarget = "multica-restore-mismatch-$suffix"
    $rejected = $false
    try { & ./scripts/selfhost-data.ps1 -Action Restore -Project $mismatchTarget -EnvFile $targetEnv -ReleaseEnv $mismatchRelease -Directory $backup } catch { $rejected = $_.Exception.Message -match 'image mismatch' }
    Assert $rejected 'Must reject release mismatch'
    Assert (-not ((D volume ls --format '{{.Name}}') -contains "${mismatchTarget}_pgdata")) 'Mismatched restore created volume'
    $corruptTarget = "multica-restore-corrupt-$suffix"
    $corruptBackup = Join-Path $out 'corrupt backup'
    Copy-Item -LiteralPath $backup -Destination $corruptBackup -Recurse
    [IO.File]::WriteAllBytes((Join-Path $corruptBackup 'database.dump'), [byte[]](1,2,3))
    $rejected = $false
    try { & ./scripts/selfhost-data.ps1 -Action Restore -Project $corruptTarget -EnvFile $targetEnv -ReleaseEnv $release -Directory $corruptBackup } catch { $rejected = $_.Exception.Message -match 'checksum mismatch' }
    Assert $rejected 'Must reject corrupt backup before creating volumes'
    Assert (-not ((D volume ls --format '{{.Name}}') -contains "${corruptTarget}_pgdata")) 'Corrupt restore created volume'
    $report = @{ observed_at = [DateTime]::UtcNow.ToString('o'); powershell = $PSVersionTable.PSVersion.ToString();
        host_os = [Runtime.InteropServices.RuntimeInformation]::OSDescription;
        compose = (D compose version) -join ''; synthetic = $true;
        result = 'PASS: probes, down/up, DB/binary restore, source isolation, existing/pilot/corrupt/image-mismatch refusal, container bindings/OOM';
        binary_sha256 = (Get-FileHash $binary -Algorithm SHA256).Hash.ToLowerInvariant();
        stats = $stats; containers = $states; windows_reboot_lte_operator_acceptance = 'BLOCKED: requires actual PC/operator' }
    [IO.File]::WriteAllText((Join-Path $out 'verification.json'), ($report | ConvertTo-Json -Depth 10), [Text.UTF8Encoding]::new($false))
    Write-Host $report.result
} finally {
    try {
        # Only the two unique projects created by THIS replay are removed.
        [void](D @b down --volumes --remove-orphans)
        [void](D @a down --volumes --remove-orphans)
    } finally {
        Remove-Item -LiteralPath $sourceEnv,$targetEnv -ErrorAction SilentlyContinue
        foreach ($name in $savedEnvironment.Keys) {
            [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
        }
        Pop-Location
    }
}
