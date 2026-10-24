// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { issueRelationsOptions, issueProvenanceOptions } from "../issues/relations";
import { QueryClient } from "@tanstack/react-query";
import { onIssueAuxiliaryRevision } from "../issues/ws-updaters";

afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");
const respond = (body: unknown, status = 200) => vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status })));

it("never turns malformed readiness into permission to run", async () => {
  respond({ relations: [], ready: "true", step: "review" });
  await expect(client.listIssueRelations("issue")).resolves.toMatchObject({ ready: false, available: false });
});

it("retains unknown server conditions for a future server without crashing", async () => {
  respond({ relations: [{ id: "edge", type: "future_relation", issue: { id: "other", identifier: "MUL-2", title: "Input", status: "custom_done", revision: 2 }, condition: { kind: "future_condition" }, satisfied: false, reason: "future_wait", created_at: "2026-10-20T00:00:00Z" }], ready: false, step: "review" });
  await expect(client.listIssueRelations("issue")).resolves.toMatchObject({ available: true, relations: [{ condition: { kind: "future_condition" }, satisfied: false }] });
});

it("rejects malformed write acknowledgements and real permission errors", async () => {
  respond({ id: null });
  await expect(client.createIssueRelation("issue", { issue_id: "other", type: "related", condition: { kind: "done" } })).rejects.toThrow("Invalid relation response");
  respond({ removed: "true" });
  await expect(client.removeIssueRelation("issue", "edge")).rejects.toThrow();
  respond({ error: "human acceptance required" }, 403);
  await expect(client.acceptIssueRelation("issue", "edge", 1,"a".repeat(64))).rejects.toThrow();
});

it("preserves immutable evidence from each native run and fails visibly on malformed links", async () => {
  const evidence = { source_sha: "a".repeat(40), report_sha256: "b".repeat(64) };
  const definition = "c".repeat(64);
  respond({ links: [], artifacts: [], runs: [{ run_id: "run", step: "test", status: "completed", created_at: "2026-10-20T00:00:00Z", workflow_id: "workflow", definition_sha256: definition, workflow_evidence: evidence }], has_more: false, offset: 0 });
  await expect(client.listIssueProvenance("issue")).resolves.toMatchObject({ available: true, runs: [{ workflow_id: "workflow", definition_sha256: definition, workflow_evidence: evidence }] });
  respond({ links: "bad" });
  await expect(client.listIssueProvenance("issue")).resolves.toMatchObject({ available: false });
});

it("scopes both projections to the workspace and disables incomplete identities", () => {
  expect(issueRelationsOptions("a", "i").queryKey).not.toEqual(issueRelationsOptions("b", "i").queryKey);
  expect(issueProvenanceOptions("a", "i").queryKey).not.toEqual(issueProvenanceOptions("b", "i").queryKey);
  expect(issueRelationsOptions("", "i").enabled).toBe(false);
});

it("refreshes dependent acceptance after auxiliary source edits within the same workspace", () => {
  const qc = new QueryClient();
  const dependent = issueRelationsOptions("a", "dependent").queryKey;
  const foreign = issueRelationsOptions("b", "dependent").queryKey;
  const ready = { relations: [], ready: true, reason: "", step: "review", available: true };
  qc.setQueryData(dependent, ready);
  qc.setQueryData(foreign, ready);
  onIssueAuxiliaryRevision(qc, "a", "source", 9);
  expect(qc.getQueryState(dependent)?.isInvalidated).toBe(true);
  expect(qc.getQueryState(foreign)?.isInvalidated).toBe(false);
  qc.clear();
});
