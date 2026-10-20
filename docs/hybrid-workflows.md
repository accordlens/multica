# Hybrid workflow pilot

Project workflows now support a local script status between agent statuses.
The existing task queue, runtime claims, start fences, cancellation watcher,
terminal outbox and queue-expiry sweep execute the entire flow. There is no
second scheduler and no new schema. Existing workflows without scripts retain
their behavior. ACCO-94 leaves migration 609 unused.

The code template is: author commits → script → one repair on an assertion
failure → script on the new commit → independent reviewer → human acceptance.
Routing and budgets use persisted data, with no model calls for orchestration.
The automated regression uses fake authors/reviewers, real Git commits, a real
Node verification command and a disposable PostgreSQL database. It invokes no
provider or installed agent CLI.

## Definition and installation

From the repository root, a human administrator generates and reviews the
definition, then uses the new CLI commands against a test instance:

```sh
node scripts/workflow-template.mjs AUTHOR_AGENT_UUID REVIEWER_AGENT_UUID multica > code-workflow.json
multica workflow create --definition code-workflow.json --output json
multica workflow use PROJECT_UUID WORKFLOW_UUID --output json
multica workflow get WORKFLOW_UUID --output json
```

Replace the UUID placeholders with existing, independently owned roles under
the workspace's invocation permissions. Both agents need an authorized human
originator. An agent's administrator credentials cannot edit/delete a script
workflow, change its project binding, or move the issue to another project.
A human administrator owns policy changes; a new definition hash invalidates
the current execution. Template generation alone changes no platform data.
The `Verify` status must resolve to the `verify` key; if that display name was
previously assigned another key, edit the reviewed definition to use that key.

`script` is optional on an existing `issueworkflow.Step`. It records a script
ID/version, checkout relative to the native workdir, working directory relative
to the checkout, exact executable/argv, required environment, timeout, total
workflow budget, repair limit, required checks and SHA256 of the gate files.
Paths use forward slashes on every platform; spaces are supported. Absolute
paths, Windows drives/UNC paths, traversal and escaping symlinks are rejected.
Commands come only from the saved definition. No conversation text is executed.

The registered code pilot runs `scripts/workflow-checks.mjs`: Go JSON tests for
the workflow engine and process cleanup. It counts actual test pass/fail/skip
events; an empty suite, build-only success or skipped required test cannot pass.
This is the scoped policy for changes to this workflow pilot. A project changing
other areas must register its reviewed checks. The fork's complete
`pnpm local:ci` and publication gate still apply before every PR push.

The runtime must advertise `workflow-scripts-v1`; an older daemon is refused
before it can launch a model for a script task. Script steps keep the author
assignment while running. Their back step is the configured author and their
next step is a different concrete reviewer. Script commands receive a temporary
HOME/cache/temp directory, selected OS/PATH/locale values and the run/report
variables below. They receive no provider keys, Multica tokens, agent custom
environment, connected apps, developer HOME or `.env` files. This is process
isolation and an environment boundary, not an OS sandbox against hostile code.
Use trusted, pinned local check commands with no external mutations.

## Report and evidence

The runtime runs checks in a disposable replica of the committed source, then removes that replica and its temporary HOME/cache. It rejects Git index flags that hide local changes. The runtime creates a fresh report location inside this checkout's Git
directory. The command receives `MULTICA_WORKFLOW_REPORT`,
`MULTICA_WORKFLOW_RUN_ID`, `MULTICA_WORKFLOW_SOURCE_SHA`,
`MULTICA_WORKFLOW_VERSION` and `MULTICA_WORKFLOW_SCRIPT_VERSION`. It must emit:

```json
{
  "run_id": "native-task-uuid",
  "source_sha": "exact-commit-sha",
  "workflow_version": "sha256-of-canonical-stored-steps",
  "script_version": "1",
  "checks": [{ "name": "unit", "passed": 1, "failed": 0, "skipped": 0 }]
}
```

Every required check must exist, have executed tests, have no skipped tests and
have nonnegative counts. Check names are unique. A PASS also requires exit 0,
the expected clean SHA before and after execution, unchanged pinned gate files,
the current workflow/script identity, a fresh run ID and a valid report/log
hash. An assertion report requires a nonzero exit and actual failed checks.
Infrastructure errors, no tests, skipped tests, stale sources/reports, timeout,
cancellation, policy changes and exhausted budgets block rather than repair.
The first pilot requires committed code; dirty code is identified and rejected.

`agent_task_queue.context.hybrid_workflow` freezes the workflow definition,
root/source native run IDs, author/runtime, step/status key, source revision,
script, deadline, repair count and predecessor evidence. Completion stores
`result.workflow_evidence` with timestamps, exit/outcome, source revision,
script/definition hashes, report ID/hash, exact report text and a bounded log.
Report bytes remain a JSON string so PostgreSQL JSONB cannot normalize them and
silently invalidate the artifact hash. Native issue/run/parent IDs are the
integration surface for ACCO-93 linking; parent-child and context links do not
satisfy an execution gate. Model usage stays on agent runs; script elapsed time
comes from its native start/completion timestamps.

