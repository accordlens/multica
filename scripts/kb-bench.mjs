import { createHash, randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { parseArgs } from "node:util";
import { loadFrozenDev } from "./kb-bench-package.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
export const hash = (value) => createHash("sha256").update(value).digest("hex");
const json = (path) => JSON.parse(readFileSync(path, "utf8"));
const fileHash = (path) => hash(readFileSync(path));
const check = (condition, message) => { if (!condition) throw new Error(message); };
const instant = (value) => {
  check(typeof value === "string" && Number.isFinite(Date.parse(value)), `Invalid timestamp: ${value}`);
  return Date.parse(value);
};
const latest = (items = [], at) => [...items].filter((item) => instant(item.at) <= at)
  .sort((a, b) => instant(b.at) - instant(a.at))[0];
const unique = (items, name) => check(new Set(items).size === items.length, `Duplicate ${name}`);
const writeJSON = (path, value, flag = "wx") => writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag, mode: 0o600 });

// This projection uses only explicit timestamps, revisions and memberships.
// It does not infer decisions, conflicts, translations, or answers from text.
export function project(sources, questions, split = "dev", config = {}) {
  check(sources.synthetic === true, "Only explicitly synthetic corpora are accepted");
  check(sources.corpus === questions.corpus, "Corpus IDs differ");
  check(["dev", "test"].includes(split), "split must be dev or test");
  unique(sources.issues.map((i) => i.id), "issue ID");
  unique(questions.questions.map((q) => q.id), "question ID");
  const allSourceIDs = sources.issues.flatMap((i) => [i.id, `${i.id}.description`, `${i.id}.status`, `${i.id}.assignee`, ...(i.comments ?? []).map((c) => c.id), ...(i.runs ?? []).map((r) => r.id)]);
  unique(allSourceIDs, "source ID");
  for (const issue of sources.issues) {
    check(sources.workspaces[issue.workspace], `Unknown workspace ${issue.workspace}`);
    for (const history of [issue.status_history, issue.assignee_history, issue.description_revisions, issue.branch_head_history]) {
      for (const entry of history ?? []) instant(entry.at);
    }
    for (const c of issue.comments ?? []) {
      instant(c.at);
      if (c.edited_at) instant(c.edited_at);
      if (c.deleted_at) instant(c.deleted_at);
      check(Number.isInteger(c.rev) && c.rev > 0, `Invalid revision ${c.id}`);
    }
  }
  const selected = questions.questions.filter((q) => q.split === split);
  check(selected.length > 0, `No ${split} questions`);
  const plans = selected.map((q) => {
    check(typeof q.q === "string" && q.q.trim(), `Empty query ${q.id}`);
    const at = instant(q.asked_at ?? questions.asked_at_default);
    check(Array.isArray(q.asker.ws), `Missing asker workspaces ${q.id}`);
    for (const ws of q.asker.ws) check(sources.workspaces[ws]?.members.includes(q.asker.id), `Invalid asker membership ${q.id}`);
    const events = (config.events_by_question?.[q.id] ?? []).map((id) => {
      const event = sources.events?.find((e) => e.id === id);
      check(event && event.type === "comment_deleted" && instant(event.at) <= at, `Unsupported event ${id}`);
      check(allSourceIDs.includes(event.comment), `Unknown event source ${id}`);
      return event;
    });
    if (config.apply_timestamped_deletions) events.push(...(sources.events ?? []).filter((e) => e.type === "comment_deleted" && instant(e.at) <= at));
    const documents = sources.issues.flatMap((issue) => {
      const description = latest(issue.description_revisions, at);
      if (!description) return [];
      const status = latest(issue.status_history, at);
      const assignee = latest(issue.assignee_history, at);
      const createdAt = Math.min(...issue.description_revisions.filter((d) => instant(d.at) <= at).map((d) => instant(d.at)));
      const updatedAt = Math.max(...[description, status, assignee].filter(Boolean).map((e) => instant(e.at)));
      const comments = (issue.comments ?? []).filter((c) => instant(c.at) <= at).map((c) => {
        let revision = c.rev;
        let text = c.text;
        if (c.revisions) {
          // Corpus v0 has a two-revision edit with only one edit timestamp.
          // More revisions require explicit per-revision times; never guess.
          check(c.revisions.length === 2 && c.edited_at, `Ambiguous revision timeline ${c.id}`);
          const chosen = c.revisions[instant(c.edited_at) <= at ? 1 : 0];
          revision = chosen.rev;
          text = chosen.text;
        }
        check(typeof text === "string", `Missing source text ${c.id}`);
        return { source_id: c.id, revision, text, at: c.at, author: c.author ?? sources.workspaces[issue.workspace].members[0],
          deleted: Boolean((c.deleted_at && instant(c.deleted_at) <= at) || events.some((e) => e.comment === c.id)) };
      });
      return [{ issue_id: issue.id, workspace: issue.workspace, title: issue.title,
        created_at: new Date(createdAt).toISOString(), updated_at: new Date(updatedAt).toISOString(),
        description: { source_id: `${issue.id}.description`, revision: description.rev, text: description.text, at: description.at, author: description.author ?? sources.workspaces[issue.workspace].members[0] },
        status: status?.status ?? "backlog", assignee: assignee?.assignee ?? "", comments }];
    });
    const expectedSources = documents.filter((d) => q.asker.ws.includes(d.workspace)).flatMap((d) => [d.description, ...d.comments.filter((c) => !c.deleted)]
      .map((s) => ({ source_id: s.source_id, revision: s.revision, content_sha256: hash(s.text), workspace: d.workspace })));
    return { question_id: q.id, query: q.q, asker_id: q.asker.id, workspaces: q.asker.ws, expected_sources: expectedSources,
      asked_at: new Date(at).toISOString(), setup: q.setup ?? null, documents };
  });
  return { synthetic: true, corpus: sources.corpus, split, workspaces: sources.workspaces, plans,
    limitations: ["Source packet order: native issue rank, then description and live comments in creation order; first five distinct references",
      "Run artifacts and branch heads are not indexed or fabricated as comments",
      "setup prose is retained, not executed; event mappings must be reviewed in config", "No answer generation, claim extraction, or summary worker"] };
}

