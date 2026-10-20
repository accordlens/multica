import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import { backendInput, claimTest, hash, project, score, sourcePacket, validateFreeze, validateLabels } from "./kb-bench.mjs";

// Independent mini-fixtures. No ACCO-97 questions or author/test labels.
export function mini() {
  const at = "2025-02-01T00:00:00Z";
  const sources = { corpus: "mini", synthetic: true, workspaces: { public: { members: ["reader"] }, private: { members: ["other"] } },
    events: [{ id: "remove", type: "comment_deleted", at: "2025-02-02T00:00:00Z", comment: "edit" }],
    issues: [
      { id: "MIN-1", workspace: "public", title: "Mango migration", description_revisions: [{ rev: 1, at, text: "Mango plan" }],
        status_history: [{ at, status: "in_progress" }], comments: [
          { id: "edit", at, edited_at: "2025-02-02T00:00:00Z", rev: 2, revisions: [{ rev: 1, text: "Old mango draft" }, { rev: 2, text: "Revised mango plan" }] },
          { id: "deleted", at, rev: 1, text: "Mango removed", deleted_at: "2025-02-02T00:00:00Z" },
        ] },
      { id: "MIN-2", workspace: "private", title: "Mango secret", description_revisions: [{ rev: 1, at, text: "Mango confidential" }], comments: [] },
    ] };
  const questions = { corpus: "mini", asked_at_default: "2025-02-03T00:00:00Z", questions: [
    { id: "mini-dev", split: "dev", asker: { id: "reader", ws: ["public"] }, q: "mango" },
    { id: "mini-test", split: "test", asker: { id: "reader", ws: ["public"] }, q: "held-out prompt" },
  ] };
  return { sources, questions };
}

test("default projection exposes only dev, the live edit, and a tombstone", () => {
  const { sources, questions } = mini();
  const plan = project(sources, questions);
  assert.equal(plan.plans.length, 1);
  assert.equal(plan.plans[0].question_id, "mini-dev");
  assert.ok(!JSON.stringify(plan).includes("held-out prompt"));
  const [edit, deleted] = plan.plans[0].documents[0].comments;
  assert.equal(edit.text, "Revised mango plan");
  assert.equal(edit.revision, 2);
  assert.equal(deleted.deleted, true);
  assert.deepEqual(plan.plans[0].expected_sources.map((s) => s.source_id), ["MIN-1.description", "edit"]);
});

test("historical query selects the earlier edit and does not apply future deletion", () => {
  const { sources, questions } = mini();
  questions.questions[0].asked_at = "2025-02-01T12:00:00Z";
  const p = project(sources, questions).plans[0];
  assert.equal(p.documents[0].comments[0].text, "Old mango draft");
  assert.equal(p.documents[0].comments[0].revision, 1);
  assert.equal(p.documents[0].comments[1].deleted, false);
});

test("scenario events require explicit config, and prose is inert", () => {
  const { sources, questions } = mini();
  questions.questions[0].setup = "delete edit; start an agent";
  assert.equal(project(sources, questions).plans[0].documents[0].comments[0].deleted, false);
  assert.equal(project(sources, questions, "dev", { events_by_question: { "mini-dev": ["remove"] } }).plans[0].documents[0].comments[0].deleted, true);
  assert.throws(() => project(sources, questions, "dev", { events_by_question: { "mini-dev": ["missing"] } }), /Unsupported event/);
});

test("frozen source deletion events apply by time, including before/after projections", () => {
  const { sources, questions } = mini();
  assert.equal(project(sources, questions, "dev", { apply_timestamped_deletions: true }).plans[0].documents[0].comments[0].deleted, true);
  questions.questions[0].asked_at = "2025-02-01T12:00:00Z";
  assert.equal(project(sources, questions, "dev", { apply_timestamped_deletions: true }).plans[0].documents[0].comments[0].deleted, false);
});

test("reject ambiguous edit timelines, duplicate IDs, and invented asker access", () => {
  const { sources, questions } = mini();
  sources.issues[0].comments[0].revisions.push({ rev: 3, text: "ambiguous" });
  assert.throws(() => project(sources, questions), /Ambiguous/);
  sources.issues[0].comments[0].revisions.pop();
  sources.issues[1].comments.push({ ...sources.issues[0].comments[0] });
  assert.throws(() => project(sources, questions), /Duplicate source/);
  sources.issues[1].comments.pop();
  questions.questions[0].asker.ws = ["private"];
  assert.throws(() => project(sources, questions), /membership/);
});

