# Verify dependencies and script workflows

Dependencies and script steps extend the existing issue workflow and task queue.
The contracts are in [issue-dependencies.md](issue-dependencies.md) and
[hybrid-workflows.md](hybrid-workflows.md). Tests use synthetic workspaces,
fixture actors, local Git repositories and Node scripts. They never invoke a
model. A fixture reviewer verifies routing; it is not independent QA acceptance.

## Small checks without services

From the repository root, run the acceptance receipt validator tests:

```sh
node --test scripts/workflow-acceptance.test.mjs
node scripts/workflow-acceptance.mjs --plan
```

`--plan` prints the exact named test selection without opening a database.
Native compile checks can verify that the integration fixtures build without
running package setup or contacting PostgreSQL:

```sh
cd server
GOMAXPROCS=2 go test -p 1 -c -o /path/to/evidence/workflow-service.test ./internal/service
GOMAXPROCS=2 go test -p 1 -c -o /path/to/evidence/workflow-handler.test ./internal/handler
```

Choose an evidence directory outside the source tree. In PowerShell set
`$env:GOMAXPROCS='2'` before the commands instead of the shell prefix above.
Compilation is not database acceptance. The offline transport scenarios also
do not prove PostgreSQL predicates, row locks, rollback or ACLs.

## PostgreSQL acceptance

Use the checkout's managed, migrated disposable test database, with its
`DATABASE_URL` already exported by the test environment. See
[CONTRIBUTING.md](../CONTRIBUTING.md) and [local-ci.md](local-ci.md).
Do not use an application or production database. Commit the tests and run:

```sh
node scripts/workflow-acceptance.mjs --out ../workflow-db-evidence
```

The runner requires an explicit database URL and clean source revision. It
runs one package at a time with one Go build worker and `GOMAXPROCS=2`.
`workflow-database.json` records the exact source SHA, commands, timestamps,
results and SHA256 of each retained, URL-redacted Go JSON log. The joint test
log also includes native issue/workflow/run IDs, workflow and script versions,
failure and repaired Git SHAs, report IDs/hashes and the routed reviewer.

Every required top-level test must pass, the package must finish, and no test
or subtest may skip or fail. The service suite's unavailable-database skip and
the handler suite's early successful `TestMain` exit therefore both produce a
failed acceptance receipt. A stale filter selecting zero tests also fails.
After failure, subsequent packages remain unexecuted; do not describe them as
passed. Tests use the existing fixture cleanup; inspect failed cleanup and
retain receipts before removing only resources owned by that attempt.

| Behavior | Canonical coverage |
| --- | --- |
| A → B → C; cancellation is not done | `TestIssueDependencyLinearChainAndCancelledInput` |
| A/B → C; concurrent claims, reopen before start | `TestIssueDependencyANDGateClaimsOnceAndReopens` |
| Self/cycle rejection under concurrent graph writes; context links; replay | `TestIssueDependencyConcurrentCycleAndReplay` |
| Real PostgreSQL AND admission → script FAIL → one repair → new SHA PASS → other reviewer | `TestScriptWorkflowDatabaseDependenciesAndRevisionEvidence` |
| Thirty concurrent/replayed completion callbacks produce five logical runs | Same joint test; each stage requires exactly one completion CAS winner |
| A prerequisite reopens or is cancelled during a passing script | `TestScriptWorkflowDatabaseReopenPreservesCompletion`: completion and full evidence commit, issue remains in verify, no review run; replay after readiness returns remains a no-op |
| A successor write fails after the issue moves | `TestScriptWorkflowDatabaseSuccessorFailureRollsBack`: injected write error through a real pgx transaction; completion, issue move and successor roll back; retry creates one successor |
| Stale source/report SHA, skipped/empty checks or changed definition version | `TestScriptWorkflowDatabaseRejectsStaleEvidence`: durable terminal result, visible blocker and no review successor |
| Human acceptance, new evidence SHA, reopen and removal history | `TestIssueDependencyHumanAcceptanceAndRemovalHistory` |
| Partial merge and infrastructure → HTTPS/auth → final human integration gate | `TestIssueDependencyInfrastructureIntegrationHasNoCycleOrImplicitAcceptance` |
| Reciprocal PR/commit/artifact provenance, correction/replay and protected/deleted-chat ACL | The selected `TestIssueProvenance*` and tenant-isolation tests |
| An agent cannot replace the frozen gate, skip to done or accept another task | The selected handler `TestScriptWorkflow*` and `TestScriptGateDefinition*` tests |

Database tests call the real service and handler boundaries. Fixture SQL only
sets up inputs or changes prerequisite state. The rollback scenario injects a
specific successor-write error; it does not claim a disk outage or a database
server crash. External commands and PR creation retain their own idempotency
limits, as documented in the contracts.

## Browser projection and recordings

After starting the isolated app through the existing test environment, run:

```sh
pnpm exec playwright test --config playwright.workflow.config.ts
```

This selects the dependency and stored-script-evidence specs, with one worker,
no retries, screenshots, traces and video for successful and failed flows. The
tests compare API readiness with the rendered state, preserve reciprocal links,
deduplicate relation replay, verify both required inputs, and revisit the page
after cancellation. The evidence view checks the stored SHA, report hash and
repair blocker. It is a seeded projection test, not an execution of the complete
agent/script workflow through HTTP.

`both-inputs-waiting.png`, `both-inputs-satisfied.png` and
`cancelled-input-after-reload.png` show checkpoints within one candidate.
They are not a before/after code comparison. Capture the base and candidate app
separately at the same issue-detail checkpoint, using the same synthetic issue
titles/states, locale and 1440×1000 viewport. Record both app SHAs, browser and
tool versions, fixture IDs, capture times and artifact SHA256 values beside the
PNGs and video. Do not present a missing baseline capture as completed.

## Delivery gates

The focused runner supplements `pnpm local:ci`; it does not replace any of its
14 gates, the normal pre-push hook or PR status publication. After the complete
exact-SHA run, use the normal push, open/update a real PR, publish with
`pnpm local:ci:publish <PR>`, and obtain independent review of the current head.
The full CI already includes daemon timeout/cancellation, lease-loss/restart,
script path and pinned-file regressions. Native Windows execution and actual
device/browser acceptance remain separate evidence. Run all current acceptance
criteria and retain real visual artifacts before requesting final human review.
