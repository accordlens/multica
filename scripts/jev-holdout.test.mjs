import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { ENDPOINT, MODEL, sha256 } from "./jev-benchmark.mjs";
import { ATTEMPT_KEY, FREEZE, ISSUE, launch, verify } from "./jev-holdout.mjs";

const hash = (s) => sha256(s);
const bytes = (v) => Buffer.from(JSON.stringify(v));
function fixture(t) {
  const root = realpathSync(mkdtempSync(join(tmpdir(), "jev-guard-")));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const body = { model: MODEL, state: { asked_at: "2026-10-10T12:00:00Z", evidence: [] }, questions:
    Object.fromEntries(["c0_support", "c0_contradict", "c1_support", "c1_contradict"].map((key) => [key, { type: "noul", instructions: "Dummy claim" }])) };
  const requests = { dispatch_authorized: false, requests: ["one", "two", "three"].map((id) => ({ id, body: structuredClone(body) })) };
  const context = { agent_id: "2c600413-6f72-45be-8a5a-11dab680fe2f", task_id: "task", runtime_id: "runtime", code_head: "head",
    clean: true, launcher_sha256: hash("launcher"), transport_sha256: hash("transport") };
  const a = { version: 1, issue_id: ISSUE, freeze_sha256: FREEZE, endpoint: ENDPOINT, model: MODEL, authorized: true,
    controller_agent_id: "independent-controller", executor_agent_id: context.agent_id, executor_task_id: context.task_id,
    executor_runtime_id: context.runtime_id, code_head: context.code_head, launcher_sha256: context.launcher_sha256,
    transport_sha256: context.transport_sha256, register_root: root, protocol_sha256: hash("protocol"),
    scoring_map_sha256: hash("scoring"), coverage_sha256: hash("coverage"), translations_sha256: hash("translations"),
    approvals: { preparation: hash("review-a"), translations: hash("review-b"), launcher: hash("review-c") },
    max_input_tokens_per_request: 64000, reserved_usd: "0.008064", reservation_id: "reserved-job" };
  const f = { root, requests, context, a };
  f.seal = () => {
    f.requestBytes = bytes(requests);
    a.requests_sha256 = hash(f.requestBytes);
    a.requests = requests.requests.map((job) => ({ id: job.id, sha256: hash(JSON.stringify(job.body)) }));
    f.authorizationBytes = bytes(a);
    const approval = { status: "approved", authorization_sha256: hash(f.authorizationBytes), controller_agent_id: a.controller_agent_id,
      register_root: root, approvals: a.approvals };
    for (const key of ["protocol_sha256", "requests_sha256", "scoring_map_sha256", "coverage_sha256", "translations_sha256"]) approval[key] = a[key];
    f.snapshot = { metadata: { jev_evaluation_v1: { config_freeze_sha256: FREEZE, execution_authorization: approval,
      holdout_preparation: { requests_sha256: a.requests_sha256, protocol_sha256: a.protocol_sha256 } },
    cost_accounting_v1: { reservations: [{ job_id: a.reservation_id, status: "reserved", requests_sha256: a.requests_sha256,
      request_count: requests.requests.length, reserved_usd: a.reserved_usd, remaining_reserved_usd: a.reserved_usd }] } } };
  };
  f.seal();
  f.check = () => verify(f.authorizationBytes, f.requestBytes, f.snapshot, context);
  f.launch = (checkpoint, transport) => launch(f.authorizationBytes, f.requestBytes, f.snapshot, context, "dummy-secret", checkpoint, transport);
  f.transport = async () => ({ status: 200, text: async () => JSON.stringify({ model: MODEL,
    answers: Object.fromEntries(Object.keys(body.questions).map((k) => [k, { type: "noul", noul: 0.2 }])), usage: { input_tokens: 10, output_tokens: 4 } }) });
  return f;
}

test("sealed execution gates reject drift before creating a claim or dispatching", (t) => {
  const f = fixture(t);
  assert.equal(f.check().jobs.length, 3);
  assert.deepEqual(readdirSync(f.root), []);
  for (const mutate of [
    (x) => { x.context.task_id = "other-task"; },
    (x) => { x.context.runtime_id = "other-host"; },
    (x) => { x.context.clean = false; },
    (x) => { x.context.transport_sha256 = hash("different"); },
    (x) => { x.snapshot.metadata.jev_evaluation_v1.execution_authorization.status = "pending"; },
    (x) => { delete x.snapshot.metadata.jev_evaluation_v1.execution_authorization.approvals.translations; },
    (x) => { x.snapshot.metadata.cost_accounting_v1.reservations[0].remaining_reserved_usd = "0"; },
    (x) => { x.snapshot.metadata[ATTEMPT_KEY] = {}; },
    (x) => { x.requestBytes = Buffer.concat([x.requestBytes, Buffer.from(" ")]); },
    (x) => { x.authorizationBytes = Buffer.concat([x.authorizationBytes, Buffer.from(" ")]); },
  ]) {
    const x = fixture(t); mutate(x); assert.throws(x.check); assert.deepEqual(readdirSync(x.root), []);
  }
});