const cite = { source_id: "s", revision: 2, content_sha256: hash("current"), workspace: "public" };
const label = (id, citations = [cite]) => ({ question_id: id, state: "answered", must_say: ["A fact"], must_not: ["False assertion"], critical: ["Critical fact"], must_cite: citations });
const row = (id, sources = [cite]) => ({ question_id: id, hits: [{ issue_id: "MIN-1", sources }], ranked_sources: sources, expected_sources: [cite], allowed_workspaces: ["public"] });
const run = (responses) => ({ evidence_kind: "mock_backend", split: "dev", dev_retrieval_min_all_sources_top5: 0.8, supported_source_kinds: ["description", "comment"], responses });

test("retrieval denominator excludes questions without required sources; mocks cannot qualify Cognee", () => {
  const labels = { labels: [label("a"), label("b", []), label("c")] };
  const report = score(labels, run([row("a"), row("b", []), row("c", [])]));
  assert.deepEqual(report.retrieval_sources_top5, { status: "measured", numerator: 1, denominator: 2, value: 0.5 });
  assert.equal(report.dev_retrieval_gate.passed, false);
  assert.equal(report.dev_retrieval_gate.cognee_eligible, false);
  assert.equal(report.answer_state_and_must_say.status, "not_measured");
  assert.equal(report.tokens.status, "not_measured");
  assert.equal(report.backend_rss.status, "not_measured");
  assert.equal(report.raw_backend_deleted_source_exposure.status, "not_measured");
});

test("a source beyond the fifth issue does not count as top-five retrieval", () => {
  const r = row("a");
  r.hits = [...Array.from({ length: 5 }, () => ({ sources: [] })), { sources: [cite] }];
  r.ranked_sources = sourcePacket(r);
  assert.equal(score({ labels: [label("a")] }, run([r])).retrieval_sources_top5.numerator, 0);
});

test("reviewed equivalent evidence sets are accepted without requiring every alternative", () => {
  const alternative = { ...cite, source_id: "equivalent-source" };
  const l = { ...label("a"), alternative_citation_sets: [[alternative]] };
  const r = { ...row("a", [alternative]), expected_sources: [alternative] };
  validateLabels({ schema_version: 1, split: "dev", labels: [l] }, [r], "dev");
  assert.equal(score({ labels: [l] }, run([r])).retrieval_sources_top5.numerator, 1);
});

test("wrong revision, content, SHA, deleted source, and ACL leaks cannot pass integrity", () => {
  for (const invalid of [{ ...cite, revision: 1 }, { ...cite, content_sha256: hash("old") }, { ...cite, source_sha: "a".repeat(40) }, { ...cite, source_id: "deleted" }]) {
    const report = score({ labels: [label("a")] }, run([row("a", [invalid])]));
    assert.equal(report.retrieval_sources_top5.numerator, 0);
    assert.equal(report.source_integrity.invalid_revision_hash_or_deleted, 1);
  }
  const report = score({ labels: [label("a")] }, run([row("a", [{ ...cite, workspace: "private" }])]));
  assert.equal(report.source_integrity.acl_leaks, 1);
});

test("empty denominators are not measured; missing or duplicate responses fail", () => {
  assert.equal(score({ labels: [label("a", [])] }, run([row("a")])).retrieval_sources_top5.status, "not_measured");
  assert.throws(() => score({ labels: [label("a")] }, run([])), /Missing/);
  assert.throws(() => score({ labels: [label("a")] }, run([row("a"), row("a")])), /Duplicate/);
});

test("top five sources cannot be inflated by hydration of a single matching issue", () => {
  const sources = [...Array.from({ length: 5 }, (_, n) => ({ ...cite, source_id: `decoy-${n}` })), cite];
  const r = row("a", sources);
  r.ranked_sources = sourcePacket(r);
  const report = score({ labels: [label("a")] }, run([r]));
  assert.equal(report.issue_hydration_coverage_at5.numerator, 1);
  assert.equal(report.retrieval_sources_top5.numerator, 0);
  assert.deepEqual(sourcePacket(row("a", [cite, cite, ...sources])).map((s) => s.source_id), ["s", "decoy-0", "decoy-1", "decoy-2", "decoy-3"]);
});

test("AND evidence groups accept any alternative in each group; flat representatives add no requirement", () => {
  const second = { ...cite, source_id: "second" };
  const unused = { ...cite, source_id: "unused-representative" };
  const l = { ...label("a", [unused]), evidence_groups: [{ id: "one", any_of: [unused, cite] }, { id: "two", any_of: [second] }] };
  const r = { ...row("a", [cite, second]), expected_sources: [cite, second] };
  assert.equal(score({ labels: [l] }, run([r])).retrieval_sources_top5.numerator, 1);
  r.ranked_sources = [cite];
  assert.equal(score({ labels: [l] }, run([r])).retrieval_sources_top5.numerator, 0);
});

