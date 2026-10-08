# AccordLens fork: build and Windows pilot

This fork builds backend, web and CLI from **one clean source SHA**. The upstream
reference is v0.6.1; the fork starts at
`e0f84dda47d42f1826af6b8f596de0b38c3d90a6`. Later chat PRs enter a release only
after ClaudeQA approves their exact head SHA and they are merged to fork `main`.
Do not treat a build of the fork base as an accepted chat release.

The pilot uses Windows, Docker Desktop with WSL2 and Linux containers, Compose,
and private HTTPS through Tailscale (network setup in ACCO-53/54). It is available
while the PC and Docker are running. Windows restart/sleep, persistent data,
restore and phone LTE/PWA acceptance must be tested on the actual PC.

## Build from a clean checkout

Install Git, PowerShell 7 (`pwsh`) and Docker Desktop/WSL2; enable Linux containers
and verify `docker version`, `docker compose version`, `docker buildx version`.
Use Compose 2.24 or newer. No registry account, panel, make, host Go or host Node
is required. Docker builds use Go 1.26 (at least 1.26.6 from `server/go.mod`),
Node 22 and repository pnpm 10.28.2. Allow enough disk/RAM for the Next.js and Go
builds; build resources have not been sized for Patryk's PC yet.

In PowerShell 7, replace the SHA with the reviewed release commit:

```powershell
git clone https://github.com/accordlens/multica.git
Set-Location multica
git remote add upstream https://github.com/multica-ai/multica.git
git fetch origin main
git checkout --detach <approved-release-sha>
pwsh -NoProfile -File ./scripts/selfhost-release.ps1 -Platform linux/amd64 -Smoke -ExportImages
if ($LASTEXITCODE -ne 0) { throw 'Release build failed' }
```

The script uses the existing Dockerfiles, exports the Windows/amd64 CLI from
the backend builder, tags app images `accordlens/multica-{backend,web}:sha-<SHA>`
and writes `release-artifacts/` (ignored by Git and Docker build context):

- `manifest.json`: source and freshly fetched main SHA, versions, image IDs,
  Buildx manifest digests, database registry digests, attribution hashes,
  actual tool versions and smoke result.
- `release.env`: immutable local image IDs for all three services; no secrets.
- `cli-windows-amd64/multica.exe`: same version/SHA; recorded SHA256.
- `images.tar`: backend, web and database, with archive SHA256 in the manifest
  when `-ExportImages` is selected.

Docker **image IDs depend on the image store**: classic stores identify configs;
containerd stores may identify manifests. The release records the Docker ID,
Buildx config digest and OCI manifest digest separately. App builds need no push and may have
empty `RepoDigests`; this is recorded honestly. Offline Compose uses image IDs
with `pull_policy: never`, so it cannot silently substitute an official image.
Use the same Docker engine/image-store mode for offline transfer; the load
verification fails if IDs change. Building on the target PC avoids that mismatch.
Keep the complete release bundle to reproduce a selected deployment: rebuilding
the same source against floating upstream base tags is not guaranteed to produce
byte-identical images. To rerun a build, choose a new directory under
`release-artifacts/` with `-OutputDirectory`; evidence is never overwritten.

The smoke creates a unique disposable Compose project with loopback ports,
random test secrets and no signup/telemetry. It waits for `/health`, `/readyz`
and frontend HTTP 200, verifies **running** image IDs against the manifest,
then removes only its own containers and test volumes. It does not run agents
or validate real Windows power management, Tailscale, mobile or user data.

## Load and verify on the Windows PC

Transfer the release bundle through an approved private channel. In a checkout
of the same SHA, copy it to `release-artifacts/` and use PowerShell 7:

```powershell
$m = Get-Content ./release-artifacts/manifest.json -Raw | ConvertFrom-Json
if ((git rev-parse HEAD).Trim() -ne $m.source_sha) { throw 'Checkout differs from release' }
if ((Get-FileHash ./release-artifacts/images.tar).Hash.ToLowerInvariant() -ne $m.archive.sha256) {
    throw 'Archive checksum mismatch'
}
docker load --input ./release-artifacts/images.tar
if ($LASTEXITCODE -ne 0) { throw 'Image load failed' }
foreach ($component in 'backend','web','database') {
    $image = $m.images.$component
    $loaded = docker image inspect $image.image_id | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or "$($loaded[0].Os)/$($loaded[0].Architecture)" -ne $m.platform) {
        throw "Missing/wrong image: $component"
    }
    if ($component -ne 'database' -and $loaded[0].Config.Labels.'org.opencontainers.image.revision' -ne $m.source_sha) {
        throw "Wrong source revision: $component"
    }
}
$cli = './release-artifacts/cli-windows-amd64/multica.exe'
if ((Get-FileHash $cli).Hash.ToLowerInvariant() -ne $m.cli.windows_amd64_sha256) { throw 'CLI checksum mismatch' }
& $cli --version
if ($LASTEXITCODE -ne 0) { throw 'CLI failed' }
```

The CLI output must show `manifest.cli.version` and the full `source_sha`.
Use this CLI for the pilot runtime (ACCO-56), rather than installing an arbitrary
latest upstream binary. Configure its self-host URL/login only after ACCO-53/54
establish the endpoint. CLI/API runtime compatibility still requires the
ACCO-56 end-to-end scenario; matching SHA is the build contract.

## Pilot configuration and launch

Create an empty **test** instance first. The following writes a local, ignored
secret file once; it refuses to overwrite an existing configuration. Replace
the example tailnet URL with the exact HTTPS URL established in ACCO-54.

