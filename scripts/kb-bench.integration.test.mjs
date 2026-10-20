import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { backendInput, project } from "./kb-bench.mjs";

test("real PostgreSQL: revisions, tombstone, workspace denial, and repeatable cleanup on an independent mini-corpus", {
  skip: process.env.KBBENCH_RUN_INTEGRATION !== "1",
}, (t) => {
  const directory = mkdtempSync(resolve(".kbbench-integration-"));
  const binary = resolve(process.env.KBBENCH_BACKEND_BIN ?? "server/bin/kbbench");
  const at = "2025-06-01T00:00:00Z";
  const sources = { corpus: "independent-integration-mini", synthetic: true,
    workspaces: { public: { members: ["reader"] }, private: { members: ["other"] } },
    issues: [
      { id: "MIN-1", workspace: "public", title: "Orchard plan", description_revisions: [{ rev: 1, text: "Orange orchard", at }], comments: [
        { id: "edited", at, rev: 2, edited_at: "2025-06-02T00:00:00Z", revisions: [{ rev: 1, text: "Old orange draft" }, { rev: 2, text: "Current orange plan" }] },
        { id: "deleted", at: "2025-06-01T12:00:00Z", rev: 1, text: "Removed orange instruction", deleted_at: "2025-06-02T00:00:00Z" },
      ] },
      { id: "MIN-2", workspace: "private", title: "Orange secret", description_revisions: [{ rev: 1, text: "Confidential orange plan", at }], comments: [] },
      { id: "MIN-3", workspace: "public", title: "Orchard plan", description_revisions: [{ rev: 1, text: "Orange orchard peer", at }], comments: [] },
    ] };
  const questions = { corpus: sources.corpus, asked_at_default: "2025-06-03T00:00:00Z", questions: [
    { id: "independent-dev", split: "dev", q: "orange", asker: { id: "reader", ws: ["public"] } },
  ] };
  const plan = project(sources, questions);
  const invoke = (args, manifest, input) => spawnSync(binary, args, { input, encoding: "utf8", timeout: 60_000,
    env: { ...process.env, KBBENCH_FIXTURE_MANIFEST: manifest } });
  try {
    let firstOrder;
    // A second full import proves cleanup left no fixture application rows.
    for (const attempt of ["first", "second"]) {
      const manifest = join(directory, `${attempt}.json`);
      const child = invoke([], manifest, JSON.stringify(backendInput(plan)));
      assert.equal(child.status, 0, child.stderr);
      const result = JSON.parse(child.stdout);
      assert.deepEqual(result.supported_source_kinds, ["description", "comment"]);
      const row = result.responses[0];
      assert.equal(row.queries[0].status, 200);
      assert.equal(row.denied_probes.length, 1);
      assert.equal(row.denied_probes[0].status, 404);
      const order = row.hits.map((h) => h.issue_id);
      assert.deepEqual([...order].sort(), ["MIN-1", "MIN-3"]);
      if (firstOrder) assert.deepEqual(order, firstOrder, "ranking ties are reproducible across imports");
      firstOrder = order;
      const primary = row.hits.find((h) => h.issue_id === "MIN-1");
      assert.deepEqual(primary.sources.map((s) => [s.source_id, s.revision]), [["MIN-1.description", 1], ["edited", 2]]);
      assert.equal(primary.sources[1].text, "Current orange plan");
      assert.ok(!JSON.stringify(row).includes("Confidential orange plan"));
      assert.ok(!JSON.stringify(row).includes("Old orange draft"));
      assert.ok(!JSON.stringify(row.hits).includes("Removed orange instruction"));
      // Existing SearchIssues can expose tombstoned text in unversioned raw
      // snippets. Keep and report this defect; do not call safe hydration a
      // passing product-level deletion test. A future fix may remove it.
      const exposed = row.queries.some((q) => JSON.stringify(q.body).includes("Removed orange instruction"));
      assert.deepEqual(row.raw_deleted_text_exposures, exposed ? ["deleted"] : []);
      t.diagnostic(`raw backend tombstone exposure detected: ${exposed}; filtered source hydration excludes it`);
      assert.equal(JSON.parse(readFileSync(manifest, "utf8")).status, "cleaned");
      assert.equal(readFileSync(`${manifest}.queries.jsonl`, "utf8").trim().split("\n").length, 2);
      assert.equal(invoke(["--cleanup", manifest], manifest).status, 0, "cleanup is idempotent");
    }
    const multiMember = structuredClone(plan);
    multiMember.workspaces.private.members.push("reader");
    const selectedManifest = join(directory, "selected-workspace.json");
    const selected = invoke([], selectedManifest, JSON.stringify(backendInput(multiMember)));
    assert.equal(selected.status, 0, selected.stderr);
    const selectedRow = JSON.parse(selected.stdout).responses[0];
    assert.equal(selectedRow.queries.length, 1);
    assert.equal(selectedRow.denied_probes.length, 0);
    const selectedTrace = readFileSync(`${selectedManifest}.queries.jsonl`, "utf8");
    assert.equal(selectedTrace.trim().split("\n").length, 1, "do not query an authorized but unselected workspace");
    assert.ok(!selectedTrace.includes("Confidential orange plan"));
    const bad = structuredClone(plan);
    bad.plans[0].documents[0].issue_id = "bad-identity";
    const failedManifest = join(directory, "failed.json");
    const failed = invoke([], failedManifest, JSON.stringify(backendInput(bad)));
    assert.notEqual(failed.status, 0, "failed imports must not look successful");
    assert.equal(JSON.parse(readFileSync(failedManifest, "utf8")).status, "cleaned", "error paths clean their own committed setup");
  } finally { rmSync(directory, { recursive: true, force: true }); }
});
