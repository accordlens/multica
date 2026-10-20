# Execution dependencies and source links

Issues share the native project workflow, `agent_task_queue` runs and daemon
claim/start protocol. A dependency filters admission of an already queued run.
There is no additional scheduler, model call, webhook requirement or new run
created when an input becomes ready.

## Ownership and existing model

ACCO-93 owns relations, admission and source linking. ACCO-94 owns configured
script execution, required checks, repair limits and review handoffs. Its
`context.hybrid_workflow` snapshot and `result.workflow_evidence` are preserved
and exposed by the source API. A completed run is a delivery fact; successful
tests and review still require ACCO-94's validated evidence for the current
source SHA, definition and required checks.

The existing surfaces remain authoritative:

- `issue.parent_issue_id` and `stage` describe hierarchy and parent barrier
  notifications. They do not create execution dependencies.
- `issue_dependency` stores context and execution relations. New execution rows
  use `blocked_by`: `issue_id` depends on `depends_on_issue_id`. Legacy `blocks`
  rows retain their original direction and are read from both endpoints.
- `issue_workflow`, its status keys and the existing handoff routes define the
  process. Claim, lost-response reclaim and start all check dependencies.
- Native task IDs identify runs. New issue-bound runs snapshot the workflow ID,
  stored definition and immutable entry status in `context`, including wakeups,
  retries and late binding of quick-create runs. Updating a workflow or the
  mutable `workflow_step` guard does not rewrite this source snapshot.
- Existing GitHub/VCS PR mirrors, link tables, exclusions, head snapshots and
  multi-issue backlinks retain their lifecycle rules. Native attachments keep
  their issue/comment/run ownership and download authorization.
- Usage and attribution remain on native issues and runs. The source API
  exposes run IDs for correlation with existing usage APIs. It adds no billing
  or routing engine. Agents use the same authorized CLI/API; no separate MCP
  relation store is introduced.

## Conditions and transitions

Every applicable execution condition must hold. `target_step` limits an edge
to one dependent workflow status; an empty value applies at every step. Both
the queued step and the issue's current step are checked at admission.

- `done`: the prerequisite currently has lifecycle category `done`.
- `in_review`: the prerequisite has delivered its result at `in_review`, or
  reached `done`. `source_step` can name another explicit delivery status.
- `accepted`: a human accepts the delivered prerequisite's exact issue
  revision **and evidence version**. The evidence version is a SHA256 of its
  recorded native workflow evidence, run provenance and linked PR head SHAs.
  New code/report evidence invalidates acceptance even when `issue.revision` has not changed.
  The request must carry the version the person inspected; stale clicks fail
  with 409. Acceptance history is retained. `source_step` can select a custom
  delivery status.
- `run_completed`: one exact native run of the prerequisite completed. An
  optional `source_step` must match its immutable entry snapshot. This can
  release configuration after an infrastructure result while the parent issue
  remains in progress. It does not establish passing tests or human acceptance.

Status-based conditions are reevaluated after reopen or cancellation.
Cancellation does not satisfy `done`. A specifically selected completed result
remains a historical delivery fact across later parent status changes. Use
revision-bound acceptance and the script gate for code acceptance.

Done/cancelled dependent issues with execution dependencies do not start new
runs. Parking and cancellation remain possible. Delivery/terminal transitions
cannot skip unmet inputs, including PR merge automation. Partial merges cannot
satisfy explicit human acceptance. Changing execution edges while a dependent
run is dispatched, running or waiting for a local directory returns 409; finish
or cancel that run first. Reopening an input does not kill an already started
process, but start and later gated transitions reevaluate the stored facts.

Execution configuration and acceptance require human credentials. Agents can
read the graph, suggest changes, maintain context and report sources. A repair
or analyzer run cannot remove its required gate. All relation endpoints check
workspace membership; links grant no additional access.

Graph writers take a transaction-level advisory lock per workspace before cycle
validation and insertion. Claims take the shared advisory lock before agent row
locks. This serializes opposing edge writes and fences claim versus graph edit
without reversing workspace teardown's owner-row lock order. Self-links and
cycles return a chain of issue keys. Context links may form networks freely.

## Automatic links and provenance

Creating or updating an issue description synchronizes exact, existing issue
keys from the selected workspace and explicit issue UUID URLs into context
links. It resolves up to 100 distinct keys and 100 URL references per sync.
Titles and semantic similarity never create links or execution edges. A
description write commits before this optional sync; interrupted sync can be
replayed with `multica issue relation sync`. Existing context and removal
tombstones are preserved, including when a description is edited later.

