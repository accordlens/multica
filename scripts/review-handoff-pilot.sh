#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$REPO_ROOT"
: "${DATABASE_URL:?Run this pilot through make env-exec against its synthetic database}"
node --input-type=module - <<'JS'
let url;
try { url = new URL(process.env.DATABASE_URL); }
catch { throw new Error("A valid managed local DATABASE_URL is required."); }
if (!["localhost", "127.0.0.1", "[::1]"].includes(url.hostname) || !url.pathname.startsWith("/multica_")) {
  throw new Error("The pilot requires the managed local worktree database (multica_*). No tests were run.");
}
JS
report=${1:-test-results/review-handoff-pilot.jsonl}
mkdir -p "$(dirname -- "$report")"

# The guard rejects an accidental invocation of any installed provider CLI.
if ! bash scripts/go-test-with-agent-cli-guard.sh -- go -C server test -race -count=1 -json \
  ./internal/issueworkflow ./internal/service ./internal/handler ./cmd/multica \
  -run '^(TestReviewHandoff|TestHandoffEnvelope|TestDefinitionVersion|TestIssueWakeupCLIReadsScoped)' >"$report"; then
  printf 'Pilot failed; retained report: %s\n' "$report" >&2
  exit 1
fi

node --input-type=module - "$report" <<'JS'
import { readFileSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
const path = process.argv[2];
const events = readFileSync(path, "utf8").trim().split("\n").map((line) => JSON.parse(line));
const required = [
  "TestReviewHandoffPilotOfflineReplayAndDecision",
  "TestReviewHandoffHTTPEndToEnd",
  "TestReviewHandoffRejectsStaleForgedAndForeignSources",
  "TestReviewHandoffRechecksRevocationAndFrozenEvidenceAtClaim",
  "TestReviewHandoffHasOneSubscriptionAndFiniteBudget",
  "TestReviewHandoffKeepsItsScopeBesideOtherQueuedWork",
  "TestReviewHandoffRetainsTheExistingCausalLoopGuard",
  "TestHandoffEnvelopeRejectsAmbiguousOrIncompleteMessages",
  "TestDefinitionVersionTracksMeaningfulDefinitionChanges",
  "TestIssueWakeupCLIReadsScopedHandoffFilter",
];
for (const test of required) {
  if (!events.some((e) => e.Test === test && e.Action === "pass")) {
    throw new Error(`Required test did not pass: ${test}. Use a migrated synthetic database; skips are not evidence.`);
  }
}
const summary = {
  version: 1, scope: "ACCO-96 synthetic local pilot", observed_at: new Date().toISOString(),
  source_revision: execFileSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).trim(),
  working_tree_dirty: execFileSync("git", ["status", "--porcelain"], { encoding: "utf8" }).trim().length > 0,
  required_tests_passed: required.length,
  passed_cases: events.filter((e) => e.Test && e.Action === "pass").length,
  skipped_cases: events.filter((e) => e.Test && e.Action === "skip").length,
  model_calls: 0, routing_tokens: 0, provider_cost_usd: "0",
  provider_boundary: "No provider invoked; the existing PostgreSQL task queue is the fake execution boundary. This does not price development or QA runtime usage.",
  observations: events.filter((e) => e.Output?.includes("ACCO-96 pilot:")).map((e) => e.Output.trim()),
  raw_report: path,
};
writeFileSync(`${path}.summary.json`, JSON.stringify(summary, null, 2) + "\n");
process.stdout.write(JSON.stringify(summary, null, 2) + "\n");
JS
