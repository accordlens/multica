// Mechanical transport only. No holdout labels, decryption, calibration or scoring.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { readFileSync, realpathSync, writeFileSync } from "node:fs";
import { isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { ENDPOINT, MODEL, runBatch, sha256 } from "./jev-benchmark.mjs";

export const ISSUE = "01a159d1-e7fd-7921-9fbd-ccb2f5a543f0";
export const FREEZE = "d0e0dc82fe7efbfac93cde3f55a177745c1d5fc4dcf9dc88409664a65d02d019";
export const ATTEMPT_KEY = "jev_holdout_attempt_v1";
const EXECUTOR = "2c600413-6f72-45be-8a5a-11dab680fe2f";
const HASH = /^[a-f0-9]{64}$/;
const save = (path, value) => writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag: "wx", flush: true, mode: 0o600 });
const parse = (value) => typeof value === "string" ? JSON.parse(value) : value;
const exactKeys = (value, keys) => assert.deepEqual(Object.keys(value).sort(), keys.toSorted(), "unexpected input fields");

function nanoUsd(value) {
  assert(typeof value === "string" && /^\d+(\.\d{1,9})?$/.test(value), "invalid USD amount");
  const [whole, fractional = ""] = value.split(".");
  return BigInt(whole) * 1000000000n + BigInt(fractional.padEnd(9, "0"));
}

export function verify(authorizationBytes, requestBytes, snapshot, context) {
  const a = JSON.parse(authorizationBytes);
  const envelope = JSON.parse(requestBytes);
  const evaluation = parse(snapshot.metadata.jev_evaluation_v1);
  const approval = evaluation.execution_authorization;
  assert.equal(approval?.status, "approved", "independent execution authorization is required");
  assert.equal(sha256(authorizationBytes), approval.authorization_sha256, "authorization bytes changed");
  assert.equal(a.version, 1);
  assert.equal(a.issue_id, ISSUE);
  assert.equal(a.freeze_sha256, FREEZE);
  assert.equal(evaluation.config_freeze_sha256, FREEZE);
  assert.equal(a.endpoint, ENDPOINT);
  assert.equal(a.model, MODEL);
  assert.equal(a.authorized, true);
  for (const key of ["executor_task_id", "executor_runtime_id", "code_head", "reservation_id"]) assert(typeof a[key] === "string" && a[key].length > 0, `missing ${key}`);
  assert.equal(approval.controller_agent_id, a.controller_agent_id);
  assert(a.controller_agent_id && a.controller_agent_id !== EXECUTOR, "independent controller required");
  assert.equal(context.agent_id, EXECUTOR);
  assert.equal(a.executor_agent_id, context.agent_id);
  assert.equal(a.executor_task_id, context.task_id);
  assert.equal(a.executor_runtime_id, context.runtime_id);
  assert.equal(a.code_head, context.code_head);
  assert.equal(context.clean, true, "executor checkout must be clean");
  assert.equal(a.launcher_sha256, context.launcher_sha256);
  assert.equal(a.transport_sha256, context.transport_sha256);
  assert.equal(approval.register_root, a.register_root);
  assert(isAbsolute(a.register_root), "fixed absolute register root required");
  assert.equal(realpathSync(a.register_root), a.register_root, "register root must be canonical");
  assert(!Object.hasOwn(snapshot.metadata, ATTEMPT_KEY), "a variant attempt is already recorded");
  for (const field of ["protocol_sha256", "requests_sha256", "scoring_map_sha256", "coverage_sha256", "translations_sha256"]) {
    assert(HASH.test(a[field]), `missing ${field}`);
    assert.equal(approval[field], a[field], "unapproved preparation hash");
  }
  assert.deepEqual(a.approvals, approval.approvals, "review receipts changed");
  exactKeys(a.approvals, ["preparation", "translations", "launcher"]);
  assert(Object.values(a.approvals).every((v) => typeof v === "string" && HASH.test(v)), "all review receipts required");
  assert.equal(sha256(requestBytes), a.requests_sha256, "sealed request envelope changed");
  assert.equal(evaluation.holdout_preparation.requests_sha256, a.requests_sha256);
  assert.equal(evaluation.holdout_preparation.protocol_sha256, a.protocol_sha256);
  assert.equal(envelope.dispatch_authorized, false, "the input seal must not be rewritten to authorize execution");
  const jobs = envelope.requests;
  assert(Array.isArray(jobs) && jobs.length > 0 && jobs.length <= 29, "bounded prepared batch required");
  assert.equal(jobs.length, a.requests.length);
  const ids = new Set();
  jobs.forEach((job, index) => {
    exactKeys(job, ["id", "body"]);
    assert(typeof job.id === "string" && /^[a-zA-Z0-9_-]{1,80}$/.test(job.id), "invalid request ID");
    assert(!ids.has(job.id), "duplicate request ID");
    ids.add(job.id);
    assert.equal(a.requests[index].id, job.id, "request order changed");
    assert.equal(a.requests[index].sha256, sha256(JSON.stringify(job.body)), "request body changed");
    assert(Buffer.byteLength(JSON.stringify(job.body)) <= 20000, "request body exceeds reviewed bound");
    exactKeys(job.body, ["model", "state", "questions"]);
    assert.equal(job.body.model, MODEL);
    exactKeys(job.body.state, ["asked_at", "evidence"]);
    assert(Number.isFinite(Date.parse(job.body.state.asked_at)), "invalid question time");
    assert(Array.isArray(job.body.state.evidence));
    for (const e of job.body.state.evidence) {
      exactKeys(e, ["ref", "issue", "observed_at", "text"]);
      assert(Object.values(e).every((v) => typeof v === "string"));
      assert(Number.isFinite(Date.parse(e.observed_at)) && Date.parse(e.observed_at) <= Date.parse(job.body.state.asked_at), "future evidence");
    }
    exactKeys(job.body.questions, ["c0_support", "c0_contradict", "c1_support", "c1_contradict"]);
    for (const q of Object.values(job.body.questions)) {
      exactKeys(q, ["type", "instructions"]);
      assert.equal(q.type, "noul");
      assert.equal(typeof q.instructions, "string");
    }
  });
  const ledger = parse(snapshot.metadata.cost_accounting_v1);
  const reservation = ledger.reservations.find((r) => r.job_id === a.reservation_id);
  assert.equal(reservation?.status, "reserved", "reservation readback required");
  assert.equal(reservation.requests_sha256, a.requests_sha256);
  assert.equal(reservation.request_count, jobs.length);
  assert.equal(a.max_input_tokens_per_request, 64000);
  const cap = BigInt(jobs.length) * 64000n * 42n; // USD nanounits at 0.042/M input.
  assert.equal(nanoUsd(a.reserved_usd), cap);
  assert.equal(nanoUsd(reservation.reserved_usd), cap);
  assert.equal(nanoUsd(reservation.remaining_reserved_usd), cap, "reservation was already used");
  return { authorization: a, jobs };
}

