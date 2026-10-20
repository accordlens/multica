import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { checkMessage, compareFixtures, contextErrors, fixtureMessages, loadContracts, pilot, preview, receipt, snapshot } from "./role-contracts.mjs";

const bundle = loadContracts();
// Synthetic IDs exist only in this test; live receipts must use API-returned IDs.
const context = {
  issue_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  task_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
  workflow_id: null, workflow_definition_sha256: null,
  workflow_unknown_reason: "No workflow definition loaded in this synthetic test.",
  status_key: "in_progress", status_source: "issue.status", source_revision: "1".repeat(40), source_dirty: false,
};
const fixtures = JSON.parse(readFileSync(new URL("../docs/agent-contracts/v1/fixtures.json", import.meta.url), "utf8"));
const message = () => fixtureMessages(fixtures.cases[0], "en", structuredClone(context), bundle).profile;
const check = (m, options = {}) => checkMessage(m, { context, bundle, ...options });
const reject = (change, pattern, options = {}) => {
  const m = message();
  change(m);
  const result = check(m, options);
  assert.equal(result.ok, false, JSON.stringify(result));
  assert.match(result.errors.join("\n"), pattern);
  return result;
};

test("profiles separate behavior, communication, permissions and runtime; preview grants no authority", () => {
  for (const role of ["dev", "qa"]) {
    const output = preview(role, bundle);
    assert.equal(output.activation, false);
    assert.equal(output.runtime.provider, null);
    assert.equal(output.runtime.model, null);
    assert.equal(output.runtime.runtime_id, null);
    assert.equal(output.runtime.skill_bindings, null);
    assert.equal(output.runtime.secret_references, null);
    assert.deepEqual(output.contracts, snapshot(role, bundle));
    for (const goal of bundle[role].value.goals) {
      assert.ok(goal.behavior && goal.anti_example && goal.evaluation, goal.id);
      assert.ok(output.instructions.includes(goal.behavior), goal.id);
    }
    for (const field of ["model", "provider", "runtime_id", "instructions", "permissions"]) {
      assert.equal(Object.hasOwn(bundle[role].value, field), false, field);
    }
  }
  assert.notEqual(snapshot("dev", bundle).role.sha256, snapshot("qa", bundle).role.sha256);
  assert.throws(() => preview("admin"), /Role must be/);
});

test("all six scenarios retain critical evidence in both languages", () => {
  assert.deepEqual(fixtures.cases.map((c) => c.id), ["success", "blocker", "unknown_fact", "review_disagreement", "requirements_change", "contradictory_evidence"]);
  for (const scenario of fixtures.cases) for (const language of ["en", "pl"]) {
    const { baseline, profile } = fixtureMessages(scenario, language, context, bundle);
    const result = check(profile);
    assert.equal(result.ok, true, `${scenario.id}/${language}: ${result.errors}`);
    assert.equal(result.critical_delivery.claims[0].text, scenario[language].fact);
    assert.equal(check(baseline).ok, false, "Legacy missing provenance/request cannot become a valid envelope through prose alone");
  }
});

test("checker does not mutate messages, including critical failures", () => {
  const m = fixtureMessages(fixtures.cases[1], "pl", context, bundle).profile;
  m.style_revision = 5;
  const original = JSON.stringify(m);
  const result = check(m);
  assert.equal(result.ok, false);
  assert.equal(JSON.stringify(m), original);
  assert.deepEqual(result.critical_delivery.prose, m.prose);
  assert.match(result.critical_delivery.claims[0].text, /ECONNREFUSED/);
});

test("malformed envelopes return diagnostics without throwing", () => {
  for (const value of [null, [], 3, "text", {}, { sources: [null], claims: [null] }, { context: [], contracts: null }]) {
    const result = check(value);
    assert.equal(result.ok, false, JSON.stringify(value));
    assert.ok(result.errors.length > 0);
  }
});

test("stable fields and statuses reject typos and contradictory status arrays", () => {
  reject((m) => { m.result.status = "completed"; }, /stable status/);
  reject((m) => { m.result = { status: "pass", statuses: ["pass", "blocked"] }; }, /stable status/);
  reject((m) => { m.run_id = m.context.task_id; }, /exact envelope/);
  reject((m) => { delete m.expected_response; }, /expected_response/);
  reject((m) => { m.expected_response.required = false; }, /contradictory request/);
  reject((m) => { m.message_type = "review"; }, /QA role/);
});

