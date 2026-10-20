# Dev/QA contracts — pilot v1.0.0

ACCO-95 provides portable role contracts, an original communication policy, a
bounded checker and an offline pilot. Activation is **false**. This change does
not edit a saved agent, model, runtime, skill, permission or secret. It adds no
scheduler, database migration or production deployment.

## Files and versioning

- `role-template.json`: copyable role template. Every goal needs an observable
  behavior, an anti-example and an evaluation method.
- `dev.json`, `qa.json`: different responsibilities, knowledge, capabilities,
  decision boundaries and evaluable goals. QA does not accept the author's
  replay as independent review.
- `communication.json`: common language and factual-preservation rules.
- `glossary.json`: original PL/EN domain terms, separate from the STE dictionary.
- `permissions.json`: proposed tool policy. The platform ACL grants access.
- `runtime-preview.json`: separate provider/model/binding proposal; null means
  no proposed change. It is not an empty value to write to an existing agent.
- `fixtures.json`: six hand-authored baseline/profile pairs in both languages.
- `rubric.md`: independent assessment and acceptance boundaries.

Contract versions and SHA-256 hashes of the exact UTF-8 file bytes are carried in
messages and run receipts. A hash change with an unchanged version is detected
when an old receipt is checked against the new bundle. Do not edit a released
contract in place: copy it to a new version directory, update the supported
version and tests, then review the diff. Existing reports retain their original
hashes. No runtime-specific syntax belongs in a role file.

The role template is an authoring aid. The pilot supports Dev and QA only.
`loadContracts` validates their goal structure and separation from configuration;
it is not a general JSON Schema implementation.

## Research and permitted source use