```powershell
if (Test-Path .env.pilot) { throw '.env.pilot already exists; edit it locally' }
$jwt = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
$dbPassword = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
$pilotUrl = 'https://YOUR-PC.YOUR-TAILNET.ts.net'
$config = @"
JWT_SECRET=$jwt
POSTGRES_PASSWORD=$dbPassword
FRONTEND_ORIGIN=$pilotUrl
MULTICA_APP_URL=$pilotUrl
DO_NOT_TRACK=true
ALLOW_SIGNUP=false
"@
[IO.File]::WriteAllText((Join-Path (Get-Location) '.env.pilot'), $config, [Text.UTF8Encoding]::new($false))
$compose = @('compose','--project-name','multica-pilot',
  '--env-file','.env.pilot','--env-file','release-artifacts/release.env',
  '-f','docker-compose.selfhost.yml','-f','docker-compose.selfhost.release.yml')
docker @compose config --quiet
if ($LASTEXITCODE -ne 0) { throw 'Compose configuration invalid' }
# Run only when the pilot launch is authorized (ACCO-53).
docker @compose up -d --no-build --pull never --wait --wait-timeout 180
if ($LASTEXITCODE -ne 0) { throw 'Pilot start failed' }
Invoke-WebRequest http://127.0.0.1:8080/health
Invoke-WebRequest http://127.0.0.1:8080/readyz
Invoke-WebRequest http://127.0.0.1:3000/
```

Compose lets process environment variables override env files: use a fresh
PowerShell session without deployment variables. `config --quiet` avoids
printing interpolated secrets. Configure an explicit signup/email allow-list
and mail or verification behavior in ACCO-55 before human login testing.
Keep ports bound to `127.0.0.1`; Tailscale/reverse-proxy routing is separate.
Never commit `.env.pilot` or include it in an evidence archive.

Check the running release without printing secrets:

```powershell
foreach ($pair in @(@('backend','backend'),@('frontend','web'),@('postgres','database'))) {
    $id = docker @compose ps -q $pair[0]
    if ($LASTEXITCODE -ne 0 -or -not $id) { throw 'Container missing' }
    $actual = docker inspect --format '{{.Image}}' $id
    if ($LASTEXITCODE -ne 0 -or $actual.Trim() -ne $m.images.($pair[1]).image_id) { throw 'Running release mismatch' }
}
docker @compose ps
docker @compose restart backend frontend
docker @compose stop
# Start again without rebuilding or pulling:
docker @compose up -d --no-build --pull never
```

Routine `stop` and `down` retain named database/upload volumes. Never use
`down --volumes` on the pilot. For updates, take and verify a backup first
(ACCO-58), load an approved bundle, retain the previous bundle and update
`release.env`. Database migrations run at backend startup; reverting images
alone does not undo them. Restore a compatible backup for rollback, following
the tested ACCO-58 runbook. Do not use the smoke's disposable cleanup on the pilot.

## CI, review and upstream maintenance

Existing CI retains frontend lint/typecheck/tests, backend build/vet/tests and
script checks. The new `selfhost` gate builds Linux/amd64, exports Windows CLI,
smokes Compose and retains manifest/CLI evidence for three days. It publishes
no images or releases and deploys nothing. Upstream `release.yml` is gated to
the exact canonical repository, including all jobs that inherit `verify`.
GitHub runner/cache/artifact usage must remain within the authorized free tier;
no paid registry/runner or billing changes are authorized by this runbook.

Small PRs target fork `main`, include the issue key, and require ClaudeQA's
explicit **APPROVE for their head SHA**. Any new commit invalidates that approval.
Merge only that approved head and respect active protections. The fork currently
has no branch protection; an owner may configure required `frontend`, `backend`
and `selfhost` checks plus review/dismissal of stale approvals. This document
does not change repository permissions. A merge gate requires successful
checks, even when the issue handoff happens while CI is still running.

For intentional negative verification, remove one `MULTICA_RELEASE_*_IMAGE`
selection: configuration must fail, and a failed `selfhost-release` must fail
the stable `selfhost` gate. Both behaviors are covered by regression tests:

```powershell
node --test scripts/selfhost-release.test.mjs scripts/ci-scope.test.mjs
```

For upstream updates: `git fetch upstream --tags`, inspect changes against the
last recorded upstream SHA, merge a selected upstream SHA into a separate
sync branch, resolve conflicts, rerun checks, and send a small reviewed PR.
Never force-reset fork `main` to upstream or publish upstream `v*` tags from
this fork. Source builds use `0.6.2-accordlens.<sha12>` plus full-SHA image tags;
record a new manifest after every accepted change. Preserve Multica branding,
`LICENSE` and `NOTICE` in source and images.

If registry publication is separately approved later, push full-SHA tags, record
the resulting repository manifest digests, and put complete
`repository@sha256:<digest>` references into `release.env`. Pull those exact
references before `up --pull never`. The release override replaces the entire
image field for backend/web/database, so it never appends `:MULTICA_IMAGE_TAG`.
Do not substitute an arbitrary local image ID for a remote manifest digest.
See [Docker image references](https://docs.docker.com/reference/compose-file/services/#image)
and [pull policy](https://docs.docker.com/reference/compose-file/services/#pull_policy).

## Acceptance evidence

Record date, OS/Docker/Compose/tool versions, source SHA, image digests/IDs,
CLI version and PASS/FAIL/BLOCKED per scenario. Build/smoke on macOS Linux
containers is valid build evidence, and remains separate from Windows/WSL2,
reviewer replay, restart/sleep/restore, real phones over LTE and the final
integrated chat acceptance. Attach sanitized manifests/logs to the issue;
never publish interpolated Compose config, secret env files or user messages.