`multica issue link` records exact commit SHAs, HTTPS PR URLs and native artifact
IDs plus SHA256 under a known run's `context.provenance_links`. The server stamps
run/step, actor, source and time. Agents may report only for their own run;
humans may associate a started/terminal accessible run with its owning issue.
Repeated reports reuse a deterministic key, including after an acknowledgement is lost. Removal
retains a tombstone and does not silently reappear on replay. A new SHA is a new
record, preserving revision history.

A known mirrored PR also receives the existing issue/PR link, respecting prior
manual exclusion. A URL reported before mirroring is immediately visible as a
source pointer on a private instance. Replaying the same report after mirroring
attaches the native backlink. This operation does not create PRs, poll GitHub,
infer successful checks or complete the issue. Correct a native PR association
with the existing PR unlink control; a recorded run pointer has its own removal
history.

The source API derives commit pointers from ACCO-94’s `workflow_evidence.run_id`,
`source.sha`, `source.dirty_hash` and completion time, requiring the native run
ID to match. Dirty sources remain visibly marked. A source correction suppresses
the pointer without rewriting the immutable report.

The source API projects native run evidence and authorized issue/comment
artifacts automatically. Private chat and source-context snapshot attachments
are excluded. Stored artifact pointers are rechecked for current ownership on
read. Supplied artifact hashes describe the reporter's source; authoritative
script report validation belongs to the configured runner. A pointer or an
agent's statement cannot establish PASS. Unreported external Git changes cannot
be discovered from database links; the script gate checks the actual checkout.

## API and CLI

`GET /api/issues/{id}/relations` returns reciprocal relations, current readiness,
waiting reasons, provenance and source approval versions. `include_removed=true`
includes history. POST adds a relation; DELETE removes it idempotently. POST
`/relations/{relationID}/accept` requires `revision` and `evidence_version`.
POST `/relations/sync` replays explicit description linking.

`GET /api/issues/{id}/links` returns recorded links, native runs/evidence and
authorized artifacts. Pages contain at most 100 runs and 100 artifacts;
`has_more` and `offset` support traversal. A run stores at most 100 reported
pointers. POST records a source; POST `/links/{linkID}/remove` carries `run_id`
and retains removal history. Source/report values remain bounded by the native
runner and request limits.

```sh
multica issue relation add C A --condition done
multica issue relation add C B --condition accepted --step in_review
multica issue relation list C --output json
# Use the source revision and evidence version returned by that read:
multica issue relation accept C RELATION_UUID --revision 7 --evidence-version SOURCE_SHA256
multica issue relation add C A --type related
multica issue relation sync C
multica issue link add C --run-id RUN_UUID --kind commit --commit-sha FULL_COMMIT_SHA
multica issue link add C --run-id RUN_UUID --kind pull_request --url https://github.com/owner/repo/pull/1
multica issue link list C --history --output json
```

The shared web/desktop issue view displays these same conditions, waiting
reasons and reciprocal links. Mutations wait for server acknowledgement.
Malformed responses never display permission to run. Issue changes, native run
completion, PR updates and WebSocket reconnect invalidate the workspace's relation/source
projections.

## Migration, verification and guarantees

Migration 608 extends `issue_dependency`, adds read/admission functions and
snapshots workflow identity on native run insertion/first issue binding. It adds
no tables, foreign keys or indexes. Existing indexes cover both endpoints and
native issue/run lookup. Legacy rows are preserved; their new `created_at`
is the migration observation time, identified by `provenance.source=legacy`.
Rollback removes the added relation columns and admission/snapshot functions;
native run contexts/results remain stored. Deploy the matching server version
with schema rollback. No production migration is part of this PR.

Run `pnpm local:ci` on a clean committed checkout for the complete fork gates.
The regression suite covers concurrent cycles and claims, AND readiness,
reopen, human acceptance/new SHA, explicit context dedup/correction, native
run delivery without parent completion, and the infrastructure → HTTPS/auth →
final integration scenario. The browser scenario is
`e2e/issue-relations.spec.ts`. Script FAIL → one repair → current-revision PASS
→ independent review is the ACCO-94 integration scenario, not a claimed outcome
of a context link or completed-run condition.

Database rows and existing queue CAS/dedup keep logical relations, reports and
claims idempotent across replay/reconnect. A manual rerun is new intent. External
commands and provider-side PR creation cannot be made exactly-once by a database
link: use stable external operation IDs/read-before-create and the script
runner's declared retry/partial-execution policy.