export function validateLabels(labels, plans, split) {
  check(labels.schema_version === 1 && labels.split === split, "Use a split-specific normalized labels file (schema_version 1)");
  const ids = plans.map((p) => p.question_id);
  unique(labels.labels.map((l) => l.question_id), "label ID");
  check(labels.labels.length === ids.length && labels.labels.every((l) => ids.includes(l.question_id)), "Labels must cover exactly the selected split; test labels cannot enter dev");
  for (const l of labels.labels) {
    check(["answered", "unknown", "conflict", "denied"].includes(l.state), `Invalid state ${l.question_id}`);
    for (const key of ["must_say", "must_not", "critical"]) check(Array.isArray(l[key]) && l[key].every((v) => typeof v === "string"), `Invalid ${key} ${l.question_id}`);
    check(Array.isArray(l.must_cite), `Invalid must_cite ${l.question_id}`);
    check(l.evidence_groups === undefined || (Array.isArray(l.evidence_groups) && l.evidence_groups.every((g) => typeof g.id === "string" && Array.isArray(g.any_of) && g.any_of.length > 0)), `Invalid evidence groups ${l.question_id}`);
    check(l.alternative_citation_sets === undefined || (l.must_cite.length > 0 && Array.isArray(l.alternative_citation_sets) && l.alternative_citation_sets.every((set) => Array.isArray(set) && set.length > 0)), `Invalid equivalent citation sets ${l.question_id}`);
    for (const c of [l.must_cite, ...(l.alternative_citation_sets ?? []), ...(l.evidence_groups ?? []).map((g) => g.any_of)].flat()) check(typeof c.source_id === "string" && ((Number.isInteger(c.revision) && c.revision > 0) || (["run", "fixture_snapshot"].includes(c.kind) && c.revision === null && /^[a-f0-9]{64}$/.test(c.sources_sha256))) && /^[a-f0-9]{64}$/.test(c.content_sha256) && (!c.source_sha || /^[a-f0-9]{40}$/.test(c.source_sha)), `Invalid citation ${l.question_id}`);
  }
}

