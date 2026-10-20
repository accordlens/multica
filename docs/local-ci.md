## Local verification before pushing

Fork verification runs on a developer machine, without automatic GitHub Actions
runners. Install Node (see `package.json`), the pinned pnpm, Go (see
`server/go.mod`), Git, Docker with Compose/buildx, Helm, and PowerShell 7 (`pwsh`).
Use macOS/Linux, or WSL2 with Docker integration on Windows. GitHub publication
also needs an authenticated `gh` with permission to comment and write commit
statuses on the repository. No `gh` extension is required.

```sh
pnpm install --frozen-lockfile  # also installs this checkout's Git hooks
git add <files>
git commit -m "fix: describe the change"
pnpm local:ci
git push
pnpm local:ci:publish 6        # existing PR number or branch
```

For an existing PR, `pnpm push` combines the last two commands. Its optional
argument is the remote branch name. On the first push, create the PR before
running `pnpm local:ci:publish <number>`. Git and `gh` authentication are used
only for GitHub operations; tests receive no runtime/provider credentials or
development `.env` files.

`pnpm local:ci` requires a clean committed checkout. It makes a temporary clone
of that exact commit, installs the frozen lockfile, provisions disposable
PostgreSQL and Redis containers on random loopback ports, and executes:

- Script, installer, Helm and UI policy regressions; image budget against main
  and generated reserved slugs.
- Frontend typecheck, lint, unit tests and builds, including docs and desktop.
- Mobile typecheck, lint and unit tests, separately from root frontend tasks.
- SQLC regeneration with a drift check; Go race build and tagged integration vet.
- All default Go tests with race detection and fresh results, serial within each
  package to keep subprocess deadlines reliable on local machines.
- Go vulnerability scanning against the current vulnerability database.
- Playwright against app processes and synthetic data owned by this run.
- Linux amd64 self-host images, Windows CLI export, HTTP smoke, backup/recovery.

Every gate is mandatory; a missing tool, skipped gate or failed cleanup is a
failure. Real-provider smoke tests are never enabled. Native Windows/macOS
matrix checks, desktop packaging and performance benchmarks remain explicitly
separate; a local PASS does not claim those ran. Existing manual Actions
workflows remain available for targeted diagnostics. The guarded upstream
release workflow still cannot publish from this fork.

The runner removes only its own processes, containers and temporary checkout.
Reports and individual logs remain in the checkout's Git directory under
`local-ci/`. The pre-push hook only reads the latest report: all gates must have
passed for the exact commit being pushed, with no staged, unstaged or untracked
changes. It does not rerun tests. Commits, amendments, rebases and failed retries
invalidate old evidence. Multiple pushed refs must all resolve to the verified
HEAD; deletions need no evidence. There is no time-based expiry.

Only one runner may use a checkout at a time. Interruptions revoke the receipt
and clean up owned resources. After an uncatchable process termination, inspect
the checkout's Git directory `local-ci/running.lock` and remove that directory
only after confirming its runner has stopped; then rerun the complete pipeline.

Publication verifies the live PR head, updates one marked summary comment and
writes `local-ci/<gate>` plus aggregate `local-ci` commit statuses linked to that
comment. Failed reports can also be published. Raw logs, database addresses,
environment variables and credentials are never uploaded. A publication error
returns failure and can be retried without rerunning tests. A new head needs its
own verification and independent review. Branch protections are not changed.

Git hooks are a local workflow guard and can be bypassed by someone controlling
their checkout. They are not independent or tamper-proof attestations. Install
them in each checkout (`pnpm hooks:install`); existing unrelated custom hooks
are preserved by refusing replacement. Multica's commit attribution hook is
forwarded, and linked worktrees keep separate configuration and receipts.
