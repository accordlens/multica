#!/usr/bin/env node
// Offline contract checks. This module never invokes agents or mutates messages.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { performance } from "node:perf_hooks";
import { pathToFileURL } from "node:url";
import { isDeepStrictEqual } from "node:util";

const root = new URL("../docs/agent-contracts/v1/", import.meta.url);
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const revision = /^[0-9a-f]{40}$/;
const digest = /^[0-9a-f]{64}$/;
const statuses = ["pass", "fail", "blocked", "unknown", "needs_changes"];
const types = ["handoff", "result", "blocker", "question", "review", "requirements_change", "evidence_conflict"];
const roleKeys = ["schema_version", "id", "version", "purpose", "responsibilities", "knowledge", "skills", "decision_boundaries", "goals"];
const contextKeys = ["issue_id", "task_id", "workflow_id", "workflow_definition_sha256", "workflow_unknown_reason", "status_key", "status_source", "source_revision", "source_dirty"];
const scope = "Envelope checks only; no test, independent review, semantic or STE compliance attestation.";

export const sha256 = (value) => createHash("sha256").update(value).digest("hex");
const readJSON = (path) => JSON.parse(readFileSync(path, "utf8"));
const record = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
const nonempty = (value) => typeof value === "string" && value.trim().length > 0;
const listOfText = (value) => Array.isArray(value) && value.length > 0 && value.every(nonempty);
const exactKeys = (value, keys) => record(value) && isDeepStrictEqual(Object.keys(value).sort(), [...keys].sort());

export function loadContracts(directory = root) {
  const bundle = {};
  for (const name of ["dev", "qa", "communication", "glossary", "permissions", "runtime-preview"]) {
    const bytes = readFileSync(new URL(`${name}.json`, directory));
    const value = JSON.parse(bytes);
    if (value.schema_version !== 1 || value.id !== name || value.version !== "1.0.0") {
      throw new Error(`Unsupported contract: ${name}`);
    }
    bundle[name] = { value, sha256: sha256(bytes) };
  }
  for (const name of ["dev", "qa"]) {
    const role = bundle[name].value;
    if (!exactKeys(role, roleKeys) || !nonempty(role.purpose) ||
      !["responsibilities", "knowledge", "skills", "decision_boundaries"].every((key) => listOfText(role[key])) ||
      !Array.isArray(role.goals) || role.goals.length === 0 ||
      !role.goals.every((g) => exactKeys(g, ["id", "behavior", "anti_example", "evaluation"]) && Object.values(g).every(nonempty)) ||
      new Set(role.goals.map((g) => g.id)).size !== role.goals.length) {
      throw new Error(`Invalid role goals or mixed role/configuration fields: ${name}`);
    }
  }
  if (bundle["runtime-preview"].value.activation !== false || bundle.permissions.value.mode !== "preview") {
    throw new Error("This pilot supports preview contracts only");
  }
  return bundle;
}

export function snapshot(role, bundle = loadContracts()) {
  if (!["dev", "qa"].includes(role)) throw new Error("Role must be dev or qa");
  return Object.fromEntries([role, "communication", "glossary", "permissions", "runtime-preview"].map((name) => [
    name === role ? "role" : name,
    { id: name, version: bundle[name].value.version, sha256: bundle[name].sha256 },
  ]));
}

export function preview(role, bundle = loadContracts()) {
  const contract = bundle[role]?.value;
  const refs = snapshot(role, bundle);
  const instructions = [
    `Role ${role} ${contract.version}: ${contract.purpose}`,
    ...contract.responsibilities,
    `Required knowledge: ${contract.knowledge.join(" ")}`,
    `Required capabilities (no automatic skill bindings): ${contract.skills.join("; ")}.`,
    ...contract.goals.map((g) => `${g.id}: ${g.behavior}`),
    ...contract.decision_boundaries,
    ...bundle.communication.value.rules,
  ].join("\n\n");
  return { activation: false, contracts: refs, instructions,
    proposed_tool_policy: bundle.permissions.value[role],
    runtime: bundle["runtime-preview"].value };
}