export function validateFreeze(freeze, hashes, split) {
  check(freeze.schema_version === 1 && freeze.status === "frozen", "An adjudicated freeze manifest is required before measurement");
  check(typeof freeze.reviewer === "string" && freeze.reviewer.trim() && typeof freeze.adjudication === "string" && freeze.adjudication.trim(), "Freeze needs reviewer and adjudication receipt");
  instant(freeze.frozen_at);
  for (const key of ["sources", "questions", "config"]) check(freeze.sha256?.[key] === hashes[key], `Freeze hash mismatch: ${key}`);
  check(freeze.sha256?.labels?.[split] === hashes.labels, `Freeze hash mismatch: ${split} labels`);
  check(/^[a-f0-9]{64}$/.test(freeze.sha256?.blind_questions ?? ""), "Freeze needs the blind projection hash");
  const threshold = freeze.dev_retrieval_min_all_sources_top5;
  check(typeof threshold === "number" && threshold > 0 && threshold <= 1, "Freeze must approve an explicit dev retrieval threshold (0, 1]");
  return threshold;
}

export function claimTest(directory, variant, corpusHashes, runID) {
  check(/^[a-z0-9][a-z0-9._-]{0,79}$/.test(variant), "Use a stable variant name");
  // The key excludes config/code/freeze timestamps: changing a threshold or
  // head does not silently authorize another holdout run for this variant.
  const key = hash(JSON.stringify([corpusHashes.sources, corpusHashes.questions, corpusHashes.labels, variant]));
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  const path = join(directory, `${key}.json`);
  writeJSON(path, { status: "reserved", run_id: runID, variant, corpus_sha256: corpusHashes, reserved_at: new Date().toISOString() });
  return path;
}

const notMeasured = (reason) => ({ status: "not_measured", reason });
const rate = (numerator, denominator) => denominator ? { status: "measured", numerator, denominator, value: numerator / denominator } : notMeasured("No eligible cases");
const sameCitation = (a, b) => a.source_id === b.source_id && a.revision === b.revision && a.content_sha256 === b.content_sha256 && (a.source_sha ?? null) === (b.source_sha ?? null);
const samples = (values, unit) => values.length ? { status: "measured", unit, samples: values, max: Math.max(...values) } : notMeasured("No instrumentation samples");

// A deliberately simple B0 source selector, separate from the judge. It sees
// only real backend candidates, never labels, thresholds or expected_sources.
export function sourcePacket(row) {
  const seen = new Set();
  return row.hits.slice(0, 5).flatMap((h) => h.sources).filter((c) => {
    const key = JSON.stringify([c.source_id, c.revision, c.content_sha256, c.source_sha ?? null]);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  }).slice(0, 5);
}

export function backendInput(plan) {
  return { synthetic: plan.synthetic, workspaces: plan.workspaces, plans: plan.plans.map((p) => ({
    question_id: p.question_id, query: p.query, asker_id: p.asker_id, workspaces: p.workspaces, documents: p.documents,
  })) };
}

const hasEvidence = (label) => label.evidence_groups ? label.evidence_groups.length > 0 : label.must_cite.length > 0;
const covers = (label, evidence, predicate = sameCitation) => label.evidence_groups
  ? label.evidence_groups.every((g) => g.any_of.some((expected) => evidence.some((actual) => predicate(expected, actual))))
  : [label.must_cite, ...(label.alternative_citation_sets ?? [])].some((set) => set.every((expected) => evidence.some((actual) => predicate(expected, actual))));

