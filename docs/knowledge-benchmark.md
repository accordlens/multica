# B0 knowledge benchmark harness (ACCO-131)

This is evaluation infrastructure for the existing Multica search, not the
knowledge pilot or proof of answer quality. No model, Cognee, daemon, event bus,
agent, autopilot, scheduler, or production service is started. Corpus text is
inserted as inert data, never submitted through issue/comment mutation handlers.
There is no schema change or new dependency.

## Evidence levels

- `node --test scripts/kb-bench.test.mjs`: independent mini-fixtures exercise
  projection, scorer and freeze/holdout guards. Mock responses are explicitly
  `mock_backend`; they cannot qualify a Cognee evaluation.
- The opt-in integration test uses real PostgreSQL, the production
  `RequireWorkspaceMember` middleware and `SearchIssues` handler in process.
  It exercises membership, SQL ranking, revision/tombstone import and cleanup
  on a separate mini-corpus. This is infrastructure evidence only.
- An authored-corpus `run` requires an independently reviewed, adjudicated freeze.
  Record the resulting run ID, clean code SHA, binary hash, input hashes, config,
  raw responses, query trace and report. Only such a **dev** run supplies the
  required B0 retrieval evidence. The initial harness PR contains no such result.

## Before freeze: parser and infrastructure only

Download only sources, original questions and the protocol through
`multica attachment download`. Keep all files outside the Git checkout. Do not
download author-draft labels or test labels for development. The runner defaults
to `dev`; its projection excludes all test prompts. `prepare` reads no labels,
touches no database and computes no quality score:

```sh
node scripts/kb-bench.mjs prepare \
  --sources ../benchmark-input/sources.json \
  --questions ../benchmark-input/questions.json \
  --out ../benchmark-input/dev-plan.json

node --test scripts/kb-bench.test.mjs
(cd server && go test ./cmd/kbbench && go vet ./cmd/kbbench)
```

Do not change ranking, extraction, query text or thresholds based on the authored
corpus before independent review. Queries are sent verbatim to the existing
endpoint. No stopword list, translation, query rewriting or decision heuristic
is introduced by this harness.

## Isolated backend

Use the checkout's managed environment and generated `.env.worktree`, following
`CONTRIBUTING.md`. It must be a **dedicated empty benchmark database**, named
`multica_*kbbench*`, on loopback. Generate with
`WORKTREE_NAME=acco131_kbbench make worktree-env`, then apply the existing migrations
using that environment. Never copy the main checkout's `.env`, connect to the
active workspace database, or use the Multica task runtime's platform API token.
Resolve local port collisions before migrating; do not alter another instance.

After verifying this environment, build and run the mini-fixture integration:

```sh
(cd server && go build -o bin/kbbench ./cmd/kbbench)
set -a
. ./.env.worktree
set +a
export KBBENCH_DATABASE_URL="$DATABASE_URL"
export KBBENCH_DATABASE_NAME="$POSTGRES_DB"
export KBBENCH_RUN_INTEGRATION=1
node --test scripts/kb-bench.integration.test.mjs
(cd server && go test ./cmd/kbbench)
```

The helper reads only `KBBENCH_DATABASE_URL` and the explicit matching
`KBBENCH_DATABASE_NAME`. It refuses populated databases and remote/fallback hosts,
takes a database advisory lock, creates random fixture IDs, and writes a cleanup
manifest **before** inserting. It has no network listener. Synthetic agent names
are projected to inert member identities with the original workspace memberships;
real agent task-token authentication and transport authentication are **not tested**.
The middleware still decides every selected workspace read and cross-workspace probe.
Workspace/user ownership IDs are random; issue/comment UUIDs and issue timestamps
are deterministic so SQL relevance tie-breaks do not depend on import timing.

## Freeze contract and split labels

The coordinator retains original independent labels and adjudication evidence.
If the reviewer format differs, normalize without changing meanings; review and
freeze the normalized files too. This interface is deliberately explicit, rather
than guessing a citation or semantic judgment from a string. Dev and test labels
are separate physical files. A dev labels file containing test records is rejected.