test("approved bytes still obey gold-free schema, unique IDs, model, time and payload bounds", (t) => {
  for (const mutate of [
    (f) => { f.requests.requests[0].body.expected = "supported"; },
    (f) => { f.requests.requests[0].body.model = "jev-latest"; },
    (f) => { f.requests.requests[1].id = "one"; },
    (f) => { f.requests.requests[0].id = "../escape"; },
    (f) => { f.requests.requests[0].body.state.evidence.push({ ref: "x", issue: "x", observed_at: "2030-01-01", text: "x" }); },
    (f) => { f.requests.requests[0].body.questions.c0_support.instructions = "x".repeat(20001); },
    (f) => { f.a.reserved_usd = "0.000000001"; },
  ]) { const f = fixture(t); mutate(f); f.seal(); assert.throws(f.check); }
});

test("successful execution checkpoints before dispatch and cannot replay with another authorization", async (t) => {
  const f = fixture(t); let checkpointed = false; let calls = 0;
  const result = await f.launch(async () => { checkpointed = true; }, async (...args) => {
    assert(checkpointed); calls++; return f.transport(...args);
  });
  assert.deepEqual(result, { status: "completed", requests: 3, input_tokens: 30, output_tokens: 12 });
  f.a.reservation_id = "another-job"; f.context.task_id = f.a.executor_task_id = "another-task"; f.seal();
  await assert.rejects(f.launch(async () => {}, f.transport));
  assert.equal(calls, 3);
  assert.equal(JSON.parse(readFileSync(join(f.root, `${FREEZE}.outcome.json`))).status, "completed");
});

test("failed checkpoint consumes the claim and sends no requests", async (t) => {
  const f = fixture(t); let calls = 0;
  await assert.rejects(f.launch(async () => { throw new Error("platform write uncertain"); }, async () => { calls++; }));
  await assert.rejects(f.launch(async () => {}, f.transport));
  assert.equal(calls, 0);
  assert(!readdirSync(f.root).some((name) => name.endsWith(".receipts")));
  assert.equal(JSON.parse(readFileSync(join(f.root, `${FREEZE}.outcome.json`))).retain_reserved_usd, "0.008064");
});

test("missing credential cannot claim an attempt or dispatch", async (t) => {
  const f = fixture(t); let calls = 0;
  await assert.rejects(launch(f.authorizationBytes, f.requestBytes, f.snapshot, f.context, "", async () => {}, async () => { calls++; }));
  assert.equal(calls, 0);
  assert.deepEqual(readdirSync(f.root), []);
});

test("partial/uncertain dispatch retains receipts and the full reservation without continuing", async (t) => {
  const f = fixture(t); let calls = 0;
  await assert.rejects(f.launch(async () => {}, async () => {
    calls++; if (calls === 2) throw new Error("lost connection dummy-secret"); return f.transport();
  }));
  assert.equal(calls, 2);
  const dir = join(f.root, `${FREEZE}.receipts`);
  assert(readdirSync(dir).includes("one.response.json"));
  assert(readdirSync(dir).includes("two.uncertain.json"));
  assert(!readdirSync(dir).some((name) => name.startsWith("three")));
  for (const name of readdirSync(dir)) assert(!readFileSync(join(dir, name), "utf8").includes("dummy-secret"));
  assert.equal(JSON.parse(readFileSync(join(f.root, `${FREEZE}.outcome.json`))).status, "uncertain");
});

test("two OS processes sharing the variant register dispatch exactly one batch", async (t) => {
  const f = fixture(t);
  const dataPath = join(f.root, "dummy.json");
  const callsPath = join(f.root, "calls.txt");
  writeFileSync(dataPath, JSON.stringify({ authorization: f.a, requests: f.requests, snapshot: f.snapshot, context: f.context }));
  const script = `import {readFileSync,appendFileSync} from 'node:fs';
    import {launch} from ${JSON.stringify(new URL("./jev-holdout.mjs", import.meta.url).href)};
    const d=JSON.parse(readFileSync(process.argv[1]));
    const b=x=>Buffer.from(JSON.stringify(x));
    try { await launch(b(d.authorization),b(d.requests),d.snapshot,d.context,'dummy-secret',async()=>{},async()=>{
      appendFileSync(process.argv[2],'call\\n');
      return {status:200,text:async()=>JSON.stringify({model:d.requests.requests[0].body.model,answers:Object.fromEntries(Object.keys(d.requests.requests[0].body.questions).map(k=>[k,{type:'noul',noul:0.1}])),usage:{input_tokens:1,output_tokens:1}})};
    }); } catch { process.exitCode=2; }`;
  const child = () => new Promise((resolve, reject) => {
    const p = spawn(process.execPath, ["--input-type=module", "-e", script, dataPath, callsPath], { stdio: "ignore" });
    p.on("error", reject); p.on("close", resolve);
  });
  const codes = await Promise.all([child(), child()]);
  assert.deepEqual(codes.sort(), [0, 2]);
  assert.equal(readFileSync(callsPath, "utf8").trim().split("\n").length, 3);
});
