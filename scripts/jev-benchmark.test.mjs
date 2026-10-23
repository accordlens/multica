import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { MODEL, ENDPOINT, evidenceAt, prepare, validateResponse, classify, calibrate, runBatch } from "./jev-benchmark.mjs";

const at = "2026-10-10T12:00:00Z";
const pkg = {
  sourcesHash: "fixture-hash", questions: { asked_at_default: at, questions: [{ id: "Q", split: "dev", asker: { id: "user", ws: ["public"] } }] },
  sources: { workspaces: { public: { members: ["user"] }, private: { members: ["other"] } }, issues: [
    { id: "I", workspace: "public", comments: [{ id: "c", rev: 1, at, text: "The flag is enabled." }, { id: "deleted", rev: 1, at, deleted_at: at, text: "Gone." }, { id: "future", rev: 1, at: "2026-10-11T00:00:00Z", text: "Later." }] },
    { id: "P", workspace: "private", comments: [{ id: "secret", rev: 1, at, text: "Private." }] },
  ] },
};
const fixture = { split: "dev", sources_sha256: "fixture-hash", cases: [{ id: "one", question_id: "Q", language: "EN", refs: ["I/c@rev1"], claims: [{ text: "The flag is enabled.", expected: "supported" }, { text: "The flag is disabled.", expected: "contradicted" }] }] };
const body = prepare(pkg, fixture)[0].body;
const response = () => ({ model: MODEL, answers: Object.fromEntries(Object.keys(body.questions).map((k) => [k, { type: "noul", noul: k === "c0_support" || k === "c1_contradict" ? 0.99 : 0.01 }])), usage: { input_tokens: 300, output_tokens: 40 } });

test("only authorized, extant, temporal source revisions reach the API; gold stays out", () => {
  const jobs = prepare(pkg, fixture);
  assert.equal(jobs[0].body.state.evidence[0].text, "The flag is enabled.");
  assert(!JSON.stringify(jobs[0].body).includes("expected"));
  assert(!JSON.stringify(jobs[0].body).includes("fixture-hash"));
  const q = pkg.questions.questions[0];
  for (const ref of ["P/secret@rev1", "I/deleted@rev1", "I/future@rev1", "I/c@rev2"]) assert.throws(() => evidenceAt(pkg, q, ref));
  assert.throws(() => prepare(pkg, { ...fixture, split: "test" }));
  assert.throws(() => prepare(pkg, { ...fixture, cases: [{ ...fixture.cases[0], question_id: "TEST" }] }));
});

test("model/schema drift, invalid probabilities and usage fail closed", () => {
  validateResponse(body, response());
  for (const mutate of [
    (r) => { r.model = "jev-latest"; },
    (r) => { r.answers.c0_support.noul = 1.01; },
    (r) => { r.answers.c0_support.noul = null; },
    (r) => { delete r.answers.c0_support; },
    (r) => { r.usage.input_tokens = -1; },
    (r) => { r.usage.input_tokens = 64001; },
  ]) { const r = response(); mutate(r); assert.throws(() => validateResponse(body, r)); }
});

test("absence and contradictory evidence are distinct, including both-high adjudication", () => {
  assert.equal(classify(0.1, 0.1, 0.8, 0.8), "unknown");
  assert.equal(classify(0.9, 0.1, 0.8, 0.8), "supported");
  assert.equal(classify(0.1, 0.9, 0.8, 0.8), "contradicted");
  assert.equal(classify(0.9, 0.9, 0.8, 0.8), "conflict");
  const score = calibrate(fixture, [{ case_id: "one", response: response() }]);
  assert.equal(score.selected.correct, 2);
  assert.equal(score.selected.unsafe, 0);
});

test("API calls have a fixed origin, no redirects/retries, create-only receipts and no secret logs", async (t) => {
  const parent = mkdtempSync(join(tmpdir(), "jev-bench-"));
  t.after(() => rmSync(parent, { recursive: true, force: true }));
  const output = join(parent, "run");
  let calls = 0;
  const transport = async (url, options) => {
    calls++;
    assert.equal(url, ENDPOINT);
    assert.equal(options.redirect, "error");
    assert.equal(options.headers.Authorization, "Bearer test-secret");
    return { status: 200, text: async () => JSON.stringify(response()) };
  };
  await runBatch(prepare(pkg, fixture), output, "test-secret", transport);
  await assert.rejects(runBatch(prepare(pkg, fixture), output, "test-secret", transport));
  assert.equal(calls, 1);
  for (const file of readdirSync(output)) assert(!readFileSync(join(output, file), "utf8").includes("test-secret"));
});

test("an uncertain paid attempt keeps evidence and stops the rest of the batch", async (t) => {
  const parent = mkdtempSync(join(tmpdir(), "jev-uncertain-"));
  t.after(() => rmSync(parent, { recursive: true, force: true }));
  const output = join(parent, "run");
  let calls = 0;
  await assert.rejects(runBatch([{ id: "one", body }, { id: "two", body }], output, "test-secret", async () => {
    calls++;
    throw new Error("connection closed test-secret");
  }));
  assert.equal(calls, 1);
  assert(readdirSync(output).includes("one.uncertain.json"));
  assert(!readFileSync(join(output, "one.uncertain.json"), "utf8").includes("test-secret"));
});

for (const mode of ["oversized body", "body read failure"]) {
  test(`a rejected response retains received evidence after ${mode}`, async (t) => {
    const parent = mkdtempSync(join(tmpdir(), "jev-rejected-"));
    t.after(() => rmSync(parent, { recursive: true, force: true }));
    const output = join(parent, "run");
    const raw = "x".repeat(65537);
    let calls = 0;
    await assert.rejects(runBatch([{ id: "one", body }, { id: "two", body }], output, "test-secret", async () => {
      calls++;
      return { status: 502, text: async () => {
        if (mode === "body read failure") throw new Error("body read failed test-secret");
        return raw;
      } };
    }));
    const bytes = readFileSync(join(output, "one.uncertain.json"), "utf8");
    const receipt = JSON.parse(bytes);
    assert.equal(receipt.status, 502);
    assert.equal(receipt.raw_response, mode === "oversized body" ? raw : undefined);
    assert.match(receipt.error, mode === "oversized body" ? /response too large/ : /body read failed/);
    assert(!bytes.includes("test-secret"));
    assert.equal(calls, 1);
    assert.deepEqual(readdirSync(output).sort(), ["one.started.json", "one.uncertain.json"]);
  });
}
