# Windows pilot operations

Use the approved SHA and manifest from [the fork release runbook](SELF_HOSTING_FORK_WINDOWS.md).
Record the actual Windows PC, operator and access method before execution. No panel
or registry is required. Mac/Linux tests do not accept Windows, reboot or LTE/PWA.

## Prerequisites and release

Target: x64 PC, supported Windows 11, virtualization enabled, WSL >= 2.1.5,
Docker Desktop with WSL2/Linux containers, Git, PowerShell 7, Compose >= **2.24.4**
(`!override`). Docker lists 8 GB host RAM as minimum, not measured stack capacity.
Check the actual edition/build against [current Docker requirements](https://docs.docker.com/desktop/setup/install/windows-install/).
ARM Windows needs a separate architecture decision; release CLI is Windows amd64.

In elevated PowerShell, install/update WSL if needed and reboot when requested:

```powershell
wsl --install
wsl --update
wsl --version
wsl --status
```

Expected: WSL >= 2.1.5, default version 2. In **PowerShell 7**, record:

```powershell
$PSVersionTable.PSVersion
Get-CimInstance Win32_OperatingSystem | Select-Object Caption,Version,BuildNumber
Get-CimInstance Win32_ComputerSystem | Select-Object TotalPhysicalMemory
git --version
docker version
docker compose version
docker info --format '{{.OSType}}/{{.Architecture}}'
```

Expected: reachable engine `linux/x86_64`. Select WSL2/Linux containers in Desktop;
leave the unauthenticated TCP Docker API disabled. Deploying offline images needs
no host Go, Node, make or extra Linux distro.

Use a checkout outside OneDrive, e.g. `Set-Location 'C:\Multica\multica'`.
Do not pass `/mnt/c/...` or WSL paths to the Windows CLI. WSL operators use Linux
paths consistently. DB/uploads are named volumes, not NTFS mounts. Binary recovery
uses `docker cp`, never PowerShell text redirection. Check
`git ls-files --eol docker/entrypoint.sh scripts/selfhost-data.ps1` (LF).
Recovery replay includes paths with spaces.

Follow the release runbook's clean checkout/build **or** archive checksum/load.
Expected: image IDs/platform/revision and Windows CLI checksum/version match the
manifest. Keep the complete previous bundle. IDs are immutable but store-dependent;
clean-engine loading needs actual Windows evidence. Never substitute upstream latest.

## Independent test and pilot

Use a fresh `pwsh -NoProfile` with no deployment process variables; they outrank env
files. Do not set global `COMPOSE_FILE`, project, DB or cloud variables. Always use
the three files below, without the development build override/global volume names.
Create independent private configs; these commands refuse overwrite:

```powershell
foreach ($file in '.env.test','.env.pilot') {
    if (Test-Path $file) { throw "$file exists; edit locally" }
    $jwt = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
    $password = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
    $text = (Get-Content .env.windows.example -Raw).Replace('JWT_SECRET=', "JWT_SECRET=$jwt").Replace('POSTGRES_PASSWORD=', "POSTGRES_PASSWORD=$password")
    if ($file -eq '.env.test') {
        $text = $text.Replace('PORT=8080','PORT=18080').Replace('PORT=3000','PORT=13000').Replace('localhost:3000','localhost:13000')
    }
    [IO.File]::WriteAllText((Join-Path (Get-Location) $file), $text, [Text.UTF8Encoding]::new($false))
}
function D {
    & docker @args
    if ($LASTEXITCODE -ne 0) { throw "Docker failed: $($args[0])" }
}
$files = @('--env-file','release-artifacts/release.env',
    '-f','docker-compose.selfhost.yml','-f','docker-compose.selfhost.release.yml',
    '-f','docker-compose.selfhost.windows.yml')
$test = @('compose','--project-name','multica-test','--env-file','.env.test') + $files
$pilot = @('compose','--project-name','multica-pilot','--env-file','.env.pilot') + $files
D @test config --quiet
D @pilot config --quiet
D @test up -d --no-build --pull never --wait --wait-timeout 180
Invoke-WebRequest http://127.0.0.1:18080/health
Invoke-WebRequest http://127.0.0.1:18080/readyz
Invoke-WebRequest http://127.0.0.1:13000/
```

Expected: config exit 0, postgres healthy, three running containers, probes HTTP 200.
`--wait` alone does not check API/UI readiness. Check running IDs using the release
runbook's loop with `$compose = $test`. On failure: `D @test ps -a` and
`D @test logs --tail 100`; redact before attaching. Do not reset Desktop/prune data.

Keep env private. Signup remains off until ACCO-55 supplies allow-list/mail config.
Set pilot `FRONTEND_ORIGIN`/`MULTICA_APP_URL` to the exact HTTPS endpoint from ACCO-54;
leave DB replica, S3 and managed cloud empty. Frontend proxies API/WebSocket traffic
internally; ACCO-54 owns HTTPS/Tailscale routing to loopback UI and LTE/PWA.
Once pilot launch is authorized on the recorded PC, run
`D @pilot up -d --no-build --pull never --wait --wait-timeout 180` (API 8080/UI 3000).
Check `D volume ls --filter label=com.docker.compose.project=multica-test` and the
pilot label. Expected different pairs: `multica-test_pgdata` / `multica-pilot_pgdata`
and `multica-test_backend_uploads` / `multica-pilot_backend_uploads`.
Keep project names stable during upgrades.

## Operator commands and listeners

```powershell
D @pilot ps -a
D @pilot logs --tail 100
D @pilot restart
# Repeat readiness probes after every restart.
D @pilot stop
D @pilot up -d --no-build --pull never --wait --wait-timeout 180
D @pilot down
# Next up recreates containers and reuses the same volumes.
D @pilot up -d --no-build --pull never --wait --wait-timeout 180
foreach ($service in 'postgres','backend','frontend') {
    $id = D @pilot ps -q $service
    D inspect --format '{{json .HostConfig.PortBindings}}' $id
}
Get-NetTCPConnection -State Listen | Where-Object LocalPort -in 5432,8080,3000,18080,13000 |
    Select-Object LocalAddress,LocalPort,OwningProcess
Get-NetFirewallProfile | Select-Object Name,Enabled,DefaultInboundAction
Get-NetFirewallRule -Enabled True -Direction Inbound -Action Allow |
    Get-NetFirewallPortFilter | Where-Object LocalPort -in '5432','8080','3000','18080','13000'
```

Run listener inspection after starting the pilot again. Expected: no published DB,
only `127.0.0.1` API/UI, no wildcard/LAN/tailnet raw listeners from these stacks.
Identify unrelated listeners by PID; do not change unrelated rules. Inspect broad
`Any` and application rules in `wf.msc` too (port query is triage). From a second
LAN device, PC LAN IP DB/API/UI connections must fail. On LTE/Tailscale raw ports
must fail while HTTPS works. ACCO-54 checks router forwarding/UPnP/public exposure.
YAML alone is not firewall evidence; record timestamps and real outcomes.

Never use `down --volumes`, volume prune, factory reset or unregister Docker's WSL
distribution on important data. Cleanup must identify a disposable test project.

## Consistent backup and isolated recovery

Pause external writers/runtimes and concurrent operations. Script stops API/UI,
`pg_dump -Fc`, archives uploads read-only and resumes API/UI in `finally`.
Recheck readiness. Partial backup is invalid: restore requires `backup.json` and
matching SHA256. Scope: local DB/uploads, excluding buckets, credentials, CLI
workdirs and direct DB writers. ACCO-58 owns wider retention/off-PC recovery.

```powershell
$backup = Join-Path (Get-Location) ('selfhost-backups/' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
pwsh -NoProfile -File ./scripts/selfhost-data.ps1 -Action Backup -Project multica-pilot `
    -EnvFile .env.pilot -ReleaseEnv release-artifacts/release.env -Directory $backup
if ($LASTEXITCODE -ne 0) { throw 'Backup failed; do not use it' }
```

Expected: `database.dump`, `uploads.tar.gz`, `backup.json`, SHA256 and running image
IDs. Save matching bundle/private env separately. Backup contains user data; keep
it private and attach only synthetic evidence.

Restore only to a **new** `multica-restore-*` with `.env.test` and same release.
Free test ports first; never use pilot env/ports. Existing destination containers/
volumes, corrupt archive and image mismatch are rejected before writes. One operator
per target; this is not an atomic lock. Failed restore retains test volumes for
diagnosis; choose a fresh target rather than reusing partially restored data.

```powershell
D @test stop
$restoreProject = 'multica-restore-' + (Get-Date -Format 'yyyyMMddHHmmss')
pwsh -NoProfile -File ./scripts/selfhost-data.ps1 -Action Restore -Project $restoreProject `
    -EnvFile .env.test -ReleaseEnv release-artifacts/release.env -Directory $backup
if ($LASTEXITCODE -ne 0) { throw 'Recovery failed' }
$restore = @('compose','--project-name',$restoreProject,'--env-file','.env.test') + $files
D @restore up -d --no-build --pull never --wait --wait-timeout 180
Invoke-WebRequest http://127.0.0.1:18080/readyz
```

Expected: snapshot issue/comment/attachment match; downloaded SHA256 equals original.
Change recovery data, prove pilot unchanged, recheck its readiness/baseline. Do not
activate recovery as pilot or overwrite pilot volumes without a reviewed migration
plan. New images may migrate DB; reverting images does not undo migrations.
Run `pwsh -NoProfile -File ./scripts/selfhost-data.test.ps1` for synthetic Docker replay:
bytes 0..255, down/up, source isolation and rejection cases, cleanup of only its own
projects, no accounts/agents. This does not accept a human workspace.

## Login, reboot, sleep and evidence

Enable **Start Docker Desktop when you sign in to your computer** in
[Docker settings](https://docs.docker.com/desktop/settings-and-maintenance/settings/).
Minimum setup requires that user's login and a ready engine; no pre-login service
availability. `unless-stopped` resumes previously running containers; manually
stopped stay stopped, `down` removes them. After login use recorded pilot `up` if
needed and probe DB/API/UI. No auto-login/scheduled task is required.
Record `powercfg /a` and `powercfg /query SCHEME_CURRENT SUB_SLEEP`. Operator chooses
and records AC sleep policy in Settings. Lock differs from sleep. Sleep/hibernate,
shutdown, updates or Docker stopping interrupt availability; no guaranteed 24/7.
Power changes need operator approval.

In an authorized window create a synthetic workspace/issue/comment/attachment with
ACCO-55 login configuration; record IDs/text and downloaded SHA256. Restart all
containers, down/up, then actually reboot Windows via Start > Restart. After login
verify same data/hash, readiness, HTTPS and LTE/PWA. Test lock, sleep/hibernate and
wake separately; record outages, recovery time/manual `up`. A second operator must
reproduce start/stop/log/restart. Missing tests remain BLOCKED.

Record date, PC alias, OS/WSL/Docker/Compose/PowerShell, source SHA, manifest/digests,
CLI version, commands, expected/actual and sanitized issue attachments. Each has
PASS/FAIL/BLOCKED: start/probes/images, workspace/file persistence, container
recreation, actual reboot, isolation/restore, listeners/firewall/LAN, LTE/PWA,
sleep, operator replay, CPU/RAM/disk and OOM/restarts.
During a timed five-minute synthetic workspace smoke, collect at start/end and load:

```powershell
$ids = @(D @pilot ps -q)
D stats --no-stream @ids
Get-Process -Name '*docker*','vmmem*' -ErrorAction SilentlyContinue |
    Select-Object ProcessName,CPU,WorkingSet64
Get-CimInstance Win32_OperatingSystem | Select-Object FreePhysicalMemory,TotalVisibleMemorySize
Get-Volume | Select-Object DriveLetter,Size,SizeRemaining
D system df
foreach ($id in $ids) {
    D inspect --format '{{.Name}} OOM={{.State.OOMKilled}} restarts={{.RestartCount}}' $id
}
```

Container stats omit Desktop/WSL overhead; save both host/stack data and Docker
data-disk location/free space. Expected: no OOM/restart loop, stable readiness.
Capacity thresholds depend on real measurements. Never attach env/config dump,
credentials, real messages or private backup to the public PR.
