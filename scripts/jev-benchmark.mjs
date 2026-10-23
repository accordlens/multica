// Optional checker experiment: external dev package only, never the final holdout.
import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const ENDPOINT = "https://api.typesafe.ai/v1/systemone";
export const MODEL = "jev-1.13.0";
export const DEV_MANIFEST = "c2d730e7d0304e53bd027ecc94e1b46ef455cd08f8b44147124de27112236234";
export const sha256 = (value) => createHash("sha256").update(value).digest("hex");
const json = (file) => JSON.parse(readFileSync(file, "utf8"));
const save = (file, value) => writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`, { flag: "wx", flush: true, mode: 0o600 });

export function loadDev(directory, manifestHash = DEV_MANIFEST) {
  const bytes = readFileSync(join(directory, "manifest.json"));
  assert.equal(sha256(bytes), manifestHash, "dev manifest fingerprint changed");
  const manifest = JSON.parse(bytes);
  assert.equal(manifest.split, "dev");
  assert.deepEqual(manifest.counts, { dev: 7, test: 0 });
  const allowed = ["README.txt", "labels.dev.json", "questions.json", "sources.json", "thresholds.dev.json"];
  assert.deepEqual(Object.keys(manifest.files).sort(), allowed.sort());
  for (const [name, expected] of Object.entries(manifest.files)) {
    const raw = readFileSync(join(directory, name));
    assert.equal(raw.length, expected.bytes, `${name}: size mismatch`);
    assert.equal(sha256(raw), expected.sha256, `${name}: hash mismatch`);
  }
  const questions = json(join(directory, "questions.json"));
  const sources = json(join(directory, "sources.json"));
  assert.equal(sources.synthetic, true, "only the authorized synthetic package may leave this process");
  assert.equal(questions.questions.length, 7);
  assert(questions.questions.every((q) => q.split === "dev"));
  return { questions, sources, sourcesHash: manifest.files["sources.json"].sha256 };
}

// Resolve only explicit oracle evidence. This is not retrieval or product ACL validation.
export function evidenceAt(pkg, question, ref) {
  const match = /^([^/]+)\/([^@]+)@(.+)$/.exec(ref);
  assert(match, "invalid source locator");
  const [, issueId, itemId, revision] = match;
  const issue = pkg.sources.issues.find((i) => i.id === issueId);
  assert(issue, "missing source issue");
  assert(question.asker.ws.includes(issue.workspace), "source outside asker workspace");
  assert(pkg.sources.workspaces[issue.workspace].members.includes(question.asker.id), "asker is not a member");
  const at = question.asked_at ?? pkg.questions.asked_at_default;
  assert(Number.isFinite(Date.parse(at)), "invalid question time");
  const available = (time) => Number.isFinite(Date.parse(time)) && Date.parse(time) <= Date.parse(at);
  let text;
  let observedAt;
  if (itemId === "assignee_history" && revision === "empty") {
    assert.equal(issue.assignee_history.filter((x) => available(x.at)).length, 0);
    text = "No assignee is recorded in the available assignment history.";
    observedAt = at;
  } else {
    const descriptions = itemId === "description_revisions";
    const collection = descriptions ? issue.description_revisions : itemId.startsWith("run-") ? issue.runs : issue.comments;
    const item = collection?.find((x) => descriptions ? `rev${x.rev}` === revision : x.id === itemId);
    assert(item && available(item.at), "missing or future source");
    assert(!item.deleted_at || (Number.isFinite(Date.parse(item.deleted_at)) && !available(item.deleted_at)), "source has been deleted");
    observedAt = item.at;
    if (itemId.startsWith("run-")) {
      assert.equal(pkg.sources.shas[item.sha], revision, "run SHA mismatch");
      text = `${item.command}: ${item.result} on ${revision}. ${item.report}`;
    } else {
      const effectiveRev = item.edited_at && !available(item.edited_at) ? 1 : item.rev;
      assert.equal(`rev${effectiveRev}`, revision, "source revision unavailable at question time");
      if (effectiveRev > 1) observedAt = item.edited_at;
      if (descriptions) {
        const latest = issue.description_revisions.filter((x) => available(x.at)).toSorted((a, b) => Date.parse(a.at) - Date.parse(b.at)).at(-1);
        assert.equal(latest.rev, item.rev, "obsolete description revision");
      }
      text = item.revisions?.find((r) => r.rev === effectiveRev)?.text ?? item.text;
    }
  }
  assert.equal(typeof text, "string");
  return { ref, issue: issueId, observed_at: observedAt, text };
}

export function prepare(pkg, fixture) {
  assert.equal(fixture.split, "dev");
  assert.equal(fixture.sources_sha256, pkg.sourcesHash);
  assert(fixture.cases.length > 0 && fixture.cases.length <= 9, "bounded dev batch only");
  const ids = new Set();
  return fixture.cases.map((item) => {
    assert(/^[a-zA-Z0-9_-]+$/.test(item.id), "invalid case ID");
    assert(!ids.has(item.id), "duplicate case ID");
    ids.add(item.id);
    const question = pkg.questions.questions.find((q) => q.id === item.question_id);
    assert(question?.split === "dev", "case references a non-dev question");
    assert.equal(item.claims.length, 2, "claim and controlled negation required");
    const evidence = item.refs.map((ref) => evidenceAt(pkg, question, ref));
    if (item.translation) {
      assert.equal(item.translation.review_status, "pending_independent_review");
      assert.deepEqual(Object.keys(item.translation.text_by_ref).sort(), item.refs.toSorted());
      for (const entry of evidence) {
        assert.equal(typeof item.translation.text_by_ref[entry.ref], "string");
        entry.text = item.translation.text_by_ref[entry.ref];
      }
    }
    const questions = {};
    item.claims.forEach((claim, i) => {
      assert.equal(typeof claim.text, "string");
      assert(["supported", "contradicted", "unknown", "conflict"].includes(claim.expected));
      const scope = `Evaluate only the supplied evidence, for the same subject, version and time. Do not use outside knowledge. Missing information alone is neither support nor contradiction. Statement: ${claim.text}`;
      questions[`c${i}_support`] = { type: "noul", instructions: `${scope}\nDoes the evidence explicitly support this statement?` };
      questions[`c${i}_contradict`] = { type: "noul", instructions: `${scope}\nDoes the evidence explicitly contradict this statement?` };
    });
    // Deliberate allowlist: never send labels, expected states, rubrics or thresholds.
    const body = { model: MODEL, state: { asked_at: question.asked_at ?? pkg.questions.asked_at_default, evidence }, questions };
    assert(Buffer.byteLength(JSON.stringify(body)) <= 20000, "request exceeds reviewed payload bound");
    return { id: item.id, body };
  });
}

export function validateResponse(body, response) {
  assert.equal(response.model, MODEL, "resolved model changed");
  assert.deepEqual(Object.keys(response.answers).sort(), Object.keys(body.questions).sort(), "answer IDs changed");
  for (const answer of Object.values(response.answers)) {
    assert.equal(answer.type, "noul");
    assert(Number.isFinite(answer.noul) && answer.noul >= 0 && answer.noul <= 1, "invalid probability");
  }
  for (const field of ["input_tokens", "output_tokens"]) assert(Number.isSafeInteger(response.usage[field]) && response.usage[field] >= 0, "invalid token usage");
  assert(response.usage.input_tokens <= 64000, "request exceeded reserved context bound");
}

export function classify(support, contradict, supportCutoff, contradictCutoff) {
  const s = support >= supportCutoff;
  const c = contradict >= contradictCutoff;
  return s ? (c ? "conflict" : "supported") : (c ? "contradicted" : "unknown");
}

export function calibrate(fixture, receipts) {
  const rows = fixture.cases.flatMap((item) => item.claims.map((claim, i) => {
    const receipt = receipts.find((r) => r.case_id === item.id);
    return { case_id: item.id, question_id: item.question_id, language: item.language, translation_pending: !!item.translation,
      claim: claim.text, expected: claim.expected, refs: item.refs,
      support: receipt.response.answers[`c${i}_support`].noul, contradict: receipt.response.answers[`c${i}_contradict`].noul };
  }));
  const grid = [0.5, 0.6, 0.7, 0.8, 0.9, 0.95];
  const candidates = grid.flatMap((s) => grid.map((c) => {
    const predicted = rows.map((r) => classify(r.support, r.contradict, s, c));
    return { support_cutoff: s, contradict_cutoff: c,
      unsafe: rows.filter((r, i) => predicted[i] !== r.expected && predicted[i] !== "unknown").length,
      correct: rows.filter((r, i) => predicted[i] === r.expected).length };
  })).sort((a, b) => a.unsafe - b.unsafe || b.correct - a.correct || b.support_cutoff - a.support_cutoff || b.contradict_cutoff - a.contradict_cutoff);
  const selected = candidates[0];
  return { kind: "dev_optional_checker_with_oracle_evidence", selection: "minimize unsafe decisions, maximize correct, then prefer higher support/contradict cutoffs", selected, candidates,
    rows: rows.map((r) => ({ ...r, predicted: classify(r.support, r.contradict, selected.support_cutoff, selected.contradict_cutoff) })),
    limitations: ["Not retrieval, source extraction, product citation generation or final-test accuracy.", "Negated pairs and translations are correlated; only seven distinct dev questions.", "Translations and checker gold are author-prepared; independent review remains required."] };
}

export async function runBatch(jobs, directory, apiKey, transport = fetch) {
  assert(apiKey, "TYPESAFE_API_KEY is required");
  // A used or uncertain run directory cannot be silently retried or overwritten.
  mkdirSync(directory);
  const receipts = [];
  const write = (name, value) => save(join(directory, name), JSON.parse(JSON.stringify(value).replaceAll(apiKey, "[REDACTED]")));
  for (const job of jobs) {
    assert(/^[a-zA-Z0-9_-]+$/.test(job.id), "invalid case ID");
    const operation = { operation_id: randomUUID(), case_id: job.id, started_at: new Date().toISOString(), request_sha256: sha256(JSON.stringify(job.body)), request: job.body };
    write(`${job.id}.started.json`, operation);
    const start = performance.now();
    const cpu = process.cpuUsage();
    let receipt;
    try {
      const response = await transport(ENDPOINT, { method: "POST", redirect: "error", signal: AbortSignal.timeout(30000), headers: { Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" }, body: JSON.stringify(job.body) });
      receipt = { ...operation, status: response.status };
      const raw = await response.text();
      receipt = { ...operation, status: response.status, latency_ms: performance.now() - start, client_cpu_microseconds: process.cpuUsage(cpu), client_max_rss_kib: process.resourceUsage().maxRSS, raw_response: raw };
      assert(Buffer.byteLength(raw) <= 65536, "response too large");
      receipt.response = JSON.parse(raw);
      assert.equal(response.status, 200, "API request failed; do not retry automatically");
      validateResponse(job.body, receipt.response);
      write(`${job.id}.response.json`, receipt);
      receipts.push(receipt);
    } catch (error) {
      write(`${job.id}.uncertain.json`, { ...operation, ...(receipt ?? {}), error: error.message, disposition: "retain reservation and inspect; no automatic retry" });
      throw new Error(`Stopped at ${job.id}; inspect durable receipt before any new dispatch`);
    }
  }
  return receipts;
}

async function main() {
  const [directory, fixturePath, output, execute] = process.argv.slice(2);
  assert(directory && fixturePath && output && (!execute || execute === "--execute"), "usage: node scripts/jev-benchmark.mjs DEV_DIR CLAIMS.json NEW_OUTPUT_DIR [--execute]");
  const pkg = loadDev(directory);
  const fixture = json(fixturePath);
  const jobs = prepare(pkg, fixture);
  if (!execute) {
    console.log(JSON.stringify({ dry_run: true, cases: jobs.length, model: MODEL, max_input_tokens_per_call: 64000, max_batch_list_price_usd: jobs.length * 64000 * 0.042 / 1e6 }));
    return;
  }
  const receipts = await runBatch(jobs, output, process.env.TYPESAFE_API_KEY);
  save(join(output, "dev-score.json"), calibrate(fixture, receipts));
  save(join(output, "run-config.json"), { model: MODEL, endpoint: ENDPOINT, split: "dev", fixture_sha256: sha256(readFileSync(fixturePath)), dev_manifest_sha256: DEV_MANIFEST, sources_sha256: pkg.sourcesHash, code_head: execFileSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).trim(), runner_sha256: sha256(readFileSync(fileURLToPath(import.meta.url))), retries: 0, cases: jobs.length, requests: jobs.map((j) => ({ id: j.id, sha256: sha256(JSON.stringify(j.body)) })), provider_cpu_ram: null, billing_invoice: null });
  console.log(JSON.stringify({ completed: receipts.length, output, input_tokens: receipts.reduce((n, r) => n + r.response.usage.input_tokens, 0) }));
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => { console.error(error.message); process.exitCode = 1; });
}
