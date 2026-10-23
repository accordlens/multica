# Optional Jev API checker experiment

This experiment measures TypeSafe's hosted `jev-1.13.0` through the existing
`TYPESAFE_API_KEY`. It does not measure the local feder-cr/jev `jevos-v4` model.
The root Node requirement is sufficient; no SDK, service or database is installed.

The external ACCO-97 freeze A dev package is required. Download attachment
`01a159eb-6394-7eeb-86f3-6d57f5e40149` from ACCO-131 with the authenticated Multica
CLI. Verify archive SHA256
`a579fcc08b265e4c6664ad78566e1e3c85a67cf6357bf0743fe9d375383d07a6`, then unpack it
into a task-local directory. The runner verifies its dev manifest and every listed
file, requires synthetic data, and rejects non-dev question IDs. Before executing,
also verify `knowledge_corpus_freeze_v1.status=frozen_published` on ACCO-97.

```sh
node --test scripts/jev-benchmark.test.mjs
node scripts/jev-benchmark.mjs /path/to/dev scripts/fixtures/jev-dev-claims.json /path/to/new-run
```

The second command is a dry run. It sends no network request and prints the
request count and maximum list-price reservation. The nine requests contain
two candidate claims each, with separate support/contradict Noul questions.
There are five native-English cases, two native-Polish cases and two controlled
Polish-to-English translations, derived from seven distinct dev questions.

Only execute when the issue authorizes this provider/account and the task ledger
has a reservation read back for this job. At the [documented rate](https://docs.typesafe.ai/models)
of $0.042/M input tokens and free output, nine calls capped at the default
64,000 input tokens reserve $0.024192. This is an internal reservation, not an
invoice guarantee; account credits, discounts or billing changes are not assumed.
The provider reports output-token usage even though its output price is zero.

```sh
# TYPESAFE_API_KEY must already be injected into this process; never print/copy it.
node scripts/jev-benchmark.mjs /path/to/dev scripts/fixtures/jev-dev-claims.json /path/to/new-run --execute
```

The endpoint is fixed to `https://api.typesafe.ai/v1/systemone`. Redirects and
automatic retries are disabled. The runner verifies the resolved versioned model,
answer IDs, probability ranges and usage. Receipts are create-only and flushed
before/after each request. A failure stops the batch; inspect its `*.uncertain.json`
and reconcile the reservation before dispatching anything else. Do not reuse a
run directory or treat a new directory as permission to retry a paid operation.
The ledger is a cooperative accounting record, not an atomic billing guard.

`dev-score.json` retains raw probabilities, expected/predicted states and the
complete calibration grid. Selection minimizes wrong non-unknown decisions,
then maximizes correct states, then prefers higher support and contradict cutoffs.
Both-high is `conflict` requiring adjudication; both-low is `unknown`. Record the
selected cutoffs, request/fixture hashes, runner hash and actual code head in a
freeze before any final test. No parameter is tuned against final-test labels.

Evidence is explicitly selected from the dev sources, with locator/revision,
asker membership and time checks. This is **oracle evidence**, not live retrieval,
product ACL assurance, answer generation or end-to-end citation accuracy.
Expected labels, rubrics and thresholds are not sent to the API. Claims are
author-prepared checker probes, not independently labeled answers to all original
questions. Negated pairs and translated pairs are correlated observations.

Translations retain the original source locator and snapshot SHA256. Their
quality and the checker gold require independent review; agreement with this
author's labels is not independent translation validation. The fixture explicitly
records that review as pending. Native-English, native-Polish and translated
results must be reported separately. Dev accuracy never establishes production
language support. CPU/RAM of the hosted provider and actual invoiced spend remain
unknown; receipts measure client CPU/RSS and wall latency only.

This CLI intentionally has no final-test mode. The independent evaluator must
receive the frozen head/config and separately held final corpus, preserve a
durable one-attempt-per-variant record, and run the final evaluation without
author tuning or label exposure. A dev smoke test cannot satisfy that gate.

## Independent holdout transport

`scripts/jev-holdout.mjs` is a separate mechanical transport for independently
prepared requests. It does not decrypt, prepare claims, read gold, calibrate or
score. The existing dev CLI and its manifest/split checks are unchanged.

```sh
node --test scripts/jev-benchmark.test.mjs scripts/jev-holdout.test.mjs
node scripts/jev-holdout.mjs /path/to/authorization.json /path/to/request-only.json
# Only after all independent gates and the exact reservation readback:
node scripts/jev-holdout.mjs /path/to/authorization.json /path/to/request-only.json --execute
```

The default only validates readiness and prints a request count. Execution needs
an independent controller's `jev_evaluation_v1.execution_authorization` with
`status: approved`, the exact authorization-file SHA256, canonical shared
`register_root`, controller ID, preparation hashes and three review-receipt
hashes (`preparation`, `translations`, `launcher`). No such authorization is
created by this command. The sealed request envelope retains
`dispatch_authorized: false`; permission is a separate record.

The authorization file uses version 1 and binds `issue_id`, `freeze_sha256`,
`endpoint`, `model`, `authorized`, `controller_agent_id`, `executor_agent_id`,
`executor_task_id`, `executor_runtime_id`, `code_head`, `launcher_sha256`,
`transport_sha256`, `register_root`, `protocol_sha256`, `requests_sha256`,
`scoring_map_sha256`, `coverage_sha256`, `translations_sha256`, `approvals`,
`max_input_tokens_per_request`, `reserved_usd`, `reservation_id`, and ordered
`requests: [{id, sha256}]`. The issue's controller record repeats the preparation
hashes and approvals. The existing cost ledger must contain a `reserved` row
whose `job_id` is `reservation_id`, bound to these request bytes/count and the
full remaining cap. USD comparison uses integer nanounits.

The launcher checks the active Multica task/runtime, a clean exact Git head,
source hashes, request-only schema, model, time and byte limits. It loads the
immutable bytes once. All prospective executors must use the same canonical
shared register root, pinned by the controller record, on the authorized host.
An exclusive, flushed file keyed by the original v2 freeze claims the attempt;
changing the job, input or output name does not bypass it. Before the first POST,
the launcher rechecks current gates, writes `jev_holdout_attempt_v1` through the
CLI and verifies its readback. A recorded attempt also prevents replay after a
missing local claim. These controls prevent accidental concurrent/repeated work;
agents sharing an OS user and write permissions are not strongly isolated.

Claim/checkpoint/transport failure never removes the claim. Partial or uncertain
work retains its full reservation and all available receipts for independent
reconciliation. No automatic retry/resume is implemented. Create-only outcome
files report counts/usage, while request/response bytes stay in the authorized
receipt directory. Console errors are generic to avoid exposing held-out content.
The original configuration freeze and measurement attribution are preserved;
this new launcher needs its own exact-head review before an execution record is
approved. Preparation alone does not authorize inference or production EN/PL.