export function contextErrors(context) {
  const errors = [];
  if (!exactKeys(context, contextKeys)) return ["context: exact fields required"];
  for (const key of ["issue_id", "task_id"]) {
    if (!uuid.test(context[key])) errors.push(`context.${key}: existing UUID required`);
  }
  if (!revision.test(context.source_revision)) errors.push("context.source_revision: exact lowercase Git SHA required");
  if (typeof context.source_dirty !== "boolean") errors.push("context.source_dirty: boolean required");
  if (!nonempty(context.status_key)) errors.push("context.status_key: existing status key required");
  if (!["issue.status", "claim.issue_status", "agent_task_queue.workflow_step"].includes(context.status_source)) {
    errors.push("context.status_source: name the actual field observed");
  }
  if (context.workflow_id === null) {
    if (context.workflow_definition_sha256 !== null || !nonempty(context.workflow_unknown_reason)) {
      errors.push("context.workflow: absent definition requires null hash and an explicit reason");
    }
  } else if (!uuid.test(context.workflow_id) || !digest.test(context.workflow_definition_sha256) || context.workflow_unknown_reason !== null) {
    errors.push("context.workflow: existing UUID, definition hash and null unknown reason required");
  }
  return errors;
}

function proseTokens(text) {
  // Heuristic only. Critical claims have a stronger, exact-text check below.
  return text.match(/`[^`]*`|https?:\/\/\S+|\b[0-9a-f]{8}-[0-9a-f-]{27,}\b|\b[0-9a-f]{40,64}\b|[+-]?\d+(?:[.,:]\d+)*|\b(?:not|no|never|none|without|cannot|\w+n't|unknown|unverified|unconfirmed|confirmed|may|might|must|nie|nigdy|bez|brak|brakuje|nieznane|niezweryfikowane|może|musi)\b/giu) ?? [];
}

function styleWarnings(prose, language, bundle) {
  const warnings = [];
  for (const [field, text] of Object.entries(prose)) {
    const plain = text.replace(/`[^`]*`|https?:\/\/\S+/gu, "");
    if (bundle.glossary.value.prose_warnings.some((term) => plain.toLowerCase().includes(term))) {
      warnings.push(`prose.${field}: ambiguous completion/certainty phrase; manual review`);
    }
    if (plain.split(/[.!?]\s+/u).some((sentence) => sentence.trim().split(/\s+/u).length > bundle.communication.value.sentence_warning_words[language])) {
      warnings.push(`prose.${field}: long sentence; manual review, do not remove facts`);
    }
  }
  return warnings;
}