Normalized labels have this shape (independent example only):

```json
{
  "schema_version": 1, "split": "dev",
  "labels": [{
    "question_id": "independent-dev", "state": "answered",
    "must_say": ["The current plan uses oranges"],
    "must_not": ["The removed draft is current"],
    "critical": ["A blocker must be stated"],
    "must_cite": [{
      "source_id": "comment-id", "revision": 2,
      "content_sha256": "<SHA256 of exact UTF-8 source text>",
      "source_sha": "<optional full 40-character code SHA for test evidence>"
    }]
  }]
}
```

States are `answered`, `unknown`, `conflict`, `denied`. Canonical references are
`<issue-id>.description` and original comment IDs; each carries its exact revision
and content hash. Run/branch/status/assignee evidence is not silently converted
into indexed prose. Expected citations to these currently unsupported source kinds
will be retrieval misses, not invented successes; inspect this limitation before
attributing a failed threshold to lexical ranking.
When the rubric permits equivalent evidence, add `alternative_citation_sets`:
an array of nonempty arrays in the same citation format. Each array is a complete
acceptable evidence bundle. Retrieval passes if one complete bundle is present;
alternatives must be independently reviewed and included in the frozen label hash.

Scenario config is a JSON object with `events_by_question`, mapping question IDs
to explicit source event IDs. Only `comment_deleted` events at/before the question
time are currently supported. Unmapped events and `setup` prose are **not executed**.
The Q26 deletion scenario needs a reviewed mapping to ev-2. Summary/cache scenarios
cannot be exercised until the pilot exists; their metrics stay `not_measured`.

Freeze manifest (placeholders are intentionally invalid until replaced):

```json
{
  "schema_version": 1, "status": "frozen",
  "reviewer": "<independent reviewer>",
  "adjudication": "<receipt resolving all disagreements>",
  "frozen_at": "2026-10-20T00:00:00Z",
  "sha256": {
    "sources": "<original file hash>", "questions": "<original file hash>",
    "blind_questions": "<blind projection hash>", "config": "<config hash>",
    "labels": { "dev": "<normalized dev hash>", "test": "<normalized test hash>" }
  },
  "dev_retrieval_min_all_sources_top5": 0.8333333333333334,
  "semantic_thresholds": "<adjudicated thresholds from the research protocol>"
}
```

**5/6 is the coordinator's proposal for adjudication, not an accepted result.** Freeze
the original-label hashes and protocol thresholds in the coordinator's retained
manifest as well. Preserve the protocol's proposed ACL/deleted/stale/unknown/conflict
gates until QA adjudicates them; never change thresholds after test measurement.
The executable guard verifies the exact sources, questions, selected labels and
config hashes plus the reviewer/adjudication receipt. The receipt and blind-file
hash are provenance supplied by the coordinator, not independently authenticated
by this local program.

## After freeze: dev measurement

Commit the code first and rebuild the helper from the clean checkout; embedded
Git revision/dirty state must match. Keep output outside the checkout:

```sh
test -z "$(git status --porcelain)"
(cd server && go build -ldflags "-X main.commit=$(git rev-parse HEAD)" -o bin/kbbench ./cmd/kbbench)
node scripts/kb-bench.mjs run \
  --sources ../benchmark-input/sources.json \
  --questions ../benchmark-input/questions.json \
  --labels ../benchmark-input/labels.dev.json \
  --config ../benchmark-input/scenario-config.json \
  --freeze ../benchmark-input/freeze.json \
  --variant b0 --backend-bin server/bin/kbbench --out ../benchmark-runs
```

The explicit linker stamp supports managed worktrees where Go omits automatic
VCS metadata. It assumes the documented clean-checkout build command was followed;
the runner checks the current clean Git SHA and records the binary hash too.