export function score(labels, run, ratings) {
  const rows = run.responses;
  unique(rows.map((r) => r.question_id), "response ID");
  check(rows.length === labels.labels.length && labels.labels.every((l) => rows.some((r) => r.question_id === l.question_id)), "Missing or extra raw responses");
  let eligible = 0, found = 0, returned = 0, invalid = 0, leaks = 0, sourceMeasured = 0, sourceFound = 0;
  const sourceCases = [];
  for (const label of labels.labels) {
    const row = rows.find((r) => r.question_id === label.question_id);
    check(Array.isArray(row.hits) && Array.isArray(row.expected_sources), "Malformed backend response");
    const evidence = row.hits.slice(0, 5).flatMap((h) => h.sources).filter((c) => row.allowed_workspaces.includes(c.workspace) && row.expected_sources.some((s) => sameCitation(s, c)));
    if (hasEvidence(label)) {
      eligible++;
      if (covers(label, evidence)) found++;
      const kinds = run.supported_source_kinds ?? [];
      const supported = Array.isArray(row.ranked_sources) && covers(label, kinds, (expected, kind) => !expected.kind || expected.kind === kind);
      if (!supported) sourceCases.push({ question_id: label.question_id, ...notMeasured("Required source kind or source ranking is unsupported by this backend") });
      else {
        const top = row.ranked_sources.slice(0, 5).filter((c) => row.allowed_workspaces.includes(c.workspace) && row.expected_sources.some((s) => sameCitation(s, c)));
        const success = covers(label, top);
        sourceMeasured++;
        if (success) sourceFound++;
        sourceCases.push({ question_id: label.question_id, status: "measured", success });
      }
    }
    for (const citation of row.hits.flatMap((h) => h.sources)) {
      returned++;
      if (!row.expected_sources.some((s) => sameCitation(s, citation))) invalid++;
      if (!row.allowed_workspaces.includes(citation.workspace)) leaks++;
    }
  }
  const retrieval = sourceMeasured === eligible ? rate(sourceFound, eligible) : {
    ...notMeasured("Some eligible questions require unsupported source kinds/ranking; do not count them as misses"),
    supported_subset: rate(sourceFound, sourceMeasured), frozen_eligible_denominator: eligible,
  };
  const deletedExposures = rows.flatMap((r) => r.raw_deleted_text_exposures ?? []);
  const result = {
    evidence_kind: run.evidence_kind, retrieval_unit: "first five distinct source references selected from native issue candidates",
    retrieval_sources_top5: retrieval, retrieval_by_question: sourceCases,
    issue_hydration_coverage_at5: rate(found, eligible),
    dev_retrieval_gate: run.split === "dev" && retrieval.status === "measured" ? {
      threshold: run.dev_retrieval_min_all_sources_top5, passed: retrieval.value >= run.dev_retrieval_min_all_sources_top5,
      cognee_eligible: run.evidence_kind === "real_search_backend" && retrieval.value < run.dev_retrieval_min_all_sources_top5,
    } : { ...notMeasured("The complete frozen dev source metric is required"), cognee_eligible: false },
    source_integrity: { returned, invalid_revision_hash_or_deleted: invalid, acl_leaks: leaks },
    raw_backend_deleted_source_exposure: rows.every((r) => Array.isArray(r.raw_deleted_text_exposures)) ? { status: "measured_lower_bound", detected_count: deletedExposures.length, source_ids: deletedExposures,
      method: "Exact full deleted-source text in raw response strings, excluding texts also present in live sources; zero does not exclude partial snippet exposure" } : notMeasured("No raw snippet detector observations"),
    raw_backend_safety: deletedExposures.length ? { status: "failed", reason: "Raw search disclosed deleted text; filtered hydration is a separate evidence layer" } : notMeasured("Snippets lack source ID/revision; complete safety cannot be established"),
    source_chunk_recall_at_5: notMeasured("No text chunk retrieval; source references are evaluated separately"),
    answer_state_and_must_say: notMeasured("Requires a real answer and a rubric assessment; evidence packets are not answers"),
    answer_citations: notMeasured("Evidence integrity is measured separately from answer citations"),
    critical_loss: notMeasured("Requires rubric assessment"), unknown: notMeasured("Requires rubric assessment"),
    true_conflicts: notMeasured("Requires rubric assessment"), false_conflicts: notMeasured("Requires rubric assessment"),
    freshness: notMeasured("No summary worker is run"), stale_decision_as_current: notMeasured("Requires rubric assessment"),
    tokens: notMeasured("No model/token instrumentation"), backend_cpu: notMeasured("Not instrumented"), backend_rss: notMeasured("Go heap samples are not process RSS"),
    search_latency: samples(rows.flatMap((r) => (r.queries ?? []).map((q) => q.latency_ms)), "ms; membership middleware + search + response encoding, excludes import/hydration"),
    backend_heap: samples(rows.map((r) => r.go_heap_sample_bytes).filter(Number.isFinite), "bytes; sampled Go heap, not peak RAM"),
    coordinator_resources: run.completion ?? notMeasured("No coordinator resource observation"),
  };
  if (ratings) {
    check(rows.every((r) => r.answer && typeof r.answer.text === "string" && typeof r.answer.state === "string"), "Rubric ratings require recorded answers; evidence packets are not answers");
    check(ratings.raw_sha256 === run.raw_sha256 && typeof ratings.reviewer === "string" && ratings.reviewer.trim(), "Ratings must identify the reviewer and bind the raw response hash");
    unique(ratings.assessments.map((a) => a.question_id), "assessment ID");
    check(ratings.assessments.length === rows.length, "Assess every selected question");
    let correct = 0, critical = 0, lost = 0;
    const counts = { unknown: [0, 0], true_conflicts: [0, 0], false_conflicts: [0, 0], freshness: [0, 0], stale_decision_as_current: [0, 0] };
    for (const label of labels.labels) {
      const a = ratings.assessments.find((r) => r.question_id === label.question_id);
      check(a && typeof a.rationale === "string" && a.rationale.trim(), "Assessment needs evidence-based rationale");
      for (const [key, expected] of [["must_say", label.must_say], ["critical", label.critical], ["must_not", label.must_not]]) {
        check(Array.isArray(a[key]) && a[key].length === expected.length && a[key].every((v) => typeof v === "boolean"), `Assessment must grade each ${key} item`);
      }
      check(typeof a.state_correct === "boolean", "Grade state correctness explicitly");
      if (a.state_correct && a.must_say.every(Boolean) && !a.must_not.some(Boolean)) correct++;
      critical += a.critical.length;
      lost += a.critical.filter((present) => !present).length;
      for (const key of Object.keys(counts)) {
        check(a[key] === null || typeof a[key] === "boolean", `Assessment ${key}: boolean or null for inapplicable`);
        if (a[key] !== null) { counts[key][1]++; if (a[key]) counts[key][0]++; }
      }
    }
    result.answer_state_and_must_say = rate(correct, rows.length);
    result.critical_loss = rate(lost, critical);
    for (const [key, [n, d]] of Object.entries(counts)) result[key] = rate(n, d);
    result.semantic_method = { kind: "manual_rubric", reviewer: ratings.reviewer, ratings_sha256: hash(JSON.stringify(ratings)) };
  }
  return result;
}