export function checkMessage(message, { context, role, before, bundle = loadContracts() } = {}) {
  const errors = [];
  const warnings = [];
  const fail = (condition, text) => { if (!condition) errors.push(text); };
  const keys = ["schema_version", "message_type", "language", "context", "contracts", "objective", "result", "claims", "sources", "uncertainty", "expected_response", "prose", "style_revision"];
  if (!exactKeys(message, keys)) errors.push("message: exact envelope fields required");
  const m = record(message) ? message : {};
  fail(m.schema_version === 1, "schema_version: unsupported version");
  fail(types.includes(m.message_type), "message_type: unknown type");
  fail(["pl", "en"].includes(m.language), "language: pl or en required");
  errors.push(...contextErrors(m.context));
  if (context !== undefined) {
    errors.push(...contextErrors(context).map((e) => `expected ${e}`));
    fail(isDeepStrictEqual(m.context, context), "context: does not match independently supplied run/revision");
  }
  const roleName = m.contracts?.role?.id;
  fail(["dev", "qa"].includes(roleName), "contracts.role: dev or qa required");
  if (["dev", "qa"].includes(roleName)) {
    fail(isDeepStrictEqual(m.contracts, snapshot(roleName, bundle)), "contracts: version/hash mismatch");
    if (role !== undefined) fail(role === roleName, "contracts.role: unexpected role");
  }
  if (m.message_type === "review") fail(roleName === "qa", "review: QA role required");
  fail(nonempty(m.objective), "objective: required");
  fail(exactKeys(m.result, ["status"]) && statuses.includes(m.result.status), "result: one stable status required");
  fail(Number.isInteger(m.style_revision) && m.style_revision >= 0 && m.style_revision <= bundle.communication.value.max_style_revisions,
    "style_revision: at most one style revision");
  if (m.style_revision > 0) fail(before !== undefined, "style: a revised message requires its original --before input");
  const proseOK = exactKeys(m.prose, ["outcome", "evidence", "next_step"]) && Object.values(m.prose).every(nonempty);
  fail(proseOK, "prose: outcome, evidence and next_step required");
  if (proseOK && ["en", "pl"].includes(m.language)) warnings.push(...styleWarnings(m.prose, m.language, bundle));

  const sources = Array.isArray(m.sources) ? m.sources : [];
  fail(sources.length > 0, "sources: inspectable evidence required");
  const sourceIDs = new Set();
  for (const source of sources) {
    if (!exactKeys(source, ["id", "kind", "reference", "sha256", "source_revision", "status", "required"])) {
      errors.push("sources: exact fields required"); continue;
    }
    fail(nonempty(source.id) && !sourceIDs.has(source.id), "sources: unique nonempty IDs required");
    sourceIDs.add(source.id);
    fail(["test_report", "artifact", "review", "requirement"].includes(source.kind), "sources.kind: unknown type");
    fail(nonempty(source.reference) && digest.test(source.sha256), "sources: reference and content hash required");
    fail(revision.test(source.source_revision) && source.source_revision === m.context?.source_revision, "sources: stale or missing source revision");
    fail(statuses.includes(source.status) && typeof source.required === "boolean", "sources: stable status and required flag required");
  }
  const claims = Array.isArray(m.claims) ? m.claims : [];
  fail(claims.length > 0, "claims: explicit fact, hypothesis or unknown required");
  const claimIDs = new Set();
  const uncertainIDs = new Set();
  for (const claim of claims) {
    if (!exactKeys(claim, ["id", "kind", "text", "source_ids", "critical"])) {
      errors.push("claims: exact fields required"); continue;
    }
    fail(nonempty(claim.id) && !claimIDs.has(claim.id), "claims: unique nonempty IDs required");
    claimIDs.add(claim.id);
    fail(["fact", "hypothesis", "unknown"].includes(claim.kind) && nonempty(claim.text) && typeof claim.critical === "boolean", "claims: invalid kind, text or critical flag");
    fail(Array.isArray(claim.source_ids) && claim.source_ids.every((id) => sourceIDs.has(id)), "claims: source reference does not resolve");
    if (claim.kind === "fact") fail(claim.source_ids?.length > 0, "claims: facts need sources");
    if (["hypothesis", "unknown"].includes(claim.kind)) uncertainIDs.add(claim.id);
    if (claim.critical) fail(proseOK && m.prose.evidence.includes(claim.text), "claims: critical text missing or changed in prose.evidence");
  }
  fail(Array.isArray(m.uncertainty) && new Set(m.uncertainty).size === m.uncertainty.length &&
    isDeepStrictEqual([...uncertainIDs].sort(), [...(Array.isArray(m.uncertainty) ? m.uncertainty : [])].sort()),
  "uncertainty: list each hypothesis/unknown claim ID exactly once");
  const response = m.expected_response;
  const responseOK = exactKeys(response, ["required", "type", "request"]);
  fail(responseOK, "expected_response: exact fields required");
  if (responseOK) {
    fail(typeof response.required === "boolean" && (response.required
      ? ["review", "fix", "clarification", "human_review"].includes(response.type) && nonempty(response.request)
      : response.type === "none" && response.request === null), "expected_response: contradictory request");
  }
  const status = m.result?.status;
  if (status === "pass") {
    fail(sources.some((s) => s?.required === true), "pass: at least one required source needed");
    fail(!m.context?.source_dirty, "pass: dirty source requires separate verification; clean PASS cannot apply");
    fail(!sources.some((s) => s?.required && s.status !== "pass"), "pass: required source did not pass");
    fail(!claims.some((c) => c?.critical && c.kind !== "fact"), "pass: critical uncertainty remains");
  }
  if (status === "blocked") fail(sources.some((s) => s?.status === "blocked"), "blocked: blocker evidence required");
  if (status === "unknown") fail(uncertainIDs.size > 0, "unknown: uncertainty required");
  if (m.message_type === "blocker") fail(status === "blocked", "blocker: status must be blocked");
  if (m.message_type === "requirements_change") fail(status === "needs_changes", "requirements_change: new scope needs changes");
  if (m.message_type === "evidence_conflict") fail(status !== "pass" && uncertainIDs.size > 0, "evidence_conflict: cannot pass unresolved evidence");
  if (m.message_type === "handoff") fail(responseOK && response.required && response.type === "review", "handoff: explicit review request required");
  if (m.message_type === "review") fail(responseOK && response.required && response.type === (status === "pass" ? "human_review" : "fix"), "review: next acceptance/fix action required");

  if (before !== undefined) {
    const immutable = ({ prose: _prose, style_revision: _style, ...data }) => data;
    fail(record(before) && isDeepStrictEqual(immutable(m), immutable(record(before) ? before : {})), "style: immutable fields changed");
    fail(m.style_revision === before?.style_revision + 1, "style: revision must increase by one");
    if (proseOK) for (const [field, text] of Object.entries(m.prose)) {
      fail(typeof before?.prose?.[field] === "string" && isDeepStrictEqual(proseTokens(text), proseTokens(before.prose[field])),
        `style: protected tokens changed in prose.${field}`);
    }
  }
  // Always retain original critical content, including when checks fail.
  const critical_delivery = { result: m.result ?? null, prose: m.prose ?? null,
    claims: claims.filter((c) => record(c) && c.critical), sources };
  return { ok: errors.length === 0, scope, provenance_compared: context !== undefined, errors, warnings, critical_delivery };
}

