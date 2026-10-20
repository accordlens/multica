// @vitest-environment node
import { expect, it } from "vitest";
import { WakeupConditionSchema, IssueWorkflowSchema } from "../api/schemas";

const handoff = {
  type: "review_handoff",
  handoff: {
    thread_id: "11111111-1111-4111-8111-111111111111",
    sender_id: "22222222-2222-4222-8222-222222222222",
    workflow_id: "33333333-3333-4333-8333-333333333333",
    workflow_version: "a".repeat(64),
    source_revision: "b".repeat(40),
  },
};

it("preserves the pinned handoff scope from the API", () => {
  expect(WakeupConditionSchema.parse(handoff)).toEqual(handoff);
});

it("rejects malformed or missing handoff provenance", () => {
  for (const invalid of [
    { ...handoff, handoff: undefined },
    { ...handoff, handoff: { ...handoff.handoff, workflow_version: "latest" } },
    { ...handoff, handoff: { ...handoff.handoff, source_revision: "abc123" } },
    { ...handoff, handoff: { ...handoff.handoff, sender_id: "QA" } },
  ]) {
    expect(WakeupConditionSchema.safeParse(invalid).success).toBe(false);
  }
});

it("accepts older workflow responses and discards an invalid optional version", () => {
  const workflow = { id: "workflow", workspace_id: "workspace", name: "Review", created_at: "now", updated_at: "now" };
  expect(IssueWorkflowSchema.parse(workflow).definition_version).toBeUndefined();
  expect(IssueWorkflowSchema.parse({ ...workflow, definition_version: "bad" }).definition_version).toBeUndefined();
  expect(IssueWorkflowSchema.parse({ ...workflow, definition_version: "a".repeat(64) }).definition_version).toBe("a".repeat(64));
});