export async function launch(authorizationBytes, requestBytes, snapshot, context, apiKey, checkpoint, transport = fetch) {
  const { authorization: a, jobs } = verify(authorizationBytes, requestBytes, snapshot, context);
  assert(apiKey, "existing injected credential required");
  assert.equal(typeof checkpoint, "function", "durable checkpoint required");
  // This path is fixed by the controller's authorization, independent of job/input/output names.
  const claimPath = join(a.register_root, `${FREEZE}.attempt.json`);
  const runDirectory = join(a.register_root, `${FREEZE}.receipts`);
  const claim = { version: 1, freeze_sha256: FREEZE, authorization_sha256: sha256(authorizationBytes),
    requests_sha256: a.requests_sha256, executor_task_id: context.task_id, reservation_id: a.reservation_id,
    reserved_usd: a.reserved_usd, request_count: jobs.length, claimed_at: new Date().toISOString() };
  save(claimPath, claim);
  try {
    // A failed/unconfirmed platform write consumes the local claim but sends no request.
    await checkpoint(claim);
    const receipts = await runBatch(jobs, runDirectory, apiKey, transport);
    const outcome = { status: "completed", requests: receipts.length,
      input_tokens: receipts.reduce((n, r) => n + r.response.usage.input_tokens, 0),
      output_tokens: receipts.reduce((n, r) => n + r.response.usage.output_tokens, 0) };
    save(join(a.register_root, `${FREEZE}.outcome.json`), outcome);
    return outcome;
  } catch {
    save(join(a.register_root, `${FREEZE}.outcome.json`), { status: "uncertain", retain_reserved_usd: a.reserved_usd,
      disposition: "No replay or automatic continuation; independently reconcile claim and receipts." });
    throw new Error("Guarded attempt stopped; reservation and evidence retained");
  }
}

async function main() {
  const [authorizationPath, requestsPath, execute] = process.argv.slice(2);
  assert(authorizationPath && requestsPath && (!execute || execute === "--execute"), "usage: node scripts/jev-holdout.mjs AUTHORIZATION.json REQUESTS.json [--execute]");
  const cli = (args) => JSON.parse(execFileSync("multica", [...args, "--output", "json"], { encoding: "utf8" }));
  const snapshot = cli(["issue", "get", ISSUE]);
  const task = cli(["issue", "runs", ISSUE, "--active"]).find((r) => r.id === process.env.MULTICA_TASK_ID);
  assert(task && task.agent_id === process.env.MULTICA_AGENT_ID, "current executor context missing");
  const repo = fileURLToPath(new URL("../", import.meta.url));
  const git = (args) => execFileSync("git", args, { cwd: repo, encoding: "utf8" }).trim();
  const context = { agent_id: task.agent_id, task_id: task.id, runtime_id: task.runtime_id, code_head: git(["rev-parse", "HEAD"]),
    clean: git(["status", "--porcelain"]) === "", launcher_sha256: sha256(readFileSync(fileURLToPath(import.meta.url))),
    transport_sha256: sha256(readFileSync(new URL("./jev-benchmark.mjs", import.meta.url))) };
  const authorizationBytes = readFileSync(authorizationPath);
  const requestBytes = readFileSync(requestsPath);
  const { jobs } = verify(authorizationBytes, requestBytes, snapshot, context);
  if (!execute) { console.log(JSON.stringify({ ready: true, requests: jobs.length, provider_calls: 0 })); return; }
  const checkpoint = async (claim) => {
    const fresh = cli(["issue", "metadata", "list", ISSUE]);
    verify(authorizationBytes, requestBytes, { metadata: fresh }, context);
    cli(["issue", "metadata", "set", ISSUE, "--key", ATTEMPT_KEY, "--type", "string", "--value", JSON.stringify(claim)]);
    assert.deepEqual(parse(cli(["issue", "metadata", "get", ISSUE, "--key", ATTEMPT_KEY])), claim);
  };
  console.log(JSON.stringify(await launch(authorizationBytes, requestBytes, snapshot, context, process.env.TYPESAFE_API_KEY, checkpoint)));
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(() => { console.error("Holdout launch refused or stopped; inspect approved records without redispatch."); process.exitCode = 1; });
}