Research checked on 2026-10-20. The current official site identifies **Issue 9,
15 January 2025**. STE is a controlled English language for technical
documentation; the project applies selected clarity principles to agent prose
and uses its own Polish adaptation. Neither is an official extension,
certification or claim of full compliance.
[ASD homepage](https://www.asd-ste100.org/),
[current overview](https://asd-ste100.org/about_STE.html),
[current FAQ](https://asd-ste100.org/STE_faq.html).

The official publication identifies ASD copyright and reserved rights. A free
official copy is available through the official site. This repository contains
original policy, examples and project terms, with links to those sources. It
does not redistribute the standard, its dictionary, logos or long extracts.
For a future use that needs reproduction rights, obtain permission from the
rights holder first; free availability is not an open-content license. This is
our conservative source-use policy, not a legal interpretation of an unseen
license. [Official Issue 9 publication](https://www.asd-ste100.org/assets/files/ASD-STE100_ISSUE9.pdf).

ASD says checking tools cannot replace the standard or the writer and does not
certify such tools. We therefore call this an envelope checker and report its
coverage below. [ASD tools guidance](https://asd-ste100.org/STEsoftware.html).

The browser returned HTTP 403 for direct page opens. The statements above were
cross-checked against the search index of these primary pages, including the
official publication's title/copyright page. No full standard was downloaded.
The older `faq.html`/`about.html` pages still describe Issue 8; use the current
`STE_faq.html`/`about_STE.html` pages. Recheck the official site before a future
claim of standards compliance.

## Commands

Node 22 or newer; standard library only, no install or provider credentials:

```sh
node --test scripts/role-contracts.test.mjs
node scripts/role-contracts.mjs preview all > ../acco-95-profiles.json
node scripts/role-contracts.mjs pilot ../context.json > ../acco-95-pilot.json
node scripts/role-contracts.mjs check ../handoff.json --context ../context.json
node scripts/role-contracts.mjs check ../revised.json --context ../context.json --before ../original.json
```

`preview dev` and `preview qa` render the exact proposed instructions and show
tool policy and runtime parameters separately. These commands print JSON; they
do not apply configuration or call an agent. Keep generated reports outside the
source tree so that their own bytes do not make the measured revision dirty.

Supply a context copied from authorized API/CLI evidence, for example:

```json
{
  "issue_id": "<existing issue.id UUID>",
  "task_id": "<existing run.id UUID>",
  "workflow_id": null,
  "workflow_definition_sha256": null,
  "workflow_unknown_reason": "The run export did not include a workflow definition.",
  "status_key": "in_progress",
  "status_source": "issue.status",
  "source_revision": "<exact lowercase 40-character Git HEAD>",
  "source_dirty": false
}
```

The placeholders above are intentionally invalid. Do not generate new UUIDs to
make them pass. Read the issue and runs using `multica issue get <issue-id>` and
`multica issue runs <issue-id>`, both with `--output json`. Read Git HEAD and
worktree status separately. A passing result rejects dirty source; finish and
commit the code first. The checker compares the message to the separately
supplied context. It cannot authenticate a fabricated input file or prove a test
really ran.

The pilot emits a complete message example in `steps[0].message`, a QA fixture
and a Polish report, plus 24 comparison rows: six cases × two languages × two
variants. It never starts another run or moves an issue. The QA fixture's task
ID is null and independent review remains pending. A failing envelope keeps its
original critical content and produces a failing CLI exit code.

## Handoff format and checker boundary

Each v1 message has a `message_type`, language, context, contract snapshots,
objective, one result status, claims, sources, uncertainty, expected response,
three prose sections and a style revision. Stable statuses are `pass`, `fail`,
`blocked`, `unknown`, `needs_changes`; they describe this result's scope, not an
automatic change to the issue status. Types cover handoff, result, blocker,
question, review, changed requirements and conflicting evidence. JSON field
names, UUIDs, codes, commands, quotes, hashes and numbers are not translated.

The checker rejects missing/unknown fields, unsupported contract versions/hashes,
stale source revisions, mismatched context, missing source references,
unlisted uncertainty, conflicting structured PASS/evidence, and an omitted
critical claim. Facts need source IDs. Every hypothesis or unknown appears in
the uncertainty list. A QA review must request human acceptance or a fix; it
does not execute either action.

A style revision requires the original message and may change only `prose` and
the revision counter. Immutable claims preserve error meaning and certainty.
Critical claim text must appear **verbatim** in the evidence section. Sensitive
tokens in each prose field, including inline code, IDs, numbers, dates, selected
negations and certainty words, must keep their sequence. At most one revision
is allowed. No automatic rewrite or retry exists.

This token guard is a heuristic. It does not understand negation scope, all
Polish inflections, unsupported claims, grammar, source truth or language
identity. Exact critical claims and independent semantic review are required.
The tests include an unsupported production claim that the checker accepts;
this is a documented semantic false negative. Sentence length and selected
ambiguous phrases produce warnings only. The valid fixture name “All Done”
produces a known false positive. These examples do not estimate real-world
precision or recall. A critical blocker is always present in `critical_delivery`
even when the checker exits 1; deliver it without an automatic style loop.

## Existing provenance and UI/API surfaces

Mapping audited against pinned upstream
`47120a4131564175da044d63d897ed9b5d76979a`; ACCO-128 owns integration:

- Work item: `issue.id`. Run/task: `agent_task_queue.id`, returned as `id` by
  issue-runs. `task_id` in our receipt references that row, not a parallel ID.
- Step: `agent_task_queue.workflow_step` when available; claim `issue_status`
  or issue `status` are observations with a different scope. `status_source`
  names which was read. A current issue status is not proof of a historical
  run step. Upstream migration 578 makes null workflow_step mean unchecked.
- Definition: existing `issue_workflow.id` and the SHA-256 of the exact retained
  exported definition bytes. Upstream has `steps` and `updated_at`, not an
  immutable numeric definition version. The contract digest is our artifact
  revision, not a new workflow execution ID. If the definition is unavailable,
  retain nulls and the reason. The claim's `project_workflow.brief` alone does
  not supply a definition ID or version.
- Source: exact Git SHA and dirty flag. Report and artifact: content hashes;
  use a platform attachment ID only after upload returns it. ACCO-93 owns
  durable linking; ACCO-94 owns authoritative script execution evidence.

For this opt-in pilot, store a compact receipt under issue metadata key
`role_contracts_v1.<task-id>.<source-sha-prefix8>` (returned as `metadata_key`).
The full source SHA stays inside the receipt; a different value at the same
key is a conflict to resolve, not an overwrite. The existing metadata surface accepts a **string**
containing JSON, not a nested object. It has an 8KB aggregate limit. Keep full
reports in attachments, do not evict other metadata or silently overwrite a
different receipt, and retain prior run receipts. This is an index of submitted
pilot artifacts, not an immutable audit log or attestation that persistent
runtime instructions were activated.

```sh
multica issue runs <issue-id> --output json > ../runs.json
node scripts/role-contracts.mjs receipt ../runs.json ../acco-95-pilot.json ../acco-95-profiles.json > ../receipt.json
```

The receipt command checks the returned run/issue/agent/runtime identity, current
contract hashes and exact inactive previews. It hashes the retained report and
artifact bytes and prints the value to store through the supported CLI metadata
command. It does not upload, invoke, mutate the issue, or replace run context.
After upload, fill the returned attachment IDs and read the metadata back.

API/CLI: `multica issue metadata get <issue-id> --key <receipt-key> --output json`
or `multica issue get <issue-id> --output json`. UI: open the issue, select
**Metadata**, find the key for the run, and follow the report/profile attachments
in the result comment. The existing dialog renders primitive string values as
JSON strings; it does not parse a dedicated profile card. Relevant code:
`server/internal/handler/issue_metadata.go`,
`packages/views/issues/components/issue-detail.tsx`,
`packages/core/api/schemas.ts`. No new endpoint or UI promise is implied.

## Gates, activation and rollback

1. `Agent contracts / contracts` runs the deterministic tests on matching PRs
   and main changes; local command is shown above. This checks the profiles and
   fixture pilot, not application tests. It grants no merge authority.
2. Run applicable repository tests for code changes. ACCO-94 integrates their
   authoritative report with the existing agent → script → review path. A
   persona instruction or this envelope checker cannot replace that gate.
3. An independent reviewer uses `rubric.md`, reruns the key case and reviews the
   exact head. A new head needs new review. Synthetic QA cannot issue APPROVE.
4. Preserve final human review: Patryk for technology, Malar for business; mixed
   scope requires the recorded applicable acceptances. On this sprint, merge
   also requires ClaudeQA APPROVE of the exact head and required checks.
5. Persistent profile activation is a separate decision after the concrete
   `preview all` artifact is reviewed. The sprint does not authorize it. Before
   activation, retain the exact previous instructions and non-secret binding
   references through the supported configuration path. Start with one approved
   Dev/QA pair and record the version at each run. Leave credentials in their
   existing secret store.
6. To roll back an approved activation, restore that prior configuration
   snapshot through the same supported path and verify it by readback. Retain
   old run receipts, reports and review history. Rollback of this inactive PR
   itself is an ordinary Git revert; there are no migrations to undo.

Final runtime/script/linking integration follows the verified main after
ACCO-128, without a second upstream merge. This bundle supplies profiles and a
checker to that existing process; it does not extend ACCO-93/94's shared model.