test("unsupported run evidence is not a retrieval failure and cannot qualify Cognee", () => {
  const artifact = { ...cite, source_id: "run", kind: "run", revision: null, source_sha: "a".repeat(40), sources_sha256: "b".repeat(64) };
  const l = { ...label("a", [artifact]), evidence_groups: [{ id: "run", any_of: [artifact] }] };
  const report = score({ labels: [l] }, { ...run([row("a", [])]), evidence_kind: "real_search_backend" });
  assert.equal(report.retrieval_sources_top5.status, "not_measured");
  assert.equal(report.retrieval_sources_top5.frozen_eligible_denominator, 1);
  assert.equal(report.dev_retrieval_gate.status, "not_measured");
  assert.equal(report.dev_retrieval_gate.cognee_eligible, false);
});

test("backend inputs contain only the query and authorized fixture projection", () => {
  const { sources, questions } = mini();
  const plan = project(sources, questions);
  plan.labels = "judge-only fact";
  plan.thresholds = "judge-only threshold";
  const input = backendInput(plan);
  assert.ok(!JSON.stringify(input).includes("judge-only"));
  assert.ok(!JSON.stringify(input).includes("expected_sources"));
  assert.ok(!JSON.stringify(input).includes("limitations"));
});

test("safe source hydration cannot conceal raw backend tombstone exposure", () => {
  const r = row("a");
  r.raw_deleted_text_exposures = ["deleted"];
  const report = score({ labels: [label("a")] }, run([r]));
  assert.equal(report.source_integrity.invalid_revision_hash_or_deleted, 0);
  assert.equal(report.raw_backend_safety.status, "failed");
  assert.equal(report.raw_backend_deleted_source_exposure.detected_count, 1);
});

test("dev labels cannot contain a holdout record", () => {
  const labels = { schema_version: 1, split: "dev", labels: [label("a")] };
  validateLabels(labels, [{ question_id: "a" }], "dev");
  assert.throws(() => validateLabels({ ...labels, labels: [...labels.labels, label("test-only")] }, [{ question_id: "a" }], "dev"), /exactly/);
});

test("measurement requires adjudicated freeze, exact split hashes, and an approved retrieval threshold", () => {
  const hashes = { sources: "s", questions: "q", config: "c", labels: "l" };
  const freeze = { schema_version: 1, status: "frozen", reviewer: "independent", adjudication: "receipt", frozen_at: "2025-03-01T00:00:00Z",
    sha256: { ...hashes, labels: { dev: "l", test: "separate" }, blind_questions: "a".repeat(64) }, dev_retrieval_min_all_sources_top5: 0.8 };
  assert.equal(validateFreeze(freeze, hashes, "dev"), 0.8);
  assert.throws(() => validateFreeze({ ...freeze, status: "draft" }, hashes, "dev"), /freeze/);
  assert.throws(() => validateFreeze(freeze, hashes, "test"), /test labels/);
  assert.throws(() => validateFreeze({ ...freeze, dev_retrieval_min_all_sources_top5: 0 }, hashes, "dev"), /threshold/);
  assert.throws(() => validateFreeze(freeze, { ...hashes, config: "changed" }, "dev"), /config/);
});

test("holdout reservation is exclusive, persists after failure, and ignores code/config changes", () => {
  const directory = mkdtempSync(join(tmpdir(), "kbbench-claim-"));
  try {
    const hashes = { sources: "s", questions: "q", labels: "l", config: "c" };
    claimTest(directory, "b0", hashes, "first");
    assert.throws(() => claimTest(directory, "b0", { ...hashes, config: "changed" }, "retry"), /EEXIST/);
    claimTest(directory, "different-variant", hashes, "other");
  } finally { rmSync(directory, { recursive: true, force: true }); }
});

test("semantic assessment grades rubric items and requires actual recorded answers", () => {
  const r = run([row("a")]);
  r.raw_sha256 = "raw";
  const ratings = { raw_sha256: "raw", reviewer: "independent", assessments: [{ question_id: "a", rationale: "The answer omits the critical fact.",
    state_correct: true, must_say: [false], must_not: [false], critical: [false], unknown: null, true_conflicts: null, false_conflicts: null, freshness: null, stale_decision_as_current: false }] };
  assert.throws(() => score({ labels: [label("a")] }, r, ratings), /recorded answers/);
  r.responses[0].answer = { state: "answered", text: "Incomplete answer" };
  const report = score({ labels: [label("a")] }, r, ratings);
  assert.equal(report.answer_state_and_must_say.numerator, 0);
  assert.equal(report.critical_loss.value, 1);
  assert.equal(report.unknown.status, "not_measured");
  assert.throws(() => score({ labels: [label("a")] }, r, { ...ratings, raw_sha256: "wrong" }), /bind/);
});