function command(program, args, options = {}) {
  const child = spawnSync(program, args, { cwd: root, encoding: "utf8", ...options });
  check(!child.error && child.status === 0, `Command failed: ${program} (exit ${child.status ?? "unavailable"})${child.stderr ? `: ${child.stderr.slice(0, 2000)}` : ""}`);
  return child.stdout.trim();
}

function main() {
  const { positionals, values: v } = parseArgs({ allowPositionals: true, options: Object.fromEntries(
    ["sources", "questions", "config", "labels", "freeze", "split", "out", "backend-bin", "variant", "test-ledger", "raw", "ratings", "dev-package", "dev-manifest-sha256"].map((name) => [name, { type: "string" }]),
  ) });
  const mode = positionals[0];
  const pkg = v["dev-package"] ? loadFrozenDev(v["dev-package"], v["dev-manifest-sha256"]) : null;
  if (pkg) check(!v.split || v.split === "dev", "The developer package cannot run test");
  if (pkg) check(!v.sources && !v.questions && !v.labels && !v.config && !v.freeze, "Use only the pinned developer package inputs");
  if (mode === "score") {
    const run = json(v.raw);
    run.raw_sha256 = fileHash(v.raw);
    const labels = pkg?.labels ?? json(v.labels);
    check(run.hashes.labels === (pkg?.hashes.labels ?? fileHash(v.labels)), "Labels differ from recorded run");
    validateLabels(labels, run.responses, run.split);
    writeJSON(v.out, score(labels, run, v.ratings ? json(v.ratings) : undefined));
    return;
  }
  check(mode === "prepare" || mode === "run", "Usage: node scripts/kb-bench.mjs prepare|run|score (see docs/knowledge-benchmark.md)");
  const config = pkg ? { source_selection: "native_issue_order_then_description_and_comment_creation_order_v1", apply_timestamped_deletions: true } : v.config ? json(v.config) : {};
  const plan = project(pkg?.sources ?? json(v.sources), pkg?.questions ?? json(v.questions), v.split ?? "dev", config);
  if (mode === "prepare") { writeJSON(v.out, plan); return; }
  check((pkg || (v.config && v.labels && v.freeze)) && v.variant && v["backend-bin"] && v.out, "run requires frozen inputs, variant, backend-bin and out");
  const labels = pkg?.labels ?? json(v.labels);
  validateLabels(labels, plan.plans, plan.split);
  if (plan.split === "test") check(plan.plans.length >= 25, "The final test requires at least 25 questions");
  const hashes = pkg ? { ...pkg.hashes, config: hash(JSON.stringify(config)) } : Object.fromEntries(["sources", "questions", "config", "labels", "freeze"].map((k) => [k, fileHash(v[k])]));
  const threshold = pkg ? pkg.thresholds.retrieval_dev.minimum / pkg.thresholds.retrieval_dev.denominator : validateFreeze(json(v.freeze), hashes, plan.split);
  const codeSHA = command("git", ["rev-parse", "HEAD"]);
  check(command("git", ["status", "--porcelain"]) === "", "Commit the harness and configuration before measurement; use an output directory outside the checkout");
  const backend = resolve(v["backend-bin"]);
  const version = JSON.parse(command(backend, ["--version"]));
  check(version.code_sha === codeSHA && version.dirty === false, "Rebuild backend from this clean code SHA");
  const runID = randomUUID();
  const runDirectory = resolve(v.out, runID);
  mkdirSync(runDirectory, { recursive: true, mode: 0o700 });
  let claim;
  if (plan.split === "test") {
    check(v["test-ledger"], "test requires an evaluator-owned persistent test-ledger");
    claim = claimTest(resolve(v["test-ledger"]), v.variant, hashes, runID);
  }
  const manifest = { schema_version: 1, run_id: runID, corpus: plan.corpus, variant: v.variant, code_sha: codeSHA,
    started_at: new Date().toISOString(), evidence_kind: "real_search_backend", split: plan.split,
    hashes, config, backend_sha256: fileHash(backend), dev_retrieval_min_all_sources_top5: threshold, limitations: plan.limitations,
    ...(pkg ? { dev_package_manifest: pkg.manifest, frozen_retrieval_spec: pkg.thresholds.retrieval_dev } : {}) };
  writeJSON(join(runDirectory, "manifest.json"), manifest);
  const started = performance.now();
  const cpu = process.cpuUsage();
  const child = spawnSync(backend, [], { cwd: root, encoding: "utf8", input: JSON.stringify(backendInput(plan)),
    maxBuffer: 64 * 1024 * 1024, timeout: 180_000,
    env: { ...process.env, KBBENCH_FIXTURE_MANIFEST: join(runDirectory, "fixture.json") } });
  // Keep failed/uncertain attempts and raw output. Never automatically retry.
  writeFileSync(join(runDirectory, "backend.stdout.json"), child.stdout ?? "", { flag: "wx", mode: 0o600 });
  writeFileSync(join(runDirectory, "backend.stderr.txt"), child.stderr ?? "", { flag: "wx", mode: 0o600 });
  const completion = { run_id: runID, status: !child.error && child.status === 0 ? "completed" : "failed_or_uncertain", exit_code: child.status,
    finished_at: new Date().toISOString(), coordinator_wall_ms: performance.now() - started, coordinator_cpu_microseconds: process.cpuUsage(cpu) };
  writeJSON(join(runDirectory, "completion.json"), completion);
  if (claim) writeJSON(claim, { ...json(claim), ...completion }, "w");
  check(completion.status === "completed", `Backend failed; retained attempt ${runID}. Inspect fixture.json and cleanup before any further run.`);
  const raw = { ...manifest, completion, ...JSON.parse(child.stdout) };
  for (const row of raw.responses) {
    const expected = plan.plans.find((p) => p.question_id === row.question_id);
    check(expected, "Unexpected backend question ID");
    // The independent projection, rather than the backend's own readback,
    // defines which exact revisions/hashes may be exposed at this snapshot.
    row.expected_sources = expected.expected_sources;
    row.allowed_workspaces = expected.workspaces;
    row.ranked_sources = sourcePacket(row);
  }
  const rawPath = join(runDirectory, "raw.json");
  writeJSON(rawPath, raw);
  raw.raw_sha256 = fileHash(rawPath);
  writeJSON(join(runDirectory, "report.json"), score(labels, raw));
  console.log(JSON.stringify({ run_id: runID, status: "completed", split: plan.split }));
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
}
