import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

const digest = (bytes) => createHash("sha256").update(bytes).digest("hex");
const requireValue = (condition, message) => { if (!condition) throw new Error(message); };
const readJSON = (path) => JSON.parse(readFileSync(path, "utf8"));

// Parse provenance mechanically. Expected answers and evidence groups never
// enter the backend request; this module is exclusively on the judge side.
function citationFor(locator, sources, sourcesHash) {
  const match = /^([^/]+)\/(.+)@([^@]+)$/.exec(locator);
  requireValue(match, `Invalid frozen locator: ${locator}`);
  const [, issueID, key, qualifier] = match;
  const issue = sources.issues.find((i) => i.id === issueID);
  requireValue(issue, `Unknown locator issue: ${locator}`);
  const common = { fixture_locator: locator, sources_sha256: sourcesHash };
  const comment = (issue.comments ?? []).find((c) => c.id === key);
  if (comment || key === "description") {
    requireValue(/^rev[1-9]\d*$/.test(qualifier), `Locator requires a revision: ${locator}`);
    const revision = Number(qualifier.slice(3));
    const entry = comment ? (comment.revisions?.find((r) => r.rev === revision) ?? (comment.rev === revision ? comment : null))
      : issue.description_revisions.find((r) => r.rev === revision);
    requireValue(entry && typeof entry.text === "string", `Unknown source revision: ${locator}`);
    return { ...common, source_id: comment ? key : `${issueID}.description`, kind: comment ? "comment" : "description",
      revision, content_sha256: digest(entry.text) };
  }
  const run = (issue.runs ?? []).find((r) => r.id === key);
  if (run) {
    const sha = sources.shas[run.sha] ?? run.sha;
    requireValue(/^[a-f0-9]{40}$/.test(sha) && qualifier === sha, `Run SHA mismatch: ${locator}`);
    return { ...common, source_id: key, kind: "run", revision: null, source_sha: sha, content_sha256: digest(JSON.stringify(run)) };
  }
  const history = issue[key];
  requireValue(Array.isArray(history) && (qualifier === "empty" ? history.length === 0 : history.some((r) => r.at === qualifier)), `Unknown field/event locator: ${locator}`);
  // The frozen protocol binds field/event locators to the source file hash.
  // A fixture snapshot is not a product entity with an invented revision.
  return { ...common, source_id: `${issueID}/${key}@${qualifier}`, kind: "fixture_snapshot", revision: null, content_sha256: sourcesHash };
}

export function loadFrozenDev(directory, expectedManifestHash) {
  requireValue(/^[a-f0-9]{64}$/.test(expectedManifestHash ?? ""), "Supply the trusted dev manifest SHA256 from the issue");
  const manifestPath = join(directory, "manifest.json");
  requireValue(digest(readFileSync(manifestPath)) === expectedManifestHash, "Dev manifest hash mismatch");
  const manifest = readJSON(manifestPath);
  requireValue(manifest.version === 1 && manifest.split === "dev" && manifest.contains_test_labels === false && manifest.decision_comment_id, "Not an adjudicated dev-only package");
  const names = ["README.txt", "labels.dev.json", "questions.json", "sources.json", "thresholds.dev.json"];
  requireValue(JSON.stringify(Object.keys(manifest.files).sort()) === JSON.stringify([...names].sort()), "Unexpected files in dev manifest");
  requireValue(JSON.stringify(readdirSync(directory).sort()) === JSON.stringify([...names, "manifest.json"].sort()), "Dev directory must contain only the manifest's dev files");
  for (const name of names) {
    const bytes = readFileSync(join(directory, name));
    requireValue(bytes.length === manifest.files[name].bytes && digest(bytes) === manifest.files[name].sha256, `Dev input hash/size mismatch: ${name}`);
  }
  const sources = readJSON(join(directory, "sources.json"));
  const questions = readJSON(join(directory, "questions.json"));
  const originalLabels = readJSON(join(directory, "labels.dev.json"));
  const thresholds = readJSON(join(directory, "thresholds.dev.json"));
  requireValue([sources, questions, originalLabels, thresholds].every((f) => f.corpus === manifest.corpus), "Dev corpus IDs differ");
  requireValue(originalLabels.split === "dev" && questions.questions.every((q) => q.split === "dev") && originalLabels.labels.every((l) => l.split === "dev"), "Test records cannot enter the developer package");
  const questionIDs = questions.questions.map((q) => q.id);
  const labelIDs = originalLabels.labels.map((l) => l.question_id);
  requireValue(new Set(questionIDs).size === questionIDs.length && new Set(labelIDs).size === labelIDs.length && JSON.stringify([...questionIDs].sort()) === JSON.stringify([...labelIDs].sort()), "Dev questions and labels must match exactly");
  requireValue(manifest.counts.dev === questions.questions.length && manifest.counts.test === 0, "Dev package counts differ");
  requireValue(thresholds.status === "FROZEN_OWNER_APPROVED_BEFORE_MEASUREMENT", "Dev thresholds are not frozen");
  const spec = thresholds.retrieval_dev;
  requireValue(spec.k === 5 && Number.isInteger(spec.minimum) && spec.minimum > 0 && spec.minimum <= spec.denominator && spec.denominator === spec.eligible_question_ids.length, "Invalid frozen retrieval threshold");
  const sourcesHash = manifest.files["sources.json"].sha256;
  const labels = { schema_version: 1, split: "dev", labels: originalLabels.labels.map((label) => {
    requireValue(label.sources_sha256 === sourcesHash && Array.isArray(label.evidence_groups), "Frozen label source hash/groups mismatch");
    const groups = label.evidence_groups.map((group) => {
      requireValue(typeof group.id === "string" && Array.isArray(group.any_of) && group.any_of.length > 0, "Empty evidence group");
      return { id: group.id, any_of: group.any_of.map((locator) => citationFor(locator, sources, sourcesHash)) };
    });
    return { ...label, critical: label.critical ?? [], evidence_groups: groups,
      // The flattened list is representative only. Group logic is authoritative.
      must_cite: groups.map((group) => group.any_of[0]) };
  }) };
  const eligible = labels.labels.filter((l) => l.evidence_groups.length > 0).map((l) => l.question_id).sort();
  requireValue(new Set(spec.eligible_question_ids).size === spec.denominator && JSON.stringify(eligible) === JSON.stringify([...spec.eligible_question_ids].sort()), "Frozen retrieval eligibility differs from evidence groups");
  return { sources, questions, labels, thresholds, manifest,
    hashes: { sources: sourcesHash, questions: manifest.files["questions.json"].sha256, labels: manifest.files["labels.dev.json"].sha256,
      thresholds: manifest.files["thresholds.dev.json"].sha256, freeze: expectedManifestHash } };
}