export function fixtureMessages(scenario, language, context, bundle = loadContracts()) {
  const example = scenario[language];
  const evidence = scenario.evidence ?? [{ id: "fixture-evidence", status: scenario.source_status, text: example.fact }];
  const claim = { id: "observation", kind: scenario.claim_kind, text: example.fact, source_ids: evidence.map((e) => e.id), critical: true };
  const message = {
    schema_version: 1, message_type: scenario.message_type, language, context,
    contracts: snapshot("dev", bundle), objective: example.objective,
    result: { status: scenario.status }, claims: [claim],
    sources: evidence.map((e) => ({ id: e.id, kind: "test_report", reference: `fixtures.json#${scenario.id}.${scenario.evidence ? `evidence.${e.id}` : language}`,
      sha256: sha256(e.text), source_revision: context.source_revision, status: e.status, required: true })),
    uncertainty: scenario.claim_kind === "fact" ? [] : [claim.id],
    expected_response: { required: true, type: scenario.message_type === "handoff" ? "review" : "clarification", request: example.next_step },
    prose: { outcome: example.outcome, evidence: example.fact, next_step: example.next_step }, style_revision: 0,
  };
  // Deliberately incomplete legacy fixture, not a measured output of a model.
  const baseline = structuredClone(message);
  delete baseline.contracts;
  delete baseline.expected_response;
  baseline.prose.outcome = example.baseline;
  return { baseline, profile: message };
}

export function compareFixtures(context, bundle = loadContracts()) {
  const fixtures = readJSON(new URL("fixtures.json", root));
  const rows = [];
  for (const scenario of fixtures.cases) for (const language of ["en", "pl"]) {
    const pair = fixtureMessages(scenario, language, context, bundle);
    for (const variant of ["baseline", "profile"]) {
      const message = pair[variant];
      const start = performance.now();
      const check = checkMessage(message, { context, role: "dev", bundle });
      rows.push({ case: scenario.id, language, variant,
        checker_errors: check.errors, style_warnings: check.warnings,
        critical_claim_preserved: message.prose.evidence.includes(message.claims[0].text),
        utf8_bytes: Buffer.byteLength(JSON.stringify(message)),
        prose_words: Object.values(message.prose).join(" ").trim().split(/\s+/u).length,
        checker_duration_ms: Number((performance.now() - start).toFixed(3)),
        provider: null, model: null, input_tokens: null, output_tokens: null, model_latency_ms: null,
        cost_usd: null, additional_model_revisions: null, task_alignment_score: null,
        fact_preservation_score: null, handoff_correctness_score: null, semantic_score: null, readability_score: null });
    }
  }
  return { schema_version: 1, kind: "synthetic_fixture_comparison", rows,
    limits: ["Hand-authored baseline/profile pairs; no causal model-quality comparison.",
      "UTF-8 bytes and whitespace words are not provider tokens. Checker time is not model latency.",
      "Provider/model, usage, price, model revisions, semantics and readability were not measured.",
      "Independent QA and human rubric scores are pending. No token or cost saving is inferred."] };
}