The run directory contains a manifest, completion record, fixture manifest, raw
stdout/stderr, incremental `fixture.json.queries.jsonl`, `raw.json` and `report.json`.
Failed or uncertain attempts remain inspectable and are never automatically retried.
Search latency excludes import/hydration. Coordinator CPU/wall time and backend
Go heap samples have explicit scope; tokens, backend CPU and process RSS remain
`not_measured`. Go heap samples are not peak RAM.

The retrieval threshold counts **questions for which every required citation
appears in the full live-source hydration of the first five ranked issues**,
divided by **questions with at least one required citation**. Both counts are
reported. Hydration preserves database revisions/hashes and excludes tombstones;
an independent snapshot projection checks source integrity and workspace scope.
This generous issue-level coverage is not ranked source/chunk recall or answer
correctness. All semantic metrics and answer citation accuracy remain
`not_measured` for the search-only B0 adapter. The mini-fixture test also found that
the current `SearchIssues` comment aggregation can expose a tombstoned comment in
raw snippets (`server/internal/handler/issue.go`, `comment_matches` CTE has no
`deleted_at` filter). Raw responses are retained. Full deleted-text detections
are reported as a **lower bound** and mark raw-backend safety failed; zero is not
proof of no partial-snippet leak because the endpoint lacks snippet provenance.
Filtered current-source hydration does not turn this into a passing product test.
No production search change is included before corpus freeze.

A failed frozen real-backend dev gate
is only a prerequisite for considering Cognee, not an installation request.

## Rubric and holdout custody

The scorer never treats word overlap as semantic correctness. When a future
adapter records actual answers, an independent assessor supplies a ratings JSON
bound to the SHA256 of `raw.json`: `reviewer`, `raw_sha256`, and one `assessment`
per question. Each has `question_id`, a source-based `rationale`, `state_correct`,
and boolean arrays for every expected `must_say`, `critical` and `must_not` item.
`true` means covered for `must_say`/`critical`, violated for `must_not`.
The answer passes only if state and every required fact are correct and no
forbidden assertion is made. Missing critical facts are counted separately.
`unknown`, `true_conflicts`, `false_conflicts`, `freshness` and
`stale_decision_as_current` are boolean outcomes or null when inapplicable; true
means the named event occurred (unknown handled correctly, true conflict found,
false conflict raised, freshness handled correctly, stale decision asserted).
Assess against source meaning, negation, scope, time and SHA, with rationale.
Search evidence packets cannot receive semantic ratings.

```sh
node scripts/kb-bench.mjs score --raw ../benchmark-runs/RUN_ID/raw.json \
  --labels ../benchmark-input/labels.dev.json --ratings ../ratings.json \
  --out ../rubric-report.json
```

Test custody belongs to an independent evaluator. They use `--split test`, the
separate test labels file and an evaluator-owned persistent `--test-ledger` path.
An exclusive reservation is written **before** the backend runs, keyed by corpus,
test labels and stable variant name. A change of code, config or freeze timestamp
does not permit retrying that same variant. Failed/uncertain reservations persist.
Do not rename a variant, delete a ledger, or alter labels to obtain another peek;
these are cooperative protocol guards, not a tamper-proof external billing system.
No automatic holdout command is run by development tests or CI.

## Cleanup and remaining delivery gates

Normal completion and error paths remove only their generated fixture IDs. After
interruption, inspect the retained fixture manifest and use the same explicit DB:

```sh
server/bin/kbbench --cleanup ../benchmark-runs/RUN_ID/fixture.json
```

Cleanup verifies database name, fixture workspace slugs and user emails, is
idempotent, and marks the manifest `cleaned`. It never drops the database or uses
`TRUNCATE`. If identities changed, it refuses cleanup for manual inspection.
Stop/remove only a task-owned disposable PostgreSQL container when finished;
shared PostgreSQL and unrelated databases belong to their own environments.

ACCO-131 remains incomplete until ACCO-128 is merged and this branch is adapted
to that base, freeze and separate normalized dev labels/config are approved,
the real-backend dev measurement is retained, and ClaudeQA reviews the exact PR
head with required checks. This PR alone is a partial infrastructure delivery.
