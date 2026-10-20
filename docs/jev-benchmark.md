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