export function pilot(context, bundle = loadContracts()) {
  const errors = contextErrors(context);
  if (errors.length) throw new Error(errors.join("; "));
  const scenario = readJSON(new URL("fixtures.json", root)).cases.find((c) => c.id === "success");
  const dev = fixtureMessages(scenario, "en", context, bundle).profile;
  const qa = structuredClone(dev);
  qa.message_type = "review";
  qa.contracts = snapshot("qa", bundle);
  qa.expected_response = { required: true, type: "human_review", request: "Review the evidence and the exact head before acceptance." };
  qa.prose.outcome = "The synthetic QA fixture repeated the recorded contract checks.";
  qa.prose.next_step = qa.expected_response.request;
  const devCheck = checkMessage(dev, { context, role: "dev", bundle });
  const qaCheck = checkMessage(qa, { context, role: "qa", bundle });
  const pl = fixtureMessages(scenario, "pl", context, bundle).profile;
  const plCheck = checkMessage(pl, { context, role: "dev", bundle });
  const passed = [devCheck, qaCheck, plCheck].every((check) => check.ok);
  return { schema_version: 1, kind: "offline_synthetic_pilot", context,
    envelope_status: passed ? "pass" : "fail",
    independent_review: "pending; a fixture is not independent QA",
    steps: [
      { kind: "dev_fixture", message: dev, check: devCheck },
      { kind: "qa_fixture", task_id: null, message: qa, check: qaCheck },
      { kind: "polish_report", message: pl, check: plCheck },
    ],
    report_pl: passed
      ? "Pilot syntetyczny zakończony. Kontrakty Dev/QA i polski raport przeszły kontrolę struktury. Niezależne review PR oraz ocena języka pozostają wymagane. Nie zmieniono konfiguracji agentów."
      : "Pilot syntetyczny wykrył błąd kontraktu. Raport zachowuje pierwotne dowody i diagnostykę. Nie traktować go jako zaliczonej kontroli ani akceptacji.",
    comparison: compareFixtures(context, bundle),
  };
}

export function receipt(runs, reportBytes, artifactBytes, bundle = loadContracts()) {
  const report = JSON.parse(reportBytes);
  const artifact = JSON.parse(artifactBytes);
  const run = (Array.isArray(runs) ? runs : [runs]).find((r) => r?.id === report.context?.task_id);
  if (!run || run.issue_id !== report.context?.issue_id || !uuid.test(run.agent_id) || !uuid.test(run.runtime_id)) {
    throw new Error("Receipt needs the existing issue/run/agent/runtime returned by the API");
  }
  if (report.kind !== "offline_synthetic_pilot" || contextErrors(report.context).length ||
    report.envelope_status !== "pass" || !Array.isArray(report.steps) || report.steps.length !== 3 ||
    report.steps.some((step, index) => !checkMessage(step.message, { context: report.context, role: index === 1 ? "qa" : "dev", bundle }).ok)) {
    throw new Error("Receipt needs a passing offline pilot for this context and these contract versions");
  }
  if (!isDeepStrictEqual(artifact, { activation: false, profiles: { dev: preview("dev", bundle), qa: preview("qa", bundle) } })) {
    throw new Error("Receipt needs the exact inactive Dev/QA previews");
  }
  return { schema_version: 1,
    metadata_key: `role_contracts_v1.${run.id}.${report.context.source_revision.slice(0, 8)}`,
    task_id: run.id, issue_id: run.issue_id,
    agent_id: run.agent_id, runtime_id: run.runtime_id, source_revision: report.context.source_revision,
    mode: "task_scoped_preview_and_synthetic_pilot",
    effective_runtime_configuration_attested: false,
    contracts: Object.fromEntries(Object.entries(bundle).map(([name, value]) => [name, { version: value.value.version, sha256: value.sha256 }])),
    report: { sha256: sha256(reportBytes), attachment_id: null },
    artifact: { sha256: sha256(artifactBytes), attachment_id: null },
    independent_qa_task_id: null,
    unknowns: ["Independent PR review is pending. Null attachment IDs are filled only after upload returns them.",
      "Provider/model experiment, token savings and effective persistent role instructions are not attested."] };
}

function cli(args) {
  const [command, file, ...rest] = args;
  switch (command) {
    case "preview":
      if (rest.length) break;
      return file === "all" ? { activation: false, profiles: { dev: preview("dev"), qa: preview("qa") } } : preview(file);
    case "check": {
      if (!file || rest[0] !== "--context" || !rest[1] || (rest.length !== 2 && (rest.length !== 4 || rest[2] !== "--before"))) break;
      const result = checkMessage(readJSON(file), { context: readJSON(rest[1]), before: rest[3] ? readJSON(rest[3]) : undefined });
      if (!result.ok) process.exitCode = 1;
      return result;
    }
    case "pilot": {
      if (!file || rest.length) break;
      const result = pilot(readJSON(file));
      if (result.steps.some((step) => !step.check.ok)) process.exitCode = 1;
      return result;
    }
    case "receipt":
      if (!file || rest.length !== 2) break;
      return receipt(readJSON(file), readFileSync(rest[0]), readFileSync(rest[1]));
  }
  throw new Error("Usage: role-contracts.mjs preview dev|qa|all; check message.json --context context.json [--before old.json]; pilot context.json; receipt runs.json report.json profiles.json");
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { console.log(JSON.stringify(cli(process.argv.slice(2)), null, 2)); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}