The API projects `workflow_execution` and `workflow_evidence` on existing task
responses. `multica issue runs ISSUE --output json` returns the same evidence;
table output labels the step and blocker. Web/desktop Runs details show the
source, script/definition versions, repair budget, report/hash, log and blocker.
Definition editing preserves script fields. A malformed future definition is
refused instead of silently dropping its gate; malformed optional display
evidence does not hide the rest of the run.

## Failure and acceptance rules

The source completion CAS, issue move and single successor INSERT commit in one
transaction. Duplicate callbacks/events therefore reuse a logical transition;
a crash after commit leaves queued work for the existing runtime poller. No
notification is needed for correctness. The daemon's durable terminal outbox
retains callback evidence through restart. A per-run local marker refuses an
uncertain execution replay and reuses a retained finished receipt. Automatic
provider retries and manual single-run reruns are disabled for this workflow.
A human restarts explicitly through the author status after inspecting the
blocker. Commands with external side effects need an additional reviewed
idempotency/partial-result contract; this pilot does not retry them.

The local supervisor owns the command tree. Closing the daemon's lease pipe,
including on abrupt daemon exit, cancels and reaps the command. Unix uses process
groups; Windows starts suspended, joins a kill-on-close Job Object, then resumes.
Timeout and cancellation clean temporary script resources while retaining the
receipt. The existing bounded queue sweep enforces total deadlines even while a
worker is offline. Failure settlement uses a non-waiting issue lock so a
task-first failure cannot deadlock an issue-first completion; contention rolls
the terminal transaction back for its normal callback/sweeper retry.

A repair must produce a new commit, and the repair limit and total deadline are
immutable for the chain. A PASS creates only the independent reviewer, with the
actual report/log and exact SHA. The reviewer ends with `APPROVE SHA`,
`CHANGES_REQUESTED SHA` or `INCOMPLETE SHA`; only a current independent APPROVE
is retained as accepted review. The issue remains in review for a human.
Human `done` requires that review status and the linked PR's exact verified
head. A missing PR, new head, old PASS, author's statement or partial merge
cannot supply this evidence. The PR snapshot is the existing backend source;
refresh it through the existing integration when source delivery changes.

## Reproduce without models

Use an isolated test database with repository migrations applied. Never point
these commands at production. Existing local CI provisions its own database.

```sh
node --test scripts/workflow-checks.test.mjs
cd server
go test ./internal/issueworkflow -count=1
go test ./internal/daemon -run 'TestWorkflowScript|TestWorkflowSupervisor|TestWorkflowDaemonCrash|TestWorkflowEnvironment' -count=1
go test ./internal/service -run '^TestScriptWorkflow' -count=1 -v
go test ./internal/handler -run 'TestScriptWorkflow|TestScriptGate|TestScriptEvidence' -count=1 -v
```

The service pilot intentionally commits a broken answer, runs the real pinned
script, delivers its FAIL to one fake author, commits the correction, verifies
a different SHA, delivers the real PASS to a fake independent reviewer and
records its verdict. Other regressions exercise concurrent completion replays,
no revision progress, repair limits, stale reports, changed policy, offline
workers, cancellation, deadline expiry, path validation and abrupt daemon death.
Native Windows execution remains a separate required environment check;
cross-compilation does not prove cancellation behavior on a Windows PC.

## Small process catalog and recurrence

- **Code:** the project lead owns the change request and scope. Output is a
  committed PR with the scoped script receipt, independent review and human
  acceptance. Exceptions block with the exact missing evidence or failing check.
- **Research:** the research owner supplies the question and source constraints.
  Output is an attributed finding with uncertainty and reproducible evidence.
  Missing access/data is an explicit exception; code gates are not imposed on
  a literature result.
- **Incident:** the incident owner supplies impact and reproduction. Output is
  a verified mitigation, a regression test or reviewed script/template fix, and
  an owner for any unresolved cause. Infrastructure failure does not masquerade
  as an assertion failure or a successful repair.
- **Decision:** the human decision owner supplies the options and constraints.
  Output is a recorded choice with supporting evidence and acceptance. Missing
  authority goes back to that owner; an agent verdict does not accept it.

ACCO-86's 20 October report (`01a15970-e566-770d-b5e6-0a06d551b53a`)
identified unreliable test evidence, interrupted runtimes and incomplete
artifacts. These regressions target those categories. Compare recurrence using
distinct `root_task_id` plus script ID/version and outcome over the report's
14-day window; retries and repair runs are not additional incidents. A repeated
incident leads to a regression or reviewed gate/template change with that
recurrence measure. This change does not alter ACCO-86's schedule or launch an
agent per event, and does not claim incidents can never recur.