test("exact revision, dirty source, run identity and workflow digest fail closed", () => {
  reject((m) => { m.context.task_id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"; }, /does not match/);
  reject((m) => { m.context.source_revision = "2".repeat(40); }, /stale/);
  reject((m) => { m.context.source_dirty = true; }, /dirty source/);
  reject((m) => { m.context.workflow_id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"; }, /definition hash/);
  reject((m) => { delete m.context.workflow_unknown_reason; }, /exact fields/);
  assert.ok(contextErrors({ ...context, task_id: "invented-run" }).length > 0);
  const bound = { ...context, workflow_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", workflow_definition_sha256: "a".repeat(64), workflow_unknown_reason: null };
  assert.deepEqual(contextErrors(bound), []);
});

test("a modified contract cannot retain the old version receipt", () => {
  reject((m) => { m.contracts.role.sha256 = "0".repeat(64); }, /hash mismatch/);
  reject((m) => { m.contracts.communication.version = "2.0.0"; }, /hash mismatch/);
  reject((m) => { m.contracts.role = snapshot("qa", bundle).role; }, /unexpected role/, { role: "dev" });
});

test("facts need resolving evidence and every uncertainty remains explicit", () => {
  reject((m) => { m.claims[0].source_ids = ["missing"]; }, /does not resolve/);
  reject((m) => { m.claims[0].source_ids = []; }, /facts need sources/);
  reject((m) => { m.sources.push(structuredClone(m.sources[0])); }, /unique/);
  reject((m) => { m.claims[0].kind = "unknown"; }, /uncertainty/);
  reject((m) => { m.claims[0].kind = "hypothesis"; m.uncertainty = ["observation"]; }, /critical uncertainty/);
  reject((m) => { m.uncertainty = ["made-up-unknown"]; }, /uncertainty/);
  reject((m) => { m.sources[0].required = false; }, /at least one required/);
});

for (const status of ["fail", "blocked", "unknown", "needs_changes"]) {
  test(`pass cannot hide required evidence with status ${status}`, () => {
    reject((m) => { m.sources[0].status = status; }, /required source did not pass/);
  });
}

test("blockers, changed requirements and conflicting evidence cannot claim success", () => {
  reject((m) => { m.message_type = "blocker"; }, /status must be blocked/);
  reject((m) => { m.result.status = "blocked"; }, /blocker evidence/);
  reject((m) => { m.message_type = "requirements_change"; }, /new scope needs changes/);
  reject((m) => { m.message_type = "evidence_conflict"; }, /cannot pass/);
  const conflict = fixtureMessages(fixtures.cases[5], "en", context, bundle).profile;
  assert.deepEqual(conflict.sources.map((s) => s.status), ["pass", "fail"]);
  assert.deepEqual(conflict.claims[0].source_ids, ["report-a", "report-b"]);
  assert.equal(check(conflict).ok, true);
});

test("QA review still hands acceptance to a human", () => {
  const m = message();
  m.contracts = snapshot("qa", bundle);
  m.message_type = "review";
  m.expected_response = { required: true, type: "human_review", request: "Review this exact revision." };
  assert.equal(check(m, { role: "qa" }).ok, true);
  m.expected_response.type = "none";
  assert.equal(check(m).ok, false);
});

test("style can change ordinary wording once while preserving immutable evidence", () => {
  const before = message();
  const after = structuredClone(before);
  after.prose.outcome = "The recorded fixture check passed.";
  after.style_revision = 1;
  assert.equal(check(after, { before }).ok, true);
  assert.match(check(after).errors.join("\n"), /requires its original/);
  const twice = structuredClone(after);
  twice.style_revision = 2;
  const result = check(twice, { before: after });
  assert.equal(result.ok, false);
  assert.match(result.errors.join("\n"), /at most one/);
});

for (const [name, from, to] of [
  ["command", "`node --version`", "`node --help`"],
  ["number", "code 0", "code 1"],
  ["date", "2026-10-20", "2026-10-21"],
  ["negation", "does not verify", "does verify"],
  ["error meaning", "does not verify the application", "does not modify the application"],
]) {
  test(`style preserves ${name} and cannot shorten away a critical claim`, () => {
    const before = message();
    const after = structuredClone(before);
    after.prose.evidence = after.prose.evidence.replace(from, to);
    after.style_revision = 1;
    const result = check(after, { before });
    assert.equal(result.ok, false);
    assert.match(result.errors.join("\n"), /critical text/);
    after.claims[0].text = after.prose.evidence;
    assert.match(check(after, { before }).errors.join("\n"), /immutable fields changed/);
  });
}

test("non-critical IDs, code, numbers, certainty and negation in prose are also guarded", () => {
  const before = message();
  before.prose.outcome = "Unknown: run aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa may need `check()` in 2 cases; do not merge.";
  for (const [from, to] of [["aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"], ["may", "must"], ["not ", ""], ["2", "3"], ["`check()`", "`check(true)`"]]) {
    const after = structuredClone(before);
    after.style_revision = 1;
    after.prose.outcome = after.prose.outcome.replace(from, to);
    const result = check(after, { before });
    assert.equal(result.ok, false, from);
    assert.match(result.errors.join("\n"), /protected tokens/);
  }
});

test("style warnings are advisory and can produce documented false positives", () => {
  const m = message();
  m.prose.outcome = "The fixture is named All Done.";
  const result = check(m);
  assert.equal(result.ok, true);
  assert.ok(result.warnings.length > 0, "A literal fixture name is an intentional false positive");
});

test("semantic false negatives remain visible: a well-shaped envelope can contain unsupported prose", () => {
  const m = message();
  m.prose.outcome = "The fixture proves every production feature works.";
  const result = check(m);
  assert.equal(result.ok, true);
  assert.match(result.scope, /no test, independent review, semantic or STE/);
});

test("comparison reports measured proxies separately from unknown model/semantic metrics", () => {
  const report = compareFixtures(context, bundle);
  assert.equal(report.rows.length, 24);
  assert.ok(report.rows.every((row) => row.critical_claim_preserved));
  assert.ok(report.rows.filter((r) => r.variant === "profile").every((row) => row.checker_errors.length === 0));
  for (const row of report.rows) {
    for (const key of ["provider", "model", "input_tokens", "output_tokens", "model_latency_ms", "cost_usd", "additional_model_revisions", "task_alignment_score", "fact_preservation_score", "handoff_correctness_score", "semantic_score", "readability_score"]) {
      assert.equal(row[key], null, key);
    }
    assert.ok(row.utf8_bytes > 0 && row.prose_words > 0 && row.checker_duration_ms >= 0);
  }
});

test("offline task → Dev → checker → QA fixture → Polish report retains contracts and no fictitious QA run", () => {
  const result = pilot(context, bundle);
  assert.equal(result.kind, "offline_synthetic_pilot");
  assert.equal(result.steps.length, 3);
  assert.ok(result.steps.every((s) => s.check.ok));
  assert.equal(result.steps[0].message.context.task_id, context.task_id);
  assert.equal(result.steps[1].task_id, null);
  assert.equal(result.steps[1].message.contracts.role.id, "qa");
  assert.equal(result.steps[2].message.language, "pl");
  assert.match(result.independent_review, /pending/);
  assert.match(result.report_pl, /syntetyczny/);
  const dirty = pilot({ ...context, source_dirty: true }, bundle);
  assert.equal(dirty.envelope_status, "fail");
  assert.match(dirty.report_pl, /błąd/);
  assert.doesNotMatch(dirty.report_pl, /przeszły kontrolę/);
});

test("run receipt binds returned platform identity and exact report/profile bytes, without attesting activation", () => {
  const run = { id: context.task_id, issue_id: context.issue_id,
    agent_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", runtime_id: "dddddddd-dddd-4ddd-8ddd-dddddddddddd" };
  const report = JSON.stringify(pilot(context, bundle));
  const artifact = JSON.stringify({ activation: false, profiles: { dev: preview("dev", bundle), qa: preview("qa", bundle) } });
  const output = receipt([run], report, artifact, bundle);
  assert.equal(output.task_id, run.id);
  assert.equal(output.effective_runtime_configuration_attested, false);
  assert.equal(output.independent_qa_task_id, null);
  assert.equal(output.report.attachment_id, null);
  assert.equal(output.report.sha256.length, 64);
  assert.throws(() => receipt([{ ...run, issue_id: "other" }], report, artifact, bundle), /existing issue/);
  assert.throws(() => receipt([run], report, artifact.replace('"activation":false', '"activation":true'), bundle), /inactive/);
  const changed = JSON.parse(report);
  changed.steps[0].message.contracts.role.sha256 = "0".repeat(64);
  assert.throws(() => receipt([run], JSON.stringify(changed), artifact, bundle), /passing offline/);
});

test("CLI failure returns critical evidence, a failing exit code and no modified files", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "role-contracts-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const path = join(dir, "message.json");
  const expected = join(dir, "context.json");
  const m = message();
  m.style_revision = 2;
  const raw = JSON.stringify(m);
  writeFileSync(path, raw);
  writeFileSync(expected, JSON.stringify(context));
  const result = spawnSync(process.execPath, [new URL("./role-contracts.mjs", import.meta.url).pathname, "check", path, "--context", expected], { encoding: "utf8" });
  assert.equal(result.status, 1, result.stderr);
  const output = JSON.parse(result.stdout);
  assert.deepEqual(output.critical_delivery.prose, m.prose);
  assert.equal(readFileSync(path, "utf8"), raw);
});
