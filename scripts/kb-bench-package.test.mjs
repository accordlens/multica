import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { hash } from "./kb-bench.mjs";
import { loadFrozenDev } from "./kb-bench-package.mjs";

function fixture(t, change = () => {}) {
  const directory = mkdtempSync(join(tmpdir(), "kbbench-dev-package-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const sources = { corpus: "independent-package-mini", synthetic: true, shas: { code: "a".repeat(40) }, issues: [{
    id: "MIN-1", description_revisions: [], comments: [{ id: "one", rev: 1, text: "First fact" }, { id: "other", rev: 1, text: "Equivalent fact" }],
    runs: [{ id: "run-mini", sha: "code", result: "FAIL", report: "Failure" }], assignee_history: [],
  }] };
  const questions = { corpus: sources.corpus, questions: [{ id: "dev-mini", split: "dev" }] };
  const labels = { corpus: sources.corpus, split: "dev", labels: [{ question_id: "dev-mini", split: "dev", state: "answered", must_say: ["A fact"], must_not: [],
    must_cite: ["MIN-1/one@rev1"], evidence_groups: [{ id: "fact", any_of: ["MIN-1/one@rev1", "MIN-1/other@rev1"] }] }] };
  const thresholds = { corpus: sources.corpus, status: "FROZEN_OWNER_APPROVED_BEFORE_MEASUREMENT", retrieval_dev: { minimum: 1, denominator: 1, k: 5, eligible_question_ids: ["dev-mini"] } };
  change({ sources, questions, labels, thresholds });
  const bytes = (v) => `${JSON.stringify(v, null, 2)}\n`;
  for (const label of labels.labels) label.sources_sha256 ??= hash(bytes(sources));
  const files = { "sources.json": bytes(sources), "questions.json": bytes(questions), "labels.dev.json": bytes(labels), "thresholds.dev.json": bytes(thresholds), "README.txt": "Independent infrastructure fixture only\n" };
  const manifest = { version: 1, corpus: sources.corpus, split: "dev", contains_test_labels: false, counts: { dev: questions.questions.length, test: 0 },
    decision_comment_id: "independent-mini-receipt", files: Object.fromEntries(Object.entries(files).map(([name, text]) => [name, { bytes: Buffer.byteLength(text), sha256: hash(text) }])) };
  for (const [name, text] of Object.entries(files)) writeFileSync(join(directory, name), text);
  writeFileSync(join(directory, "manifest.json"), bytes(manifest));
  return { directory, manifestHash: hash(bytes(manifest)) };
}

test("pins every dev file and preserves AND/ANY-OF groups without imposing the flat list", (t) => {
  const { directory, manifestHash } = fixture(t);
  const pkg = loadFrozenDev(directory, manifestHash);
  assert.equal(pkg.labels.labels[0].evidence_groups[0].any_of.length, 2);
  assert.equal(pkg.labels.labels[0].evidence_groups[0].any_of[1].content_sha256, hash("Equivalent fact"));
  assert.equal(pkg.hashes.freeze, manifestHash);
  assert.equal(pkg.labels.labels[0].must_cite.length, 1);
});

test("changed manifest and changed source bytes fail before measurement", (t) => {
  const { directory, manifestHash } = fixture(t);
  assert.throws(() => loadFrozenDev(directory, "f".repeat(64)), /manifest hash/);
  writeFileSync(join(directory, "sources.json"), `${readFileSync(join(directory, "sources.json"), "utf8")} `);
  assert.throws(() => loadFrozenDev(directory, manifestHash), /hash\/size/);
});

test("even a rehashed package containing test records is rejected", (t) => {
  const { directory, manifestHash } = fixture(t, ({ questions }) => { questions.questions[0].split = "test"; });
  assert.throws(() => loadFrozenDev(directory, manifestHash), /Test records/);
});

test("extra holdout files and wrong label source bindings are rejected", (t) => {
  const one = fixture(t);
  writeFileSync(join(one.directory, "labels.test.json"), "{}");
  assert.throws(() => loadFrozenDev(one.directory, one.manifestHash), /only the manifest/);
  const two = fixture(t, ({ labels }) => { labels.labels[0].sources_sha256 = "f".repeat(64); });
  assert.throws(() => loadFrozenDev(two.directory, two.manifestHash), /source hash/);
});

test("run SHA and empty-history citations do not fabricate product revisions", (t) => {
  const { directory, manifestHash } = fixture(t, ({ labels }) => {
    labels.labels[0].evidence_groups = [{ id: "run", any_of: [`MIN-1/run-mini@${"a".repeat(40)}`] }, { id: "empty", any_of: ["MIN-1/assignee_history@empty"] }];
  });
  const pkg = loadFrozenDev(directory, manifestHash);
  const [run, empty] = pkg.labels.labels[0].must_cite;
  assert.equal(run.kind, "run");
  assert.equal(run.source_sha, "a".repeat(40));
  assert.equal(run.revision, null);
  assert.equal(empty.kind, "fixture_snapshot");
  assert.equal(empty.revision, null);
  assert.equal(empty.content_sha256, pkg.hashes.sources);
});
